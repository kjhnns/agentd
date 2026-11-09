package main

import (
	"context"
	"github.com/kjhnns/agentd/internal/fuel"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandlerServesOnlyFuelAndRecoversPanics(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fuel/boom" {
			panic("boom")
		}
		w.WriteHeader(204)
	}))
	for path, want := range map[string]int{"/fuel/snapshot": 204, "/sessions": 404, "/": 404, "/watch/ping": 404, "/fuel/boom": 500, "/fuel": 404, "/fuelx": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Fatalf("%s: %d want %d", path, rec.Code, want)
		}
		if want >= 400 && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s: body %s", path, rec.Body)
		}
	}
	// The server keeps serving after a panic.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/fuel/x", nil))
	if rec.Code != 204 {
		t.Fatal("not serving after a panic")
	}
}

func TestBuildLocalWithoutVariablesCredential(t *testing.T) {
	cfg := fuel.DefaultDaemonConfig()
	cfg.StoreMode = "local"
	cfg.StateDir = t.TempDir()
	cfg.TargetsFile = filepath.Join(cfg.StateDir, "targets.json")
	cfg.StaplesFile = filepath.Join(cfg.StateDir, "staples.json")
	cfg.StravaDir = filepath.Join(cfg.StateDir, "strava")
	cfg.Token = "local-test-token-0123456789"
	cfg.ModelKey = "test-model-key"
	cfg.VariablesKey = ""
	svc, err := build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
}
