// Package gitops runs git for the merge queue. It shells out to the git
// binary so behaviour matches what developers see on their own machines.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Identity used for commits the server rewrites (trailers) — the author stays the agent's.
var Env = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_COMMITTER_NAME=Trainyard",
	"GIT_COMMITTER_EMAIL=trainyard@localhost",
}

type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v\n%s", strings.Join(redact(e.Args), " "), e.Err, redactText(e.Output))
}

// Run runs git in dir and returns trimmed combined output.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), Env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, &Error{Args: args, Output: out, Err: err}
	}
	return out, nil
}

// Lines splits git output into non-empty lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Hide tokens embedded in clone URLs when printing errors.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactText(a)
	}
	return out
}

func redactText(s string) string {
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			return s
		}
		rest := s[i+3:]
		at := strings.Index(rest, "@")
		sl := strings.IndexAny(rest, "/ \n")
		if at < 0 || (sl >= 0 && sl < at) {
			return s[:i+3] + redactText(rest)
		}
		return s[:i+3] + "***@" + redactText(rest[at+1:])
	}
}
