// Command agentd is the single-binary agent server (design section 3). This
// scaffold wires the Phase 0-1 core: config, event bus, durable run-log, the
// Claude Code harness adapter (stream-json), the in-process Telegram channel,
// the Session Manager, and the local HTTP+WS API.
//
// Subcommands:
//
//	agentd serve   -config config.toml     run the daemon (default)
//	agentd smoke   -prompt "..."           one headless Claude Code turn, print
//	                                        parsed normalized events (the vertical)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kjhnns/agentd/internal/api"
	"github.com/kjhnns/agentd/internal/channel/telegram"
	"github.com/kjhnns/agentd/internal/channel/web"
	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/harness/claudecode"
	"github.com/kjhnns/agentd/internal/notify"
	"github.com/kjhnns/agentd/internal/runlog"
	"github.com/kjhnns/agentd/internal/scheduler"
	"github.com/kjhnns/agentd/internal/session"
	"github.com/kjhnns/agentd/internal/workspace"
)

func main() {
	if len(os.Args) < 2 {
		serve(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "smoke":
		smoke(os.Args[2:])
	case "chat":
		chat(os.Args[2:])
	case "init-workspace":
		initWorkspace(os.Args[2:])
	case "memory":
		memoryCmd(os.Args[2:])
	case "jobs":
		jobsCmd(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println("usage: agentd [serve|smoke|chat|init-workspace|memory|jobs] [flags]")
		fmt.Println("  init-workspace [name]              scaffold a workspace (default ~/.agentd/workspaces/<name>)")
		fmt.Println("  memory index|links|add|get [...]   maintain a workspace's memory wiki")
		fmt.Println("  jobs list|run <name>|runs <name>   inspect and fire scheduler jobs (talks to the running daemon)")
	default:
		serve(os.Args[1:])
	}
}

// initWorkspace scaffolds a named workspace under the workspaces root: starter
// instructions (AGENTS.md + CLAUDE.md mirror), an example memory wiki, an
// empty handoff, and a git repo with the initial commit.
func initWorkspace(args []string) {
	fs := flag.NewFlagSet("init-workspace", flag.ExitOnError)
	root := fs.String("root", workspace.DefaultRoot(), "directory holding workspaces")
	_ = fs.Parse(args)
	name := fs.Arg(0)
	if name == "" {
		name = "default"
	}
	st := workspace.NewStore(*root, name)
	ws, err := workspace.Init(st.Path(name))
	if err != nil {
		log.Fatalf("init-workspace: %v", err)
	}
	if !workspace.GitAvailable() {
		log.Printf("init-workspace: git not found on PATH; workspace is unversioned")
	}
	fmt.Printf("workspace %q ready at %s\n", ws.Name, ws.Root)
	fmt.Println("  instructions/AGENTS.md   the constitution (CLAUDE.md mirrors it)")
	fmt.Println("  memory/INDEX.md          injected memory index; pages in memory/pages/")
	fmt.Println("  context.md               session handoff")
	fmt.Println("  work/                    scratch space")
}

// memoryCmd maintains a workspace's memory wiki: rebuild the INDEX from page
// frontmatter, check [[wikilinks]], add/get pages.
func memoryCmd(args []string) {
	if len(args) < 1 {
		log.Fatal("usage: agentd memory <index|links|add|get> [flags]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("memory "+sub, flag.ExitOnError)
	root := fs.String("root", workspace.DefaultRoot(), "directory holding workspaces")
	wsName := fs.String("workspace", "default", "workspace name")
	slug := fs.String("slug", "", "page slug (add/get)")
	title := fs.String("title", "", "page title (add)")
	hook := fs.String("hook", "", "one-line hook shown in the INDEX (add)")
	tags := fs.String("tags", "", "comma-separated tags (add)")
	body := fs.String("body", "", "page body; reads stdin if empty (add)")
	_ = fs.Parse(args[1:])

	ws, err := workspace.Load(workspace.NewStore(*root, *wsName).Path(*wsName))
	if err != nil {
		log.Fatalf("memory: %v", err)
	}
	switch sub {
	case "index":
		content, err := ws.RebuildIndex()
		if err != nil {
			log.Fatalf("memory index: %v", err)
		}
		fmt.Print(content)
	case "links":
		links, dangling, err := ws.CheckLinks()
		if err != nil {
			log.Fatalf("memory links: %v", err)
		}
		for from, targets := range links {
			fmt.Printf("%s -> %s\n", from, strings.Join(targets, ", "))
		}
		if len(dangling) == 0 {
			fmt.Println("no dangling wikilinks")
			return
		}
		for _, d := range dangling {
			fmt.Printf("DANGLING: [[%s]] in pages/%s.md\n", d.Target, d.FromSlug)
		}
		os.Exit(1)
	case "add":
		if *slug == "" {
			log.Fatal("memory add: -slug required")
		}
		b := *body
		if b == "" {
			data, _ := io.ReadAll(os.Stdin)
			b = string(data)
		}
		var tagList []string
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tagList = append(tagList, t)
			}
		}
		p := &workspace.Page{Slug: *slug, Title: *title, Hook: *hook, Tags: tagList, Body: b}
		if p.Title == "" {
			p.Title = *slug
		}
		if err := ws.PutPage(p); err != nil {
			log.Fatalf("memory add: %v", err)
		}
		// A memory update is a natural commit boundary of its own, so a page
		// re-synthesis reads clearly in the workspace's git history.
		if workspace.GitAvailable() && ws.IsGitRepo() {
			if sha, err := ws.GitCommitAll("memory: " + p.Slug); err != nil {
				log.Printf("memory add: autocommit failed: %v", err)
			} else if sha != "" {
				fmt.Printf("committed %s\n", sha)
			}
		}
		fmt.Printf("wrote pages/%s.md and rebuilt INDEX.md\n", p.Slug)
	case "get":
		if *slug == "" {
			log.Fatal("memory get: -slug required")
		}
		p, err := ws.GetPage(*slug)
		if err != nil {
			log.Fatalf("memory get: %v", err)
		}
		fmt.Print(p.Render())
	default:
		log.Fatalf("memory: unknown subcommand %q (want index|links|add|get)", sub)
	}
}

// jobsCmd is the local ops surface for the scheduler: it talks to the RUNNING
// daemon's API (bind + bearer from config.toml), so lists reflect live state.
//
//	agentd jobs list          jobs + next fire + last run status
//	agentd jobs run <name>    fire a job now (manual trigger)
//	agentd jobs runs <name>   recent run history for a job
func jobsCmd(args []string) {
	if len(args) < 1 {
		log.Fatal("usage: agentd jobs <list|run NAME|runs NAME> [-config config.toml]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("jobs "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "config.toml", "path to config.toml (for bind + bearer)")
	_ = fs.Parse(args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("jobs: config: %v", err)
	}
	base := "http://" + cfg.Server.Bind
	call := func(method, path string) []byte {
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			log.Fatalf("jobs: %v", err)
		}
		if cfg.Server.APIBearer != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Server.APIBearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatalf("jobs: %v (is the daemon running on %s?)", err, cfg.Server.Bind)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			log.Fatalf("jobs: %s %s -> %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
		}
		return body
	}

	switch sub {
	case "list":
		var jobs []scheduler.JobStatus
		if err := json.Unmarshal(call(http.MethodGet, "/jobs"), &jobs); err != nil {
			log.Fatalf("jobs list: %v", err)
		}
		if len(jobs) == 0 {
			fmt.Println("no jobs configured (add [[job]] blocks to config.toml)")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tENABLED\tTRIGGER\tWORKSPACE\tNOTIFY\tNEXT FIRE\tLAST RUN")
		for _, j := range jobs {
			next := "-"
			if j.NextFire != nil {
				next = j.NextFire.Local().Format("2006-01-02 15:04:05")
			}
			last := "-"
			if j.LastRun != nil {
				last = fmt.Sprintf("%s %s (%s)", j.LastRun.Status, j.LastRun.Start.Local().Format("01-02 15:04"), j.LastRun.Trigger)
			}
			ws := j.Workspace
			if ws == "" {
				ws = "(default)"
			}
			fmt.Fprintf(w, "%s\t%v\t%s\t%s\t%s\t%s\t%s\n", j.Name, j.Enabled, j.Trigger, ws, j.Notify, next, last)
		}
		_ = w.Flush()
	case "run":
		name := fs.Arg(0)
		if name == "" {
			log.Fatal("usage: agentd jobs run NAME")
		}
		fmt.Println(strings.TrimSpace(string(call(http.MethodPost, "/jobs/"+name+"/run"))))
	case "runs":
		name := fs.Arg(0)
		if name == "" {
			log.Fatal("usage: agentd jobs runs NAME")
		}
		var runs []scheduler.JobRun
		if err := json.Unmarshal(call(http.MethodGet, "/jobs/"+name+"/runs"), &runs); err != nil {
			log.Fatalf("jobs runs: %v", err)
		}
		if len(runs) == 0 {
			fmt.Println("no runs recorded")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RUN\tTRIGGER\tSTART\tDURATION\tSTATUS\tRESULT/ERROR")
		for _, r := range runs {
			dur := "-"
			if !r.End.IsZero() {
				dur = r.End.Sub(r.Start).Round(time.Millisecond).String()
			}
			msg := r.Result
			if r.Error != "" {
				msg = r.Error
			}
			if len(msg) > 80 {
				msg = msg[:80] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.RunID, r.Trigger, r.Start.Local().Format("01-02 15:04:05"), dur, r.Status, msg)
		}
		_ = w.Flush()
	default:
		log.Fatalf("jobs: unknown subcommand %q (want list|run|runs)", sub)
	}
}

// smoke proves the Claude Code stream-json mapping with ONE classic `claude -p`
// headless turn (one-shot) and prints the normalized events as JSON.
func smoke(args []string) {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	prompt := fs.String("prompt", "Reply with exactly the word PONG and nothing else", "prompt to send")
	bin := fs.String("claude", "claude", "claude binary")
	skip := fs.Bool("skip-permissions", true, "pass --dangerously-skip-permissions")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	evs, err := claudecode.OneShot(ctx, *bin, *prompt, *skip)
	if err != nil {
		log.Fatalf("smoke: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(evs)
}

// chat demos a real 2-turn PERSISTENT exchange on ONE claude process, proving
// continuity: turn 1 sets a codeword, turn 2 asks for it back.
func chat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	bin := fs.String("claude", "claude", "claude binary")
	skip := fs.Bool("skip-permissions", true, "pass --dangerously-skip-permissions")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	a := claudecode.New(*bin)
	h, err := a.Start(ctx, harness.SessionConfig{
		SessionID: "chat-demo", Cwd: os.TempDir(), SkipPermissions: *skip,
	})
	if err != nil {
		log.Fatalf("chat start: %v", err)
	}
	defer a.Teardown(h)

	// Collect result events off the persistent stream.
	results := make(chan string, 4)
	go func() {
		for e := range a.Events(h) {
			if e.Kind == eventbus.KindResult {
				results <- e.Text
			}
		}
	}()

	turns := []string{
		"Remember the codeword is HELIOTROPE. Reply with just OK.",
		"What is the codeword? Reply with just the word.",
	}
	for i, t := range turns {
		fmt.Printf("turn %d >>> %s\n", i+1, t)
		if err := a.Send(ctx, h, harness.Input{Text: t}); err != nil {
			log.Fatalf("chat turn %d: %v", i+1, err)
		}
		select {
		case r := <-results:
			fmt.Printf("turn %d <<< %s\n", i+1, r)
		case <-time.After(120 * time.Second):
			log.Fatalf("chat turn %d: no result", i+1)
		}
	}
	fmt.Println("(both turns ran on the SAME process; turn 2 recalling the codeword proves continuity)")
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.toml", "path to config.toml")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	bus := eventbus.New()
	logPath := filepath.Join(cfg.Server.StateDir, "runlog.jsonl")
	rl, err := runlog.Open(logPath)
	if err != nil {
		log.Fatalf("runlog: %v", err)
	}
	defer rl.Close()
	log.Printf("agentd: run-log at %s", logPath)

	// Harness: only claude-code is implemented in this scaffold.
	var model string
	if len(cfg.Harness) > 0 {
		model = cfg.Harness[0].Model
	}
	adapter := claudecode.New("claude").WithContextWindow(cfg.Session.ContextWindow)
	mgr := session.NewManager(adapter, bus, rl)
	if len(cfg.Harness) > 0 {
		mgr.SkipPermissions = cfg.Harness[0].SkipPermissions
	}
	// ACP-ready permission policy (value, not a hard-coded flag): derive from
	// the skip_permissions toggle. A future ACP adapter maps the SAME policy to
	// client-side permission handling.
	if mgr.SkipPermissions {
		mgr.PermissionMode = harness.PermissionSkip
	} else {
		mgr.PermissionMode = harness.PermissionPrompt
	}
	// Session lifecycle + context-reset tuning ([session] config).
	mgr.Policy = session.Policy{
		ContextResetPressure: cfg.Session.ContextResetPressure,
		IdleTimeout:          cfg.Session.IdleTimeout,
		MaxTurns:             cfg.Session.MaxTurns,
		MaxWallclock:         cfg.Session.MaxWallclock,
		GCInterval:           cfg.Session.GCInterval,
		CheckpointTimeout:    cfg.Session.CheckpointTimeout,
	}
	// Workspaces: every session is homed in a workspace (cwd = workspace root,
	// composed injection via --append-system-prompt). See internal/workspace.
	mgr.Workspaces = workspace.NewStore(cfg.Workspace.Root, cfg.Workspace.Default)
	mgr.GitAutoCommit = cfg.Workspace.GitAutocommit
	log.Printf("agentd: harness=claude-code skip_permissions=%v workspaces=%s default=%q git_autocommit=%v",
		mgr.SkipPermissions, mgr.Workspaces.Root, mgr.Workspaces.Default, mgr.GitAutoCommit)
	log.Printf("agentd: session policy context_window=%d reset_pressure=%.2f idle=%s max_turns=%d max_wallclock=%s gc=%s",
		cfg.Session.ContextWindow, mgr.Policy.ContextResetPressure, mgr.Policy.IdleTimeout,
		mgr.Policy.MaxTurns, mgr.Policy.MaxWallclock, mgr.Policy.GCInterval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Session GC/sweeper: enforces idle_timeout (checkpoint-flush + reclaim) and
	// recovers dead sessions on a ticker. Reset triggers (pressure/turns/
	// wallclock) fire after each completed turn, in Manager.Send.
	mgr.StartGC(ctx)

	// Notification hub: the generic, channel-agnostic wire from any producer of
	// user-facing notifications to every notify-capable channel. The scheduler's
	// per-job notify policy is the first producer; each channel that can surface a
	// notification (web, telegram) registers as a sink below. This is what finally
	// gives scheduler.OnNotify a consumer WITHOUT hard-wiring it to any channel.
	hub := notify.NewHub()

	// Scheduler: declarative [[job]] blocks become proactive triggered sessions
	// (a job run = one ordinary turn on the target workspace's warm session).
	// Durable state (last fires + run history) lives in scheduler.jsonl next to
	// the run-log; records are mirrored into the main run-log too.
	jobDefs, err := scheduler.FromConfig(cfg.Job)
	if err != nil {
		log.Fatalf("jobs config: %v", err)
	}
	runner := &scheduler.ManagerRunner{Mgr: mgr, Bus: bus, Model: model}
	sched, err := scheduler.New(jobDefs, runner, filepath.Join(cfg.Server.StateDir, "scheduler.jsonl"), rl)
	if err != nil {
		log.Fatalf("scheduler: %v", err)
	}
	// Wire the scheduler's notify hook to the hub: a job run that the notify
	// policy says should surface (issues/always) fans out to every channel.
	sched.OnNotify = func(job *scheduler.Job, run scheduler.JobRun) {
		lvl := notify.LevelResult
		if run.Status != "ok" {
			lvl = notify.LevelIssue
		}
		hub.Dispatch(notify.Notification{
			Source: "job:" + job.Name,
			Level:  lvl,
			Text:   scheduler.NotifyText(job, run),
			Ts:     run.End,
		})
	}
	defer sched.Close()
	sched.Start(ctx)
	log.Printf("agentd: scheduler started with %d job(s)", len(jobDefs))

	// API server (created before channels so the web channel can mount its UI +
	// WS routes on this ONE server, behind the SAME bearer gate, on no 2nd port).
	var transportUp atomic.Bool
	srv := api.New(cfg.Server.APIBearer, mgr, bus, func() bool { return transportUp.Load() })
	srv.AttachScheduler(sched)

	cwd := ""
	if len(cfg.Harness) > 0 {
		cwd = cfg.Harness[0].Cwd
	}

	// Channels. Each configured channel is an in-process ChannelAdapter; the ones
	// that can also deliver notifications register as notify-hub sinks.
	var tg *telegram.Adapter
	for _, c := range cfg.Channel {
		if !c.Enabled {
			log.Printf("agentd: channel %q disabled (enabled=false); skipped", c.Kind)
			continue
		}
		switch c.Kind {
		case "telegram":
			token := config.ResolveToken(c.Token)
			if token == "" {
				log.Printf("agentd: telegram channel configured but token empty (%s); channel NOT started", c.Token)
				continue
			}
			tg = telegram.New(token, c.Allow)
			if err := tg.Start(ctx); err != nil {
				log.Printf("agentd: telegram start failed: %v", err)
				continue
			}
			transportUp.Store(true)
			hub.Register(tg) // Telegram is a notify sink too (uniform hub)
			log.Printf("agentd: telegram channel started (allow=%v)", c.Allow)

			sessionForChat := map[string]string{}
			go func(ch *telegram.Adapter) {
				for in := range ch.Inbound() {
					in := in
					go func() {
						if err := mgr.RouteInbound(ctx, ch, in, sessionForChat, cwd, model); err != nil {
							log.Printf("agentd: route inbound error: %v", err)
						}
					}()
				}
			}(tg)

		case "web":
			webCh := web.New(bus, c.Title)
			if err := webCh.Start(ctx); err != nil {
				log.Printf("agentd: web start failed: %v", err)
				continue
			}
			// Serve the UI + WS + confirm on the shared server (bearer-gated).
			srv.Mount("/ui", webCh.UIHandler())
			srv.Mount("/ws", webCh.WSHandler())
			srv.Mount("/confirm/", webCh.ConfirmHandler())
			srv.Mount("/", webCh.RootHandler())
			hub.Register(webCh) // web is a notify sink: notifications push over its WS
			transportUp.Store(true)
			log.Printf("agentd: web channel started (UI at /ui, title=%q)", c.Title)

			// Inbound: a message typed in the UI routes through the SAME
			// RouteInbound path as any channel (one warm web session).
			sessionForWeb := map[string]string{}
			go func(ch *web.Adapter) {
				for in := range ch.Inbound() {
					in := in
					go func() {
						if err := mgr.RouteInbound(ctx, ch, in, sessionForWeb, cwd, model); err != nil {
							log.Printf("agentd: web route inbound error: %v", err)
						}
					}()
				}
			}(webCh)

		default:
			log.Printf("agentd: unknown channel kind %q; skipped", c.Kind)
		}
	}
	log.Printf("agentd: notification hub has %d sink(s)", hub.Count())

	httpSrv := &http.Server{Addr: cfg.Server.Bind, Handler: srv.Handler()}
	go func() {
		log.Printf("agentd: API listening on http://%s (bearer %s)", cfg.Server.Bind, redact(cfg.Server.APIBearer))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("agentd: shutting down")
	cancel()
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func redact(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "****"
}
