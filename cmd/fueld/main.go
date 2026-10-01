// Command fueld is the Fuel service: the fast food-logging path of the Fuel
// iOS app (docs/specs/2026-10-fuel-api.md, section 15). It serves ONLY
// /fuel/* behind its own token, on its own config and state dir, and shares
// no process with agentd.
//
//	fueld [-config ~/.config/fueld/config.toml]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/internal/fuel"
	"github.com/kjhnns/agentd/internal/media"
)

func main() {
	cfgPath := flag.String("config", fuel.DefaultConfigPath(), "path to the fueld config (mode 0600)")
	flag.Parse()
	cfg, err := fuel.LoadDaemonConfig(*cfgPath)
	if err != nil {
		log.Fatalf("fueld: config: %v", err)
	}
	svc, err := build(cfg)
	if err != nil {
		log.Fatalf("fueld: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Resolve the variables: a definitive misconfiguration stops fueld; a
	// network error retries every 30 s while the routes answer 503.
	if err := svc.Resolve(ctx); err != nil {
		if fuel.IsConfigError(err) {
			log.Fatalf("fueld: %v", err)
		}
		go func() {
			for err != nil {
				log.Printf("fueld: resolving variables failed (%v); retry in 30s", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				err = svc.Resolve(ctx)
				if err != nil && fuel.IsConfigError(err) {
					log.Printf("fueld: %v; routes stay unavailable", err)
					return
				}
			}
			svc.Start(ctx)
			log.Printf("fueld: ready")
		}()
	} else {
		go func() { svc.Start(ctx); log.Printf("fueld: ready") }()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           Handler(svc.Handler()),
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("fueld: listening on http://%s (test_mode=%v, food_log_var=%q)", cfg.Listen, cfg.TestMode, cfg.FoodLogVar)
		errc <- srv.ListenAndServe()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("fueld: listener: %v", err)
			svc.Close()
			os.Exit(1)
		}
	}
	log.Printf("fueld: shutting down")
	// In-flight requests finish: upload deadline 30 s + slot wait 10 s +
	// budget 30 s. Detached Variables writes are then drained by Close, so
	// their outcome is journaled before the stores close.
	sctx, scancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("fueld: shutdown: %v (closing remaining connections)", err)
		_ = srv.Close()
	}
	cancel()
	svc.Close()
}

func build(cfg fuel.DaemonConfig) (*fuel.Service, error) {
	modelKey := fuel.ResolveSecret(cfg.ModelKey)
	varsKey := fuel.ResolveSecret(cfg.VariablesKey)
	switch {
	case modelKey == "":
		return nil, errors.New("model_key resolves to empty")
	case varsKey == "":
		return nil, errors.New("variables_key resolves to empty")
	}
	return fuel.New(fuel.Options{
		Token:       fuel.ResolveSecret(cfg.Token),
		Vars:        &fuel.VariablesHTTP{Base: cfg.VariablesURL, Key: varsKey, Client: &http.Client{Timeout: 30 * time.Second}},
		FoodVar:     cfg.FoodLogVar,
		BodyVar:     cfg.BodyVar,
		Model:       &fuel.OpenAI{Key: modelKey, Model: cfg.Model, Effort: cfg.ModelEffort, Client: &http.Client{Timeout: 30 * time.Second}},
		ASR:         media.NewWhisper(modelKey, cfg.WhisperModel, ""),
		TargetsFile: cfg.TargetsFile,
		StaplesFile: cfg.StaplesFile,
		StateDir:    cfg.StateDir,
		StravaDir:   cfg.StravaDir,
		TestMode:    cfg.TestMode,
	})
}

// Handler serves only /fuel/* (everything else 404) and recovers a panic
// per request (logged without the request body; 500 to the client).
func Handler(fuelH http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/fuel/", fuelH)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/fuel/") {
			writeError(w, http.StatusNotFound, "not_found", "no such route")
			return
		}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("fueld: panic in %s %s: %v", r.Method, r.URL.Path, v)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, code int, c, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": c, "message": msg, "retryable": code >= 500}})
}
