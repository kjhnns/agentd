// Command agentd is the single-binary agent server (design section 3). This
// scaffold wires the Phase 0-1 core: config, event bus, durable run-log, the
// Claude Code harness adapter (stream-json), the in-process Telegram channel,
// the Session Manager, and the local HTTP+WS API.
//
// Subcommands:
//
//	agentd serve   -config config.toml     run the daemon (default)
//	agentd chat                            interactive REPL against the RUNNING daemon
//	agentd run     "<prompt>"              one-shot turn for scripts (stdout + exit code)
//	agentd sessions                        list the daemon's live sessions
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/internal/api"
	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/channel/telegram"
	"github.com/kjhnns/agentd/internal/channel/watch"
	"github.com/kjhnns/agentd/internal/channel/web"
	"github.com/kjhnns/agentd/internal/cli"
	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/harness/claudecode"
	"github.com/kjhnns/agentd/internal/harness/codex"
	"github.com/kjhnns/agentd/internal/media"
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
	case "chat":
		os.Exit(cli.Chat(os.Args[2:], os.Stdout, os.Stderr, os.Stdin))
	case "run":
		os.Exit(cli.Run(os.Args[2:], os.Stdout, os.Stderr, os.Stdin))
	case "sessions":
		os.Exit(cli.Sessions(os.Args[2:], os.Stdout, os.Stderr))
	case "init-workspace":
		initWorkspace(os.Args[2:])
	case "memory":
		memoryCmd(os.Args[2:])
	case "jobs":
		os.Exit(cli.Jobs(os.Args[2:], os.Stdout, os.Stderr))
	case "-h", "--help", "help":
		fmt.Println("usage: agentd [serve|chat|run|sessions|init-workspace|memory|jobs] [flags]")
		fmt.Println("  chat                               interactive REPL against the RUNNING daemon")
		fmt.Println("  run \"<prompt>\"                     one-shot turn for scripts (answer on stdout)")
		fmt.Println("  sessions                           list the daemon's live sessions")
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

// resolveWorkspaceTarget fills an empty -root / -workspace from a config.toml
// ([workspace] root/default) so `agentd memory ...` works bare on a configured
// machine. Config path precedence: explicit cfgPath flag, $AGENTD_CONFIG, then
// ~/.agentd/config.toml, then ./config.toml. Explicit flags always win; if nothing
// resolves, the empty values fall through to workspace.NewStore's defaults
// (~/.agentd/workspaces, "default").
func resolveWorkspaceTarget(root, name, cfgPath string) (string, string) {
	if root != "" && name != "" {
		return root, name
	}
	path := cli.ConfigPath(cfgPath)
	if path == "" {
		return root, name
	}
	cfg, err := config.Load(path)
	if err != nil {
		return root, name
	}
	if root == "" {
		root = cfg.Workspace.Root
	}
	if name == "" {
		name = cfg.Workspace.Default
	}
	return root, name
}

