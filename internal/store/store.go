// Package store keeps Trainyard's state in SQLite: people, jobs, claims,
// feedback for agents, and an activity log.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Job states, in the order a job normally moves through them.
const (
	Working     = "working"      // agent is editing in its workspace
	Queued      = "queued"       // submitted, waiting for the merge queue
	Testing     = "testing"      // rebased on main, tests running on the code host
	SentBack    = "sent_back"    // failed; feedback waiting for the agent
	Ready       = "ready"        // passed; pull request waiting for a reviewer
	Landed      = "landed"       // merged into main
	NeedsPerson = "needs_person" // failed too many times, or needs a decision
	Abandoned   = "abandoned"    // stopped or closed without merging
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Job struct {
	ID            string    `json:"id"`
	Repo          string    `json:"repo"`
	Title         string    `json:"title"`
	Agent         string    `json:"agent"`
	AgentTool     string    `json:"agent_tool"`
	AgentModel    string    `json:"agent_model"`
	StartedBy     string    `json:"started_by"`
	Branch        string    `json:"branch"`
	State         string    `json:"state"`
	Attempts      int       `json:"attempts"`
	Note          string    `json:"note"`
	Summary       string    `json:"summary"`
	PR            int       `json:"pr"`
	PRURL         string    `json:"pr_url"`
	QueueSHA      string    `json:"queue_sha"`
	WipSHA        string    `json:"wip_sha"` // latest snapshot of the agent's unfinished work`
	BaseSHA       string    `json:"base_sha"`
	ChangedFiles  []string  `json:"changed_files"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	StateSince    time.Time `json:"state_since"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Claim struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	Repo      string    `json:"repo"`
	Pattern   string    `json:"pattern"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"` // "claimed" by the agent, or "detected" from its actual edits
	CreatedAt time.Time `json:"created_at"`
	// Filled in by queries that join jobs.
	Agent     string `json:"agent,omitempty"`
	StartedBy string `json:"started_by,omitempty"`
	JobTitle  string `json:"job_title,omitempty"`
}

type Feedback struct {
	ID      int64     `json:"id"`
	JobID   string    `json:"job_id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // overlap, test_failed, conflict, check_failed, note
	Message string    `json:"message"`
	Log     string    `json:"log,omitempty"`
}

type Event struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Repo    string    `json:"repo"`
	JobID   string    `json:"job_id"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, token_hash TEXT UNIQUE NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, seq INTEGER UNIQUE NOT NULL, repo TEXT NOT NULL, title TEXT NOT NULL,
  agent TEXT NOT NULL, agent_tool TEXT NOT NULL DEFAULT '', agent_model TEXT NOT NULL DEFAULT '',
  started_by TEXT NOT NULL, branch TEXT NOT NULL, state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', pr INTEGER NOT NULL DEFAULT 0, pr_url TEXT NOT NULL DEFAULT '',
  queue_sha TEXT NOT NULL DEFAULT '', base_sha TEXT NOT NULL DEFAULT '', changed_files TEXT NOT NULL DEFAULT '[]',
  last_heartbeat INTEGER NOT NULL, state_since INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_state ON jobs(repo, state);
CREATE TABLE IF NOT EXISTS claims (
  id INTEGER PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), repo TEXT NOT NULL, pattern TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, released_at INTEGER);
CREATE INDEX IF NOT EXISTS claims_active ON claims(repo, released_at);
CREATE TABLE IF NOT EXISTS feedback (
  id INTEGER PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), at INTEGER NOT NULL, kind TEXT NOT NULL,
  message TEXT NOT NULL, log TEXT NOT NULL DEFAULT '', read INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS line_checks (
  a TEXT NOT NULL, b TEXT NOT NULL, sha_a TEXT NOT NULL, sha_b TEXT NOT NULL, conflict INTEGER NOT NULL,
  files TEXT NOT NULL DEFAULT '[]', at INTEGER NOT NULL, PRIMARY KEY(a, b));
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, repo TEXT NOT NULL DEFAULT '', job_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL, message TEXT NOT NULL);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps things simple and safe
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Columns added after v0.1.0; older databases get them here.
	for _, m := range []string{
		`ALTER TABLE jobs ADD COLUMN wip_sha TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE claims ADD COLUMN source TEXT NOT NULL DEFAULT 'claimed'`,
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64           { return time.Now().UnixMilli() }
func ms(v int64) time.Time { return time.UnixMilli(v) }

// ---- People and tokens ----

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

// AddUser creates a person and returns their API token. Only the hash is stored.
func (s *Store) AddUser(name string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "ty_" + hex.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO users(name, token_hash, created_at) VALUES(?,?,?)`, name, hashToken(tok), now())
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Store) UserByToken(tok string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, name FROM users WHERE token_hash=?`, hashToken(tok)).Scan(&u.ID, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- Jobs ----

const jobCols = `id, repo, title, agent, agent_tool, agent_model, started_by, branch, state, attempts, note, summary,
 pr, pr_url, queue_sha, wip_sha, base_sha, changed_files, last_heartbeat, state_since, created_at, updated_at`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	var files string
	var hb, since, ca, ua int64
	err := sc.Scan(&j.ID, &j.Repo, &j.Title, &j.Agent, &j.AgentTool, &j.AgentModel, &j.StartedBy, &j.Branch, &j.State,
		&j.Attempts, &j.Note, &j.Summary, &j.PR, &j.PRURL, &j.QueueSHA, &j.WipSHA, &j.BaseSHA, &files, &hb, &since, &ca, &ua)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(files), &j.ChangedFiles)
	j.LastHeartbeat, j.StateSince, j.CreatedAt, j.UpdatedAt = ms(hb), ms(since), ms(ca), ms(ua)
	return j, nil
}

