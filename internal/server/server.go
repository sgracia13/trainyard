// Package server is the Trainyard server: the HTTP API agents and the CLI call,
// the dashboard, and one merge-queue worker per repo.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sgracia13/trainyard/internal/claims"
	"github.com/sgracia13/trainyard/internal/host"
	"github.com/sgracia13/trainyard/internal/queue"
	"github.com/sgracia13/trainyard/internal/store"
	"github.com/sgracia13/trainyard/internal/web"
)

type Server struct {
	Cfg   *Config
	Store *store.Store
	Host  host.Host

	workers map[string]*queue.Worker
	kicks   map[string]chan struct{}
	mu      sync.Mutex // serialises claim checks so two agents can't miss each other
}

func New(cfg *Config, st *store.Store, h host.Host) *Server {
	s := &Server{Cfg: cfg, Store: st, Host: h, workers: map[string]*queue.Worker{}, kicks: map[string]chan struct{}{}}
	for _, r := range cfg.Repos {
		s.workers[r.Name] = &queue.Worker{Repo: r, Store: st, Host: h, DataDir: cfg.DataDir, PublicURL: cfg.PublicURL,
			ClaimTTL: time.Duration(cfg.ClaimTTLMinutes) * time.Minute}
		s.kicks[r.Name] = make(chan struct{}, 1)
	}
	return s
}

// RunWorkers runs every repo's merge queue until ctx ends.
func (s *Server) RunWorkers(ctx context.Context) {
	for name, w := range s.workers {
		go func(name string, w *queue.Worker) {
			t := time.NewTicker(time.Duration(s.Cfg.PollSeconds) * time.Second)
			defer t.Stop()
			for {
				if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
					log.Printf("queue %s: %v", name, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				case <-s.kicks[name]:
				}
			}
		}(name, w)
	}
}

