// Package mcp exposes Trainyard to AI agents as MCP tools over stdio
// (JSON-RPC 2.0, one message per line). Works with Copilot agent mode in VS Code,
// Claude Code, Cursor, Codex and any other MCP client.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/sgracia13/trainyard/internal/client"
)

const Instructions = `Trainyard coordinates AI agents working on the same repo.
Workflow: start_job (once) -> work only inside the workspace folder it returns -> claim the files you will change before editing them ->
heartbeat after editing files (Trainyard then checks your actual edits against other agents', line by line) -> submit when done -> get_feedback until the job lands.
If submit is sent back, fix what the feedback says in the same workspace and submit again. Claims are a heads-up, not a lock: if claim
reports an overlap, avoid changing the same lines as the other agent.`

type Server struct {
	Dir     string // default workspace (where the MCP server was started)
	Version string

	mu         sync.Mutex
	clientName string
	out        io.Writer
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from r and writes responses to w until r closes.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	s.out = w
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.reply(nil, nil, &rpcErr{-32700, "parse error"})
			continue
		}
		if len(req.ID) == 0 { // notification
			continue
		}
		res, e := s.handle(req)
		s.reply(req.ID, res, e)
	}
	return sc.Err()
}

func (s *Server) reply(id json.RawMessage, result any, e *rpcErr) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		msg["id"] = nil
	}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	b, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "%s\n", b)
}

var supported = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

func (s *Server) handle(req rpcReq) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.clientName = p.ClientInfo.Name
		v := p.ProtocolVersion
		if !supported[v] {
			v = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "trainyard", "version": s.Version},
			"instructions":    Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "bad params"}
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Error: " + err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcErr{-32601, "method not found: " + req.Method}
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	str     = func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	strList = func(d string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": d}
	}
	wsProp = str("Workspace folder of the job. Optional: defaults to the folder from start_job or where Trainyard was started.")
)

var toolDefs = []map[string]any{
	{"name": "start_job", "description": "Register a new job with Trainyard and create its own workspace folder (a separate copy of the repo on a new branch from the latest main). Call once at the start of a task. Then make ALL edits inside the returned folder.",
		"inputSchema": schema(map[string]any{"title": str("Short description of the task, e.g. 'Fix the login redirect bug'"),
			"files": strList("Optional: files or folders you already know you'll change (paths or globs like src/auth/*). Claimed right away.")}, "title")},
	{"name": "claim", "description": "Tell Trainyard which files or folders you are about to change, BEFORE editing them. Returns any overlap with other agents' claims. Claims are a heads-up, not a lock.",
		"inputSchema": schema(map[string]any{"files": strList("Repo-relative paths or globs, e.g. ['src/auth/LoginPage.tsx', 'src/auth/*']"),
			"reason": str("One line on what you're changing, shown to other agents"), "workspace": wsProp}, "files")},
	{"name": "heartbeat", "description": "Check in after editing files and every few minutes on long tasks. Trainyard looks at what you actually changed, claims any files you forgot to claim, and checks line by line whether your edits will conflict with another agent's. Optionally say what you're doing.",
		"inputSchema": schema(map[string]any{"note": str("What you're doing right now"), "workspace": wsProp})},
	{"name": "submit", "description": "Finish your work: commits any uncommitted changes, pushes your branch and puts it in the merge queue, where it's tested on top of the latest main. Then call get_feedback to see the result.",
		"inputSchema": schema(map[string]any{"summary": str("What you changed and why, for reviewers. Also used as the commit message for uncommitted changes."), "workspace": wsProp}, "summary")},
	{"name": "get_feedback", "description": "Get your job's status and any new feedback: test failures with logs, merge conflicts, overlap notices from other agents, or review status.",
		"inputSchema": schema(map[string]any{"workspace": wsProp})},
	{"name": "release", "description": "Release claims you no longer need (all of them if files is empty).",
		"inputSchema": schema(map[string]any{"files": strList("Claims to release; empty releases all"), "workspace": wsProp})},
	{"name": "list_claims", "description": "List every active claim in this repo: which agent is working on which files.",
		"inputSchema": schema(map[string]any{"workspace": wsProp})},
}

