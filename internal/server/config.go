package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sgracia13/trainyard/internal/queue"
)

// Config is read from trainyard.json. See trainyard.example.json.
type Config struct {
	Listen    string `json:"listen"`     // default ":7070"
	PublicURL string `json:"public_url"` // how people reach the dashboard; used in links on pull requests
	DataDir   string `json:"data_dir"`   // database and working clones; default "./trainyard-data"
	GitHub    struct {
		APIURL   string `json:"api_url"`   // default https://api.github.com
		WebURL   string `json:"web_url"`   // default https://github.com
		TokenEnv string `json:"token_env"` // env var holding the token; default TRAINYARD_GITHUB_TOKEN
	} `json:"github"`
	ClaimTTLMinutes int                `json:"claim_ttl_minutes"` // default 30
	PollSeconds     int                `json:"poll_seconds"`      // default 15
	Repos           []queue.RepoConfig `json:"repos"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	return c, c.Validate()
}

func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":7070"
	}
	if c.DataDir == "" {
		c.DataDir = "./trainyard-data"
	}
	if c.GitHub.TokenEnv == "" {
		c.GitHub.TokenEnv = "TRAINYARD_GITHUB_TOKEN"
	}
	if c.ClaimTTLMinutes <= 0 {
		c.ClaimTTLMinutes = 30
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 15
	}
	if c.PublicURL == "" {
		host := c.Listen
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		c.PublicURL = "http://" + host
	}
	for i := range c.Repos {
		c.Repos[i].Defaults()
	}
}

func (c *Config) Validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("config: add at least one repo under \"repos\"")
	}
	seen := map[string]bool{}
	for _, r := range c.Repos {
		if strings.Count(r.Name, "/") != 1 {
			return fmt.Errorf("config: repo %q should look like owner/name", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("config: repo %q listed twice", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

func (c *Config) Repo(name string) (*queue.RepoConfig, bool) {
	for i := range c.Repos {
		if strings.EqualFold(c.Repos[i].Name, name) {
			return &c.Repos[i], true
		}
	}
	return nil, false
}
