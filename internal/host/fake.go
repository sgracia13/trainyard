package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/sgracia13/trainyard/internal/gitops"
)

// Fake is an in-memory code host backed by local bare git repos. Its "CI"
// runs TestCmd in a checkout of the commit. Used by tests and `trainyard demo`.
type Fake struct {
	Root    string // folder holding <owner>/<name>.git bare repos
	TestCmd string // shell command run as CI; exit 0 = pass
	// StatusErr, if set, is returned by SetStatus (simulates a token without that permission).
	StatusErr error

	mu       sync.Mutex
	prs      map[string]*FakePR // repo#n
	nextPR   int
	statuses map[string][2]string // sha -> state, description
	results  map[string]CheckResult
	Comments  []string
	Summaries map[string]string // repo#n -> latest summary comment
	Edits     int               // how many times a summary was edited in place
}

type FakePR struct {
	Repo, Head, Base, Title, Body string
	Number                        int
	Labels                        []string
	Merged, Closed                bool
}

func NewFake(root, testCmd string) *Fake {
	return &Fake{Root: root, TestCmd: testCmd, prs: map[string]*FakePR{}, statuses: map[string][2]string{}, results: map[string]CheckResult{}}
}

func (f *Fake) CloneURL(repo string) string { return filepath.Join(f.Root, repo+".git") }
func (f *Fake) WebURL(repo string) string   { return "file://" + f.CloneURL(repo) }

func key(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (f *Fake) EnsurePR(_ context.Context, repo, head, base, title, body string) (int, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.Repo == repo && p.Head == head && !p.Closed {
			return p.Number, fmt.Sprintf("fake://%s/pull/%d", repo, p.Number), false, nil
		}
	}
	f.nextPR++
	p := &FakePR{Repo: repo, Head: head, Base: base, Title: title, Body: body, Number: f.nextPR}
	f.prs[key(repo, p.Number)] = p
	return p.Number, fmt.Sprintf("fake://%s/pull/%d", repo, p.Number), true, nil
}

func (f *Fake) AddLabels(_ context.Context, repo string, pr int, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[key(repo, pr)]; p != nil {
		p.Labels = append(p.Labels, labels...)
	}
	return nil
}

func (f *Fake) Comment(_ context.Context, repo string, pr int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments = append(f.Comments, fmt.Sprintf("%s#%d: %s", repo, pr, body))
	return nil
}

func (f *Fake) UpsertComment(_ context.Context, repo string, pr int, marker, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Summaries == nil {
		f.Summaries = map[string]string{}
	}
	if _, ok := f.Summaries[key(repo, pr)]; ok {
		f.Edits++
	}
	f.Summaries[key(repo, pr)] = body
	return nil
}

func (f *Fake) Summary(repo string, pr int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Summaries[key(repo, pr)]
}

func (f *Fake) SetStatus(_ context.Context, repo, sha, state, desc, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return f.StatusErr
	}
	f.statuses[sha] = [2]string{state, desc}
	return nil
}

func (f *Fake) Status(sha string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.statuses[sha]
	return s[0], s[1]
}

// Checks runs the test command once per commit and caches the result.
func (f *Fake) Checks(ctx context.Context, repo, sha string) (CheckResult, error) {
	f.mu.Lock()
	if r, ok := f.results[sha]; ok {
		f.mu.Unlock()
		return r, nil
	}
	f.mu.Unlock()
	if f.TestCmd == "" {
		return CheckResult{State: ChecksNone}, nil
	}
	dir, err := os.MkdirTemp("", "fakeci-")
	if err != nil {
		return CheckResult{}, err
	}
	defer os.RemoveAll(dir)
	if _, err := gitops.Run(ctx, dir, "clone", "-q", f.CloneURL(repo), "."); err != nil {
		return CheckResult{}, err
	}
	if _, err := gitops.Run(ctx, dir, "checkout", "-q", sha); err != nil {
		return CheckResult{}, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", f.TestCmd)
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	r := CheckResult{State: ChecksSuccess, Total: 1}
	if runErr != nil {
		r.State = ChecksFailure
		r.Failed = []FailedCheck{{Name: "tests", Log: TailLog(string(out), 150)}}
	}
	f.mu.Lock()
	f.results[sha] = r
	f.mu.Unlock()
	return r, nil
}

func (f *Fake) PRState(_ context.Context, repo string, pr int) (PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prs[key(repo, pr)]
	if p == nil {
		return PRState{}, fmt.Errorf("no PR %d", pr)
	}
	return PRState{Merged: p.Merged, Closed: p.Closed}, nil
}

func (f *Fake) PR(repo string, n int) *FakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[key(repo, n)]
}

// Merge fast-forwards (or merges) the PR head into its base, like pressing "Merge".
func (f *Fake) Merge(ctx context.Context, repo string, n int) error {
	p := f.PR(repo, n)
	if p == nil {
		return fmt.Errorf("no PR %d", n)
	}
	dir, err := os.MkdirTemp("", "fakemerge-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	steps := [][]string{
		{"clone", "-q", f.CloneURL(repo), "."},
		{"checkout", "-q", p.Base},
		{"-c", "user.name=Reviewer", "-c", "user.email=r@example.com", "merge", "-q", "--no-edit", "origin/" + p.Head},
		{"push", "-q", "origin", p.Base},
	}
	for _, s := range steps {
		if _, err := gitops.Run(ctx, dir, s...); err != nil {
			return err
		}
	}
	f.mu.Lock()
	p.Merged, p.Closed = true, true
	f.mu.Unlock()
	return nil
}