func argStr(a map[string]any, k string) string { v, _ := a[k].(string); return strings.TrimSpace(v) }

func argList(a map[string]any, k string) []string {
	var out []string
	switch v := a[k].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func (s *Server) job(a map[string]any) (*client.JobFile, string, error) {
	dir := argStr(a, "workspace")
	if dir == "" {
		dir = s.Dir
	}
	return client.FindJob(dir)
}

func (s *Server) call(name string, a map[string]any) (string, error) {
	c, err := client.Load()
	if err != nil {
		return "", err
	}
	switch name {
	case "start_job":
		return s.startJob(c, a)
	case "list_claims":
		repo := ""
		if jf, _, err := s.job(a); err == nil {
			repo = jf.Repo
		} else if repo, err = client.DetectRepo(s.Dir); err != nil {
			return "", err
		}
		cl, err := c.Claims(repo)
		if err != nil {
			return "", err
		}
		if len(cl) == 0 {
			return "No active claims in " + repo + ".", nil
		}
		var b strings.Builder
		for _, x := range cl {
			fmt.Fprintf(&b, "- %s: %s (%s, job %s: %s)\n", x.Pattern, x.Agent, x.StartedBy, x.JobID, x.JobTitle)
		}
		return b.String(), nil
	}
	jf, ws, err := s.job(a)
	if err != nil {
		return "", err
	}
	switch name {
	case "claim":
		files := argList(a, "files")
		if len(files) == 0 {
			return "", fmt.Errorf("list the files or folders you'll change in \"files\"")
		}
		r, err := c.Claim(jf.JobID, files, argStr(a, "reason"))
		if err != nil {
			return "", err
		}
		return FormatClaim(r), nil
	case "heartbeat":
		return CheckIn(c, jf, ws, argStr(a, "note"))
	case "submit":
		return SubmitWork(c, jf, ws, argStr(a, "summary"))
	case "get_feedback":
		r, err := c.Feedback(jf.JobID)
		if err != nil {
			return "", err
		}
		return FormatFeedback(r), nil
	case "release":
		n, err := c.Release(jf.JobID, argList(a, "files"))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Released %d claim(s).", n), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) startJob(c *client.Client, a map[string]any) (string, error) {
	title := argStr(a, "title")
	if title == "" {
		return "", fmt.Errorf("give the job a title")
	}
	repo, err := client.DetectRepo(s.Dir)
	if err != nil {
		return "", err
	}
	tool := os.Getenv("TRAINYARD_AGENT_TOOL")
	if tool == "" {
		tool = s.clientName
	}
	res, err := c.StartJob(repo, title, os.Getenv("TRAINYARD_AGENT"), tool, os.Getenv("TRAINYARD_AGENT_MODEL"))
	if err != nil {
		return "", err
	}
	j := res.Job
	path, err := client.CreateWorkspace(s.Dir, client.JobFile{JobID: j.ID, Repo: j.Repo, Branch: j.Branch, Main: res.Main, Title: j.Title, Agent: j.Agent})
	if err != nil {
		_ = c.Abandon(j.ID)
		return "", fmt.Errorf("couldn't create the workspace: %w", err)
	}
	s.Dir = path // later calls default to this job
	out := fmt.Sprintf("Started job %s: %s\nWorkspace: %s\nBranch: %s (from the latest %s)\n\nWork ONLY inside that folder. If you run a dev server, use the port in %s/.env.trainyard.\nBefore editing a file, call claim with it.",
		j.ID, j.Title, path, j.Branch, res.Main, path)
	if files := argList(a, "files"); len(files) > 0 {
		r, err := c.Claim(j.ID, files, title)
		if err != nil {
			return out, nil
		}
		out += "\n\n" + FormatClaim(r)
	}
	return out, nil
}

// ---- Shared by the CLI ----

func FormatClaim(r *client.ClaimResult) string {
	var b strings.Builder
	if len(r.Claimed) > 0 {
		fmt.Fprintf(&b, "Claimed: %s.", strings.Join(r.Claimed, ", "))
	} else {
		b.WriteString("Already claimed.")
	}
	if len(r.Overlaps) == 0 {
		b.WriteString(" No overlap with other agents.")
		return b.String()
	}
	b.WriteString("\nHeads-up, this overlaps other work (you can still go ahead; they've been told too):")
	for _, o := range r.Overlaps {
		fmt.Fprintf(&b, "\n- %s overlaps %s held by %s (%s, job %s: %s)", o.Pattern, o.OtherPattern, o.OtherAgent, o.OtherPerson, o.OtherJob, o.OtherTitle)
	}
	b.WriteString("\nAvoid changing the same lines. If theirs lands first, yours is re-tested on top of it automatically.")
	return b.String()
}

var stateText = map[string]string{
	"working":      "Working. Submit when you're done.",
	"queued":       "In the merge queue, waiting to be tested on top of the latest main.",
	"testing":      "Tests are running on top of the latest main. Check again in a minute.",
	"sent_back":    "Sent back. Fix what the feedback below says, then submit again.",
	"ready":        "Passed. The pull request is waiting for a human review. Nothing to do unless review asks for changes.",
	"landed":       "Landed on main. Job done.",
	"needs_person": "Failed too many times; a person has been asked to look. Stop and wait for them.",
	"abandoned":    "Stopped.",
}

func FormatFeedback(r *client.FeedbackResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Status: %s", stateText[r.State])
	if r.PRURL != "" {
		fmt.Fprintf(&b, "\nPull request: %s", r.PRURL)
	}
	if len(r.Feedback) == 0 {
		b.WriteString("\nNo new feedback.")
	}
	for _, f := range r.Feedback {
		fmt.Fprintf(&b, "\n\n[%s] %s", f.Kind, f.Message)
		if f.Log != "" {
			fmt.Fprintf(&b, "\nLog:\n%s", f.Log)
		}
	}
	return b.String()
}

// SubmitWork commits anything uncommitted, pushes the job branch and submits it.
func SubmitWork(c *client.Client, jf *client.JobFile, ws, summary string) (string, error) {
	msg := summary
	if msg == "" {
		msg = jf.Title
	}
	committed, err := client.CommitAll(ws, msg)
	if err != nil {
		return "", fmt.Errorf("couldn't commit your changes: %w", err)
	}
	if err := client.Push(ws, jf.Branch); err != nil {
		return "", fmt.Errorf("couldn't push %s: %w", jf.Branch, err)
	}
	if _, err := c.Submit(jf.JobID, summary); err != nil {
		return "", err
	}
	out := "Submitted to the merge queue. It will be put on top of the latest main and tested; call get_feedback in a minute or two."
	if committed {
		out = "Committed your uncommitted changes. " + out
	}
	return out, nil
}

// CheckIn sends a heartbeat with the workspace's real changes and explains the result.
func CheckIn(c *client.Client, jf *client.JobFile, ws, note string) (string, error) {
	files, sha, err := client.Snapshot(ws, jf)
	if err != nil {
		files, sha = nil, "" // still check in; detection just won't happen this time
	}
	r, err := c.Heartbeat(jf.JobID, note, files, sha)
	if err != nil {
		return "", err
	}
	return FormatCheckIn(r, len(files)), nil
}

func FormatCheckIn(r *client.HeartbeatResult, nFiles int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Checked in. %d changed file(s) seen.", nFiles)
	if len(r.Detected) > 0 {
		fmt.Fprintf(&b, "\nYou hadn't claimed %s, so Trainyard claimed it for you.", strings.Join(r.Detected, ", "))
	}
	for _, o := range r.Overlaps {
		fmt.Fprintf(&b, "\nHeads-up: %s overlaps %s held by %s (%s, job %s: %s). They've been told.", o.Pattern, o.OtherPattern, o.OtherAgent, o.OtherPerson, o.OtherJob, o.OtherTitle)
	}
	for _, l := range r.LineChecks {
		if l.Conflict {
			fmt.Fprintf(&b, "\nConflict ahead: your edits and %s's (job %s) change the same lines in %s. Coordinate before going further.", l.OtherAgent, l.OtherJob, strings.Join(l.Files, ", "))
		} else {
			fmt.Fprintf(&b, "\nLine check vs %s (job %s): same files, different lines. Your work merges cleanly with theirs.", l.OtherAgent, l.OtherJob)
		}
	}
	return b.String()
}
