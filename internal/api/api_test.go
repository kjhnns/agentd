package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
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

// TestSessionHistoryEndpoint: GET /sessions/:id/history replays the recorded
// conversation from the run-log (bearer-gated like everything else), applying
// the single-visible-reply rule to stored records: a result whose text
// duplicates the turn's final output is returned as a text-less turn marker.
func TestSessionHistoryEndpoint(t *testing.T) {
	rl, err := runlog.Open(filepath.Join(t.TempDir(), "runlog.jsonl"))
	if err != nil {
		t.Fatalf("runlog.Open: %v", err)
	}
	defer rl.Close()
	_ = rl.Append("input", map[string]string{"session": "s1", "text": "say pong"})
	_ = rl.Append("event", eventbus.Event{SessionID: "s1", Kind: eventbus.KindOutput, Text: "PONG"})
	_ = rl.Append("event", eventbus.Event{SessionID: "s1", Kind: eventbus.KindResult, Text: "PONG", Status: "idle"})

	bus := eventbus.New()
	mgr := session.NewManager(nil, bus, rl)
	s := New("tok", mgr, bus, func() bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/history", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions/s1/history = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var hist []session.HistoryEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(hist), hist)
	}
	visible := 0
	for _, en := range hist {
		if en.Text == "PONG" {
			visible++
		}
	}
	if visible != 1 {
		t.Fatalf("reply text appears %d times, want exactly 1: %+v", visible, hist)
	}

	// No bearer -> 401 (same gate as the rest of the API).
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/sessions/s1/history", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated history = %d, want 401", rec2.Code)
	}
}