// TickAll runs one pass of every queue now (used by tests).
func (s *Server) TickAll(ctx context.Context) error {
	for _, w := range s.workers {
		if err := w.Tick(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) kick(repo string) {
	if ch, ok := s.kicks[repo]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ---- HTTP ----

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/me", s.auth(s.me))
	mux.HandleFunc("GET /api/repos", s.auth(s.repos))
	mux.HandleFunc("GET /api/state", s.auth(s.state))
	mux.HandleFunc("GET /api/claims", s.auth(s.listClaims))
	mux.HandleFunc("POST /api/jobs", s.auth(s.startJob))
	mux.HandleFunc("GET /api/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("POST /api/jobs/{id}/claims", s.auth(s.claim))
	mux.HandleFunc("POST /api/jobs/{id}/release", s.auth(s.release))
	mux.HandleFunc("POST /api/jobs/{id}/heartbeat", s.auth(s.heartbeat))
	mux.HandleFunc("POST /api/jobs/{id}/submit", s.auth(s.submit))
	mux.HandleFunc("GET /api/jobs/{id}/feedback", s.auth(s.feedback))
	mux.HandleFunc("POST /api/jobs/{id}/abandon", s.auth(s.abandon))
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "trainyard", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	static, _ := fs.Sub(web.Files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page := "static/login.html"
		if _, err := s.userFrom(r); err == nil {
			page = "static/index.html"
		}
		b, _ := web.Files.ReadFile(page)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	return mux
}

type ctxKey struct{}

func (s *Server) userFrom(r *http.Request) (*store.User, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		if c, err := r.Cookie("trainyard"); err == nil {
			tok = c.Value
		}
	}
	if tok == "" {
		return nil, store.ErrNotFound
	}
	return s.Store.UserByToken(tok)
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.userFrom(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "missing or unknown token")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.FormValue("token"))
	if _, err := s.Store.UserByToken(tok); err != nil {
		http.Redirect(w, r, "/?bad=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "trainyard", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.Cfg.PublicURL, "https://"), MaxAge: 60 * 60 * 24 * 30})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u *store.User) { writeJSON(w, 200, u) }

func (s *Server) repos(w http.ResponseWriter, r *http.Request, u *store.User) {
	type repo struct {
		Name     string `json:"name"`
		Main     string `json:"main"`
		CloneURL string `json:"web_url"`
	}
	var out []repo
	for _, rc := range s.Cfg.Repos {
		out = append(out, repo{rc.Name, rc.Main, s.Host.WebURL(rc.Name)})
	}
	writeJSON(w, 200, out)
}

// ---- Jobs ----

type startReq struct {
	Repo       string `json:"repo"`
	Title      string `json:"title"`
	Agent      string `json:"agent"`
	AgentTool  string `json:"agent_tool"`
	AgentModel string `json:"agent_model"`
}

func (s *Server) startJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req startReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rc, ok := s.Cfg.Repo(req.Repo)
	if !ok {
		writeErr(w, 400, fmt.Sprintf("repo %q isn't set up in Trainyard", req.Repo))
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeErr(w, 400, "a job needs a title, e.g. \"Fix the login bug\"")
		return
	}
	if req.Agent == "" {
		req.Agent = u.Name + "'s agent"
	}
	j := &store.Job{Repo: rc.Name, Title: req.Title, Agent: req.Agent, AgentTool: req.AgentTool, AgentModel: req.AgentModel, StartedBy: u.Name}
	if err := s.Store.CreateJob(j); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.Store.Log(j.Repo, j.ID, "started", fmt.Sprintf("%s started %s: %s", u.Name, j.Agent, j.Title))
	writeJSON(w, 201, map[string]any{"job": j, "main": rc.Main})
}

// jobFor loads a job and checks the caller started it.
func (s *Server) jobFor(w http.ResponseWriter, r *http.Request, u *store.User) *store.Job {
	j, err := s.Store.Job(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "no such job")
		return nil
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil
	}
	if j.StartedBy != u.Name {
		writeErr(w, 403, "this job belongs to "+j.StartedBy)
		return nil
	}
	return j
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, u *store.User) {
	j, err := s.Store.Job(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "no such job")
		return
	}
	writeJSON(w, 200, j)
}

type Overlap struct {
	Pattern      string `json:"pattern"`
	OtherPattern string `json:"other_pattern"`
	OtherJob     string `json:"other_job"`
	OtherAgent   string `json:"other_agent"`
	OtherPerson  string `json:"other_person"`
	OtherTitle   string `json:"other_title"`
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	var req struct {
		Patterns []string `json:"patterns"`
		Reason   string   `json:"reason"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Patterns) == 0 {
		writeErr(w, 400, "send the files or folders you plan to change in \"patterns\"")
		return
	}
	if j.State == store.Landed || j.State == store.Abandoned {
		writeErr(w, 409, "this job is "+j.State)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	claimed, overlaps, err := s.addClaims(j, req.Patterns, req.Reason, "claimed")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.Store.Heartbeat(j.ID, req.Reason)
	writeJSON(w, 200, map[string]any{"claimed": claimed, "overlaps": overlaps})
}

// addClaims records claims for a job and tells every other agent whose claims overlap.
// source is "claimed" (the agent said so) or "detected" (Trainyard saw the edits).
// Patterns already covered by one of the job's claims are skipped. Callers hold s.mu.
func (s *Server) addClaims(j *store.Job, patterns []string, reason, source string) ([]string, []Overlap, error) {
	active, err := s.Store.ActiveClaims(j.Repo)
	if err != nil {
		return nil, nil, err
	}
	var mine []string
	for _, c := range active {
		if c.JobID == j.ID {
			mine = append(mine, c.Pattern)
		}
	}
	covered := func(p string) bool {
		for _, m := range mine {
			if m == p || (source == "detected" && claims.Match(m, p)) {
				return true
			}
		}
		return false
	}
	var claimed []string
	overlaps := []Overlap{}
	for _, p := range patterns {
		p = claims.Normalize(p)
		if covered(p) {
			if source == "claimed" { // re-claiming: still report overlaps
				for _, c := range active {
					if c.JobID != j.ID && claims.Overlap(p, c.Pattern) {
						overlaps = append(overlaps, Overlap{p, c.Pattern, c.JobID, c.Agent, c.StartedBy, c.JobTitle})
					}
				}
			}
			continue
		}
		for _, c := range active {
			if c.JobID != j.ID && claims.Overlap(p, c.Pattern) {
				overlaps = append(overlaps, Overlap{p, c.Pattern, c.JobID, c.Agent, c.StartedBy, c.JobTitle})
			}
		}
		mine = append(mine, p)
		if err := s.Store.AddClaim(&store.Claim{JobID: j.ID, Repo: j.Repo, Pattern: p, Reason: reason, Source: source}); err != nil {
			return nil, nil, err
		}
		claimed = append(claimed, p)
	}
	verb := "claimed"
	if source == "detected" {
		verb = "is editing (detected, not claimed)"
	}
	if len(claimed) > 0 {
		s.Store.Log(j.Repo, j.ID, "claimed", fmt.Sprintf("%s %s %s", j.Agent, verb, strings.Join(claimed, ", ")))
	}
	told := map[string]bool{}
	for _, o := range overlaps {
		if told[o.OtherJob] || !contains(claimed, o.Pattern) {
			continue // only announce overlaps that are new
		}
		told[o.OtherJob] = true
		why := ""
		if reason != "" {
			why = fmt.Sprintf(" (%q)", reason)
		}
		action := "just claimed"
		if source == "detected" {
			action = "is editing"
		}
		_ = s.Store.AddFeedback(&store.Feedback{JobID: o.OtherJob, Kind: "overlap",
			Message: fmt.Sprintf("Heads-up: %s (%s, job %s: %s) %s %s, which overlaps your claim on %s%s. Coordinate: avoid changing the same lines, and expect a re-test if theirs lands first.",
				j.Agent, j.StartedBy, j.ID, j.Title, action, o.Pattern, o.OtherPattern, why)})
		if source == "detected" {
			_ = s.Store.AddFeedback(&store.Feedback{JobID: j.ID, Kind: "overlap",
				Message: fmt.Sprintf("Heads-up: you're editing %s without claiming it, and it overlaps %s's claim on %s (%s, job %s: %s). Trainyard claimed it for you; both agents have been told.",
					o.Pattern, o.OtherAgent, o.OtherPattern, o.OtherPerson, o.OtherJob, o.OtherTitle)})
		}
		s.Store.Log(j.Repo, j.ID, "overlap", fmt.Sprintf("%s and %s are both working on %s — both agents told", j.Agent, o.OtherAgent, o.Pattern))
	}
	return claimed, overlaps, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	var req struct {
		Patterns []string `json:"patterns"`
	}
	_ = readJSON(r, &req)
	for i := range req.Patterns {
		req.Patterns[i] = claims.Normalize(req.Patterns[i])
	}
	n, err := s.Store.ReleaseClaims(j.ID, req.Patterns)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n > 0 {
		s.Store.Log(j.Repo, j.ID, "released", fmt.Sprintf("%s released %d claim(s)", j.Agent, n))
	}
	writeJSON(w, 200, map[string]any{"released": n})
}

// heartbeat keeps claims alive. When the CLI or agent tools send the files actually
// changed and a snapshot of the work, Trainyard also claims anything the agent forgot
// to claim, and trial-merges the snapshot with overlapping jobs to predict conflicts.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	var req struct {
		Note   string   `json:"note"`
		Files  []string `json:"files"`
		WipSHA string   `json:"wip_sha"`
	}
	_ = readJSON(r, &req)
	if err := s.Store.Heartbeat(j.ID, req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{"ok": true, "state": j.State, "detected": []string{}, "overlaps": []Overlap{}, "line_checks": []LineResult{}}
	active := j.State == store.Working || j.State == store.SentBack
	if !active || (len(req.Files) == 0 && req.WipSHA == "") {
		writeJSON(w, 200, resp)
		return
	}
	s.mu.Lock()
	detected, overlaps, err := s.addClaims(j, req.Files, "", "detected")
	if err == nil && req.WipSHA != "" {
		j, err = s.Store.Job(j.ID)
		if err == nil {
			j.WipSHA, j.ChangedFiles = req.WipSHA, req.Files
			err = s.Store.UpdateJob(j)
		}
	}
	s.mu.Unlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if detected != nil {
		resp["detected"] = detected
	}
	resp["overlaps"] = overlaps
	resp["line_checks"] = s.checkLines(r.Context(), j)
	writeJSON(w, 200, resp)
}

// LineResult says whether two jobs' work would merge cleanly.
type LineResult struct {
	OtherJob   string   `json:"other_job"`
	OtherAgent string   `json:"other_agent"`
	Conflict   bool     `json:"conflict"`
	Files      []string `json:"files"`
	Changed    bool     `json:"changed"` // result differs from the last check
}

// latestSHA is the newest version of a job's work Trainyard knows about.
func latestSHA(j *store.Job) string {
	switch j.State {
	case store.Working, store.SentBack:
		if j.WipSHA != "" {
			return j.WipSHA
		}
	case store.Queued, store.Testing, store.Ready, store.NeedsPerson:
		return j.QueueSHA
	}
	return ""
}

// checkLines trial-merges a job's latest work with every open job it overlaps.
func (s *Server) checkLines(ctx context.Context, j *store.Job) []LineResult {
	out := []LineResult{}
	w := s.workers[j.Repo]
	mySHA := latestSHA(j)
	if w == nil || mySHA == "" {
		return out
	}
	active, _ := s.Store.ActiveClaims(j.Repo)
	others, _ := s.Store.JobsInState(j.Repo, store.Working, store.SentBack, store.Queued, store.Testing, store.Ready, store.NeedsPerson)
	patterns := func(job *store.Job) []string {
		p := append([]string{}, job.ChangedFiles...)
		for _, c := range active {
			if c.JobID == job.ID {
				p = append(p, c.Pattern)
			}
		}
		return p
	}
	mine := patterns(j)
	for _, o := range others {
		if o.ID == j.ID {
			continue
		}
		theirSHA := latestSHA(o)
		if theirSHA == "" || !anyOverlap(mine, patterns(o)) {
			continue
		}
		prev, _ := s.Store.GetLineCheck(j.ID, o.ID)
		if prev != nil && ((prev.A == j.ID && prev.ShaA == mySHA && prev.ShaB == theirSHA) || (prev.B == j.ID && prev.ShaB == mySHA && prev.ShaA == theirSHA)) {
			out = append(out, LineResult{o.ID, o.Agent, prev.Conflict, prev.Files, false})
			continue
		}
		conflict, files, err := w.TrialMerge(ctx, mySHA, theirSHA)
		if err != nil {
			log.Printf("line check %s vs %s: %v", j.ID, o.ID, err)
			continue
		}
		_ = s.Store.PutLineCheck(j.ID, o.ID, mySHA, theirSHA, conflict, files)
		changed := prev == nil || prev.Conflict != conflict
		out = append(out, LineResult{o.ID, o.Agent, conflict, files, changed})
		if !changed {
			continue
		}
		if conflict {
			msg := func(other *store.Job) string {
				return fmt.Sprintf("Conflict ahead: your edits and %s's (%s, job %s: %s) change the same lines in %s. One of you will have to redo work when the other lands. Coordinate now: agree who goes first, or split the change.",
					other.Agent, other.StartedBy, other.ID, other.Title, strings.Join(files, ", "))
			}
			_ = s.Store.AddFeedback(&store.Feedback{JobID: j.ID, Kind: "conflict_ahead", Message: msg(o)})
			_ = s.Store.AddFeedback(&store.Feedback{JobID: o.ID, Kind: "conflict_ahead", Message: msg(j)})
			s.Store.Log(j.Repo, j.ID, "conflict_ahead", fmt.Sprintf("%s and %s change the same lines in %s — conflict predicted, both told", j.Agent, o.Agent, strings.Join(files, ", ")))
		} else {
			s.Store.Log(j.Repo, j.ID, "lines_clear", fmt.Sprintf("Line check: %s and %s touch the same files but not the same lines — they'll merge cleanly", j.Agent, o.Agent))
		}
	}
	return out
}

func anyOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if claims.Overlap(x, y) {
				return true
			}
		}
	}
	return false
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	var req struct {
		Summary string `json:"summary"`
	}
	_ = readJSON(r, &req)
	switch j.State {
	case store.Working, store.SentBack, store.NeedsPerson, store.Ready:
	default:
		writeErr(w, 409, "can't submit a job that is "+j.State)
		return
	}
	if j.State == store.NeedsPerson {
		j.Attempts = 0 // a person stepped in; give it a fresh set of tries
	}
	if req.Summary != "" {
		j.Summary = req.Summary
	}
	j.State = store.Queued
	j.LastHeartbeat = time.Now()
	if err := s.Store.UpdateJob(j); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.Store.Log(j.Repo, j.ID, "queued", fmt.Sprintf("%s submitted %s to the merge queue", j.Agent, j.Title))
	s.kick(j.Repo)
	writeJSON(w, 200, j)
}

func (s *Server) feedback(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	_ = s.Store.Heartbeat(j.ID, "")
	fb, err := s.Store.UnreadFeedback(j.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if fb == nil {
		fb = []store.Feedback{}
	}
	writeJSON(w, 200, map[string]any{"state": j.State, "attempts": j.Attempts, "pr_url": j.PRURL, "feedback": fb})
}

func (s *Server) abandon(w http.ResponseWriter, r *http.Request, u *store.User) {
	j := s.jobFor(w, r, u)
	if j == nil {
		return
	}
	j.State = store.Abandoned
	if err := s.Store.UpdateJob(j); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_, _ = s.Store.ReleaseClaims(j.ID, nil)
	s.Store.Log(j.Repo, j.ID, "abandoned", fmt.Sprintf("%s stopped %s", u.Name, j.Title))
	writeJSON(w, 200, j)
}

func (s *Server) listClaims(w http.ResponseWriter, r *http.Request, u *store.User) {
	cl, err := s.Store.ActiveClaims(r.URL.Query().Get("repo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if cl == nil {
		cl = []store.Claim{}
	}
	writeJSON(w, 200, cl)
}
