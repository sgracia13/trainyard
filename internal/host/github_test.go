package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A small stand-in for the GitHub API, checking the requests Trainyard sends.
func fakeGitHub(t *testing.T) (*httptest.Server, *[]string) {
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" && !strings.HasPrefix(r.URL.Path, "/blob") {
			t.Errorf("missing auth on %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/repos/acme/shop/pulls":
			if r.URL.Query().Get("head") != "acme:trainyard/queue/j1" {
				t.Errorf("head filter = %q", r.URL.Query().Get("head"))
			}
			w.Write([]byte(`[]`))
		case r.Method == "POST" && p == "/repos/acme/shop/pulls":
			if body["head"] != "trainyard/queue/j1" || body["base"] != "main" {
				t.Errorf("pull body %v", body)
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"number":7,"html_url":"https://github.com/acme/shop/pull/7","state":"open"}`))
		case p == "/repos/acme/shop/issues/7/labels" || p == "/repos/acme/shop/issues/7/comments":
			w.Write([]byte(`{}`))
		case strings.HasPrefix(p, "/repos/acme/shop/statuses/"):
			if body["context"] != "trainyard" || body["state"] != "failure" || len(body["description"].(string)) > 140 {
				t.Errorf("status body %v", body)
			}
			w.WriteHeader(201)
		case p == "/repos/acme/shop/commits/abc/check-runs":
			w.Write([]byte(`{"check_runs":[
				{"id":11,"name":"unit","status":"completed","conclusion":"failure","html_url":"https://x/11","app":{"slug":"github-actions"}},
				{"id":12,"name":"lint","status":"completed","conclusion":"success","app":{"slug":"github-actions"}},
				{"id":13,"name":"trainyard","status":"completed","conclusion":"failure","app":{"slug":"other"}}]}`))
		case p == "/repos/acme/shop/commits/abc/status":
			w.Write([]byte(`{"statuses":[{"context":"trainyard","state":"pending"},{"context":"ci/legacy","state":"success"}]}`))
		case p == "/repos/acme/shop/actions/jobs/11/logs":
			http.Redirect(w, r, srv.URL+"/blob/log", http.StatusFound)
		case p == "/blob/log":
			fmt.Fprint(w, "\ufeff2026-11-09T10:00:00.0000000Z Current runner version: '2.338.2'\r\n"+
				"2026-11-09T10:00:00.1Z ##[group]Run actions/checkout@v4\n2026-11-09T10:00:00.2Z with:\n2026-11-09T10:00:00.3Z ##[endgroup]\n"+
				"2026-11-09T10:00:00.4Z checkout noise\n"+
				"2026-11-09T10:00:01.0Z ##[group]Run npm test\n2026-11-09T10:00:01.1Z \x1b[36;1mnpm test\x1b[0m\n2026-11-09T10:00:01.2Z shell: bash\n2026-11-09T10:00:01.3Z ##[endgroup]\n"+
				"2026-11-09T10:00:02.0Z FAIL refund.spec.ts\n2026-11-09T10:00:02.1Z ##[error]Process completed with exit code 1.\n"+
				"2026-11-09T10:00:03.0Z Post job cleanup.\n")
		case p == "/repos/acme/shop/pulls/7":
			w.Write([]byte(`{"number":7,"state":"closed","merged":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestGitHub(t *testing.T) {
	srv, _ := fakeGitHub(t)
	g := NewGitHub(srv.URL, "https://github.com", "tok")
	ctx := context.Background()

	if u := g.CloneURL("acme/shop"); u != "https://x-access-token:tok@github.com/acme/shop.git" {
		t.Errorf("clone url %s", u)
	}
	n, url, created, err := g.EnsurePR(ctx, "acme/shop", "trainyard/queue/j1", "main", "T", "B")
	if err != nil || n != 7 || !created || !strings.HasSuffix(url, "/pull/7") {
		t.Fatalf("EnsurePR: %d %s %v %v", n, url, created, err)
	}
	if err := g.AddLabels(ctx, "acme/shop", 7, []string{"trainyard"}); err != nil {
		t.Fatal(err)
	}
	if err := g.SetStatus(ctx, "acme/shop", "abc", "failure", strings.Repeat("x", 200), "http://localhost:7070/#job=j1"); err != nil {
		t.Fatal(err)
	}
	res, err := g.Checks(ctx, "acme/shop", "abc")
	if err != nil {
		t.Fatal(err)
	}
	// Trainyard's own check and status are ignored: unit (fail) + lint + ci/legacy = 3.
	if res.State != ChecksFailure || res.Total != 3 || len(res.Failed) != 1 || res.Failed[0].Name != "unit" {
		t.Fatalf("checks: %+v", res)
	}
	if res.Failed[0].Log != "$ npm test\nFAIL refund.spec.ts\nProcess completed with exit code 1." {
		t.Fatalf("log: %q", res.Failed[0].Log)
	}
	st, err := g.PRState(ctx, "acme/shop", 7)
	if err != nil || !st.Merged {
		t.Fatalf("pr state %+v %v", st, err)
	}
}
