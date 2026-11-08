package fuel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

func TestLegacyImportAndShadowJournalDedup(t *testing.T) {
	for _, order := range []string{"shadow-first", "import-first"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			// Repeated events, a blank line, surrounding whitespace and CRLF
			// exercise physical byte offsets rather than trimmed payload sizes.
			raw := []byte("  {\"t\":\"attempt\",\"at\":\"2020-01-01T12:00:00Z\",\"op_id\":\"op\",\"attempts\":1} \r")
			contents := append([]byte("\n"), raw...)
			contents = append(contents, '\n')
			firstOffset := int64(len(contents))
			contents = append(contents, raw...)
			contents = append(contents, '\n')
			if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), contents, 0600); err != nil {
				t.Fatal(err)
			}
			batch, warnings, err := ReadLegacyState(dir)
			if err != nil || len(warnings) != 0 || len(batch.Lines) != 2 {
				t.Fatalf("read legacy: %+v %v %v", batch, warnings, err)
			}
			db, err := sqlstore.Open(filepath.Join(dir, "fuel.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			shadow := func() {
				sh := &shadowStore{db: db, journal: newJournalState()}
				for _, offset := range []int64{firstOffset, int64(len(contents))} {
					if err := sh.write(ctx, shadowJob{kind: "journal", key: sqlstore.JournalEventKey(offset, raw), data: raw}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if order == "shadow-first" {
				shadow()
			}
			if got, err := db.Import(ctx, batch); err != nil || got.Lines != 2 {
				t.Fatalf("import: %+v %v", got, err)
			}
			shadow()
			if got, err := db.Import(ctx, batch); err != nil || got != (sqlstore.ImportSummary{}) {
				t.Fatalf("repeat import: %+v %v", got, err)
			}
			counts, err := db.RowCounts(ctx)
			if err != nil || counts["journal"] != 2 || counts["legacy_import_lines"] != 2 {
				t.Fatalf("events duplicated or collapsed: %v %v", counts, err)
			}
		})
	}
}

func historyValue(id, date, raw string) Value {
	var data map[string]any
	_ = json.Unmarshal([]byte(raw), &data)
	return Value{ID: id, VariableID: "food", RecordDate: date, CreatedAt: time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC), Raw: raw, Data: data}
}

func TestHistoryToolsCorrectionAliasesAndMoves(t *testing.T) {
	values := []Value{
		historyValue("a", "2020-01-01", `{"item":"rice","item_id":"rice","op_id":"original","kcal":100}`),
		historyValue("b", "2020-01-01", `{"item":"rice","item_id":"rice","op_id":"original","kcal":100}`),
		historyValue("c", "2020-01-01", `{"corrects":"b","kcal":-25}`),
		historyValue("d", "2020-01-02", `{"item":"rice moved","moved_from":"rice","moved_from_date":"2020-01-01","kcal":75}`),
	}
	totals := HistoryTotals(values, "food")
	if totals["2020-01-01"].Totals.Sum["kcal"] != 750 || totals["2020-01-01"].Items != 1 {
		t.Fatalf("duplicate correction alias: %+v", totals)
	}
	if totals["2020-01-02"].Totals.Sum["kcal"] != 0 || totals["2020-01-02"].Items != 0 {
		t.Fatalf("pending move counted: %+v", totals)
	}
	values = append(values, historyValue("e", "2020-01-01", `{"corrects":"rice","reason":"undo","kcal":-75}`))
	totals = HistoryTotals(values, "food")
	if totals["2020-01-01"].Items != 0 || totals["2020-01-01"].Totals.Sum["kcal"] != 0 || totals["2020-01-02"].Items != 1 || totals["2020-01-02"].Totals.Sum["kcal"] != 750 {
		t.Fatalf("completed move: %+v", totals)
	}
}

func TestHistoryToolsDeterministicImportAndRawPayload(t *testing.T) {
	a := historyValue("a", "2020-01-01", `{"item":"meal","fiber_g":null,"op_id":"same","future":{"x":0}}`)
	b := historyValue("b", "2020-01-01", `{"item":"duplicate","op_id":"same"}`)
	rows, records, err := HistoryImportRows([]Value{b, a}, VarIDs{Food: "food"})
	if err != nil || len(rows) != 2 || len(records) != 0 {
		t.Fatalf("%v %v %v", rows, records, err)
	}
	if rows[0].VariablesValueID != "a" || rows[1].VariablesValueID != "b" || string(rows[0].Data) != a.Raw {
		t.Fatalf("lost ordering or payload: %+v", rows)
	}
	if _, _, err := HistoryImportRows([]Value{a, a}, VarIDs{Food: "food"}); err == nil {
		t.Fatal("accepted repeated physical identity")
	}
	a.CreatedAt = time.Time{}
	if _, _, err := HistoryImportRows([]Value{a}, VarIDs{Food: "food"}); err == nil {
		t.Fatal("accepted missing creation time")
	}
}
