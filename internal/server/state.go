package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/sgracia13/trainyard/internal/claims"
	"github.com/sgracia13/trainyard/internal/store"
)

// JobView is a job as the dashboard shows it.
type JobView struct {
	*store.Job
	Claims       []string        `json:"claims"`
	LastFeedback *store.Feedback `json:"last_feedback,omitempty"`
	Overlapping  []string        `json:"overlapping"` // other job ids sharing a claim
}

type OverlapPair struct {
	Repo    string    `json:"repo"`
	A       string    `json:"a"`
	B       string    `json:"b"`
	AgentA  string    `json:"agent_a"`
	AgentB  string    `json:"agent_b"`
	Pattern string    `json:"pattern"`
	Since   time.Time `json:"since"`
	// Result of trial-merging both jobs' latest work: "", "clean" or "conflict".
	Lines     string    `json:"lines"`
	LineFiles []string  `json:"line_files"`
	LinesAt   time.Time `json:"lines_at"`
}

type State struct {
	Me       string        `json:"me"`
	ClaimTTL int           `json:"claim_ttl_minutes"`
	Now      time.Time     `json:"now"`
	Repos    []string      `json:"repos"`
	Jobs     []JobView     `json:"jobs"`
	Claims   []store.Claim `json:"claims"`
	Overlaps []OverlapPair `json:"overlaps"`
	Events   []store.Event `json:"events"`
	Counts   struct {
		NeedsYou    int `json:"needs_you"`
		InQueue     int `json:"in_queue"`
		LandedToday int `json:"landed_today"`
		Working     int `json:"working"`
	} `json:"counts"`
}

func (s *Server) BuildState(me string) (*State, error) {
	st := &State{Me: me, Now: time.Now(), ClaimTTL: s.Cfg.ClaimTTLMinutes, Overlaps: []OverlapPair{}, Jobs: []JobView{}}
	for _, r := range s.Cfg.Repos {
		st.Repos = append(st.Repos, r.Name)
	}
	jobs, err := s.Store.RecentJobs(36 * time.Hour)
	if err != nil {
		return nil, err
	}
	st.Claims, err = s.Store.ActiveClaims("")
	if err != nil {
		return nil, err
	}
	if st.Claims == nil {
		st.Claims = []store.Claim{}
	}
	byJob := map[string][]string{}
	for _, c := range st.Claims {
		byJob[c.JobID] = append(byJob[c.JobID], c.Pattern)
	}
	overlapping := map[string]map[string]bool{}
	seen := map[[2]string]bool{}
	for i, a := range st.Claims {
		for _, b := range st.Claims[i+1:] {
			if a.JobID == b.JobID || a.Repo != b.Repo || !claims.Overlap(a.Pattern, b.Pattern) {
				continue
			}
			k := [2]string{a.JobID, b.JobID}
			if overlapping[a.JobID] == nil {
				overlapping[a.JobID] = map[string]bool{}
			}
			if overlapping[b.JobID] == nil {
				overlapping[b.JobID] = map[string]bool{}
			}
			overlapping[a.JobID][b.JobID], overlapping[b.JobID][a.JobID] = true, true
			if seen[k] {
				continue
			}
			seen[k] = true
			pat := a.Pattern
			if len(b.Pattern) > len(pat) {
				pat = b.Pattern // the more specific one reads better
			}
			since := b.CreatedAt
			if a.CreatedAt.After(since) {
				since = a.CreatedAt
			}
			op := OverlapPair{Repo: a.Repo, A: a.JobID, B: b.JobID, AgentA: a.Agent, AgentB: b.Agent, Pattern: pat, Since: since, LineFiles: []string{}}
			if lc, _ := s.Store.GetLineCheck(a.JobID, b.JobID); lc != nil {
				op.Lines, op.LinesAt = "clean", lc.At
				if lc.Conflict {
					op.Lines, op.LineFiles = "conflict", lc.Files
				}
			}
			st.Overlaps = append(st.Overlaps, op)
		}
	}
	y, m, d := st.Now.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, st.Now.Location())
	for _, j := range jobs {
		v := JobView{Job: j, Claims: byJob[j.ID], Overlapping: []string{}}
		if v.Claims == nil {
			v.Claims = []string{}
		}
		for o := range overlapping[j.ID] {
			v.Overlapping = append(v.Overlapping, o)
		}
		sort.Strings(v.Overlapping)
		if j.State == store.SentBack || j.State == store.NeedsPerson {
			v.LastFeedback, _ = s.Store.LatestFeedback(j.ID)
		}
		switch j.State {
		case store.Ready, store.NeedsPerson:
			st.Counts.NeedsYou++
		case store.Queued, store.Testing:
			st.Counts.InQueue++
		case store.Working, store.SentBack:
			st.Counts.Working++
		case store.Landed:
			if j.StateSince.After(midnight) {
				st.Counts.LandedToday++
			}
		}
		st.Jobs = append(st.Jobs, v)
	}
	st.Counts.NeedsYou += len(st.Overlaps)
	st.Events, err = s.Store.Events(60)
	if st.Events == nil {
		st.Events = []store.Event{}
	}
	return st, err
}

func (s *Server) state(w http.ResponseWriter, r *http.Request, u *store.User) {
	st, err := s.BuildState(u.Name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}
