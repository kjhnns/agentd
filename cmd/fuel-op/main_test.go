package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const tok = "agent-token-secret-123"

type fakeFueld struct {
	mu      sync.Mutex
	srv     *httptest.Server
	reqs    []*http.Request
	bodies  []map[string]any
	handler func(n int, r *http.Request, body map[string]any) (int, any)
}

func newFake(t *testing.T) *fakeFueld {
	f := &fakeFueld{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.bodies = append(f.bodies, body)
		n := len(f.reqs)
		h := f.handler
		f.mu.Unlock()
		code, v := h(n, r, body)
		if code == 0 {
			// Drop the connection: a transport error for the client.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setup points fuel-op at the fake and replaces the pass lookup.
func setup(t *testing.T, f *fakeFueld) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "fuel-op.conf")
	if err := os.WriteFile(conf, []byte("# test\nurl = "+f.srv.URL+"\ntoken_entry = fuel/agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUEL_OP_CONF", conf)
	old := token
	token = func(entry string) (string, error) {
		if entry != "fuel/agent-token" {
			t.Errorf("token entry %q", entry)
		}
		return tok, nil
	}
	t.Cleanup(func() { token = old })
}

func runOp(args ...string) (int, string) {
	var out bytes.Buffer
	code := run(args, &out)
	return code, out.String()
}

var snap = map[string]any{"date": "2026-10-02", "budgets": []any{
	map[string]any{"key": "protein_g", "unit": "g", "kind": "floor", "status": "behind", "consumed": 82.0, "target": 160.0}}}

func TestWriteCarriesTheTurnAndPrintsTheResult(t *testing.T) {
	f := newFake(t)
	setup(t, f)
	f.handler = func(n int, r *http.Request, body map[string]any) (int, any) {
		return 200, map[string]any{"result": "written", "entry_id": "en_1", "date": "2026-10-02", "lines": []string{},
			"items":    []any{map[string]any{"item_id": "it_a1", "item": "scrambled eggs", "effective": map[string]any{"kcal": 300.0, "protein_g": 20.0, "carbs_g": 2.0, "fat_g": 24.0, "sat_fat_g": 6.0, "fiber_g": 0.0}}},
			"snapshot": snap}
	}
	code, out := runOp("items", "--turn", "e_cap", "--day", "yesterday", "--new", `[{"item":"scrambled eggs","portion_g":200,"kcal":300,"protein_g":20,"carbs_g":2,"fat_g":24,"sat_fat_g":6}]`)
	if code != 0 || !strings.HasPrefix(out, "written\n") || !strings.Contains(out, "it_a1 scrambled eggs: now 300 kcal, protein 20 g") || !strings.Contains(out, "protein_g: 82 of 160 g, left 78 (floor, behind)") {
		t.Fatalf("%d %s", code, out)
	}
	r, body := f.reqs[0], f.bodies[0]
	if r.Method != "POST" || r.URL.Path != "/fuel/items" || r.Header.Get("X-Fuel-Turn") != "e_cap" || r.Header.Get("Authorization") != "Bearer "+tok {
		t.Errorf("request %s %s %v", r.Method, r.URL.Path, r.Header)
	}
	if body["day"] != "yesterday" || body["new"] != true || body["client_id"] == nil || len(body["items"].([]any)) != 1 {
		t.Errorf("body %v", body)
	}
	if strings.Contains(out, tok) {
		t.Error("the token is in the output")
	}
}

func TestWriteResults(t *testing.T) {
	f := newFake(t)
	setup(t, f)
	// pending
	f.handler = func(int, *http.Request, map[string]any) (int, any) {
		return 200, map[string]any{"result": "pending", "date": "2026-10-02", "snapshot": snap}
	}
	if code, out := runOp("undo", "--turn", "e_cap", "it_a1"); code != 0 || !strings.HasPrefix(out, "pending: it is being saved. Do NOT write it again") {
		t.Errorf("pending: %d %s", code, out)
	}
	// refused
	f.handler = func(int, *http.Request, map[string]any) (int, any) {
		return 409, map[string]any{"error": map[string]any{"code": "likely_duplicate", "message": "looks like scrambled eggs (id it_a1)"}}
	}
	if code, out := runOp("items", "--turn", "e_cap", `[{"item":"eggs","kcal":1,"protein_g":1,"carbs_g":1,"fat_g":1,"sat_fat_g":1}]`); code != 1 || !strings.HasPrefix(out, "refused (409 likely_duplicate): looks like scrambled eggs (id it_a1)") {
		t.Errorf("refused: %d %s", code, out)
	}
	// no capability: nothing is sent
	n := len(f.reqs)
	if code, out := runOp("undo", "it_a1"); code != 1 || !strings.Contains(out, "needs --turn") || len(f.reqs) != n {
		t.Errorf("no turn: %d %s", code, out)
	}
	// two forms of a fix: nothing is sent
	if code, out := runOp("fix", "--turn", "e_cap", "it_a1", "--portion", "100", "--add-g", "20"); code != 1 || !strings.Contains(out, "exactly one") || len(f.reqs) != n {
		t.Errorf("two forms: %d %s", code, out)
	}
}

// A transport error is retried with the SAME client_id; when fueld never
// answers the result is "unknown".
func TestRetryKeepsTheClientID(t *testing.T) {
	f := newFake(t)
	setup(t, f)
	f.handler = func(n int, r *http.Request, body map[string]any) (int, any) {
		if n < 3 {
			return 0, nil
		}
		return 200, map[string]any{"result": "written", "date": "2026-10-02", "snapshot": snap, "lines": []string{"Corrected chicken to 280 g."}}
	}
	old := time.Second
	_ = old
	var out bytes.Buffer
	a, _ := parseArgs([]string{"--turn", "e_cap", "it_a1", "--add-g", "20"})
	cfg, _ := loadConfig()
	c := &client{cfg: cfg, tok: tok, http: &http.Client{Timeout: 2 * time.Second}, out: &out, wait: time.Millisecond}
	if code := c.fix(a); code != 0 || !strings.Contains(out.String(), "written") || !strings.Contains(out.String(), "Corrected chicken to 280 g.") {
		t.Fatalf("%d %s", code, out.String())
	}
	if len(f.bodies) != 3 || f.bodies[0]["client_id"] != f.bodies[2]["client_id"] || f.bodies[2]["portion_g_delta"] != 20.0 {
		t.Errorf("retries: %v", f.bodies)
	}
	f.handler = func(int, *http.Request, map[string]any) (int, any) { return 0, nil }
	out.Reset()
	if code := c.undo(a); code != 1 || !strings.HasPrefix(out.String(), "unknown: fueld did not answer") {
		t.Errorf("unknown: %d %s", code, out.String())
	}
}

func TestReadsAndConfig(t *testing.T) {
	f := newFake(t)
	setup(t, f)
	f.handler = func(n int, r *http.Request, body map[string]any) (int, any) {
		if r.URL.Path == "/fuel/preview" {
			return 200, map[string]any{"date": "2026-10-02", "written": false,
				"items":   []any{map[string]any{"item": "almonds", "portion_g": 30.0, "macros": map[string]any{"kcal": 174.0, "protein_g": 6.3, "carbs_g": 6.0, "fat_g": 15.0, "sat_fat_g": 1.1, "fiber_g": 3.7}}},
				"sum":     map[string]any{"kcal": 174.0, "protein_g": 6.3, "carbs_g": 6.0, "fat_g": 15.0, "sat_fat_g": 1.1, "fiber_g": 3.7},
				"budgets": []any{map[string]any{"key": "sat_fat_g", "unit": "g", "kind": "budget", "consumed": 18.0, "target": 20.0, "adds": 1.1, "after": 19.1, "left_after": 0.9}}}
		}
		return 200, map[string]any{"date": "2026-10-02", "snapshot": snap, "items": []any{
			map[string]any{"row_key": "it_a1", "item": "skyr", "kind": "food", "portion_g": 250.0, "eaten_at": "2026-10-02T10:00:00Z", "source": "fuel",
				"macros": map[string]any{"kcal": 160.0, "protein_g": 27.0, "carbs_g": 10.0, "fat_g": 0.5, "sat_fat_g": 0.3, "fiber_g": 0.0}}}}
	}
	code, out := runOp("day", "--date", "2026-10-02")
	if code != 0 || !strings.Contains(out, "it_a1 | ") || !strings.Contains(out, "skyr (food) | 250 g | 160 kcal") || f.reqs[0].Header.Get("X-Fuel-Turn") != "" || f.reqs[0].URL.RawQuery != "date=2026-10-02" {
		t.Errorf("day: %d %s", code, out)
	}
	code, out = runOp("preview", `[{"item":"almonds","portion_g":30,"kcal":174,"protein_g":6.3,"carbs_g":6,"fat_g":15,"sat_fat_g":1.1}]`)
	if code != 0 || !strings.Contains(out, "NOTHING was written") || !strings.Contains(out, "sat_fat_g: now 18 of 20 g, adds 1.1, after 19.1, left after 0.9") {
		t.Errorf("preview: %d %s", code, out)
	}
	if code, out := runOp("help"); code != 0 || !strings.Contains(out, "fuel-op items --turn C") {
		t.Errorf("help: %s", out)
	}
	if code, out := runOp("frobnicate"); code != 1 || !strings.Contains(out, "unknown command") {
		t.Errorf("unknown: %s", out)
	}
	// No config file: an error, no request.
	t.Setenv("FUEL_OP_CONF", filepath.Join(t.TempDir(), "missing.conf"))
	n := len(f.reqs)
	if code, out := runOp("day"); code != 1 || !strings.Contains(out, "no config file") || len(f.reqs) != n {
		t.Errorf("no config: %d %s", code, out)
	}
}
