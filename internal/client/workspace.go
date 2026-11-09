package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sgracia13/trainyard/internal/gitops"
)

// JobFile lives at <workspace>/.trainyard/job.json and ties a folder to a job.
type JobFile struct {
	JobID  string `json:"job_id"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Main   string `json:"main"`
	Title  string `json:"title"`
	Agent  string `json:"agent"`
	Port   int    `json:"port"`
}

const PortFirst, PortLast = 3001, 3099

var ErrNoJob = errors.New("this folder isn't a Trainyard workspace: run `trainyard start \"what to do\"` first, or cd into a job's folder")

func git(dir string, args ...string) (string, error) { return gitops.Run(context.Background(), dir, args...) }

// FindJob walks up from dir looking for .trainyard/job.json.
func FindJob(dir string) (*JobFile, string, error) {
	d, _ := filepath.Abs(dir)
	for {
		b, err := os.ReadFile(filepath.Join(d, ".trainyard", "job.json"))
		if err == nil {
			var j JobFile
			if err := json.Unmarshal(b, &j); err != nil {
				return nil, "", err
			}
			return &j, d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return nil, "", ErrNoJob
		}
		d = parent
	}
}

var remoteRE = regexp.MustCompile(`(?:[:/])([^/:]+/[^/]+?)(?:\.git)?/?$`)

// DetectRepo reads owner/name from the origin remote.
func DetectRepo(dir string) (string, error) {
	u, err := git(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("couldn't read the origin remote; pass --repo owner/name")
	}
	m := remoteRE.FindStringSubmatch(u)
	if m == nil {
		return "", fmt.Errorf("couldn't tell the repo from %q; pass --repo owner/name", u)
	}
	return m[1], nil
}

// MainCheckout returns the developer's main checkout, even when called from a job's worktree.
func MainCheckout(dir string) (string, error) {
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", errors.New("not inside a git repository")
	}
	return filepath.Dir(common), nil
}

// CreateWorkspace makes a separate folder (a git worktree) for the job, on its own
// branch from the latest main, and gives it a free port.
func CreateWorkspace(dir string, jf JobFile) (string, error) {
	root, err := MainCheckout(dir)
	if err != nil {
		return "", err
	}
	if _, err := git(root, "fetch", "-q", "origin", jf.Main); err != nil {
		return "", fmt.Errorf("couldn't fetch %s from origin: %w", jf.Main, err)
	}
	jobsDir := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-trainyard")
	path := filepath.Join(jobsDir, jf.JobID)
	if _, err := git(root, "worktree", "add", "-q", "-b", jf.Branch, path, "origin/"+jf.Main); err != nil {
		return "", err
	}
	jf.Port = freePort(jobsDir)
	if err := os.MkdirAll(filepath.Join(path, ".trainyard"), 0o755); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(jf, "", "  ")
	if err := os.WriteFile(filepath.Join(path, ".trainyard", "job.json"), b, 0o644); err != nil {
		return "", err
	}
	env := fmt.Sprintf("# Written by Trainyard for job %s. Use this port so agents don't collide.\nPORT=%d\nTRAINYARD_JOB=%s\n", jf.JobID, jf.Port, jf.JobID)
	if err := os.WriteFile(filepath.Join(path, ".env.trainyard"), []byte(env), 0o644); err != nil {
		return "", err
	}
	if err := excludeLocal(root, ".trainyard/", ".env.trainyard"); err != nil {
		return "", err
	}
	return path, nil
}

// freePort picks the lowest port in the pool not used by another job folder.
func freePort(jobsDir string) int {
	used := map[int]bool{}
	entries, _ := os.ReadDir(jobsDir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(jobsDir, e.Name(), ".trainyard", "job.json"))
		if err != nil {
			continue
		}
		var j JobFile
		if json.Unmarshal(b, &j) == nil {
			used[j.Port] = true
		}
	}
	for p := PortFirst; p <= PortLast; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// excludeLocal keeps Trainyard's files out of commits without touching .gitignore.
func excludeLocal(root string, patterns ...string) error {
	common, err := git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	p := filepath.Join(common, "info", "exclude")
	cur, _ := os.ReadFile(p)
	var add []string
	for _, pat := range patterns {
		if !strings.Contains(string(cur), pat) {
			add = append(add, pat)
		}
	}
	if len(add) == 0 {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n# Trainyard\n" + strings.Join(add, "\n") + "\n")
	return err
}

// CommitAll commits any uncommitted work. Returns false if there was nothing to commit.
func CommitAll(ws, message string) (bool, error) {
	st, err := git(ws, "status", "--porcelain")
	if err != nil || st == "" {
		return false, err
	}
	if _, err := git(ws, "add", "-A"); err != nil {
		return false, err
	}
	_, err = git(ws, "commit", "-q", "-m", message)
	return err == nil, err
}

// Push sends the job branch to origin.
func Push(ws, branch string) error {
	_, err := git(ws, "push", "-q", "--force-with-lease", "origin", "HEAD:refs/heads/"+branch)
	return err
}

// RemoveWorkspace deletes a finished job's folder.
func RemoveWorkspace(ws string) error {
	root, err := MainCheckout(ws)
	if err != nil {
		return err
	}
	_, err = git(root, "worktree", "remove", "--force", ws)
	return err
}
