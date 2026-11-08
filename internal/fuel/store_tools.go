package fuel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

// ReadLegacyState replays state without opening appenders, truncating torn tails,
// compacting idempotency or starting workers. A torn final line is reported and
// skipped; malformed complete lines fail the import instead of losing history.
func ReadLegacyState(dir string) (sqlstore.ImportBatch, []string, error) {
	var batch sqlstore.ImportBatch
	var warnings []string
	info, err := os.Stat(dir)
	if err != nil {
		return batch, nil, err
	}
	if !info.IsDir() {
		return batch, nil, fmt.Errorf("state-dir is not a directory")
	}
	j := &Journal{entries: map[string]*Entry{}, items: map[string]*Item{}, ops: map[string]*Op{}, itemOps: map[string][]string{}, fractions: map[string]FractionChoice{}, doneByDay: map[string]int{}, idents: map[string]Identity{}, pairUndo: map[string]string{}}
	var order []string
	for _, name := range []string{"journal.jsonl", "idem.jsonl", "feed.jsonl", "coach-events.jsonl", "records.jsonl"} {
		f, err := os.Open(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return batch, warnings, err
		}
		reader := bufio.NewReader(f)
		occurrences := map[string]int{}
		lineNo := 0
		var offset int64
		for {
			line, readErr := reader.ReadBytes('\n')
			offset += int64(len(line))
			lineNo++
			if readErr != nil && readErr != io.EOF {
				f.Close()
				return batch, warnings, readErr
			}
			if readErr == io.EOF {
				if len(bytes.TrimSpace(line)) > 0 {
					warnings = append(warnings, fmt.Sprintf("%s:%d: ignored incomplete final line", name, lineNo))
				}
				break
			}
			raw := bytes.TrimSpace(line)
			if len(raw) == 0 {
				continue
			}
			if !json.Valid(raw) || raw[0] != '{' {
				f.Close()
				return batch, warnings, fmt.Errorf("%s:%d: invalid JSON object", name, lineNo)
			}
			if name == "journal.jsonl" {
				var r journalRec
				if err := json.Unmarshal(raw, &r); err != nil {
					f.Close()
					return batch, warnings, fmt.Errorf("%s:%d: %w", name, lineNo, err)
				}
				if r.T != "txn" && r.T != "attempt" && r.T != "state" {
					f.Close()
					return batch, warnings, fmt.Errorf("%s:%d: unknown journal kind", name, lineNo)
				}
				for _, op := range r.Ops {
					if _, ok := j.ops[op.ID]; !ok {
						order = append(order, op.ID)
					}
				}
				j.apply(r)
			}
			// Canonicalize whitespace only. Preserve original payload bytes in storage.
			var compact bytes.Buffer
			_ = json.Compact(&compact, raw)
			key := compact.String()
			occurrences[key]++
			legacy := sqlstore.LegacyLine{File: name, Identity: sqlstore.LegacyIdentity(compact.Bytes(), occurrences[key]), Data: append(json.RawMessage(nil), raw...)}
			if name == "journal.jsonl" {
				legacy.EventKey = sqlstore.JournalEventKey(offset, line[:len(line)-1])
			}
			batch.Lines = append(batch.Lines, legacy)
		}
		if err := f.Close(); err != nil {
			return batch, warnings, err
		}
	}
	for _, id := range order {
		raw, err := json.Marshal(j.ops[id])
		if err != nil {
			return batch, warnings, err
		}
		batch.Operations = append(batch.Operations, raw)
	}
	return batch, warnings, nil
}

// HistoryImportRows keeps every physical Variables value, including op_id
// duplicates. Initial insertion order matches the legacy reducer's tie-break.
func HistoryImportRows(values []Value, ids VarIDs) ([]sqlstore.Row, []json.RawMessage, error) {
	sorted := append([]Value(nil), values...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	var food []sqlstore.Row
	var records []json.RawMessage
	seen := map[string]bool{}
	for _, v := range sorted {
		if v.VariableID == "" {
			continue
		}
		vr := ""
		switch v.VariableID {
		case ids.Food:
			vr = "food"
		case ids.Body:
			vr = "body"
		case ids.BP:
			if ids.BP != "" {
				vr = "bp"
			}
		case ids.Symptom:
			if ids.Symptom != "" {
				vr = "symptom"
			}
		}
		if vr == "" {
			continue
		}
		_, dateErr := time.Parse("2006-01-02", v.RecordDate)
		if dateErr != nil || v.ID == "" || v.CreatedAt.IsZero() || v.Data == nil || !json.Valid([]byte(v.Raw)) {
			return nil, nil, fmt.Errorf("invalid Variables value %q", v.ID)
		}
		if seen[v.ID] {
			return nil, nil, fmt.Errorf("repeated Variables value id %q", v.ID)
		}
		seen[v.ID] = true
		op, _ := v.Data["op_id"].(string)
		if vr == "food" {
			food = append(food, sqlstore.Row{ID: "v:" + v.ID, Source: sqlstore.SourceVariablesImport, VariablesValueID: v.ID, OpID: op, RecordDate: v.RecordDate, CreatedAt: v.CreatedAt, Data: json.RawMessage(v.Raw)})
			continue
		}
		id, _ := v.Data["record_id"].(string)
		if id == "" {
			id = "v:" + v.ID
		}
		typ, _ := v.Data["type"].(string)
		if typ == "" {
			typ = map[string]string{"body": "body_composition", "bp": "blood_pressure", "symptom": "symptom"}[vr]
		}
		kind := "record"
		if voids, _ := v.Data["voids"].(string); voids != "" {
			kind = "void"
		}
		raw, err := json.Marshal(map[string]any{"op_id": "v:" + v.ID, "remote_op_id": op, "record_id": id, "type": typ, "var": vr, "kind": kind, "record_date": v.RecordDate, "payload": json.RawMessage(v.Raw), "state": OpDone, "value_id": v.ID, "at": v.CreatedAt})
		if err != nil {
			return nil, nil, err
		}
		records = append(records, raw)
	}
	return food, records, nil
}

// HistoryDay contains totals and the active named item count of the day endpoint.
type HistoryDay struct {
	Totals DayTotals
	Items  int
}

// HistoryTotals uses the service reducer and move visibility rules over a
// complete history. All source dates remain available when resolving moves.
func HistoryTotals(values []Value, foodID string) map[string]HistoryDay {
	c := &Cache{days: map[string]*dayCache{}}
	for _, v := range values {
		if v.VariableID != foodID {
			continue
		}
		d := c.days[v.RecordDate]
		if d == nil {
			d = &dayCache{ok: true}
			c.days[v.RecordDate] = d
		}
		d.rows = append(d.rows, v)
	}
	out := make(map[string]HistoryDay, len(c.days))
	for date := range c.days {
		rows, _, _ := c.rowsLocked(date)
		items := 0
		for _, g := range groupRows(rows) {
			name, _ := g.orig.Data["item"].(string)
			if !g.c.undone && strings.TrimSpace(name) != "" {
				items++
			}
		}
		out[date] = HistoryDay{Totals: totalsFromRows(rows), Items: items}
	}
	return out
}
