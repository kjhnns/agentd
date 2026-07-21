package runlog

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestAppendAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	type msg struct {
		Chat string `json:"chat"`
		Text string `json:"text"`
	}
	if err := l.Append("inbound", msg{Chat: "123", Text: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Append("outbound", msg{Chat: "123", Text: "hello back"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recs, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("replayed %d records, want 2", len(recs))
	}
	if recs[0].Type != "inbound" || recs[1].Type != "outbound" {
		t.Fatalf("record types = %q, %q", recs[0].Type, recs[1].Type)
	}
	var got msg
	if err := json.Unmarshal(recs[1].Data, &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if got.Text != "hello back" {
		t.Fatalf("data.Text = %q", got.Text)
	}
	if recs[0].TS.IsZero() {
		t.Fatal("record TS should be stamped")
	}
}

func TestReplayMissingFile(t *testing.T) {
	recs, err := Replay(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("Replay missing: %v", err)
	}
	if recs != nil {
		t.Fatalf("expected nil records for missing file, got %d", len(recs))
	}
}
