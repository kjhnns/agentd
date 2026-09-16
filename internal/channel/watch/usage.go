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
func (a *Adapter) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, 405, "method not allowed")
		return
	}
	s, err := a.usage.read(r.Context())
	if err != nil {
		writeErr(w, 503, "Claude usage unavailable; check Claude login on the server.")
		return
	}
	writeJSON(w, 200, s)
}
