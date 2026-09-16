package watch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageAuthAndMethods(t *testing.T) {
	a := New("watch-secret", nil)
	for _, tc := range []struct {
		method, token string
		status        int
	}{{"GET", "", 401}, {"GET", "wrong", 401}, {"POST", "watch-secret", 405}} {
		req := httptest.NewRequest(tc.method, "/watch/usage", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: got %d", tc.method, w.Code)
		}
	}
}
func TestUsageCacheAndFailure(t *testing.T) {
	calls := 0
	fail := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("anthropic-beta") == "" {
			t.Error("missing provider auth")
		}
		if fail {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"seven_day":{"utilization":99,"resets_at":"2026-10-01T10:00:00.123456+00:00"},"five_hour":{"utilization":2,"resets_at":null}}`))
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"provider-secret"}}`), 0600)
	u := newUsageReader()
	u.endpoint = upstream.URL
	u.credentials = path
	a := New("watch-secret", nil)
	a.usage = u
	req := httptest.NewRequest("GET", "/watch/usage", nil)
	req.Header.Set("Authorization", "Bearer watch-secret")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"utilization":99`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
	s, err := u.read(context.Background())
	if err != nil || calls != 1 || s.Stale {
		t.Fatalf("cache: %v %d", err, calls)
	}
	original := s.UpdatedAt
	fail = true
	u.attempted = time.Now().Add(-6 * time.Minute)
	s, err = u.read(context.Background())
	if err != nil || !s.Stale || !s.UpdatedAt.Equal(original) {
		t.Fatal("must preserve measurement age on failure")
	}
	u.cached = nil
	u.attempted = time.Time{}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, req)
	if w.Code != 503 || strings.Contains(w.Body.String(), "provider-secret") {
		t.Fatal("cold failure must be sanitized 503")
	}
}
func TestUsageMissingMeasurement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"seven_day":{"utilization":null}}`)) }))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"test"}}`), 0600)
	u := newUsageReader()
	u.endpoint = upstream.URL
	u.credentials = path
	if _, err := u.read(context.Background()); err == nil {
		t.Fatal("null utilization cannot become zero usage")
	}
}
