// Package queue is Trainyard's merge queue. For each submitted job it:
//
//  1. puts the agent's commits on top of the latest main (adding name-tag trailers),
//  2. runs optional quick checks on the server,
//  3. pushes trainyard/queue/<job> and opens a pull request,
//  4. waits for the code host's CI, then marks the "trainyard" check passed
//     or sends the job back to the agent with the failing log,
//  5. notices when the PR is merged, releases claims, and re-tests waiting jobs
//     whose files overlap what just landed.
//
// Trainyard never edits an agent's code itself.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sgracia13/trainyard/internal/claims"
	"github.com/sgracia13/trainyard/internal/gitops"
	"github.com/sgracia13/trainyard/internal/host"
	"github.com/sgracia13/trainyard/internal/store"
)

type RepoConfig struct {
	Name        string `json:"name"`         // owner/repo
	Main        string `json:"main"`         // default "main"
	QuickCheck  string `json:"quick_check"`  // optional shell command run on the server before pushing
	MaxAttempts int    `json:"max_attempts"` // default 3
	// If no CI check reports within this many minutes, treat the job as passed.
	NoChecksAfterMinutes int `json:"no_checks_after_minutes"` // default 10
}

func (r *RepoConfig) Defaults() {
	if r.Main == "" {
		r.Main = "main"
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.NoChecksAfterMinutes <= 0 {
		r.NoChecksAfterMinutes = 10
	}
}

type Worker struct {
	mu        sync.Mutex // one git operation at a time in the working clone
	Repo      RepoConfig
	Store     *store.Store
	Host      host.Host
	DataDir   string
	PublicURL string
	ClaimTTL  time.Duration
	Now       func() time.Time // for tests
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) dir() string {
	return filepath.Join(w.DataDir, "repos", strings.ReplaceAll(w.Repo.Name, "/", "__"))
}

// DataDirRepo is the server's working clone for this repo.
func (w *Worker) DataDirRepo() string { return w.dir() }

func (w *Worker) git(ctx context.Context, args ...string) (string, error) {
	return gitops.Run(ctx, w.dir(), args...)
}

func QueueBranch(jobID string) string { return "trainyard/queue/" + jobID }

// Tick does one pass over the repo. It is safe to call repeatedly.
func (w *Worker) Tick(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expireClaims()
	if err := w.sync(ctx); err != nil {
		return fmt.Errorf("sync %s: %w", w.Repo.Name, err)
	}
	queued, err := w.Store.JobsInState(w.Repo.Name, store.Queued)
	if err != nil {
		return err
	}
	for _, j := range queued {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.prepare(ctx, j); err != nil {
			log.Printf("queue %s %s: %v", w.Repo.Name, j.ID, err)
			w.Store.Log(j.Repo, j.ID, "error", "Merge queue error: "+firstLine(err.Error()))
		}
	}
	testing, err := w.Store.JobsInState(w.Repo.Name, store.Testing)
	if err != nil {
		return err
	}
	for _, j := range testing {
		if err := w.checkTests(ctx, j); err != nil {
			log.Printf("checks %s %s: %v", w.Repo.Name, j.ID, err)
		}
	}
	ready, err := w.Store.JobsInState(w.Repo.Name, store.Ready, store.SentBack, store.NeedsPerson, store.Testing)
	if err != nil {
		return err
	}
	for _, j := range ready {
		if err := w.checkPR(ctx, j); err != nil {
			log.Printf("pr %s %s: %v", w.Repo.Name, j.ID, err)
		}
	}
	return nil
}

// sync makes sure the server's working clone exists and is up to date.
func (w *Worker) sync(ctx context.Context) error {
	url := w.Host.CloneURL(w.Repo.Name)
	if _, err := os.Stat(filepath.Join(w.dir(), ".git")); err != nil {
		if err := os.MkdirAll(w.dir(), 0o755); err != nil {
			return err
		}
		if _, err := gitops.Run(ctx, w.dir(), "clone", "-q", url, "."); err != nil {
			return err
		}
	} else if _, err := w.git(ctx, "remote", "set-url", "origin", url); err != nil {
		return err
	}
	_, err := w.git(ctx, "fetch", "-q", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*")
	return err
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func trailerCmd(j *store.Job) string {
	t := []string{
		"Agent: " + j.Agent,
		"Agent-Tool: " + orDash(j.AgentTool),
		"Agent-Model: " + orDash(j.AgentModel),
		"Job: " + j.ID + " " + oneLine(j.Title),
		"Started-By: " + j.StartedBy,
	}
	cmd := "git -c trailer.ifexists=addIfDifferent commit -q --amend --no-edit --no-verify"
	for _, x := range t {
		cmd += " --trailer " + shQuote(x)
	}
	return cmd
}

// prepare rebases a queued job onto main, runs quick checks, pushes and opens the PR.
func (w *Worker) prepare(ctx context.Context, j *store.Job) error {
	main := "origin/" + w.Repo.Main
	if _, err := w.git(ctx, "rev-parse", "--verify", "-q", "origin/"+j.Branch); err != nil {
		return w.sendBack(ctx, j, "not_pushed", "Trainyard couldn't find branch "+j.Branch+". Push your work, then submit again.", "", false)
	}
	base, err := w.git(ctx, "rev-parse", main)
	if err != nil {
		return err
	}
	_, _ = w.git(ctx, "rebase", "--abort") // leftover from a crash, if any
	if _, err := w.git(ctx, "checkout", "-q", "-f", "-B", QueueBranch(j.ID), "origin/"+j.Branch); err != nil {
		return err
	}
	if _, err := w.git(ctx, "clean", "-qfdx"); err != nil {
		return err
	}
	ahead, err := w.git(ctx, "rev-list", "--count", main+"..HEAD")
	if err != nil {
		return err
	}
	if ahead == "0" {
		return w.sendBack(ctx, j, "no_changes", "Your branch has no commits that aren't already on main. Commit your work, push, and submit again.", "", false)
	}
	if out, err := w.git(ctx, "rebase", "-q", "--exec", trailerCmd(j), main); err != nil {
		files, _ := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
		_, _ = w.git(ctx, "rebase", "--abort")
		msg := "Your change conflicts with the latest main. Update your branch on top of " + w.Repo.Main + " and submit again."
		if files != "" {
			msg += "\nConflicting files: " + strings.Join(gitops.Lines(files), ", ")
		}
		return w.sendBack(ctx, j, "conflict", msg, out, true)
	}
	if w.Repo.QuickCheck != "" {
		if out, err := w.runQuickCheck(ctx); err != nil {
			return w.sendBack(ctx, j, "check_failed", "Quick checks failed on top of the latest main: "+w.Repo.QuickCheck, host.TailLog(out, 150), true)
		}
	}
	changed, err := w.git(ctx, "diff", "--name-only", main+"...HEAD")
	if err != nil {
		return err
	}
	sha, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := w.git(ctx, "push", "-q", "--force", "origin", "HEAD:refs/heads/"+QueueBranch(j.ID)); err != nil {
		return err
	}
	num, url, created, err := w.Host.EnsurePR(ctx, j.Repo, QueueBranch(j.ID), w.Repo.Main, j.Title, w.prBody(j, gitops.Lines(changed)))
	if err != nil {
		return err
	}
	if created {
		if err := w.Host.AddLabels(ctx, j.Repo, num, []string{"trainyard", "agent: " + j.Agent}); err != nil {
			log.Printf("labels %s#%d: %v (continuing)", j.Repo, num, err)
		}
	}
	w.setStatus(ctx, j, sha, "pending", "Testing on top of the latest "+w.Repo.Main)
	j.State, j.PR, j.PRURL, j.QueueSHA, j.BaseSHA, j.ChangedFiles = store.Testing, num, url, sha, base, gitops.Lines(changed)
	if err := w.Store.UpdateJob(j); err != nil {
		return err
	}
	w.summary(ctx, j, 0, "", "")
	w.Store.Log(j.Repo, j.ID, "testing", fmt.Sprintf("%s: on top of latest %s, tests running (PR #%d)", j.Title, w.Repo.Main, num))
	return nil
}

// setStatus posts the "trainyard" commit status. A failure here (e.g. a token
// without the commit-status permission) is logged but doesn't stop the queue:
// the job still moves on, and the dashboard and PR comments still say what happened.
func (w *Worker) setStatus(ctx context.Context, j *store.Job, sha, state, desc string) {
	if err := w.Host.SetStatus(ctx, j.Repo, sha, state, desc, w.jobURL(j)); err != nil {
		log.Printf("couldn't set the trainyard status on %s for %s (check the token's \"Commit statuses\" permission): %v", short(sha), j.ID, err)
	}
}

func (w *Worker) runQuickCheck(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", w.Repo.QuickCheck)
	cmd.Dir = w.dir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkTests reads CI results for a job's queue commit.
func (w *Worker) checkTests(ctx context.Context, j *store.Job) error {
	res, err := w.Host.Checks(ctx, j.Repo, j.QueueSHA)
	if err != nil {
		return err
	}
	switch res.State {
	case host.ChecksPending:
		return nil
	case host.ChecksNone:
		if w.now().Sub(j.StateSince) < time.Duration(w.Repo.NoChecksAfterMinutes)*time.Minute {
			return nil
		}
		return w.pass(ctx, j, "No CI checks reported; passed quick checks on latest "+w.Repo.Main)
	case host.ChecksSuccess:
		return w.pass(ctx, j, fmt.Sprintf("Tested on top of the latest %s (%d checks passed)", w.Repo.Main, res.Total))
	}
	var names []string
	var logs strings.Builder
	for _, f := range res.Failed {
		names = append(names, f.Name)
		if f.Log != "" {
			fmt.Fprintf(&logs, "=== %s ===\n%s\n", f.Name, f.Log)
		} else if f.URL != "" {
			fmt.Fprintf(&logs, "=== %s ===\n(see %s)\n", f.Name, f.URL)
		}
	}
	msg := fmt.Sprintf("Tests failed on top of the latest %s: %s. Fix it in your workspace, push, and submit again.", w.Repo.Main, strings.Join(names, ", "))
	return w.sendBack(ctx, j, "test_failed", msg, logs.String(), true)
}

func (w *Worker) pass(ctx context.Context, j *store.Job, desc string) error {
	w.setStatus(ctx, j, j.QueueSHA, "success", desc)
	j.State = store.Ready
	if err := w.Store.UpdateJob(j); err != nil {
		return err
	}
	w.summary(ctx, j, 0, "", "")
	w.Store.Log(j.Repo, j.ID, "ready", fmt.Sprintf("%s passed. Ready for review (PR #%d)", j.Title, j.PR))
	return nil
}

// sendBack hands the job back to its agent with feedback. countsAsTry=false is
// used for problems that aren't really a failed attempt (e.g. branch not pushed).
func (w *Worker) sendBack(ctx context.Context, j *store.Job, kind, msg, logText string, countsAsTry bool) error {
	if err := ctx.Err(); err != nil {
		return err // shutting down: a git command may have failed only because it was cancelled
	}
	if countsAsTry {
		j.Attempts++
	}
	j.State = store.SentBack
	desc := "Sent back to " + j.Agent
	if countsAsTry {
		desc = fmt.Sprintf("Sent back to %s (try %d of %d)", j.Agent, j.Attempts, w.Repo.MaxAttempts)
	}
	if countsAsTry && j.Attempts >= w.Repo.MaxAttempts {
		j.State = store.NeedsPerson
		desc = fmt.Sprintf("Failed %d times; needs a person", j.Attempts)
		msg += fmt.Sprintf("\nThis was try %d of %d, so a person has been asked to take a look.", j.Attempts, w.Repo.MaxAttempts)
	}
	if err := w.Store.AddFeedback(&store.Feedback{JobID: j.ID, Kind: kind, Message: msg, Log: logText}); err != nil {
		return err
	}
	if j.QueueSHA != "" && j.State != store.Working {
		w.setStatus(ctx, j, j.QueueSHA, "failure", desc)
	}
	if err := w.Store.UpdateJob(j); err != nil {
		return err
	}
	if j.PR > 0 && countsAsTry {
		failedStep := map[string]int{"conflict": 1, "check_failed": 2}[kind]
		if failedStep == 0 {
			failedStep = 3
		}
		w.summary(ctx, j, failedStep, msg, logText)
	}
	w.Store.Log(j.Repo, j.ID, j.State, j.Title+": "+desc+". "+firstLine(msg))
	return nil
}

// checkPR notices merges and closes, and re-tests ready jobs when main changes
// in files they touch.
func (w *Worker) checkPR(ctx context.Context, j *store.Job) error {
	if j.PR == 0 {
		return nil
	}
	st, err := w.Host.PRState(ctx, j.Repo, j.PR)
	if err != nil {
		return err
	}
	switch {
	case st.Merged:
		j.State = store.Landed
		if err := w.Store.UpdateJob(j); err != nil {
			return err
		}
		_, _ = w.Store.ReleaseClaims(j.ID, nil)
		w.summary(ctx, j, 0, "", "")
		w.Store.Log(j.Repo, j.ID, "landed", fmt.Sprintf("%s landed on %s (PR #%d)", j.Title, w.Repo.Main, j.PR))
		return nil
	case st.Closed:
		j.State = store.Abandoned
		if err := w.Store.UpdateJob(j); err != nil {
			return err
		}
		_, _ = w.Store.ReleaseClaims(j.ID, nil)
		w.Store.Log(j.Repo, j.ID, "abandoned", fmt.Sprintf("%s: PR #%d closed without merging", j.Title, j.PR))
		return nil
	}
	if j.State != store.Ready {
		return nil
	}
	head, err := w.git(ctx, "rev-parse", "origin/"+w.Repo.Main)
	if err != nil || head == j.BaseSHA {
		return err
	}
	moved, err := w.git(ctx, "diff", "--name-only", j.BaseSHA, head)
	if err != nil {
		return err
	}
	if claims.AnyOverlap(j.ChangedFiles, gitops.Lines(moved)) {
		j.State = store.Queued
		if err := w.Store.UpdateJob(j); err != nil {
			return err
		}
		w.Store.Log(j.Repo, j.ID, "retest", j.Title+": main changed in files this job touches, re-testing")
		return nil
	}
	j.BaseSHA = head // nothing in common; still good
	return w.Store.UpdateJob(j)
}

// expireClaims releases claims of agents that stopped checking in.
func (w *Worker) expireClaims() {
	if w.ClaimTTL <= 0 {
		return
	}
	jobs, err := w.Store.JobsInState(w.Repo.Name, store.Working, store.SentBack)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if w.now().Sub(j.LastHeartbeat) < w.ClaimTTL {
			continue
		}
		n, _ := w.Store.ReleaseClaims(j.ID, nil)
		if n > 0 {
			mins := int(w.ClaimTTL.Minutes())
			_ = w.Store.AddFeedback(&store.Feedback{JobID: j.ID, Kind: "note",
				Message: fmt.Sprintf("Your claims were released because you hadn't checked in for %d minutes. Claim your files again before editing.", mins)})
			w.Store.Log(j.Repo, j.ID, "expired", fmt.Sprintf("%s's claims released: no check-in for %d minutes", j.Agent, mins))
		}
	}
}

func (w *Worker) prBody(j *store.Job, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** · opened by Trainyard for %s\n\n", j.Title, j.StartedBy)
	if j.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", j.Summary)
	}
	fmt.Fprintf(&b, "| | |\n|---|---|\n| Job | `%s` |\n| Agent | %s (%s, %s) |\n| Started by | %s |\n| Files | %d |\n\n",
		j.ID, j.Agent, orDash(j.AgentTool), orDash(j.AgentModel), j.StartedBy, len(files))
	fmt.Fprintf(&b, "This branch is the agent's work rebased on the latest `%s`. The `trainyard` check turns green once CI passes. "+
		"Request changes as usual — the agent picks them up from Trainyard.", w.Repo.Main)
	return b.String()
}

func (w *Worker) jobURL(j *store.Job) string {
	if w.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(w.PublicURL, "/") + "/#job=" + j.ID
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLine(s string) string { return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0] }

var ErrNoRepo = errors.New("repo not configured")

// TrialMerge merges two commits in memory (git merge-tree, nothing is written to any
// branch) and reports whether they'd conflict, and in which files.
func (w *Worker) TrialMerge(ctx context.Context, a, b string) (bool, []string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := os.Stat(filepath.Join(w.dir(), ".git")); err != nil {
		if err := w.sync(ctx); err != nil {
			return false, nil, err
		}
	}
	for _, sha := range []string{a, b} {
		if _, err := w.git(ctx, "cat-file", "-e", sha+"^{commit}"); err != nil {
			if _, err := w.git(ctx, "fetch", "-q", "origin", "+refs/heads/trainyard/*:refs/remotes/origin/trainyard/*"); err != nil {
				return false, nil, err
			}
			break
		}
	}
	out, err := w.git(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", a, b)
	if err == nil {
		return false, nil, nil
	}
	var ge *gitops.Error
	if errors.As(err, &ge) && strings.Contains(ge.Err.Error(), "exit status 1") {
		lines := gitops.Lines(out)
		if len(lines) > 1 {
			return true, lines[1:], nil // first line is the tree id
		}
		return true, nil, nil
	}
	return false, nil, err
}

const summaryMarker = "<!-- trainyard:summary -->"

// summary keeps one comment on the PR up to date with where the job stands.
// failedStep is 1-3 when a step failed (msg and logText explain it), else 0.
func (w *Worker) summary(ctx context.Context, j *store.Job, failedStep int, msg, logText string) {
	if j.PR == 0 {
		return
	}
	ok, fail, wait, run := "✅ Passed", "❌ Failed", "· Waiting", "⏳ Running"
	quick := ok
	if w.Repo.QuickCheck == "" {
		quick = "— Not set up"
	}
	steps := [4]string{ok, quick, ok, ok}
	var headline string
	switch {
	case j.State == store.Landed:
		headline = "**Landed on `" + w.Repo.Main + "`.** Claims released."
		steps[3] = "✅ Merged"
	case failedStep > 0:
		for i := failedStep - 1; i < 4; i++ {
			steps[i] = wait
		}
		steps[failedStep-1] = fail
		headline = fmt.Sprintf("**Sent back to %s** (try %d of %d). The agent has the details below and is fixing it.", j.Agent, j.Attempts, w.Repo.MaxAttempts)
		if j.State == store.NeedsPerson {
			headline = fmt.Sprintf("**Needs a person.** Failed %d times; the agent has stopped.", j.Attempts)
		}
	case j.State == store.Testing:
		steps[2], steps[3] = run, wait
		headline = "**Testing** on top of the latest `" + w.Repo.Main + "`."
	case j.State == store.Ready:
		steps[3] = "👀 Waiting for review"
		headline = "**Ready for review.** Tested on top of the latest `" + w.Repo.Main + "`."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n### Trainyard\n\n%s\n\n", summaryMarker, headline)
	fmt.Fprintf(&b, "| Step | |\n|---|---|\n| 1 · Update: on top of latest `%s` (`%s`) | %s |\n| 2 · Quick checks | %s |\n| 3 · Tests (CI) | %s |\n| 4 · Review | %s |\n\n",
		w.Repo.Main, short(j.BaseSHA), steps[0], steps[1], steps[2], steps[3])
	if failedStep > 0 && msg != "" {
		fmt.Fprintf(&b, "%s\n\n", msg)
		if logText != "" {
			fmt.Fprintf(&b, "<details><summary>What the agent was sent</summary>\n\n```\n%s\n```\n</details>\n\n", host.TailLog(logText, 60))
		}
	}
	fmt.Fprintf(&b, "**Agent** %s (%s, %s) · **started by** %s · **job** `%s` · **tries** %d of %d\n\n",
		j.Agent, orDash(j.AgentTool), orDash(j.AgentModel), j.StartedBy, j.ID, max(j.Attempts, 1), w.Repo.MaxAttempts)
	fmt.Fprintf(&b, "<sub>Updated %s. This comment is edited in place as the job moves.</sub>", w.now().UTC().Format("Jan 2, 15:04 MST"))
	if err := w.Host.UpsertComment(ctx, j.Repo, j.PR, summaryMarker, b.String()); err != nil {
		log.Printf("summary comment %s#%d: %v", j.Repo, j.PR, err)
	}
}
