// Package claims decides whether two file claims overlap.
//
// A claim is a repo-relative path or glob, e.g. "src/auth/LoginPage.tsx",
// "src/auth/*" or "src/**". Claims are advisory: overlapping is allowed, but
// both agents are told about it.
package claims

import (
	"path"
	"strings"
)

// Normalize cleans a claim pattern: forward slashes, no leading "./" or "/",
// and a trailing "/" turned into "/**" (a whole folder).
func Normalize(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	folder := strings.HasSuffix(p, "/")
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if p == "" || p == "." {
		return "**"
	}
	if folder {
		p += "/**"
	}
	return p
}

func hasWild(s string) bool { return strings.ContainsAny(s, "*?[") }

// Match reports whether a concrete file path matches a claim pattern.
// "**" matches any number of folders; "*" stays within one folder.
func Match(pattern, file string) bool {
	return matchSegs(strings.Split(Normalize(pattern), "/"), strings.Split(Normalize(file), "/"))
}

func matchSegs(p, f []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(f); i++ {
				if matchSegs(p[1:], f[i:]) {
					return true
				}
			}
			return false
		}
		if len(f) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], f[0]); !ok {
			return false
		}
		p, f = p[1:], f[1:]
	}
	return len(f) == 0
}

// Overlap reports whether two claims could touch the same file, i.e. whether
// some path matches both. It errs on the side of "yes" when two wildcard
// segments meet (e.g. *.ts and *.tsx): a false warning is cheap, a missed one is not.
func Overlap(a, b string) bool {
	return intersect(strings.Split(Normalize(a), "/"), strings.Split(Normalize(b), "/"))
}

func intersect(a, b []string) bool {
	switch {
	case len(a) == 0 && len(b) == 0:
		return true
	case len(a) > 0 && a[0] == "**":
		return intersect(a[1:], b) || (len(b) > 0 && intersect(a, b[1:]))
	case len(b) > 0 && b[0] == "**":
		return intersect(a, b[1:]) || (len(a) > 0 && intersect(a[1:], b))
	case len(a) == 0 || len(b) == 0:
		return false
	}
	return segmentsMeet(a[0], b[0]) && intersect(a[1:], b[1:])
}

func segmentsMeet(x, y string) bool {
	if hasWild(x) && hasWild(y) {
		return true
	}
	ok1, _ := path.Match(x, y)
	ok2, _ := path.Match(y, x)
	return ok1 || ok2
}

// AnyOverlap reports whether any changed file touches any of the claims.
func AnyOverlap(patterns, files []string) bool {
	for _, p := range patterns {
		for _, f := range files {
			if Overlap(p, f) {
				return true
			}
		}
	}
	return false
}
