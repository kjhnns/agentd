package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTornTailRepairedOnOpen is the D7 regression: a crash in the middle of
// a write leaves a torn last line. Before the fix, Open appended the next
// record onto that torn line (one unreadable line) and Replay stopped at the
// first malformed line, so every later record was hidden. Now Open keeps the
// torn bytes in runlog.torn-<ts>, truncates them, and Replay sees both good
// records.
func TestTornTailRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runlog.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append("turn", map[string]string{"turn_id": "t1"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// Simulate a crash mid-write: half a record, no newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2026-10-24T00:00:00Z","type":"turn","data":{"turn_id":"t`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Append("turn", map[string]string{"turn_id": "t2"}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	recs, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("replayed %d records, want 2 (the committed one and the one after the torn tail)", len(recs))
	}
	if !strings.Contains(string(recs[1].Data), "t2") {
		t.Fatalf("second record = %s, want t2", recs[1].Data)
	}
	torn, _ := filepath.Glob(filepath.Join(dir, "runlog.torn-*"))
	if len(torn) != 1 {
		t.Fatalf("torn copies = %v, want one", torn)
	}
	b, _ := os.ReadFile(torn[0])
	if !strings.Contains(string(b), `"turn_id":"t`) {
		t.Fatalf("torn copy does not hold the torn bytes: %q", b)
	}
}

// TestReplaySkipsInteriorCorruption: a corrupt line in the middle is reported
// and skipped; replay does not stop there.
func TestReplaySkipsInteriorCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runlog.jsonl")
	content := `{"ts":"2026-10-24T00:00:00Z","type":"a","data":{}}` + "\n" +
		"garbage{{{\n" +
		`{"ts":"2026-10-24T00:00:01Z","type":"b","data":{}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 || recs[1].Type != "b" {
		t.Fatalf("records = %+v, want a and b", recs)
	}
}
