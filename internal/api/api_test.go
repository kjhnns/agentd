package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
)

func TestHealthReportsBothSignals(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(nil, bus, nil) // no harness needed for /health
	transportUp := true
	s := New("tok", mgr, bus, func() bool { return transportUp })

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["transport_ok"]; !ok {
		t.Error("health missing transport_ok signal")
	}
	if _, ok := body["sessions"]; !ok {
		t.Error("health missing sessions (per-session agent_ok) signal")
	}
	if body["transport_ok"] != true {
		t.Errorf("transport_ok = %v, want true", body["transport_ok"])
	}
}

func TestBearerGate(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(nil, bus, nil)
	s := New("secret", mgr, bus, func() bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/health", nil) // no auth header
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong bearer, got %d", rec2.Code)
	}
}
