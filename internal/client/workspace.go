package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func git(dir string, args ...string) (string, error) {
	return gitops.Run(context.Background(), dir, args...)
}

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

// Snapshot records the workspace's current state, including uncommitted and new files,
// as a commit on trainyard/wip/<job> without touching the agent's branch, index or files.
// It returns the files changed since main and the snapshot commit.
func Snapshot(ws string, jf *JobFile) ([]string, string, error) {
	idx, err := os.CreateTemp("", "trainyard-index-")
	if err != nil {
		return nil, "", err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	env := append(os.Environ(), "GIT_INDEX_FILE="+idx.Name())
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = ws, append(env, gitops.Env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("read-tree", "HEAD"); err != nil {
		return nil, "", err
	}
	if _, err := run("add", "-A"); err != nil {
		return nil, "", err
	}
	tree, err := run("write-tree")
	if err != nil {
		return nil, "", err
	}
	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return nil, "", err
	}
	sha, err := run("commit-tree", tree, "-p", head, "-m", "Trainyard snapshot of "+jf.JobID)
	if err != nil {
		return nil, "", err
	}
	base, err := git(ws, "merge-base", "origin/"+jf.Main, sha)
	if err != nil {
		return nil, "", err
	}
	files, err := git(ws, "diff", "--name-only", base, sha)
	if err != nil {
		return nil, "", err
	}
	if _, err := git(ws, "push", "-q", "--force", "origin", sha+":refs/heads/trainyard/wip/"+jf.JobID); err != nil {
		return nil, "", fmt.Errorf("couldn't share the snapshot: %w", err)
	}
	return gitops.Lines(files), sha, nil
}
