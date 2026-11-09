package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/fuel"
	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

// Use real HTTP and httptest over net.Pipe. The sandbox forbids TCP listeners.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80} }
func fakeVariables(t *testing.T, values *[]map[string]any) string {
	t.Helper()
	listener := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected write: %s", r.Method)
			http.Error(w, "write", 405)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing key")
		}
		if r.URL.Query().Get("includeJson") != "true" {
			t.Error("missing includeJson")
		}
		switch r.URL.Path {
		case "/variables":
			io.WriteString(w, `[{"id":"food","name":"Food log","type":"json"},{"id":"body","name":"Body composition","type":"json"},{"id":"bp","name":"Blood pressure","type":"json"},{"id":"symptom","name":"Symptom log","type":"json"}]`)
		case "/values":
			if r.URL.Query().Get("date") != "" {
				t.Error("history must not be bounded")
			}
			json.NewEncoder(w).Encode(*values)
		default:
			http.NotFound(w, r)
		}
	})}}
	server.Start()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, peer := net.Pipe()
		select {
		case listener.connections <- peer:
			return client, nil
		case <-ctx.Done():
			client.Close()
			peer.Close()
			return nil, ctx.Err()
		}
	}}
	old := variablesClient
	variablesClient = &http.Client{Transport: transport, Timeout: time.Second * 5}
	t.Cleanup(func() { transport.CloseIdleConnections(); server.Close(); variablesClient = old })
	t.Setenv("FUEL_TEST_KEY", "test-key")
	return server.URL
}
func value(id, variable, date, raw string) map[string]any {
	return map[string]any{"id": id, "variableId": variable, "recordDate": date, "createdAt": date + "T12:00:00Z", "data": raw}
}
func command(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := run(context.Background(), args, &out, &err)
	return code, out.String(), err.String()
}
func writeLines(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func counts(t *testing.T, path string) map[string]int64 {
	t.Helper()
	s, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.RowCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func fixture(t *testing.T) (string, string, []map[string]any) {
	t.Helper()
	state := t.TempDir()
	db := filepath.Join(t.TempDir(), "fuel.sqlite")
	writeLines(t, state, "journal.jsonl", `{"t":"txn","at":"2020-01-01T12:00:00Z","entry":{"id":"e","created_at":"2020-01-01T12:00:00Z"},"ops":[{"op_id":"op","entry_id":"e","item_id":"i","date":"2020-01-01","state":"pending","created_at":"2020-01-01T12:00:00Z","data":{"item":"rice"}}]}`+"\n"+`{"t":"attempt","at":"2020-01-01T12:01:00Z","op_id":"op","attempts":2}`+"\n"+`{"t":"state","at":"2020-01-01T12:02:00Z","op_id":"op","state":"uncertain"}`+"\n")
	writeLines(t, state, "idem.jsonl", `{"client_id":"client","hash":"hash","kind":"log","at":"2020-01-01T12:00:00Z","status":200,"response":{"ok":true}}`+"\n")
	writeLines(t, state, "feed.jsonl", `{"id":"feed1","seq":9,"entry_id":"e","at":"2020-01-01T12:00:00Z","role":"user","text":"rice"}`+"\n"+`{"id":"feed2","seq":10,"entry_id":"e","at":"2019-01-01T12:00:00Z","role":"fuel","text":"saved"}`+"\n")
	writeLines(t, state, "coach-events.jsonl", `{"entry_id":"e","at":"2020-01-01T12:00:00Z","date":"2020-01-01","items":[],"totals":{}}`+"\n")
	writeLines(t, state, "records.jsonl", `{"op_id":"record-op","record_id":"r","type":"waist","var":"body","kind":"record","record_date":"2020-01-01","payload":{"waist_cm":88},"state":"pending","at":"2020-01-01T12:00:00Z"}`+"\n")
	values := []map[string]any{
		value("original", "food", "2020-01-01", `{"item":"rice","op_id":"op","kcal":100,"protein_g":10,"sat_fat_g":1,"fiber_g":2,"carbs_g":15}`),
		value("body1", "body", "2020-01-01", `{"weight_kg":70,"future":{"x":null}}`),
		value("bp1", "bp", "2020-01-01", `{"systolic_mmhg":120,"diastolic_mmhg":80}`),
		value("symptom1", "symptom", "2020-01-01", `{"category":"headache","present":true}`),
	}
	return state, db, values
}
func importArgs(state, db, url string) []string {
	return []string{"import", "--state-dir", state, "--db", db, "--variables-url", url, "--key-env", "FUEL_TEST_KEY", "--food-var", "food", "--body-var", "body"}
}

func TestImportFullHistoryIdempotencyAndDryRun(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	args := importArgs(state, db, url)
	code, out, errOut := command(t, append(args, "--dry-run")...)
	if code != 0 || !strings.Contains(out, "food +1") {
		t.Fatalf("dry-run: %d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("dry-run created database")
	}
	code, out, errOut = command(t, args...)
	if code != 0 {
		t.Fatalf("import: %d %s %s", code, out, errOut)
	}
	want := map[string]int64{"food_rows": 1, "journal": 3, "operations": 1, "idem": 1, "feed": 2, "coach_events": 1, "records": 4, "export_outbox": 0, "legacy_import_lines": 8, "store_metadata": 0, "import_cursor": 0, "local_events": 0, "local_values": 0, "local_aliases": 0, "export_objects": 1}
	c := counts(t, db)
	if len(c) != len(want) {
		t.Fatalf("unexpected schema tables: %v", c)
	}
	for key, n := range want {
		if c[key] != n {
			t.Errorf("%s=%d want %d", key, c[key], n)
		}
	}
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut = command(t, append(args, "--dry-run")...)
	if code != 0 || !strings.Contains(out, "state lines +0; food +0 updated 0; remote records +0 updated 0") {
		t.Fatalf("repeat dry-run: %d %s %s", code, out, errOut)
	}
	after, _ := os.ReadFile(db)
	if !bytes.Equal(before, after) {
		t.Fatal("dry-run changed database")
	}
	code, out, errOut = command(t, args...)
	if code != 0 || !strings.Contains(out, "state lines +0; food +0 updated 0; remote records +0 updated 0") {
		t.Fatalf("repeat: %d %s %s", code, out, errOut)
	}
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var keyed int
	if err := conn.QueryRow("SELECT count(DISTINCT event_key) FROM journal WHERE event_key <> ''").Scan(&keyed); err != nil || keyed != 3 {
		t.Fatalf("journal event keys: %d, %v", keyed, err)
	}
	var payload string
	if err := conn.QueryRow("SELECT payload FROM operations WHERE op_id='op'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"state":"uncertain"`) || !strings.Contains(payload, `"attempts":2`) {
		t.Fatalf("lost pending replay: %s", payload)
	}
	var order string
	conn.QueryRow("SELECT group_concat(id, ',') FROM (SELECT id FROM feed ORDER BY local_seq)").Scan(&order)
	if order != "feed1,feed2" {
		t.Fatal(order)
	}
}

func TestImportUpdatesAndAppendReplay(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	args := importArgs(state, db, url)
	if code, _, err := command(t, args...); code != 0 {
		t.Fatal(err)
	}
	values[0]["data"] = `{"item":"rice","op_id":"op","kcal":200}`
	values[1]["data"] = `{"weight_kg":71}`
	values = append(values, value("duplicate", "food", "2020-01-01", `{"item":"rice","op_id":"op","kcal":200}`), value("record-echo", "body", "2020-01-01", `{"op_id":"record-op","record_id":"r","waist_cm":88}`))
	f, err := os.OpenFile(filepath.Join(state, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"t":"state","at":"2020-01-01T12:03:00Z","op_id":"op","state":"done","value_id":"original"}`)
	f.Close()
	code, out, errOut := command(t, args...)
	if code != 0 || !strings.Contains(out, "food +1 updated 1") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	c := counts(t, db)
	if c["food_rows"] != 2 || c["records"] != 4 || c["journal"] != 4 {
		t.Fatal(c)
	}
	code, out, errOut = command(t, args...)
	if code != 0 || !strings.Contains(out, "state lines +0; food +0 updated 0; remote records +0 updated 0") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
}

func TestImportRejectsMalformedAndPreservesTornTail(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	args := importArgs(state, db, url)
	path := filepath.Join(state, "feed.jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tail := append(original, []byte(`{"unfinished":`)...)
	writeLines(t, state, "feed.jsonl", string(tail))
	code, _, errOut := command(t, args...)
	if code != 0 || !strings.Contains(errOut, "incomplete final line") {
		t.Fatalf("%d %s", code, errOut)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, tail) {
		t.Fatal("source was truncated")
	}
	writeLines(t, state, "feed.jsonl", "not JSON\n")
	if code, _, _ := command(t, args...); code == 0 {
		t.Fatal("accepted corrupt complete line")
	}
}

func TestImportRollsBackInvalidEnvelope(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	writeLines(t, state, "records.jsonl", `{"op_id":"invalid"}`+"\n")
	if code, _, _ := command(t, importArgs(state, db, url)...); code == 0 {
		t.Fatal("accepted invalid record")
	}
	c := counts(t, db)
	for name, n := range c {
		if n != 0 {
			t.Errorf("partial import %s=%d", name, n)
		}
	}
}

func TestCompareParityDifferencesAndUnknowns(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	values = append(values, value("correction", "food", "2020-01-01", `{"item":"correction","corrects":"original","kcal":-25,"protein_g":-2,"sat_fat_g":-0.2,"fiber_g":-0.5,"carbs_g":-3}`))
	if code, _, err := command(t, importArgs(state, db, url)...); code != 0 {
		t.Fatal(err)
	}
	args := []string{"compare", "--db", db, "--variables-url", url, "--key-env", "FUEL_TEST_KEY", "--food-var", "food", "--from", "2020-01-01", "--to", "2020-01-02"}
	code, out, err := command(t, args...)
	if code != 0 || !strings.Contains(out, "75.0") || !strings.Contains(out, "2020-01-02") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	values[0]["data"] = `{"item":"rice","op_id":"op","kcal":101,"protein_g":10,"sat_fat_g":1,"fiber_g":2,"carbs_g":15}`
	if code, out, _ := command(t, args...); code != 1 || !strings.Contains(out, "DIFF") {
		t.Fatalf("missed difference: %d %s", code, out)
	}
	values[0]["data"] = `{"item":"rice","op_id":"op","kcal":100,"protein_g":10,"sat_fat_g":1,"fiber_g":null,"carbs_g":15}`
	if code, out, _ := command(t, args...); code != 1 || !strings.Contains(out, "+?1") {
		t.Fatalf("missed unknown: %d %s", code, out)
	}
}

func TestBackupRestoreRotationAndReadOnly(t *testing.T) {
	state, db, values := fixture(t)
	url := fakeVariables(t, &values)
	if code, _, err := command(t, importArgs(state, db, url)...); code != 0 {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLines(t, dir, "unrelated.db", "keep")
	writeLines(t, dir, "fuel-invalid.db", "keep")
	sourceBefore, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 3; day++ {
		var out bytes.Buffer
		err := backupCommand(context.Background(), []string{"--db", db, "--out", dir, "--keep", "2"}, &out, &out, time.Date(2026, 11, day, 3, 20, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
	}
	sourceAfter, err := os.ReadFile(db)
	if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatalf("backup changed source: %v", err)
	}
	if c := counts(t, db); c["store_metadata"] != 0 {
		t.Fatalf("read-only backup wrote metadata: %v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "fuel-20261101-0320.db")); !os.IsNotExist(err) {
		t.Fatal("old snapshot retained")
	}
	for _, name := range []string{"unrelated.db", "fuel-invalid.db", "fuel-20261102-0320.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := filepath.Join(dir, "fuel-20261103-0320.db")
	before, _ := os.ReadFile(snapshot)
	if err := os.Chmod(snapshot, 0400); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := command(t, "restore-check", "--backup", snapshot)
	if code != 0 || !strings.Contains(out, "food_rows\t1") || !strings.Contains(out, "integrity_check ok") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	after, _ := os.ReadFile(snapshot)
	if !bytes.Equal(before, after) {
		t.Fatal("restore-check changed snapshot")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(snapshot + suffix); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s created", suffix)
		}
	}
	if err := backupCommand(context.Background(), []string{"--db", db, "--out", dir}, io.Discard, io.Discard, time.Date(2026, 11, 3, 3, 20, 0, 0, time.UTC)); err == nil {
		t.Fatal("overwrote snapshot")
	}
	missing := filepath.Join(dir, "missing.db")
	if code, _, _ := command(t, "backup", "--db", missing, "--out", dir); code == 0 {
		t.Fatal("backed up missing source")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing source")
	}
}

func TestRestoreCheckRejectsCorruptOrMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, path + "missing"} {
		if code, _, _ := command(t, "restore-check", "--backup", file); code == 0 {
			t.Fatal("accepted invalid backup")
		}
	}
}

func TestCLIValidation(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"import"}, {"compare", "--db", "x", "--from", "2020-02-02", "--to", "2020-01-01"}, {"backup", "--db", "x", "--out", "x", "--keep", "-1"}, {"restore-check"}, {"backup", "extra"}} {
		if code, _, _ := command(t, args...); code != 2 {
			t.Fatalf("%v returned %d", args, code)
		}
	}
	if code, _, _ := command(t, "import", "--help"); code != 0 {
		t.Fatal(code)
	}
}

func TestBackupUnitUsesConfiguredStorePath(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/fuel-store-backup.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "\nEnvironmentFile=%h/.config/fueld/backup.env\n") {
		t.Fatal("backup unit must require its path EnvironmentFile")
	}
	var args []string
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			args = strings.Fields(strings.TrimPrefix(line, "ExecStart="))[1:]
		}
	}
	if len(args) < 3 || args[1] != "--db" || args[2] != "${FUEL_STORE_PATH}" {
		t.Fatalf("backup unit has a separate database default: %v", args)
	}
	for _, mode := range []string{"default", "custom", "missing"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, ".local/state/fueld")
			config := fmt.Sprintf("state_dir = %q\n", state)
			if mode == "custom" {
				config += fmt.Sprintf("store_path = %q\n", filepath.Join(home, "custom store", "meals.sqlite"))
			}
			cfg, err := fuel.ParseDaemonConfig([]byte(config))
			if err != nil {
				t.Fatal(err)
			}
			path := cfg.StorePath
			if mode == "missing" {
				path = ""
			} else {
				s, err := sqlstore.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				row := sqlstore.Row{ID: "configured-store", Source: sqlstore.SourceFueld, OpID: "op", RecordDate: "2026-11-08", CreatedAt: time.Now(), Data: json.RawMessage(`{"item":"meal"}`)}
				if err := s.PutShadowRow(context.Background(), row, "food"); err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			// systemd expands ${VAR} as one argument, including embedded spaces.
			expanded := make([]string, len(args))
			for i, arg := range args {
				expanded[i] = strings.ReplaceAll(strings.ReplaceAll(arg, "${FUEL_STORE_PATH}", path), "%h", home)
			}
			code, out, errOut := command(t, expanded...)
			if mode == "missing" {
				if code != 2 || !strings.Contains(errOut, "db, out and nonnegative keep are required") {
					t.Fatalf("missing path used a fallback: %d %s %s", code, out, errOut)
				}
				return
			}
			if code != 0 {
				t.Fatalf("backup: %d %s %s", code, out, errOut)
			}
			snapshot := strings.TrimSuffix(strings.TrimSpace(out), ": integrity_check ok")
			s, err := sqlstore.OpenReadOnly(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			rows, err := s.ListRows(context.Background(), "2026-11-08", "2026-11-08")
			if err != nil || len(rows) != 1 || rows[0].ID != "configured-store" {
				t.Fatalf("wrong store backed up: %+v %v", rows, err)
			}
		})
	}
}
