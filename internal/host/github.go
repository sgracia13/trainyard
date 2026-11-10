package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// GitHub implements Host with the GitHub REST API and a token
// (a fine-grained personal access token for now; a GitHub App later).
type GitHub struct {
	APIURL  string // https://api.github.com, or https://HOST/api/v3 for GitHub Enterprise Server
	WebBase string // https://github.com
	Token   string
	HTTP    *http.Client
}

func NewGitHub(apiURL, webBase, token string) *GitHub {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	if webBase == "" {
		webBase = "https://github.com"
	}
	return &GitHub{APIURL: strings.TrimRight(apiURL, "/"), WebBase: strings.TrimRight(webBase, "/"), Token: token,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (g *GitHub) CloneURL(repo string) string {
	u, _ := url.Parse(g.WebBase + "/" + repo + ".git")
	if g.Token != "" {
		u.User = url.UserPassword("x-access-token", g.Token)
	}
	return u.String()
}

func (g *GitHub) WebURL(repo string) string { return g.WebBase + "/" + repo }

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.APIURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "trainyard")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type ghPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
}

func (g *GitHub) EnsurePR(ctx context.Context, repo, head, base, title, body string) (int, string, bool, error) {
	owner := strings.SplitN(repo, "/", 2)[0]
	var open []ghPull
	q := url.Values{"head": {owner + ":" + head}, "state": {"open"}}
	if err := g.do(ctx, "GET", "/repos/"+repo+"/pulls?"+q.Encode(), nil, &open); err != nil {
		return 0, "", false, err
	}
	if len(open) > 0 {
		return open[0].Number, open[0].HTMLURL, false, nil
	}
	var pr ghPull
	err := g.do(ctx, "POST", "/repos/"+repo+"/pulls", map[string]any{"title": title, "head": head, "base": base, "body": body}, &pr)
	if err != nil {
		return 0, "", false, err
	}
	return pr.Number, pr.HTMLURL, true, nil
}

func (g *GitHub) AddLabels(ctx context.Context, repo string, pr int, labels []string) error {
	return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/labels", repo, pr), map[string]any{"labels": labels}, nil)
}

func (g *GitHub) Comment(ctx context.Context, repo string, pr int, body string) error {
	return g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), map[string]any{"body": body}, nil)
}

func (g *GitHub) UpsertComment(ctx context.Context, repo string, pr int, marker, body string) error {
	var list []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, pr), nil, &list); err != nil {
		return err
	}
	for _, c := range list {
		if strings.Contains(c.Body, marker) {
			return g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/comments/%d", repo, c.ID), map[string]any{"body": body}, nil)
		}
	}
	return g.Comment(ctx, repo, pr, body)
}

func (g *GitHub) SetStatus(ctx context.Context, repo, sha, state, desc, target string) error {
	if len(desc) > 140 { // GitHub's limit
		desc = desc[:137] + "..."
	}
	in := map[string]any{"state": state, "context": "trainyard", "description": desc}
	if strings.HasPrefix(target, "http") {
		in["target_url"] = target
	}
	return g.do(ctx, "POST", "/repos/"+repo+"/statuses/"+sha, in, nil)
}

func (g *GitHub) PRState(ctx context.Context, repo string, pr int) (PRState, error) {
	var p ghPull
	if err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), nil, &p); err != nil {
		return PRState{}, err
	}
	return PRState{Merged: p.Merged, Closed: p.State == "closed"}, nil
}

type ghCheckRuns struct {
	CheckRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		App        struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_runs"`
}

type ghStatus struct {
	Statuses []struct {
		Context   string `json:"context"`
		State     string `json:"state"`
		TargetURL string `json:"target_url"`
	} `json:"statuses"`
}

var badConclusions = map[string]bool{"failure": true, "timed_out": true, "cancelled": true, "action_required": true, "startup_failure": true}

func (g *GitHub) Checks(ctx context.Context, repo, sha string) (CheckResult, error) {
	var runs ghCheckRuns
	if err := g.do(ctx, "GET", "/repos/"+repo+"/commits/"+sha+"/check-runs?per_page=100", nil, &runs); err != nil {
		return CheckResult{}, err
	}
	var st ghStatus
	if err := g.do(ctx, "GET", "/repos/"+repo+"/commits/"+sha+"/status", nil, &st); err != nil {
		return CheckResult{}, err
	}
	res := CheckResult{State: ChecksNone}
	pending := false
	for _, r := range runs.CheckRuns {
		if r.Name == "trainyard" {
			continue
		}
		res.Total++
		if r.Status != "completed" {
			pending = true
			continue
		}
		if badConclusions[r.Conclusion] {
			fc := FailedCheck{Name: r.Name, URL: r.HTMLURL}
			if r.App.Slug == "github-actions" {
				fc.Log = g.actionsLog(ctx, repo, r.ID)
			}
			res.Failed = append(res.Failed, fc)
		}
	}
	for _, s := range st.Statuses {
		if s.Context == "trainyard" {
			continue
		}
		res.Total++
		switch s.State {
		case "pending":
			pending = true
		case "failure", "error":
			res.Failed = append(res.Failed, FailedCheck{Name: s.Context, URL: s.TargetURL})
		}
	}
	switch {
	case res.Total == 0:
		res.State = ChecksNone
	case pending:
		res.State = ChecksPending
	case len(res.Failed) > 0:
		res.State = ChecksFailure
	default:
		res.State = ChecksSuccess
	}
	return res, nil
}

var tsPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[0-9:.]+Z `)

// actionsLog fetches the tail of a GitHub Actions job log (check run id == job id).
func (g *GitHub) actionsLog(ctx context.Context, repo string, jobID int64) string {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/repos/%s/actions/jobs/%d/logs", g.APIURL, repo, jobID), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("User-Agent", "trainyard")
	resp, err := g.HTTP.Do(req) // follows the redirect to the log file
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return FocusLog(string(data))
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// FocusLog keeps the part of a GitHub Actions log an agent needs: the step that
// failed, from its "Run ..." line to the error, without runner setup noise.
func FocusLog(s string) string {
	lines := strings.Split(strings.TrimPrefix(strings.ReplaceAll(s, "\r", ""), "\ufeff"), "\n")
	for i, l := range lines {
		lines[i] = ansi.ReplaceAllString(tsPrefix.ReplaceAllString(l, ""), "")
	}
	errAt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "##[error]") {
			errAt = i
			break
		}
	}
	if errAt < 0 {
		return TailLog(strings.Join(lines, "\n"), 80)
	}
	start := 0
	for i := errAt; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "##[group]Run ") {
			start = i
			break
		}
	}
	var out []string
	inGroup := false
	for _, l := range lines[start : errAt+1] {
		switch {
		case strings.HasPrefix(l, "##[group]"):
			out = append(out, "$ "+strings.TrimPrefix(strings.TrimPrefix(l, "##[group]"), "Run "))
			inGroup = true // the group body is the step's settings (shell, env): skip it
		case strings.HasPrefix(l, "##[endgroup]"):
			inGroup = false
		case inGroup:
		default:
			out = append(out, strings.TrimPrefix(l, "##[error]"))
		}
	}
	return TailLog(strings.Join(out, "\n"), 150)
}

// TailLog keeps the last n lines and strips GitHub's per-line timestamps.
func TailLog(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = tsPrefix.ReplaceAllString(strings.TrimRight(l, "\r"), "")
	}
	return strings.Join(lines, "\n")
}
