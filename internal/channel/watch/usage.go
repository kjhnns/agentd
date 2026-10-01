package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Subscription credentials stay on the daemon. Only allowance measurements
// cross the watch API. A bounded cache avoids polling the provider per widget.
type usageWindow struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}
type usageSnapshot struct {
	UpdatedAt time.Time    `json:"updated_at"`
	Weekly    *usageWindow `json:"weekly"`
	Session   *usageWindow `json:"session"`
	Stale     bool         `json:"stale"`
}
type usageReader struct {
	mu          sync.Mutex
	client      *http.Client
	endpoint    string
	credentials string
	codex       bool
	cached      *usageSnapshot
	attempted   time.Time
}

func newUsageReader() *usageReader {
	home, _ := os.UserHomeDir()
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	return &usageReader{client: &http.Client{Timeout: 10 * time.Second}, endpoint: "https://api.anthropic.com/api/oauth/usage", credentials: filepath.Join(dir, ".credentials.json")}
}

// The Codex CLI keeps its ChatGPT login in $CODEX_HOME/auth.json and refreshes
// it itself. The daemon only reads the access token; it never refreshes, so it
// cannot race the CLI for the rotating refresh token. An expired token reads
// as "unavailable" until Codex runs again.
func newCodexUsageReader() *usageReader {
	home, _ := os.UserHomeDir()
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".codex")
	}
	return &usageReader{client: &http.Client{Timeout: 10 * time.Second}, endpoint: "https://chatgpt.com/backend-api/wham/usage", credentials: filepath.Join(dir, "auth.json"), codex: true}
}
func (u *usageReader) read(ctx context.Context) (*usageSnapshot, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Since(u.attempted) < 5*time.Minute {
		if u.cached != nil {
			s := *u.cached
			s.Stale = s.Stale || time.Since(s.UpdatedAt) > 15*time.Minute
			return &s, nil
		}
		return nil, errors.New("usage unavailable")
	}
	u.attempted = time.Now()
	s, err := u.fetch(ctx)
	if err == nil {
		u.cached = s
		return s, nil
	}
	if u.cached != nil {
		u.cached.Stale = true
		s := *u.cached
		return &s, nil
	}
	return nil, err
}
func (u *usageReader) fetch(ctx context.Context) (*usageSnapshot, error) {
	if u.codex {
		return u.fetchCodex(ctx)
	}
	raw, err := os.ReadFile(u.credentials)
	if err != nil {
		return nil, err
	}
	var credentials struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &credentials) != nil || credentials.OAuth.AccessToken == "" {
		return nil, errors.New("Claude login unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.OAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("provider unavailable")
	}
	var body struct {
		Weekly  *usageWindow `json:"seven_day"`
		Session *usageWindow `json:"five_hour"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if body.Weekly == nil || body.Weekly.Utilization == nil {
		return nil, errors.New("weekly allowance unavailable")
	}
	return &usageSnapshot{UpdatedAt: time.Now().UTC(), Weekly: body.Weekly, Session: body.Session}, nil
}

type codexWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt       int64    `json:"reset_at"`
}

func (c *codexWindow) window() *usageWindow {
	w := &usageWindow{Utilization: c.UsedPercent}
	if c.ResetAt > 0 {
		t := time.Unix(c.ResetAt, 0).UTC()
		w.ResetsAt = &t
	}
	return w
}
func (u *usageReader) fetchCodex(ctx context.Context) (*usageSnapshot, error) {
	raw, err := os.ReadFile(u.credentials)
	if err != nil {
		return nil, err
	}
	var credentials struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &credentials) != nil || credentials.Tokens.AccessToken == "" {
		return nil, errors.New("Codex login unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.Tokens.AccessToken)
	if credentials.Tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", credentials.Tokens.AccountID)
	}
	req.Header.Set("User-Agent", "codex-cli")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("provider unavailable")
	}
	var body struct {
		RateLimit struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	// The plan decides which slot holds the week: classify by window length,
	// never by position.
	s := &usageSnapshot{UpdatedAt: time.Now().UTC()}
	for _, c := range []*codexWindow{body.RateLimit.Primary, body.RateLimit.Secondary} {
		if c == nil || c.UsedPercent == nil {
			continue
		}
		if c.WindowSeconds >= 6*24*3600 {
			s.Weekly = c.window()
		} else if s.Session == nil {
			s.Session = c.window()
		}
	}
	if s.Weekly == nil {
		return nil, errors.New("weekly allowance unavailable")
	}
	return s, nil
}

// GET /watch/usage answers the Claude snapshot (the original contract).
// GET /watch/usage?provider=all answers {"claude":…,"codex":…}; a provider
// that cannot be read is null, so one failure does not hide the other.
func (a *Adapter) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, 405, "method not allowed")
		return
	}
	if r.URL.Query().Get("provider") == "all" {
		claude, _ := a.usage.read(r.Context())
		codex, _ := a.codexUsage.read(r.Context())
		if claude == nil && codex == nil {
			writeErr(w, 503, "Usage unavailable; check the Claude and Codex logins on the server.")
			return
		}
		writeJSON(w, 200, map[string]*usageSnapshot{"claude": claude, "codex": codex})
		return
	}
	s, err := a.usage.read(r.Context())
	if err != nil {
		writeErr(w, 503, "Claude usage unavailable; check Claude login on the server.")
		return
	}
	writeJSON(w, 200, s)
}
