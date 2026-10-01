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

func TestUsageAllProviders(t *testing.T) {
	claudeFail := false
	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claudeFail {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"seven_day":{"utilization":40,"resets_at":null}}`))
	}))
	defer claude.Close()
	codexBody := `{"rate_limit":{"primary_window":{"used_percent":36,"limit_window_seconds":604800,"reset_at":1791055808},"secondary_window":null}}`
	codex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer codex-secret" || r.Header.Get("ChatGPT-Account-Id") != "acct" {
			t.Error("missing codex auth")
		}
		w.Write([]byte(codexBody))
	}))
	defer codex.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{"claudeAiOauth":{"accessToken":"provider-secret"}}`), 0600)
	os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"codex-secret","account_id":"acct"}}`), 0600)
	a := New("watch-secret", nil)
	a.usage.endpoint, a.usage.credentials = claude.URL, filepath.Join(dir, "c.json")
	a.codexUsage.endpoint, a.codexUsage.credentials = codex.URL, filepath.Join(dir, "auth.json")
	get := func() (int, string) {
		req := httptest.NewRequest("GET", "/watch/usage?provider=all", nil)
		req.Header.Set("Authorization", "Bearer watch-secret")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	code, body := get()
	if code != 200 || !strings.Contains(body, `"claude":{`) || !strings.Contains(body, `"utilization":36`) ||
		!strings.Contains(body, `"resets_at":"2026-10-03T`) || strings.Contains(body, "secret") {
		t.Fatalf("unexpected response: %d %s", code, body)
	}
	// One provider failing cold must not hide the other.
	claudeFail = true
	a.usage.cached, a.usage.attempted = nil, time.Time{}
	code, body = get()
	if code != 200 || !strings.Contains(body, `"claude":null`) || !strings.Contains(body, `"utilization":36`) {
		t.Fatalf("partial response: %d %s", code, body)
	}
	// The week is found by window length: a five-hour primary is the session.
	codexBody = `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_at":0},"secondary_window":{"used_percent":70,"limit_window_seconds":604800,"reset_at":1791055808}}}`
	a.codexUsage.cached, a.codexUsage.attempted = nil, time.Time{}
	s, err := a.codexUsage.read(context.Background())
	if err != nil || *s.Weekly.Utilization != 70 || *s.Session.Utilization != 5 || s.Session.ResetsAt != nil {
		t.Fatalf("window classification: %v %+v", err, s)
	}
	// No weekly window is unavailable, never zero usage.
	codexBody = `{"rate_limit":{"primary_window":null}}`
	a.codexUsage.cached, a.codexUsage.attempted = nil, time.Time{}
	if _, err := a.codexUsage.read(context.Background()); err == nil {
		t.Fatal("missing weekly window must be an error")
	}
}
