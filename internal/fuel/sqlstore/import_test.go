package sqlstore

import (
	"context"
	"encoding/json"
	"testing"
)

func TestBatchImportEchoIsNoOpAndRollback(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	local := mustInsert(t, s, ownRow("local"))
	echo := importedRow("echo", "remote")
	echo.OpID = local.OpID
	b := ImportBatch{Food: []Row{echo}}
	first, err := s.Import(ctx, b)
	if err != nil || first.FoodAdded != 0 || first.FoodUpdated != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := s.Import(ctx, b)
	if err != nil || second != (ImportSummary{}) {
		t.Fatalf("repeat: %+v %v", second, err)
	}
	good := importedRow("good", "good")
	bad := importedRow("bad", "bad")
	bad.RecordDate = "invalid"
	if _, err := s.Import(ctx, ImportBatch{Food: []Row{good, bad}}); err == nil {
		t.Fatal("accepted invalid row")
	}
	if count(t, s, "food_rows") != 1 {
		t.Fatal("partial batch survived")
	}
}

func TestBatchImportRepeatedEventsAndRecordAdoption(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	raw := json.RawMessage(`{"t":"txn","at":"2026-11-08T11:00:00Z","future":true}`)
	b := ImportBatch{Lines: []LegacyLine{{File: "journal.jsonl", Identity: LegacyIdentity(raw, 1), Data: raw, EventKey: JournalEventKey(int64(len(raw)+1), raw)}, {File: "journal.jsonl", Identity: LegacyIdentity(raw, 2), Data: raw, EventKey: JournalEventKey(2*int64(len(raw)+1), raw)}}}
	remote := json.RawMessage(`{"op_id":"v:value","remote_op_id":"record-op","record_id":"record","type":"waist","var":"body","kind":"record","record_date":"2026-11-08","value_id":"value","state":"done","at":"2026-11-08T11:00:00Z","payload":{"waist_cm":88}}`)
	b.Records = []json.RawMessage{remote}
	if _, err := s.Import(ctx, b); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "journal") != 2 {
		t.Fatal("collapsed repeated events")
	}
	if got, err := s.Import(ctx, b); err != nil || got != (ImportSummary{}) {
		t.Fatalf("%+v %v", got, err)
	}
	local := json.RawMessage(`{"op_id":"record-op","record_id":"record","type":"waist","var":"body","kind":"record","record_date":"2026-11-08","value_id":"value","state":"done","at":"2026-11-08T11:00:00Z","client_id":"client","payload":{"waist_cm":88}}`)
	b.Lines = append(b.Lines, LegacyLine{File: "records.jsonl", Identity: LegacyIdentity(local, 1), Data: local})
	if _, err := s.Import(ctx, b); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "records") != 1 {
		t.Fatal("record echo duplicated")
	}
	var op, payload string
	if err := s.db.QueryRow("SELECT op_id,payload FROM records").Scan(&op, &payload); err != nil {
		t.Fatal(err)
	}
	if op != "record-op" || payload != string(local) {
		t.Fatalf("lost local envelope: %s %s", op, payload)
	}
	var archived string
	if err := s.db.QueryRow("SELECT payload FROM legacy_import_lines WHERE source='records.jsonl'").Scan(&archived); err != nil || archived != string(local) {
		t.Fatalf("lost raw history: %s %v", archived, err)
	}
}
