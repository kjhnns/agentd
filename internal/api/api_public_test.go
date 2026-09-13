package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
)

// A public route bypasses the API bearer on the main handler; the public
// handler serves nothing else.
func TestMountPublicBypassesBearerAndPublicHandlerIsRestricted(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(&recordAdapter{}, bus, nil)
	s := New("tok", mgr, bus, func() bool { return true })
	s.MountPublic("/watch/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	get := func(h http.Handler, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if c := get(s.Handler(), "/watch/ping"); c != http.StatusTeapot {
		t.Fatalf("main handler, public route without bearer = %d, want 418", c)
	}
	if c := get(s.Handler(), "/sessions"); c != http.StatusUnauthorized {
		t.Fatalf("main handler, API route without bearer = %d, want 401", c)
	}
	if c := get(s.PublicHandler(), "/watch/ping"); c != http.StatusTeapot {
		t.Fatalf("public handler, public route = %d, want 418", c)
	}
	for _, p := range []string{"/sessions", "/health", "/ui", "/", "/jobs"} {
		if c := get(s.PublicHandler(), p); c != http.StatusNotFound {
			t.Fatalf("public handler must 404 %s, got %d", p, c)
		}
	}
}
