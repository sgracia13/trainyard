package server

import (
	"bufio"
	"errors"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgracia13/trainyard/internal/client"
	"github.com/sgracia13/trainyard/internal/gitops"
	"github.com/sgracia13/trainyard/internal/host"
	"github.com/sgracia13/trainyard/internal/mcp"
	"github.com/sgracia13/trainyard/internal/queue"
	"github.com/sgracia13/trainyard/internal/store"
)

// The whole flow against a fake code host: two people, overlapping claims, a test
// failure sent back, landing, re-testing on top of new main, and giving up after 3 tries.

const repo = "acme/shop"

type env struct {
	t       *testing.T
	ctx     context.Context
	root    string
	fake    *host.Fake
	srv     *Server
	url     string
	tokens  map[string]string
	clones  map[string]string
}

// Saved before tests point HOME at a temp folder, so `go build` still finds its caches.
var goEnv = func() []string {
	var out []string
	for _, k := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
		if v, err := exec.Command("go", "env", k).Output(); err == nil {
			out = append(out, k+"="+strings.TrimSpace(string(v)))
		}
	}
	return append(out, "GOFLAGS=-mod=mod", "GOPROXY=off")
}()

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitops.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_AUTHOR_NAME", "Dev")
	t.Setenv("GIT_AUTHOR_EMAIL", "dev@example.com")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.WriteFile(filepath.Join(root, ".gitconfig"), []byte("[user]\n name = Dev\n email = dev@example.com\n[init]\n defaultBranch = main\n"), 0o644)

	bare := filepath.Join(root, "remote", repo+".git")
	os.MkdirAll(bare, 0o755)
	run(t, bare, "init", "-q", "--bare", "-b", "main")
	seed := filepath.Join(root, "seed")
	os.MkdirAll(filepath.Join(seed, "src"), 0o755)
	run(t, seed, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(seed, "src", "login.txt"), []byte("title: Log in\nline2\nline3\nline4\nbutton: Go\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "check.sh"), []byte("out=$(grep -rn BUG src docs 2>/dev/null)\nif [ -n \"$out\" ]; then echo \"$out\"; echo 'FAIL: found BUG'; exit 1; fi\necho ok\n"), 0o644)
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "-q", "-m", "seed")
	run(t, seed, "push", "-q", bare, "main")

	fake := host.NewFake(filepath.Join(root, "remote"), "sh check.sh")
	st, err := store.Open(filepath.Join(root, "ty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &Config{DataDir: filepath.Join(root, "data"), Repos: []queue.RepoConfig{{Name: repo}}}
	cfg.Defaults()
	srv := New(cfg, st, fake)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	e := &env{t: t, ctx: context.Background(), root: root, fake: fake, srv: srv, url: hs.URL, tokens: map[string]string{}, clones: map[string]string{}}
	for _, who := range []string{"maya", "sam"} {
		tok, err := st.AddUser(who)
		if err != nil {
			t.Fatal(err)
		}
		e.tokens[who] = tok
		dir := filepath.Join(root, who, "shop")
		os.MkdirAll(filepath.Dir(dir), 0o755)
		run(t, filepath.Dir(dir), "clone", "-q", bare, "shop")
		e.clones[who] = dir
	}
	return e
}

func (e *env) client(who string) *client.Client {
	return &client.Client{Server: e.url, Token: e.tokens[who], HTTP: &http.Client{}}
}

// start creates a job and its workspace for a person, like `trainyard start`.
func (e *env) start(who, title, agent string) (*client.JobFile, string) {
	e.t.Helper()
	c := e.client(who)
	res, err := c.StartJob(repo, title, agent, "copilot", "test-model")
	if err != nil {
		e.t.Fatal(err)
	}
	jf := client.JobFile{JobID: res.Job.ID, Repo: repo, Branch: res.Job.Branch, Main: res.Main, Title: title, Agent: agent}
	ws, err := client.CreateWorkspace(e.clones[who], jf)
	if err != nil {
		e.t.Fatal(err)
	}
	got, _, err := client.FindJob(ws)
	if err != nil {
		e.t.Fatal(err)
	}
	return got, ws
}

func (e *env) edit(ws, file, from, to string) {
	e.t.Helper()
	p := filepath.Join(ws, file)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	s := string(b)
	if from == "" {
		s += to
	} else {
		if !strings.Contains(s, from) {
			e.t.Fatalf("%s doesn't contain %q", file, from)
		}
		s = strings.Replace(s, from, to, 1)
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) submit(who string, jf *client.JobFile, ws, summary string) {
	e.t.Helper()
	if _, err := mcp.SubmitWork(e.client(who), jf, ws, summary); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.srv.TickAll(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) job(id string) *store.Job {
	e.t.Helper()
	j, err := e.srv.Store.Job(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) wantState(id, state string) *store.Job {
	e.t.Helper()
	j := e.job(id)
	if j.State != state {
		fb, _ := e.srv.Store.LatestFeedback(id)
		e.t.Fatalf("job %s is %s, want %s (feedback: %+v)", id, j.State, state, fb)
	}
	return j
}

func TestFullFlow(t *testing.T) {
	e := setup(t)

	// 1. Two agents claim overlapping files; both are told.
	j1, ws1 := e.start("maya", "Rewrite login page text", "Agent C")
	j2, ws2 := e.start("sam", "Fix the login bug", "Agent B")
	if ws1 == ws2 {
		t.Fatal("workspaces should differ")
	}
	if j1.Port == j2.Port && filepath.Dir(ws1) == filepath.Dir(ws2) {
		t.Fatal("ports should differ within one machine")
	}
	r1, err := e.client("maya").Claim(j1.JobID, []string{"src/login.txt"}, "headings")
	if err != nil || len(r1.Overlaps) != 0 {
		t.Fatalf("first claim: %+v %v", r1, err)
	}
	r2, err := e.client("sam").Claim(j2.JobID, []string{"src/*"}, "session check")
	if err != nil || len(r2.Overlaps) != 1 || r2.Overlaps[0].OtherJob != j1.JobID {
		t.Fatalf("overlap not reported to sam: %+v %v", r2, err)
	}
	fb, _ := e.client("maya").Feedback(j1.JobID)
	if len(fb.Feedback) != 1 || fb.Feedback[0].Kind != "overlap" || !strings.Contains(fb.Feedback[0].Message, "Agent B") {
		t.Fatalf("maya wasn't told about the overlap: %+v", fb)
	}
	if _, err := e.client("sam").Claim(j1.JobID, []string{"x"}, ""); err == nil {
		t.Fatal("sam shouldn't be able to act on maya's job")
	}
	st, _ := e.srv.BuildState("maya")
	if len(st.Overlaps) != 1 || st.Counts.Working != 2 {
		t.Fatalf("state: overlaps=%d working=%d", len(st.Overlaps), st.Counts.Working)
	}

	// 2. Maya's work passes and waits for review.
	e.edit(ws1, "src/login.txt", "title: Log in", "title: Sign in to your account")
	e.submit("maya", j1, ws1, "Clearer login heading")
	e.wantState(j1.JobID, store.Queued)
	e.tick() // rebase, push, PR, tests (the fake CI finishes within the same pass)
	j := e.wantState(j1.JobID, store.Ready)
	if j.PR == 0 || len(j.ChangedFiles) != 1 {
		t.Fatalf("expected PR and 1 changed file: %+v", j)
	}
	if s, _ := e.fake.Status(j.QueueSHA); s != "success" {
		t.Fatalf("trainyard status = %q, want success", s)
	}
	pr := e.fake.PR(repo, j.PR)
	if pr.Head != "trainyard/queue/"+j1.JobID || !strings.Contains(strings.Join(pr.Labels, ","), "agent: Agent C") {
		t.Fatalf("PR: %+v", pr)
	}
	msg := run(t, e.srv.workers[repo].DataDirRepo(), "log", "-1", "--format=%B", j.QueueSHA)
	for _, want := range []string{"Agent: Agent C", "Agent-Tool: copilot", "Agent-Model: test-model", "Started-By: maya", "Job: " + j1.JobID} {
		if !strings.Contains(msg, want) {
			t.Fatalf("commit message missing %q:\n%s", want, msg)
		}
	}

	// 3. Sam's work fails tests and is sent back with the log.
	e.edit(ws2, "src/login.txt", "button: Go", "button: Go BUG")
	e.submit("sam", j2, ws2, "Fix session check")
	e.tick()
	e.tick()
	j = e.wantState(j2.JobID, store.SentBack)
	if j.Attempts != 1 {
		t.Fatalf("attempts = %d", j.Attempts)
	}
	fb, _ = e.client("sam").Feedback(j2.JobID)
	var failed *store.Feedback
	for i := range fb.Feedback {
		if fb.Feedback[i].Kind == "test_failed" {
			failed = &fb.Feedback[i]
		}
	}
	if failed == nil || !strings.Contains(failed.Log, "FAIL: found BUG") {
		t.Fatalf("sam should get the failing log: %+v", fb)
	}
	if sum := e.fake.Summary(repo, j.PR); !strings.Contains(sum, "Sent back to Agent B") || !strings.Contains(sum, "FAIL: found BUG") {
		t.Fatalf("summary should show the failure:\n%s", sum)
	}

	// 4. Maya's PR is merged: landed, claims released.
	if err := e.fake.Merge(e.ctx, repo, e.job(j1.JobID).PR); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.wantState(j1.JobID, store.Landed)
	if cl, _ := e.srv.Store.ActiveClaims(repo); len(cl) != 1 || cl[0].JobID != j2.JobID {
		t.Fatalf("only sam's claim should remain: %+v", cl)
	}

	// 5. Sam fixes it; it's rebased on top of Maya's landed change and passes.
	e.edit(ws2, "src/login.txt", "button: Go BUG", "button: Continue")
	e.submit("sam", j2, ws2, "Fix session check")
	e.tick()
	e.tick()
	j = e.wantState(j2.JobID, store.Ready)
	if sum := e.fake.Summary(repo, j.PR); !strings.Contains(sum, "Ready for review") || !strings.Contains(sum, "Agent B") {
		t.Fatalf("summary comment should say ready:\n%s", sum)
	}
	content := run(t, e.srv.workers[repo].DataDirRepo(), "show", j.QueueSHA+":src/login.txt")
	if !strings.Contains(content, "Sign in to your account") || !strings.Contains(content, "button: Continue") {
		t.Fatalf("sam's queue commit should include maya's landed change:\n%s", content)
	}

	// 6. Two more jobs wait for review; when Sam's lands, only the overlapping one is re-tested.
	j3, ws3 := e.start("maya", "Tweak line 3", "Agent C")
	e.edit(ws3, "src/login.txt", "line3", "line three")
	e.submit("maya", j3, ws3, "line 3")
	j4, ws4 := e.start("maya", "Add docs", "Agent D")
	e.edit(ws4, "docs/readme.txt", "", "hello\n")
	e.submit("maya", j4, ws4, "docs")
	e.tick()
	e.tick()
	e.wantState(j3.JobID, store.Ready)
	e.wantState(j4.JobID, store.Ready)
	if err := e.fake.Merge(e.ctx, repo, e.job(j2.JobID).PR); err != nil {
		t.Fatal(err)
	}
	e.tick() // sees the merge and the new main in one pass
	e.wantState(j2.JobID, store.Landed)
	e.wantState(j3.JobID, store.Queued) // touches src/login.txt, which just changed on main
	e.wantState(j4.JobID, store.Ready)  // docs only: unaffected
	e.tick()
	e.tick()
	e.wantState(j3.JobID, store.Ready)

	// 7. A job that keeps failing goes to a person after 3 tries.
	j5, ws5 := e.start("sam", "Risky change", "Agent E")
	for try := 1; try <= 3; try++ {
		e.edit(ws5, "src/new.txt", "", "BUG "+string(rune('0'+try))+"\n")
		e.submit("sam", j5, ws5, "try")
		e.tick()
		e.tick()
	}
	j = e.wantState(j5.JobID, store.NeedsPerson)
	if s, d := e.fake.Status(j.QueueSHA); s != "failure" || !strings.Contains(d, "needs a person") {
		t.Fatalf("status %q %q", s, d)
	}
	st, _ = e.srv.BuildState("sam")
	if st.Counts.NeedsYou < 3 { // j3, j4 ready + j5 needs a person
		t.Fatalf("needs you = %d", st.Counts.NeedsYou)
	}
}

func TestConflictSentBack(t *testing.T) {
	e := setup(t)
	a, wsA := e.start("maya", "A", "Agent A")
	b, wsB := e.start("sam", "B", "Agent B")
	e.edit(wsA, "src/login.txt", "title: Log in", "title: AAA")
	e.edit(wsB, "src/login.txt", "title: Log in", "title: BBB")
	e.submit("maya", a, wsA, "a")
	e.tick()
	e.tick()
	if err := e.fake.Merge(e.ctx, repo, e.job(a.JobID).PR); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.submit("sam", b, wsB, "b")
	e.tick()
	e.wantState(b.JobID, store.SentBack)
	fb, _ := e.client("sam").Feedback(b.JobID)
	if len(fb.Feedback) == 0 || fb.Feedback[len(fb.Feedback)-1].Kind != "conflict" || !strings.Contains(fb.Feedback[len(fb.Feedback)-1].Message, "src/login.txt") {
		t.Fatalf("expected a conflict naming the file: %+v", fb)
	}
}

func TestClaimsExpire(t *testing.T) {
	e := setup(t)
	j, _ := e.start("maya", "Quiet agent", "Agent Q")
	if _, err := e.client("maya").Claim(j.JobID, []string{"src/*"}, ""); err != nil {
		t.Fatal(err)
	}
	w := e.srv.workers[repo]
	w.ClaimTTL = 30 * time.Minute
	w.Now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	e.tick()
	if cl, _ := e.srv.Store.ActiveClaims(repo); len(cl) != 0 {
		t.Fatalf("claims should have expired: %+v", cl)
	}
	fb, _ := e.client("maya").Feedback(j.JobID)
	if len(fb.Feedback) != 1 || !strings.Contains(fb.Feedback[0].Message, "released") {
		t.Fatalf("agent should be told: %+v", fb)
	}
}

// TestMCP drives the real `trainyard mcp` binary like an agent would.
func TestMCP(t *testing.T) {
	e := setup(t)
	bin := filepath.Join(e.root, "trainyard")
	build := exec.Command("go", "build", "-o", bin, "github.com/sgracia13/trainyard/cmd/trainyard")
	build.Env = append(os.Environ(), goEnv...)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "mcp", "--dir", e.clones["maya"])
	cmd.Env = append(os.Environ(), "TRAINYARD_SERVER="+e.url, "TRAINYARD_TOKEN="+e.tokens["maya"], "TRAINYARD_AGENT=Agent M")
	in, _ := cmd.StdinPipe()
	outPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	rd := bufio.NewReader(outPipe)
	id := 0
	call := func(method string, params any) map[string]any {
		t.Helper()
		id++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		io.WriteString(in, string(b)+"\n")
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		json.Unmarshal([]byte(line), &resp)
		if resp["error"] != nil {
			t.Fatalf("%s: %v", method, resp["error"])
		}
		return resp["result"].(map[string]any)
	}
	text := func(r map[string]any) string {
		c := r["content"].([]any)[0].(map[string]any)
		if r["isError"] == true {
			t.Fatalf("tool error: %s", c["text"])
		}
		return c["text"].(string)
	}
	init := call("initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "copilot-test"}, "capabilities": map[string]any{}})
	if init["serverInfo"].(map[string]any)["name"] != "trainyard" {
		t.Fatalf("init: %v", init)
	}
	io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	tools := call("tools/list", map[string]any{})["tools"].([]any)
	if len(tools) != 7 {
		t.Fatalf("tools: %d", len(tools))
	}
	out := text(call("tools/call", map[string]any{"name": "start_job", "arguments": map[string]any{"title": "Agent via MCP", "files": []string{"src/login.txt"}}}))
	if !strings.Contains(out, "Workspace:") || !strings.Contains(out, "Claimed: src/login.txt") {
		t.Fatalf("start_job: %s", out)
	}
	ws := strings.TrimSpace(strings.SplitN(strings.SplitN(out, "Workspace: ", 2)[1], "\n", 2)[0])
	e.edit(ws, "src/login.txt", "line2", "line two")
	out = text(call("tools/call", map[string]any{"name": "submit", "arguments": map[string]any{"summary": "Edit via MCP"}}))
	if !strings.Contains(out, "Submitted") {
		t.Fatalf("submit: %s", out)
	}
	e.tick()
	e.tick()
	out = text(call("tools/call", map[string]any{"name": "get_feedback", "arguments": map[string]any{}}))
	if !strings.Contains(out, "Passed") {
		t.Fatalf("feedback: %s", out)
	}
	j := e.job("j1")
	if j.Agent != "Agent M" || j.AgentTool != "copilot-test" {
		t.Fatalf("agent identity: %+v", j)
	}
}

// A token that can't post commit statuses must not stall the queue (seen on a live run).
func TestStatusFailureDoesNotBlock(t *testing.T) {
	e := setup(t)
	e.fake.StatusErr = errors.New("HTTP 403: not permitted")
	j, ws := e.start("maya", "A", "Agent A")
	e.edit(ws, "src/login.txt", "line2", "line two")
	e.submit("maya", j, ws, "a")
	e.tick()
	e.wantState(j.JobID, store.Ready)
	e.tick()
	if n := len(e.fake.Comments); n != 0 {
		t.Fatalf("no retry comments expected, got %d: %v", n, e.fake.Comments)
	}
}

// Found on a live run: stopping the server mid-pass made a cancelled git command
// look like "branch not pushed". A cancelled pass must leave jobs untouched.
func TestShutdownDoesNotSendBack(t *testing.T) {
	e := setup(t)
	j, ws := e.start("maya", "A", "Agent A")
	e.edit(ws, "src/login.txt", "line2", "line two")
	e.submit("maya", j, ws, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = e.srv.TickAll(ctx)
	e.wantState(j.JobID, store.Queued)
}

// Agents that forget to claim are still caught, and overlaps are checked line by line.
func TestDetectedOverlapAndLineCheck(t *testing.T) {
	e := setup(t)
	a, wsA := e.start("maya", "Login heading", "Agent C")
	if _, err := e.client("maya").Claim(a.JobID, []string{"src/login.txt"}, "heading"); err != nil {
		t.Fatal(err)
	}
	e.edit(wsA, "src/login.txt", "title: Log in", "title: Welcome back")
	if _, err := mcp.CheckIn(e.client("maya"), a, wsA, "heading"); err != nil {
		t.Fatal(err)
	}

	// Sam's agent never claims anything; it edits a different line of the same file.
	b, wsB := e.start("sam", "Button label", "Agent B")
	e.edit(wsB, "src/login.txt", "button: Go", "button: Continue")
	out, err := mcp.CheckIn(e.client("sam"), b, wsB, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claimed it for you", "src/login.txt", "merges cleanly"} {
		if !strings.Contains(out, want) {
			t.Fatalf("check-in output missing %q:\n%s", want, out)
		}
	}
	cl, _ := e.srv.Store.ActiveClaims(repo)
	var detected bool
	for _, c := range cl {
		if c.JobID == b.JobID && c.Source == "detected" && c.Pattern == "src/login.txt" {
			detected = true
		}
	}
	if !detected {
		t.Fatalf("expected a detected claim: %+v", cl)
	}
	fb, _ := e.client("maya").Feedback(a.JobID)
	if len(fb.Feedback) == 0 || !strings.Contains(fb.Feedback[0].Message, "is editing src/login.txt") {
		t.Fatalf("maya should hear about sam's unclaimed edit: %+v", fb)
	}

	// A third agent changes the same line as Maya's: conflict predicted for both.
	c, wsC := e.start("maya", "Heading again", "Agent D")
	e.edit(wsC, "src/login.txt", "title: Log in", "title: Hello")
	out, err = mcp.CheckIn(e.client("maya"), c, wsC, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Conflict ahead") || !strings.Contains(out, "Agent C") {
		t.Fatalf("expected a predicted conflict with Agent C:\n%s", out)
	}
	fb, _ = e.client("maya").Feedback(a.JobID)
	var warned bool
	for _, f := range fb.Feedback {
		if f.Kind == "conflict_ahead" && strings.Contains(f.Message, "Agent D") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("Agent C should be warned about Agent D: %+v", fb)
	}
	// Checking in again without changes doesn't repeat the warning.
	_, _ = mcp.CheckIn(e.client("maya"), c, wsC, "")
	fb, _ = e.client("maya").Feedback(a.JobID)
	if len(fb.Feedback) != 0 {
		t.Fatalf("no repeat warnings expected: %+v", fb)
	}
}
