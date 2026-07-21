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
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/internal/api"
	"github.com/kjhnns/agentd/internal/channel/telegram"
	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/harness/claudecode"
	"github.com/kjhnns/agentd/internal/runlog"
	"github.com/kjhnns/agentd/internal/session"
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
	case "-h", "--help", "help":
		fmt.Println("usage: agentd [serve|smoke] [flags]")
	default:
		serve(os.Args[1:])
	}
}

// smoke proves the Claude Code stream-json vertical without the full server: it
// runs ONE headless turn and prints the normalized events as JSON.
func smoke(args []string) {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	prompt := fs.String("prompt", "Reply with exactly the word PONG and nothing else", "prompt to send")
	bin := fs.String("claude", "claude", "claude binary")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	a := claudecode.New(*bin)
	h, err := a.Start(ctx, harness.SessionConfig{SessionID: "smoke", Cwd: os.TempDir()})
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	evs, err := a.SendPrompt(ctx, h, *prompt)
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(evs)
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
	adapter := claudecode.New("claude")
	mgr := session.NewManager(adapter, bus, rl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Channel: telegram, if configured and a token is present.
	var transportUp atomic.Bool
	var tg *telegram.Adapter
	for _, c := range cfg.Channel {
		if c.Kind != "telegram" {
			continue
		}
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
		log.Printf("agentd: telegram channel started (allow=%v)", c.Allow)

		sessionForChat := map[string]string{}
		cwd := ""
		if len(cfg.Harness) > 0 {
			cwd = cfg.Harness[0].Cwd
		}
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
	}

	// API server.
	srv := api.New(cfg.Server.APIBearer, mgr, bus, func() bool { return transportUp.Load() })
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
