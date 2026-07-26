package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/session"
)

// replyHandle/replyAdapter is a harness fake that answers each turn on the bus
// the way the real one does: streamed output plus a result whose text is
// BLANKED because it duplicates that output (the single-visible-reply rule).
type replyHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *replyHandle) ID() string { return h.id }

type replyAdapter struct{}

func (replyAdapter) Name() string                       { return "reply" }
func (replyAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (replyAdapter) Interrupt(harness.Handle) error     { return nil }
func (replyAdapter) Status(harness.Handle) harness.Status {
	return harness.StatusIdle
}
func (replyAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (replyAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &replyHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 16)}, nil
}
func (replyAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &replyHandle{id: existing, events: make(chan eventbus.Event, 16)}, nil
}
func (replyAdapter) Teardown(h harness.Handle) error {
	close(h.(*replyHandle).events)
	return nil
}
func (replyAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*replyHandle).events
}
func (replyAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	rh := h.(*replyHandle)
	rh.events <- eventbus.Event{SessionID: rh.id, Kind: eventbus.KindOutput, Text: "answer to: " + in.Text}
	rh.events <- eventbus.Event{SessionID: rh.id, Kind: eventbus.KindResult, Text: ""}
	return nil
}

var _ harness.Adapter = replyAdapter{}

// TestInputReturnsTheAnswer: POST /sessions/:id/input answers with the turn's
// RESULT, not just an ack. This is what lets a non-streaming client (the
// `agentd run` CLI, a curl in a script) read the reply at all; the call already
// blocked for the whole turn, so returning "sent" and nothing else threw the
// answer away.
func TestInputReturnsTheAnswer(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(replyAdapter{}, bus, nil)
	mgr.Policy = session.Policy{}
	s := New("tok", mgr, bus, func() bool { return true })

	sess, err := mgr.Create(context.Background(), "", t.TempDir(), "", "cli:test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/input", strings.NewReader(`{"text":"ping"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "sent" || body["id"] != sess.ID {
		t.Errorf("response dropped the existing fields: %v", body)
	}
	if body["result"] != "answer to: ping" {
		t.Fatalf("result = %q, want the turn's answer (blanked result falls back to the last output)", body["result"])
	}
}

// TestInputUnknownSessionIs404: a stale session id is a 404, so a client can
// tell "that session is gone" from "the turn failed" (500).
func TestInputUnknownSessionIs404(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(replyAdapter{}, bus, nil)
	s := New("tok", mgr, bus, func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/sessions/nope/input", strings.NewReader(`{"text":"ping"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
