package claims

import "testing"

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"src/auth/LoginPage.tsx", "src/auth/LoginPage.tsx", true},
		{"src/auth/*", "src/auth/LoginPage.tsx", true},
		{"src/auth/*", "src/auth/deep/x.ts", false},
		{"src/auth/**", "src/auth/deep/x.ts", true},
		{"src/auth/", "src/auth/deep/x.ts", true},
		{"src/auth/*", "src/search/*", false},
		{"src/**", "src/search/*", true},
		{"ui/theme/*", "src/auth/*", false},
		{"./src/a.ts", "src/a.ts", true},
		{"src/*.ts", "src/*.tsx", true}, // both globs in the same folder: warn
		{"README.md", "docs/README.md", false},
		{"src/*", "src/payments/*", false},
		{"src/**/*.ts", "src/a/b/c.ts", true},
		{"**/test/*", "src/test/x.go", true},
	}
	for _, c := range cases {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
		if got := Overlap(c.b, c.a); got != c.want {
			t.Errorf("Overlap(%q,%q) not symmetric", c.b, c.a)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"./a/b/": "a/b/**", "/x.go": "x.go", "a\\b.go": "a/b.go", "": "**"} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q)=%q want %q", in, got, want)
		}
	}
}
