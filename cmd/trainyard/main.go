// Command trainyard is both the server and the developer/agent CLI.
//
//	trainyard server --config trainyard.json     run the server, merge queue and dashboard
//	trainyard admin add-user NAME                create a person and print their token
//	trainyard login --server URL --token TOKEN   save your server and token
//	trainyard start "Fix the login bug"          new job + its own workspace folder
//	trainyard claim src/auth/*                   say which files you'll change
//	trainyard submit -m "what changed"           commit, push, enter the merge queue
//	trainyard feedback                           status and any test failures
//	trainyard status                             every job and claim in this repo
//	trainyard mcp --dir .                        agent tools over stdio (MCP)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/sgracia13/trainyard/internal/client"
	"github.com/sgracia13/trainyard/internal/host"
	"github.com/sgracia13/trainyard/internal/mcp"
	"github.com/sgracia13/trainyard/internal/server"
	"github.com/sgracia13/trainyard/internal/store"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "server":
		err = runServer(args)
	case "admin":
		err = runAdmin(args)
	case "login":
		err = runLogin(args)
	case "start":
		err = runStart(args)
	case "claim":
		err = runClaim(args)
	case "release":
		err = withJob(func(c *client.Client, jf *client.JobFile, _ string) error {
			n, err := c.Release(jf.JobID, args)
			fmt.Printf("Released %d claim(s).\n", n)
			return err
		})
	case "heartbeat":
		err = withJob(func(c *client.Client, jf *client.JobFile, _ string) error {
			return c.Heartbeat(jf.JobID, strings.Join(args, " "))
		})
	case "submit":
		err = runSubmit(args)
	case "feedback":
		err = withJob(func(c *client.Client, jf *client.JobFile, _ string) error {
			r, err := c.Feedback(jf.JobID)
			if err == nil {
				fmt.Println(mcp.FormatFeedback(r))
			}
			return err
		})
	case "status":
		err = runStatus(args)
	case "abandon":
		err = withJob(func(c *client.Client, jf *client.JobFile, _ string) error {
			if err := c.Abandon(jf.JobID); err != nil {
				return err
			}
			fmt.Println("Stopped job", jf.JobID, "and released its claims. Remove the folder with `trainyard done`.")
			return nil
		})
	case "done":
		err = runDone()
	case "mcp":
		err = runMCP(args)
	case "version", "--version":
		fmt.Println("trainyard", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trainyard:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Trainyard — many agents, one clean main line.

Server:
  trainyard server --config trainyard.json
  trainyard admin add-user NAME [--config trainyard.json]

Developers and agents:
  trainyard login --server URL --token TOKEN
  trainyard start "what to do" [--agent NAME --tool copilot --model MODEL --repo owner/name]
  trainyard claim PATH... [--reason "what you're changing"]
  trainyard submit -m "what changed"
  trainyard feedback | status | heartbeat [note] | release [PATH...] | abandon | done
  trainyard mcp [--dir FOLDER]     agent tools for Copilot, Claude Code, Cursor, Codex
`)
}

// ---- Server ----

func openFromConfig(path string) (*server.Config, *store.Store, error) {
	cfg, err := server.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "trainyard.db"))
	return cfg, st, err
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	cfgPath := fs.String("config", "trainyard.json", "config file")
	fs.Parse(args)
	cfg, st, err := openFromConfig(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()
	token := os.Getenv(cfg.GitHub.TokenEnv)
	if token == "" {
		return fmt.Errorf("set %s to a GitHub token that can read and write code, pull requests and commit statuses on: %s",
			cfg.GitHub.TokenEnv, repoNames(cfg))
	}
	if n, _ := st.CountUsers(); n == 0 {
		tok, err := st.AddUser("admin")
		if err != nil {
			return err
		}
		fmt.Printf("\nFirst start: created user \"admin\".\nToken (shown once): %s\nAdd people with: trainyard admin add-user NAME\n\n", tok)
	}
	srv := server.New(cfg, st, host.NewGitHub(cfg.GitHub.APIURL, cfg.GitHub.WebURL, token))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.RunWorkers(ctx)
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sh)
	}()
	log.Printf("Trainyard %s: dashboard at %s, watching %s", version, cfg.PublicURL, repoNames(cfg))
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func repoNames(cfg *server.Config) string {
	var n []string
	for _, r := range cfg.Repos {
		n = append(n, r.Name)
	}
	return strings.Join(n, ", ")
}

func runAdmin(args []string) error {
	if len(args) < 2 || args[0] != "add-user" {
		return errors.New("usage: trainyard admin add-user NAME [--config trainyard.json]")
	}
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	cfgPath := fs.String("config", "trainyard.json", "config file")
	fs.Parse(args[2:])
	_, st, err := openFromConfig(*cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()
	tok, err := st.AddUser(args[1])
	if err != nil {
		return err
	}
	fmt.Printf("Created %s. Their token (shown once):\n%s\n", args[1], tok)
	return nil
}

// ---- Developer commands ----

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	srv := fs.String("server", "", "Trainyard server URL, e.g. https://trainyard.example.com")
	tok := fs.String("token", "", "your token from `trainyard admin add-user`")
	fs.Parse(args)
	if *srv == "" || *tok == "" {
		return errors.New("usage: trainyard login --server URL --token TOKEN")
	}
	c := &client.Client{Server: strings.TrimRight(*srv, "/"), Token: *tok, HTTP: http.DefaultClient}
	var me store.User
	if err := c.Do("GET", "/api/me", nil, &me); err != nil {
		return err
	}
	if err := client.SaveSettings(client.Settings{Server: c.Server, Token: c.Token}); err != nil {
		return err
	}
	fmt.Printf("Signed in to %s as %s.\n", c.Server, me.Name)
	return nil
}

// interleaved lets flags come after positional words: `start "title" --tool copilot`.
func interleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos
}

func runStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	agent := fs.String("agent", os.Getenv("TRAINYARD_AGENT"), "agent name shown to the team")
	tool := fs.String("tool", os.Getenv("TRAINYARD_AGENT_TOOL"), "e.g. copilot, claude-code, cursor, codex")
	model := fs.String("model", os.Getenv("TRAINYARD_AGENT_MODEL"), "model name, if you know it")
	repo := fs.String("repo", "", "owner/name (default: from the origin remote)")
	pos := interleaved(fs, args)
	title := strings.Join(pos, " ")
	if title == "" {
		return errors.New(`usage: trainyard start "what to do"`)
	}
	c, err := client.Load()
	if err != nil {
		return err
	}
	if *repo == "" {
		if *repo, err = client.DetectRepo("."); err != nil {
			return err
		}
	}
	res, err := c.StartJob(*repo, title, *agent, *tool, *model)
	if err != nil {
		return err
	}
	j := res.Job
	path, err := client.CreateWorkspace(".", client.JobFile{JobID: j.ID, Repo: j.Repo, Branch: j.Branch, Main: res.Main, Title: j.Title, Agent: j.Agent})
	if err != nil {
		_ = c.Abandon(j.ID)
		return fmt.Errorf("couldn't create the workspace: %w", err)
	}
	fmt.Printf("Started job %s: %s\nWorkspace: %s\nBranch:    %s (from the latest %s)\n\nNext: cd %q, point your agent there, and claim files before editing.\n",
		j.ID, j.Title, path, j.Branch, res.Main, path)
	return nil
}

func withJob(fn func(c *client.Client, jf *client.JobFile, ws string) error) error {
	jf, ws, err := client.FindJob(".")
	if err != nil {
		return err
	}
	c, err := client.Load()
	if err != nil {
		return err
	}
	return fn(c, jf, ws)
}

func runClaim(args []string) error {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	reason := fs.String("reason", "", "what you're changing, shown to other agents")
	files := interleaved(fs, args)
	if len(files) == 0 {
		return errors.New("usage: trainyard claim PATH... [--reason TEXT]")
	}
	return withJob(func(c *client.Client, jf *client.JobFile, _ string) error {
		r, err := c.Claim(jf.JobID, files, *reason)
		if err == nil {
			fmt.Println(mcp.FormatClaim(r))
		}
		return err
	})
}

func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	msg := fs.String("m", "", "summary of what changed (also the commit message for uncommitted work)")
	fs.Parse(args)
	return withJob(func(c *client.Client, jf *client.JobFile, ws string) error {
		out, err := mcp.SubmitWork(c, jf, ws, *msg)
		if err == nil {
			fmt.Println(out)
		}
		return err
	})
}

func runStatus(args []string) error {
	c, err := client.Load()
	if err != nil {
		return err
	}
	var st server.State
	if err := c.Do("GET", "/api/state", nil, &st); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tAGENT\tBY\tSTATE\tTITLE\tCLAIMS")
	for _, j := range st.Jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Agent, j.StartedBy, j.State, j.Title, strings.Join(j.Claims, " "))
	}
	tw.Flush()
	for _, o := range st.Overlaps {
		fmt.Printf("\nOverlap: %s (%s) and %s (%s) both claimed %s\n", o.AgentA, o.A, o.AgentB, o.B, o.Pattern)
	}
	return nil
}

func runDone() error {
	return withJob(func(c *client.Client, jf *client.JobFile, ws string) error {
		j, err := c.Job(jf.JobID)
		if err != nil {
			return err
		}
		if j.State != store.Landed && j.State != store.Abandoned {
			return fmt.Errorf("job %s is still %s; use `trainyard abandon` first if you want to stop it", j.ID, j.State)
		}
		if err := client.RemoveWorkspace(ws); err != nil {
			return err
		}
		fmt.Println("Removed workspace", ws)
		return nil
	})
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dir := fs.String("dir", "", "repo or workspace folder the agent works in (default: current folder)")
	fs.Parse(args)
	d := *dir
	if d == "" {
		d = os.Getenv("TRAINYARD_WORKSPACE")
	}
	if d == "" {
		d, _ = os.Getwd()
	}
	return (&mcp.Server{Dir: d, Version: version}).Serve(os.Stdin, os.Stdout)
}
