// Package client is used by the trainyard CLI and the agent tools (MCP) to talk
// to a Trainyard server, and to manage each job's workspace on the developer's machine.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgracia13/trainyard/internal/store"
)

type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

type Settings struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func SettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "trainyard", "config.json")
}

// Load reads TRAINYARD_SERVER / TRAINYARD_TOKEN, falling back to the saved login.
func Load() (*Client, error) {
	var s Settings
	if b, err := os.ReadFile(SettingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if v := os.Getenv("TRAINYARD_SERVER"); v != "" {
		s.Server = v
	}
	if v := os.Getenv("TRAINYARD_TOKEN"); v != "" {
		s.Token = v
	}
	if s.Server == "" || s.Token == "" {
		return nil, errors.New("not signed in: run `trainyard login --server URL --token TOKEN` (or set TRAINYARD_SERVER and TRAINYARD_TOKEN)")
	}
	return &Client{Server: strings.TrimRight(s.Server, "/"), Token: s.Token, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

func SaveSettings(s Settings) error {
	p := SettingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

func (c *Client) Do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the Trainyard server at %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("server said %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- Typed calls ----

type StartResult struct {
	Job  store.Job `json:"job"`
	Main string    `json:"main"`
}

func (c *Client) StartJob(repo, title, agent, tool, model string) (*StartResult, error) {
	var r StartResult
	err := c.Do("POST", "/api/jobs", map[string]string{"repo": repo, "title": title, "agent": agent, "agent_tool": tool, "agent_model": model}, &r)
	return &r, err
}

type Overlap struct {
	Pattern      string `json:"pattern"`
	OtherPattern string `json:"other_pattern"`
	OtherJob     string `json:"other_job"`
	OtherAgent   string `json:"other_agent"`
	OtherPerson  string `json:"other_person"`
	OtherTitle   string `json:"other_title"`
}

type ClaimResult struct {
	Claimed  []string  `json:"claimed"`
	Overlaps []Overlap `json:"overlaps"`
}

func (c *Client) Claim(job string, patterns []string, reason string) (*ClaimResult, error) {
	var r ClaimResult
	err := c.Do("POST", "/api/jobs/"+job+"/claims", map[string]any{"patterns": patterns, "reason": reason}, &r)
	return &r, err
}

func (c *Client) Release(job string, patterns []string) (int, error) {
	var r struct {
		Released int `json:"released"`
	}
	err := c.Do("POST", "/api/jobs/"+job+"/release", map[string]any{"patterns": patterns}, &r)
	return r.Released, err
}

type LineResult struct {
	OtherJob   string   `json:"other_job"`
	OtherAgent string   `json:"other_agent"`
	Conflict   bool     `json:"conflict"`
	Files      []string `json:"files"`
	Changed    bool     `json:"changed"`
}

type HeartbeatResult struct {
	State      string       `json:"state"`
	Detected   []string     `json:"detected"`
	Overlaps   []Overlap    `json:"overlaps"`
	LineChecks []LineResult `json:"line_checks"`
}

// Heartbeat checks in. files and wipSHA (from Snapshot) are optional; when sent,
// Trainyard claims edited files automatically and checks for conflicting lines.
func (c *Client) Heartbeat(job, note string, files []string, wipSHA string) (*HeartbeatResult, error) {
	var r HeartbeatResult
	err := c.Do("POST", "/api/jobs/"+job+"/heartbeat", map[string]any{"note": note, "files": files, "wip_sha": wipSHA}, &r)
	return &r, err
}

func (c *Client) Submit(job, summary string) (*store.Job, error) {
	var j store.Job
	err := c.Do("POST", "/api/jobs/"+job+"/submit", map[string]string{"summary": summary}, &j)
	return &j, err
}

type FeedbackResult struct {
	State    string           `json:"state"`
	Attempts int              `json:"attempts"`
	PRURL    string           `json:"pr_url"`
	Feedback []store.Feedback `json:"feedback"`
}

func (c *Client) Feedback(job string) (*FeedbackResult, error) {
	var r FeedbackResult
	err := c.Do("GET", "/api/jobs/"+job+"/feedback", nil, &r)
	return &r, err
}

func (c *Client) Abandon(job string) error {
	return c.Do("POST", "/api/jobs/"+job+"/abandon", nil, nil)
}

func (c *Client) Claims(repo string) ([]store.Claim, error) {
	var r []store.Claim
	err := c.Do("GET", "/api/claims?repo="+repo, nil, &r)
	return r, err
}

func (c *Client) Job(id string) (*store.Job, error) {
	var j store.Job
	err := c.Do("GET", "/api/jobs/"+id, nil, &j)
	return &j, err
}