// memoryCmd maintains a workspace's memory wiki: rebuild the INDEX from page
// frontmatter, check [[wikilinks]], add/get pages.
func memoryCmd(args []string) {
	if len(args) < 1 {
		log.Fatal("usage: agentd memory <index|links|add|get> [flags]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("memory "+sub, flag.ExitOnError)
	root := fs.String("root", "", "directory holding workspaces (default: [workspace] root from config, else ~/.agentd/workspaces)")
	wsName := fs.String("workspace", "", "workspace name (default: [workspace] default from config, else \"default\")")
	cfgPath := fs.String("config", "", "config.toml to resolve workspace root/default from (default: $AGENTD_CONFIG, else ~/.agentd/config.toml)")
	slug := fs.String("slug", "", "page slug (add/get)")
	title := fs.String("title", "", "page title (add)")
	hook := fs.String("hook", "", "one-line hook shown in the INDEX (add)")
	tags := fs.String("tags", "", "comma-separated tags (add)")
	body := fs.String("body", "", "page body; reads stdin if empty (add)")
	_ = fs.Parse(args[1:])

	r, n := resolveWorkspaceTarget(*root, *wsName, *cfgPath)
	ws, err := workspace.Load(workspace.NewStore(r, n).Path(n))
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

	// Select the configured backend; the session/channel machinery is shared.
	var model string
	if len(cfg.Harness) > 0 {
		model = cfg.Harness[0].Model
	}
	adapter, err := configuredHarness(cfg)
	if err != nil {
		log.Fatal(err)
	}
	mgr := session.NewManager(adapter, bus, rl)
	mgr.DefaultModel = model
	if len(cfg.Harness) > 0 {
		mgr.DefaultCwd = cfg.Harness[0].Cwd
	}
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
		TurnTimeout:          cfg.Session.TurnTimeout,
		TurnCeiling:          cfg.Session.TurnCeiling,
	}
	// Workspaces: every session is homed in a workspace (cwd = workspace root,
	// composed injection via --append-system-prompt). See internal/workspace.
	mgr.Reply = session.ReplyPolicy{SummaryBudget: cfg.Session.SummaryBudget}
	mgr.Workspaces = workspace.NewStore(cfg.Workspace.Root, cfg.Workspace.Default)
	mgr.GitAutoCommit = cfg.Workspace.GitAutocommit
	log.Printf("agentd: harness=%s skip_permissions=%v workspaces=%s default=%q git_autocommit=%v",
		adapter.Name(), mgr.SkipPermissions, mgr.Workspaces.Root, mgr.Workspaces.Default, mgr.GitAutoCommit)
	// turn_quiet is named for what it now MEASURES: a turn is abandoned after
	// that long with no progress event, not after that long of working.
	log.Printf("agentd: session policy context_window=%d reset_pressure=%.2f idle=%s max_turns=%d max_wallclock=%s gc=%s turn_quiet=%s turn_ceiling=%s",
		cfg.Session.ContextWindow, mgr.Policy.ContextResetPressure, mgr.Policy.IdleTimeout,
		mgr.Policy.MaxTurns, mgr.Policy.MaxWallclock, mgr.Policy.GCInterval,
		mgr.TurnTimeout(), mgr.TurnCeiling())
	if mgr.Reply.SummaryBudget > 0 {
		log.Printf("agentd: replies longer than %d chars are delivered as full answer + summary (summary last)", mgr.Reply.SummaryBudget)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Media: the CORE multimodal capability (internal/media). Storage defaults
	// to <default workspace>/work/media (gitignored inside the workspace repo so
	// autocommit never sweeps binaries into history); voice transcription is
	// wired when a whisper key resolves, otherwise audio ingest fails typed and
	// the channel apologizes. Channels only ACQUIRE bytes; rendering into turn
	// text happens once, in session.RenderInbound.
	var mediaSvc *media.Service
	if cfg.Media.Enabled {
		mediaDir := cfg.Media.Dir
		if mediaDir == "" {
			ws, err := mgr.Workspaces.Ensure("")
			if err != nil {
				log.Printf("agentd: media disabled: cannot resolve default workspace: %v", err)
			} else {
				mediaDir = filepath.Join(ws.WorkDir(), "media")
				if err := media.EnsureGitignoreLine(ws.Root, "work/media/"); err != nil {
					log.Printf("agentd: could not gitignore work/media/ in workspace %s: %v", ws.Name, err)
				}
			}
		}
		if mediaDir != "" {
			var tr media.Transcriber
			if key := config.ResolveToken(cfg.Media.WhisperKey); key != "" {
				tr = media.NewWhisper(key, cfg.Media.WhisperModel, cfg.Media.WhisperLang)
			} else {
				log.Printf("agentd: media enabled but whisper_key empty; voice transcription disabled")
			}
			mediaSvc = &media.Service{
				Dir:         mediaDir,
				MaxBytes:    int64(cfg.Media.MaxFileMB) << 20,
				Retention:   cfg.Media.Retention,
				Transcriber: tr,
				Log:         rl,
			}
			mediaSvc.StartSweeper(ctx)
			log.Printf("agentd: media service at %s (cap %d MB, retention %s, whisper=%v)",
				mediaDir, cfg.Media.MaxFileMB, cfg.Media.Retention, tr != nil)
		}
	}

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
	// the run-log.
	jobDefs, err := scheduler.FromConfig(cfg.Job)
	if err != nil {
		log.Fatalf("jobs config: %v", err)
	}
	runner := &scheduler.ManagerRunner{Mgr: mgr, Model: model}
	sched, err := scheduler.New(jobDefs, runner, filepath.Join(cfg.Server.StateDir, "scheduler.jsonl"))
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
	// The API bearer honours the same env: indirection as every channel token,
	// and an EMPTY result refuses to start rather than opening the API.
	apiBearer := config.ResolveToken(cfg.Server.APIBearer)
	if apiBearer == "" {
		log.Fatalf("agentd: [server] api_bearer resolves to empty; refusing to start an unauthenticated API (set a literal or env:VAR)")
	}
	srv := api.New(apiBearer, mgr, bus, func() bool { return transportUp.Load() })
	srv.AttachScheduler(sched)
	srv.AttachMedia(mediaSvc)

	cwd := ""
	if len(cfg.Harness) > 0 {
		cwd = cfg.Harness[0].Cwd
	}

	// Channels. Each configured channel is an in-process ChannelAdapter; the ones
	// that can also deliver notifications register as notify-hub sinks.
	var tg *telegram.Adapter
	var tgAllow []string
	sessionForChat := session.NewChatMap(session.LaneMain) // Telegram chat id -> session (shared with the watch when share_session)
	var watchCfgs []config.Channel                         // wired AFTER the loop: the watch may piggyback on Telegram
	for _, c := range cfg.Channel {
		if !c.Enabled {
			log.Printf("agentd: channel %q disabled (enabled=false); skipped", c.Kind)
			continue
		}
		switch c.Kind {
		case "telegram":
			token := config.ResolveToken(c.Token)
			if token == "" {
				// Log only the REFERENCE, never a literal: an "env:FOO" ref is a
				// var name (safe and the useful debug detail), anything else could
				// be a credential and is described instead of printed.
				ref := "<literal>"
				if strings.HasPrefix(c.Token, "env:") || c.Token == "" {
					ref = c.Token
				}
				log.Printf("agentd: telegram channel configured but token empty (ref %q); channel NOT started", ref)
				continue
			}
			tg = telegram.New(token, c.Allow).WithReactions(c.Reactions)
			tgAllow = c.Allow
			if mediaSvc != nil {
				tg.WithMedia(mediaSvc)
			}
			if err := tg.Start(ctx); err != nil {
				log.Printf("agentd: telegram start failed: %v", err)
				continue
			}
			transportUp.Store(true)
			hub.Register(tg) // Telegram is a notify sink too (uniform hub)
			log.Printf("agentd: telegram channel started (allow=%v, reactions=%v)", c.Allow, c.Reactions)

			go consumeInbound(ctx, tg, mgr, sessionForChat, cwd, model, "agentd: route inbound error: %v")

		case "web":
			webCh := web.New(bus, c.Title)
			if err := webCh.Start(ctx); err != nil {
				log.Printf("agentd: web start failed: %v", err)
				continue
			}
			// Serve the UI + WS on the shared server (bearer-gated).
			srv.Mount("/ui", webCh.UIHandler())
			srv.Mount("/ws", webCh.WSHandler())
			srv.Mount("/", webCh.RootHandler())
			hub.Register(webCh) // web is a notify sink: notifications push over its WS
			transportUp.Store(true)
			log.Printf("agentd: web channel started (UI at /ui, title=%q)", c.Title)

			// Inbound: a message typed in the UI routes through the SAME
			// RouteInbound path as any channel (one warm web session).
			sessionForWeb := session.NewChatMap(session.LaneMain)
			go consumeInbound(ctx, webCh, mgr, sessionForWeb, cwd, model, "agentd: web route inbound error: %v")

		case "watch":
			watchCfgs = append(watchCfgs, c)

		default:
			log.Fatalf("agentd: unknown channel kind %q", c.Kind)
		}
	}

	// Watch (wrist) channel: an async inbox/outbox behind its OWN token under
	// /watch/, mounted as a PUBLIC route (no API bearer) so a public HTTPS
	// proxy can forward to it. With share_session it drives the Telegram
	// chat's session (one brain, two surfaces); with mirror every transcript
	// and reply is echoed into that chat.
	for _, c := range watchCfgs {
		token := config.ResolveToken(c.Token)
		if token == "" {
			log.Printf("agentd: watch channel configured but token empty; channel NOT started")
			continue
		}
		store, err := watch.OpenStore(filepath.Join(cfg.Server.StateDir, "watch-messages.jsonl"))
		if err != nil {
			log.Printf("agentd: watch store: %v; channel NOT started", err)
			continue
		}
		defer store.Close()
		wch := watch.New(token, store).WithTitle(c.Title)
		if mediaSvc != nil {
			wch.WithMedia(mediaSvc)
		}
		sessMap := session.NewChatMap(session.LaneMain)
		shared := false
		if c.ShareSession {
			if tg != nil && len(tgAllow) > 0 {
				wch.WithUserID(tgAllow[0])
				sessMap = sessionForChat
				shared = true
			} else {
				log.Printf("agentd: watch share_session=true but no Telegram channel with an allowlist; using an own session")
			}
		}
		if c.Mirror {
			if tg != nil && len(tgAllow) > 0 {
				chat := tgAllow[0]
				wch.WithMirror(func(text string, silent bool) {
					mctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if _, err := tg.Send(mctx, channel.OutboundMsg{ChatID: chat, Text: text, Silent: silent}); err != nil {
						log.Printf("agentd: watch mirror to telegram failed: %v", err)
					}
				})
			} else {
				log.Printf("agentd: watch mirror=true but no Telegram channel; mirror off")
			}
		}
		// Side conversations (R4): one side session per instance, keyed
		// session.SideChat in its own lane-side map, in the same workspace as
		// main. Artifacts of earlier conversations go to the state dir, never
		// into a git-autocommitted workspace. No config key: it rides the
		// watch channel, shared or own session alike.
		sideMap := session.NewChatMap(session.LaneSide)
		wsRoot := ""
		if mgr.Workspaces != nil {
			wsRoot = mgr.Workspaces.Root
		}
		if err := wch.WithArtifacts(filepath.Join(cfg.Server.StateDir, "watch-artifacts"), wsRoot); err != nil {
			log.Printf("agentd: watch artifacts disabled (%v); continuing an earlier conversation will fail with a notice", err)
		}
		srv.MountPublic("/watch/", wch.Handler())
		hub.Register(wch)
		log.Printf("agentd: watch channel started (routes /watch/*, shared_session=%v, mirror=%v, conversations=on)", shared, c.Mirror)

		// In order: the wrist's messages share one session, and the restart
		// reconciliation re-enqueues queued requests in their order. Main
		// and side turns run one after the other anyway (one workspace).
		go func(ch *watch.Adapter, m *session.ChatMap) {
			session.DispatchInOrder(ch.Inbound(), func(in channel.InboundMsg) {
				if err := mgr.RouteConversation(ctx, ch, in, m, sideMap, cwd, model, ch); err != nil {
					log.Printf("agentd: watch route inbound error: %v", err)
				}
			})
		}(wch, sessMap)

		// Restart reconciliation (D5): every request the last run left
		// unfinished is re-enqueued, answered from its committed turn record,
		// or failed with a notice. Runs before the HTTP surface serves.
		wch.Reconcile(ctx,
			func(msgID, reqID string) (watch.CommittedTurn, bool) {
				tr, _, ok := mgr.FindTurn(msgID, reqID)
				return watch.CommittedTurn{SessionID: tr.Session, Result: tr.Result, Error: tr.Error}, ok
			},
			func(dctx context.Context, in channel.InboundMsg, sid, result string, finalOnly bool) error {
				return mgr.DeliverStored(dctx, wch, in, sid, result, finalOnly)
			})
	}
	log.Printf("agentd: notification hub has %d sink(s)", hub.Count())

	httpSrv := &http.Server{Addr: cfg.Server.Bind, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("agentd: API listening on http://%s (bearer %s)", cfg.Server.Bind, redact(apiBearer))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Optional PUBLIC listener: only MountPublic routes, nothing else. The bind
	// is retried instead of fatal because a tailnet address may come up after
	// the daemon at boot.
	var pubSrv *http.Server
	if pb := cfg.Server.PublicBind; pb != "" {
		pubSrv = &http.Server{Addr: pb, Handler: srv.PublicHandler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
		go func() {
			for {
				ln, err := net.Listen("tcp", pb)
				if err != nil {
					log.Printf("agentd: public bind %s failed: %v (retry in 10s)", pb, err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Second):
						continue
					}
				}
				log.Printf("agentd: public routes listening on http://%s (public routes only)", pb)
				if err := pubSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("agentd: public listener: %v", err)
				}
				return
			}
		}()
	}

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("agentd: shutting down")
	cancel()
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = httpSrv.Shutdown(shutdownCtx)
	if pubSrv != nil {
		_ = pubSrv.Shutdown(shutdownCtx)
	}
}

// consumeInbound routes each message in its own goroutine.
func consumeInbound(ctx context.Context, ch channel.Adapter, mgr *session.Manager, chats *session.ChatMap, cwd, model, errorFormat string) {
	for in := range ch.Inbound() {
		in := in
		go func() {
			if err := mgr.RouteInbound(ctx, ch, in, chats, cwd, model); err != nil {
				log.Printf(errorFormat, err)
			}
		}()
	}
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

// configuredHarness preserves the default Claude backend for existing configs.
func configuredHarness(cfg *config.Config) (harness.Adapter, error) {
	var h config.Harness
	if len(cfg.Harness) > 1 {
		return nil, fmt.Errorf("configure exactly one [[harness]] backend per daemon")
	}
	if len(cfg.Harness) > 0 {
		h = cfg.Harness[0]
	}
	switch h.Kind {
	case "", "claude-code":
		return claudecode.New(h.Bin).WithContextWindow(cfg.Session.ContextWindow).WithPromptDir(h.PromptDir), nil
	case "codex":
		if h.PromptDir != "" {
			// The codex adapter passes developer_instructions in argv.
			return nil, fmt.Errorf("prompt_dir is set but the codex harness cannot keep the prompt out of argv")
		}
		return codex.New(h.Bin), nil
	default:
		return nil, fmt.Errorf("unsupported harness kind %q (use claude-code or codex)", h.Kind)
	}
}
