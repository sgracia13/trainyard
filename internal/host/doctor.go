package host

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sgracia13/trainyard/internal/gitops"
)

// Check is one line of `trainyard doctor` output.
type Check struct {
	Level string // "ok", "warn" or "fail"
	Text  string
}

// Doctor runs read-only checks that a repo is set up for Trainyard. It never writes.
func (g *GitHub) Doctor(ctx context.Context, repo, main string) []Check {
	var out []Check
	add := func(level, f string, a ...any) { out = append(out, Check{level, fmt.Sprintf(f, a...)}) }

	var info struct {
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo, nil, &info); err != nil {
		add("fail", "Can't reach %s with this token: %v", repo, err)
		return out
	}
	add("ok", "Token can read %s", repo)
	if info.Permissions.Push {
		add("ok", "Token can push (needed for trainyard/queue/* branches)")
	} else {
		add("fail", "Token can't push to %s: give it Contents: Read and write", repo)
	}
	if info.DefaultBranch == main {
		add("ok", "Main branch is %q", main)
	} else {
		add("warn", "Config says main branch %q, but GitHub's default is %q", main, info.DefaultBranch)
	}
	if _, err := gitops.Run(ctx, "", "ls-remote", "--exit-code", g.CloneURL(repo), "refs/heads/"+main); err != nil {
		add("fail", "Git can't fetch %s from %s (check the token and network)", main, repo)
	} else {
		add("ok", "Git can fetch %s", main)
	}

	var wf struct {
		Workflows []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			State string `json:"state"`
		} `json:"workflows"`
	}
	if err := g.do(ctx, "GET", "/repos/"+repo+"/actions/workflows", nil, &wf); err != nil {
		add("warn", "Couldn't list GitHub Actions workflows (token needs Actions: Read): %v", short(err))
	} else {
		var onPR []string
		for _, w := range wf.Workflows {
			if w.State != "active" {
				continue
			}
			var file struct {
				Content string `json:"content"`
			}
			if err := g.do(ctx, "GET", "/repos/"+repo+"/contents/"+w.Path+"?ref="+url.QueryEscape(main), nil, &file); err != nil {
				continue
			}
			body, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
			if strings.Contains(string(body), "pull_request") {
				onPR = append(onPR, w.Path[strings.LastIndex(w.Path, "/")+1:])
			}
		}
		if len(onPR) > 0 {
			add("ok", "CI runs on pull requests (%s)", strings.Join(onPR, ", "))
		} else {
			add("fail", "No workflow runs on pull_request, so Trainyard has no tests to wait for. Add `on: pull_request` to your CI")
		}
	}

	var rsc struct {
		Contexts []string `json:"contexts"`
	}
	err := g.do(ctx, "GET", "/repos/"+repo+"/branches/"+main+"/protection/required_status_checks", nil, &rsc)
	var ae *apiError
	switch {
	case err == nil:
		required := false
		for _, c := range rsc.Contexts {
			required = required || c == "trainyard"
		}
		if required {
			add("ok", "%s requires the trainyard check", main)
		} else {
			add("warn", "%s doesn't require the trainyard check yet (Settings → Branches), so untested work can still be merged", main)
		}
	case errors.As(err, &ae) && ae.Status == 404:
		add("warn", "%s has no required checks. Recommended: require the trainyard check (Settings → Branches)", main)
	default:
		add("warn", "Couldn't read branch protection (needs Administration: Read). Check by hand that %s requires the trainyard check", main)
	}
	return out
}

func short(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": {"); i > 0 {
		return s[:i]
	}
	return s
}
