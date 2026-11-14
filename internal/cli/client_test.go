package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientReset(t *testing.T) {
	c := NewClient("daemon:8788", "tok", 0)
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/sessions/a%2Fb/reset" || string(body) != `{"reason":"explicit (cli)"}` {
			t.Errorf("reset request: %s %s %s", r.Method, r.URL.EscapedPath(), body)
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("reset headers: %v", r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err := c.Reset(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}

func TestClientUnreachableHint(t *testing.T) {
	c := NewClient("daemon:8788", "", 0)
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	err := c.Health(context.Background())
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "is the agentd service running?") {
		t.Fatalf("unreachable error = %v", err)
	}
}

func TestEndpointEnvBearer(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nbind = \"127.0.0.1:8788\"\napi_bearer = \"env:J3_TEST_API_BEARER\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_CONFIG", cfg)
	t.Setenv("AGENTD_ADDR", "")
	t.Setenv("AGENTD_TOKEN", "")
	t.Setenv("J3_TEST_API_BEARER", "resolved-token")
	addr, token, err := Endpoint("")
	if err != nil || addr != "127.0.0.1:8788" || token != "resolved-token" {
		t.Fatalf("endpoint = %q, %q, %v", addr, token, err)
	}
}

func TestConfigPathDiscovery(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	t.Setenv("AGENTD_CONFIG", "")
	t.Setenv("HOME", dir)
	if got := ConfigPath(""); got != "" {
		t.Fatalf("no config: %q", got)
	}
	if err := os.WriteFile("config.toml", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ConfigPath(""); got != "config.toml" {
		t.Fatalf("local config: %q", got)
	}
	homeConfig := filepath.Join(dir, ".agentd", "config.toml")
	if err := os.MkdirAll(filepath.Dir(homeConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ConfigPath(""); got != homeConfig {
		t.Fatalf("home config: %q", got)
	}
	t.Setenv("AGENTD_CONFIG", "env.toml")
	if got := ConfigPath(""); got != "env.toml" {
		t.Fatalf("env config: %q", got)
	}
	if got := ConfigPath("explicit.toml"); got != "explicit.toml" {
		t.Fatalf("explicit config: %q", got)
	}
}
