package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/fuel"
	"github.com/kjhnns/agentd/internal/media"
)

// buildFuel turns the [fuel] section into a Service (docs/specs/2026-10-fuel-api.md).
// A missing or invalid section returns nil and a reason: the routes then stay
// unmounted and the rest of the daemon runs on.
func buildFuel(cfg *config.Config) (*fuel.Service, error) {
	fc := cfg.Fuel
	if !fc.Present {
		return nil, fmt.Errorf("no [fuel] section")
	}
	if fc.Err != "" {
		return nil, fmt.Errorf("invalid [fuel] section: %s", fc.Err)
	}
	if fc.ModelProvider != "openai" {
		return nil, fmt.Errorf("model_provider %q is not supported (openai)", fc.ModelProvider)
	}
	modelKey := config.ResolveToken(fc.ModelKey)
	if modelKey == "" {
		return nil, fmt.Errorf("model_key resolves to empty")
	}
	varsKey := config.ResolveToken(fc.VariablesKey)
	if varsKey == "" {
		return nil, fmt.Errorf("variables_key resolves to empty")
	}
	if fc.Model == "" {
		return nil, fmt.Errorf("model is empty")
	}
	stateDir := fc.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(cfg.Server.StateDir, "fuel")
	}
	// Voice: the [media] whisper key when set, else the model key (both are
	// OpenAI keys). The fast path uses its own 15 s ASR deadline.
	whisperKey := config.ResolveToken(cfg.Media.WhisperKey)
	if whisperKey == "" {
		whisperKey = modelKey
	}
	var asr media.Transcriber = media.NewWhisper(whisperKey, cfg.Media.WhisperModel, cfg.Media.WhisperLang)
	return fuel.New(fuel.Options{
		Token:       config.ResolveToken(fc.Token),
		Vars:        &fuel.VariablesHTTP{Base: fc.VariablesURL, Key: varsKey, Client: &http.Client{Timeout: 30 * time.Second}},
		FoodVar:     fc.FoodLogVar,
		BodyVar:     fc.BodyVar,
		Model:       &fuel.OpenAI{Key: modelKey, Model: fc.Model, Effort: fc.ModelEffort, Client: &http.Client{Timeout: 30 * time.Second}},
		ASR:         asr,
		TargetsFile: fc.TargetsFile,
		StaplesFile: fc.StaplesFile,
		StateDir:    stateDir,
		StravaDir:   fc.StravaDir,
		TestMode:    fc.TestMode,
	})
}

// startFuel resolves the variables (a definitive misconfiguration unmounts
// the routes; a network error retries every 30 s while the routes answer
// 503) and then starts the service. It returns whether to mount.
func startFuel(ctx context.Context, svc *fuel.Service) bool {
	err := svc.Resolve(ctx)
	if err != nil && fuel.IsConfigError(err) {
		log.Printf("agentd: fuel NOT mounted: %v", err)
		return false
	}
	go func() {
		for err != nil {
			log.Printf("agentd: fuel: resolving variables failed (%v); retry in 30s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			err = svc.Resolve(ctx)
			if err != nil && fuel.IsConfigError(err) {
				log.Printf("agentd: fuel stays unavailable: %v", err)
				return
			}
		}
		svc.Start(ctx)
		log.Printf("agentd: fuel ready")
	}()
	return true
}