// CreateJob gives the job a short id (j1, j2, ...) and its branch name.
func (s *Store) CreateJob(j *Job) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM jobs`).Scan(&seq); err != nil {
		return err
	}
	t := now()
	j.ID = fmt.Sprintf("j%d", seq)
	j.Branch = "trainyard/job/" + j.ID
	j.State = Working
	j.LastHeartbeat, j.StateSince, j.CreatedAt, j.UpdatedAt = ms(t), ms(t), ms(t), ms(t)
	_, err = tx.Exec(`INSERT INTO jobs(id, seq, repo, title, agent, agent_tool, agent_model, started_by, branch, state,
		last_heartbeat, state_since, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, seq, j.Repo, j.Title, j.Agent, j.AgentTool, j.AgentModel, j.StartedBy, j.Branch, j.State, t, t, t, t)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Job(id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func (s *Store) queryJobs(q string, args ...any) ([]*Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobsInState returns a repo's jobs in the given states, oldest first.
func (s *Store) JobsInState(repo string, states ...string) ([]*Job, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := []any{repo}
	for _, st := range states {
		args = append(args, st)
	}
	return s.queryJobs(`WHERE repo=? AND state IN (`+ph+`) ORDER BY state_since`, args...)
}

// RecentJobs returns open jobs plus anything that finished in the last `since`.
func (s *Store) RecentJobs(since time.Duration) ([]*Job, error) {
	cut := time.Now().Add(-since).UnixMilli()
	return s.queryJobs(`WHERE state NOT IN (?,?) OR updated_at > ? ORDER BY created_at DESC`, Landed, Abandoned, cut)
}

// UpdateJob saves every mutable field. If the state changed, state_since resets.
func (s *Store) UpdateJob(j *Job) error {
	old, err := s.Job(j.ID)
	if err != nil {
		return err
	}
	t := now()
	since := old.StateSince.UnixMilli()
	if old.State != j.State {
		since = t
	}
	files, _ := json.Marshal(j.ChangedFiles)
	_, err = s.db.Exec(`UPDATE jobs SET state=?, attempts=?, note=?, summary=?, pr=?, pr_url=?, queue_sha=?, wip_sha=?, base_sha=?,
		changed_files=?, last_heartbeat=?, state_since=?, updated_at=? WHERE id=?`,
		j.State, j.Attempts, j.Note, j.Summary, j.PR, j.PRURL, j.QueueSHA, j.WipSHA, j.BaseSHA, string(files),
		j.LastHeartbeat.UnixMilli(), since, t, j.ID)
	j.StateSince, j.UpdatedAt = ms(since), ms(t)
	return err
}

func (s *Store) Heartbeat(id, note string) error {
	_, err := s.db.Exec(`UPDATE jobs SET last_heartbeat=?, note=CASE WHEN ?='' THEN note ELSE ? END, updated_at=? WHERE id=?`,
		now(), note, note, now(), id)
	return err
}

// ---- Claims ----

func (s *Store) AddClaim(c *Claim) error {
	c.CreatedAt = time.Now()
	if c.Source == "" {
		c.Source = "claimed"
	}
	res, err := s.db.Exec(`INSERT INTO claims(job_id, repo, pattern, reason, source, created_at) VALUES(?,?,?,?,?,?)`,
		c.JobID, c.Repo, c.Pattern, c.Reason, c.Source, c.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// ActiveClaims lists unreleased claims; repo "" means every repo.
func (s *Store) ActiveClaims(repo string) ([]Claim, error) {
	q := `SELECT c.id, c.job_id, c.repo, c.pattern, c.reason, c.source, c.created_at, j.agent, j.started_by, j.title
	      FROM claims c JOIN jobs j ON j.id=c.job_id WHERE c.released_at IS NULL`
	var args []any
	if repo != "" {
		q += ` AND c.repo=?`
		args = append(args, repo)
	}
	rows, err := s.db.Query(q+` ORDER BY c.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		var ca int64
		if err := rows.Scan(&c.ID, &c.JobID, &c.Repo, &c.Pattern, &c.Reason, &c.Source, &ca, &c.Agent, &c.StartedBy, &c.JobTitle); err != nil {
			return nil, err
		}
		c.CreatedAt = ms(ca)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleaseClaims releases a job's claims: all of them, or only the given patterns.
func (s *Store) ReleaseClaims(jobID string, patterns []string) (int64, error) {
	if len(patterns) == 0 {
		res, err := s.db.Exec(`UPDATE claims SET released_at=? WHERE job_id=? AND released_at IS NULL`, now(), jobID)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	var n int64
	for _, p := range patterns {
		res, err := s.db.Exec(`UPDATE claims SET released_at=? WHERE job_id=? AND pattern=? AND released_at IS NULL`, now(), jobID, p)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, nil
}

// ---- Feedback for agents ----

func (s *Store) AddFeedback(f *Feedback) error {
	f.At = time.Now()
	res, err := s.db.Exec(`INSERT INTO feedback(job_id, at, kind, message, log) VALUES(?,?,?,?,?)`,
		f.JobID, f.At.UnixMilli(), f.Kind, f.Message, f.Log)
	if err != nil {
		return err
	}
	f.ID, _ = res.LastInsertId()
	return nil
}

// UnreadFeedback returns feedback the agent hasn't seen and marks it read.
func (s *Store) UnreadFeedback(jobID string) ([]Feedback, error) {
	rows, err := s.db.Query(`SELECT id, job_id, at, kind, message, log FROM feedback WHERE job_id=? AND read=0 ORDER BY id`, jobID)
	if err != nil {
		return nil, err
	}
	var out []Feedback
	for rows.Next() {
		var f Feedback
		var at int64
		if err := rows.Scan(&f.ID, &f.JobID, &at, &f.Kind, &f.Message, &f.Log); err != nil {
			rows.Close()
			return nil, err
		}
		f.At = ms(at)
		out = append(out, f)
	}
	rows.Close()
	if len(out) > 0 {
		_, err = s.db.Exec(`UPDATE feedback SET read=1 WHERE job_id=? AND id<=?`, jobID, out[len(out)-1].ID)
	}
	return out, err
}

// LatestFeedback returns the most recent feedback of any kind (for the dashboard).
func (s *Store) LatestFeedback(jobID string) (*Feedback, error) {
	f := &Feedback{}
	var at int64
	err := s.db.QueryRow(`SELECT id, job_id, at, kind, message, log FROM feedback WHERE job_id=? ORDER BY id DESC LIMIT 1`, jobID).
		Scan(&f.ID, &f.JobID, &at, &f.Kind, &f.Message, &f.Log)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	f.At = ms(at)
	return f, err
}

// ---- Activity ----

func (s *Store) Log(repo, jobID, kind, msg string) {
	_, _ = s.db.Exec(`INSERT INTO events(at, repo, job_id, kind, message) VALUES(?,?,?,?,?)`, now(), repo, jobID, kind, msg)
}

func (s *Store) Events(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, at, repo, job_id, kind, message FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Repo, &e.JobID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.At = ms(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Line-level checks between two jobs' work ----

// LineCheck is the result of trial-merging two jobs' latest work.
type LineCheck struct {
	A, B     string // job ids, A < B
	ShaA     string
	ShaB     string
	Conflict bool
	Files    []string
	At       time.Time
}

func pair(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

func (s *Store) GetLineCheck(a, b string) (*LineCheck, error) {
	a, b = pair(a, b)
	lc := &LineCheck{}
	var files string
	var at, c int64
	err := s.db.QueryRow(`SELECT a, b, sha_a, sha_b, conflict, files, at FROM line_checks WHERE a=? AND b=?`, a, b).
		Scan(&lc.A, &lc.B, &lc.ShaA, &lc.ShaB, &c, &files, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lc.Conflict, lc.At = c == 1, ms(at)
	_ = json.Unmarshal([]byte(files), &lc.Files)
	return lc, nil
}

// PutLineCheck stores a result; shas are given in the same order as the job ids.
func (s *Store) PutLineCheck(a, b, shaA, shaB string, conflict bool, files []string) error {
	if b < a {
		a, b, shaA, shaB = b, a, shaB, shaA
	}
	f, _ := json.Marshal(files)
	c := 0
	if conflict {
		c = 1
	}
	_, err := s.db.Exec(`INSERT INTO line_checks(a, b, sha_a, sha_b, conflict, files, at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(a, b) DO UPDATE SET sha_a=excluded.sha_a, sha_b=excluded.sha_b, conflict=excluded.conflict, files=excluded.files, at=excluded.at`,
		a, b, shaA, shaB, c, string(f), now())
	return err
}
