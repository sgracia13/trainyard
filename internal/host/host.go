// Package host talks to the code host (GitHub first; Forgejo and GitLab later).
// The merge queue only uses this interface, so adding a host means adding one file.
package host

import "context"

// Check states, summarised across every check on a commit (Trainyard's own excluded).
const (
	ChecksNone    = "none"    // nothing has reported yet
	ChecksPending = "pending" // at least one still running
	ChecksSuccess = "success"
	ChecksFailure = "failure"
)

type FailedCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Log  string `json:"log"` // tail of the log, when the host can provide it
}

type CheckResult struct {
	State  string        `json:"state"`
	Total  int           `json:"total"`
	Failed []FailedCheck `json:"failed"`
}

type PRState struct {
	Merged bool
	Closed bool
}

type Host interface {
	// CloneURL is the URL the server uses to fetch and push (may include credentials).
	CloneURL(repo string) string
	// WebURL is a human link to the repo.
	WebURL(repo string) string
	// EnsurePR opens a pull request from head to base, or returns the open one.
	EnsurePR(ctx context.Context, repo, head, base, title, body string) (number int, url string, created bool, err error)
	AddLabels(ctx context.Context, repo string, pr int, labels []string) error
	Comment(ctx context.Context, repo string, pr int, body string) error
	// UpsertComment edits the PR comment containing marker, or adds it. Trainyard keeps
	// one summary comment per PR up to date instead of adding a new one each time.
	UpsertComment(ctx context.Context, repo string, pr int, marker, body string) error
	// SetStatus posts the required "trainyard" commit status.
	SetStatus(ctx context.Context, repo, sha, state, description, targetURL string) error
	// Checks summarises CI results for a commit.
	Checks(ctx context.Context, repo, sha string) (CheckResult, error)
	PRState(ctx context.Context, repo string, pr int) (PRState, error)
}
