package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgracia13/trainyard/internal/store"
)

// TestDemoServer fills Trainyard with a realistic morning and serves the dashboard
// on 127.0.0.1:7071 for a while, for screenshots. Run with TRAINYARD_DEMO=1.
func TestDemoServer(t *testing.T) {
	if os.Getenv("TRAINYARD_DEMO") == "" {
		t.Skip("set TRAINYARD_DEMO=1")
	}
	e := setup(t)
	// Landed earlier.
	a, wsA := e.start("maya", "Fix typo in checkout email", "Agent C")
	e.edit(wsA, "src/email.txt", "", "Thanks for your order\n")
	e.submit("maya", a, wsA, "Typo")
	e.tick()
	e.fake.Merge(e.ctx, repo, e.job(a.JobID).PR)
	e.tick()
	// Waiting for review.
	b, wsB := e.start("maya", "Add dark mode", "Agent A")
	e.client("maya").Claim(b.JobID, []string{"ui/theme/*"}, "theme colours")
	e.edit(wsB, "ui/theme/dark.css", "", "body{background:#000}\n")
	e.submit("maya", b, wsB, "Adds a dark colour theme")
	e.tick()
	// Overlap.
	c, _ := e.start("maya", "Rewrite login page text", "Agent C")
	e.client("maya").Claim(c.JobID, []string{"src/login.txt"}, "headings and error messages")
	d, _ := e.start("sam", "Fix the login bug", "Agent B")
	e.client("sam").Claim(d.JobID, []string{"src/*"}, "expired sessions")
	// Sent back.
	f, wsF := e.start("sam", "Update payment API", "Agent E")
	e.client("sam").Claim(f.JobID, []string{"src/payments/*"}, "refund amounts")
	e.edit(wsF, "src/payments/refund.txt", "", "amount: cents BUG\n")
	e.submit("sam", f, wsF, "Refunds in cents")
	e.tick()
	os.WriteFile(filepath.Join(os.TempDir(), "ty-demo-token"), []byte(e.tokens["maya"]), 0o644)
	_ = store.Ready
	srv := &http.Server{Addr: "127.0.0.1:7071", Handler: e.srv.Handler()}
	go srv.ListenAndServe()
	time.Sleep(90 * time.Second)
	srv.Close()
}
