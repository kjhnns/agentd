package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/session"
)

// recordAdapter is a minimal harness fake that records every Send text so the
// test can assert the media turn was routed with the rendered marker.
type recordHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *recordHandle) ID() string { return h.id }

type recordAdapter struct {
	mu    sync.Mutex
	sends []string
}

func (a *recordAdapter) Name() string                       { return "record" }
func (a *recordAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (a *recordAdapter) Interrupt(harness.Handle) error     { return nil }
func (a *recordAdapter) Status(harness.Handle) harness.Status {
	return harness.StatusIdle
}
func (a *recordAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (a *recordAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &recordHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 8)}, nil
}
func (a *recordAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &recordHandle{id: existing, events: make(chan eventbus.Event, 8)}, nil
}
func (a *recordAdapter) Teardown(h harness.Handle) error {
	close(h.(*recordHandle).events)
	return nil
}
func (a *recordAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*recordHandle).events
}
func (a *recordAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	a.mu.Lock()
	a.sends = append(a.sends, in.Text)
	a.mu.Unlock()
	return nil
}
func (a *recordAdapter) sentTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.sends))
	copy(out, a.sends)
	return out
}

var _ harness.Adapter = (*recordAdapter)(nil)

func multipartBody(t *testing.T, field, filename, mime, content, text string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="` + filename + `"`}
	hdr["Content-Type"] = []string{mime}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if text != "" {
		_ = mw.WriteField("text", text)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestMediaUploadEndpoint(t *testing.T) {
	bus := eventbus.New()
	ra := &recordAdapter{}
	mgr := session.NewManager(ra, bus, nil)
	mgr.Policy = session.Policy{}
	svc := &media.Service{Dir: t.TempDir()}

	s := New("tok", mgr, bus, func() bool { return true })
	s.AttachMedia(svc)

	sess, err := mgr.Create(context.Background(), "", t.TempDir(), "", "media-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 401 without auth.
	body, ctype := multipartBody(t, "file", "pic.png", "image/png", "PNG", "")
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", rec.Code)
	}

	// 200 with auth; artifact lands on disk; turn routed with the marker.
	body, ctype = multipartBody(t, "file", "pic.png", "image/png", "PNGDATA", "what do you see?")
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status   string         `json:"status"`
		Artifact media.Artifact `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "routed" || resp.Artifact.Kind != media.KindImage {
		t.Errorf("resp = %+v", resp)
	}
	data, err := os.ReadFile(resp.Artifact.Path)
	if err != nil || string(data) != "PNGDATA" {
		t.Errorf("artifact not on disk: %q %v", data, err)
	}

	sends := ra.sentTexts()
	if len(sends) != 1 {
		t.Fatalf("turns routed = %d, want 1 (%v)", len(sends), sends)
	}
	if !strings.HasPrefix(sends[0], "what do you see?\n\n") ||
		!strings.Contains(sends[0], "[The user sent an image saved at "+resp.Artifact.Path+".") {
		t.Errorf("routed turn text = %q", sends[0])
	}
}

func TestMediaUploadUnknownSessionAnd503(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(&recordAdapter{}, bus, nil)
	s := New("tok", mgr, bus, func() bool { return true })

	// Media not attached -> 503.
	body, ctype := multipartBody(t, "file", "a.png", "image/png", "X", "")
	req := httptest.NewRequest(http.MethodPost, "/sessions/nope/media", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-service status = %d, want 503", rec.Code)
	}

	// Attached but unknown session -> 404.
	s.AttachMedia(&media.Service{Dir: t.TempDir()})
	body, ctype = multipartBody(t, "file", "a.png", "image/png", "X", "")
	req = httptest.NewRequest(http.MethodPost, "/sessions/nope/media", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-session status = %d, want 404", rec.Code)
	}
}

func TestMediaUploadTooLarge(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(&recordAdapter{}, bus, nil)
	s := New("tok", mgr, bus, func() bool { return true })
	s.AttachMedia(&media.Service{Dir: t.TempDir(), MaxBytes: 4})

	sess, err := mgr.Create(context.Background(), "", t.TempDir(), "", "big")
	if err != nil {
		t.Fatal(err)
	}
	body, ctype := multipartBody(t, "file", "big.png", "image/png", "MORETHANFOUR", "")
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/media", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
}
