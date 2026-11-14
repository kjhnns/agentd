package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobsCommandsWithEnvBearer(t *testing.T) {
	t.Setenv("AGENTD_ADDR", "")
	t.Setenv("AGENTD_TOKEN", "")
	t.Setenv("J3_TEST_API_BEARER", "resolved-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer resolved-token" {
			t.Errorf("authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /jobs":
			fmt.Fprint(w, `[{"name":"daily","enabled":true,"trigger":"@daily"}]`)
		case "POST /jobs/daily/run":
			fmt.Fprintln(w, `{"status":"started"}`)
		case "GET /jobs/daily/runs":
			fmt.Fprint(w, `[{"run_id":"run-1","trigger":"manual","start":"2026-01-01T00:00:00Z","end":"2026-01-01T00:00:01Z","status":"ok","result":"PONG"}]`)
		case "GET /jobs/empty/runs":
			fmt.Fprint(w, `[]`)
		default:
			http.Error(w, "missing job", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("[server]\nbind = %q\napi_bearer = \"env:J3_TEST_API_BEARER\"\n", addrOf(srv))), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_CONFIG", cfg)
	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"list"}, "daily", ExitOK},
		{[]string{"run", "daily"}, "{\"status\":\"started\"}\n", ExitOK},
		{[]string{"runs", "daily"}, "PONG", ExitOK},
		{[]string{"runs", "empty"}, "no runs recorded\n", ExitOK},
		{[]string{"run", "missing"}, "missing job", ExitFailure},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Jobs(tc.args, &out, &errb); code != tc.code {
				t.Fatalf("exit = %d, want %d: %s", code, tc.code, errb.String())
			}
			got := out.String()
			if tc.code != ExitOK {
				got = errb.String()
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJobsInvocationExitCodes(t *testing.T) {
	t.Setenv("AGENTD_ADDR", "127.0.0.1:8788")
	t.Setenv("AGENTD_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, ExitFailure},
		{[]string{"run"}, ExitFailure},
		{[]string{"runs"}, ExitFailure},
		{[]string{"unknown"}, ExitFailure},
		{[]string{"list", "-unknown"}, ExitUsage},
		{[]string{"list", "-help"}, ExitOK},
		{[]string{"list", "-config", "missing.toml"}, ExitFailure},
	} {
		var out, errb bytes.Buffer
		if code := Jobs(tc.args, &out, &errb); code != tc.code {
			t.Errorf("Jobs(%v) = %d, want %d: %s", tc.args, code, tc.code, errb.String())
		}
	}
}
