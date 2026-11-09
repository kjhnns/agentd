package fuel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

// localStore implements the existing read interface entirely from SQLite.
// The remote client is private to import/export background work.
type localStore struct {
	db       *sqlstore.Store
	remote   Variables
	food     string
	defaults []VarInfo
	export   bool
	mu       sync.Mutex // serialize importer/exporter, never acquired by requests
}

func (l *localStore) ListVariables(ctx context.Context) ([]VarInfo, error) {
	b, err := l.db.LocalMeta(ctx, "local_variables")
	if err != nil {
		return nil, err
	}
	var list []VarInfo
	if b != "" {
		if err = json.Unmarshal([]byte(b), &list); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	for i := range list {
		list[i].ID = list[i].Name
		seen[list[i].Name] = true
	}
	for _, v := range l.defaults {
		if v.Name != "" && !seen[v.Name] {
			list = append(list, v)
			seen[v.Name] = true
		}
	}
	return list, nil
}
func (l *localStore) ByDate(ctx context.Context, date string) ([]Value, error) {
	return l.values(ctx, date)
}
func (l *localStore) All(ctx context.Context) ([]Value, error) { return l.values(ctx, "") }
func (l *localStore) Post(context.Context, string, map[string]any, string) (string, error) {
	return "", errors.New("local writes require a transaction")
}
func (l *localStore) values(ctx context.Context, date string) ([]Value, error) {
	rows, err := l.db.LocalValues(ctx, date)
	if err != nil {
		return nil, err
	}
	out := make([]Value, 0, len(rows))
	for _, r := range rows {
		v := Value{localSeq: r.Sequence, aliases: r.Aliases, ID: r.ID, VariableID: r.Variable, RecordDate: r.Date, CreatedAt: r.At, Raw: r.Raw}
		v.aliases = append(v.aliases, r.ID)
		if r.RemoteID != "" {
			v.ID = r.RemoteID
		}
		_ = json.Unmarshal([]byte(r.Raw), &v.Data)
		out = append(out, v)
	}
	return out, nil
}
func localValue(op Op, food string) sqlstore.LocalValue {
	b, _ := json.Marshal(withTitle(op.Data))
	return sqlstore.LocalValue{ID: op.ID, Variable: food, Date: op.Date, At: op.CreatedAt, OpID: op.WireID(), Own: true, Food: true, Raw: string(b)}
}
func (s *Service) initLocal() error {
	db, err := sqlstore.Open(s.o.StorePath)
	if err != nil {
		return err
	}
	l := &localStore{db: db, remote: s.o.Vars, food: s.o.FoodVar, export: s.o.ExportVariables == nil || *s.o.ExportVariables}
	s.local = l
	for _, v := range []string{s.o.FoodVar, s.o.BodyVar, s.o.BPVar, s.o.SymptomVar} {
		l.defaults = append(l.defaults, VarInfo{ID: v, Name: v, Type: "json"})
	}
	for _, v := range []string{"Push ups", "Pull ups"} {
		l.defaults = append(l.defaults, VarInfo{ID: v, Name: v, Type: "numeric"})
	}
	ctx := context.Background()
	seeded, err := db.LocalMeta(ctx, "local_initialized")
	if err != nil {
		db.Close()
		return err
	}
	if seeded == "" {
		// Import the final legacy replay once. Promotion refuses unresolved writes.
		var events []sqlstore.LocalEvent
		for _, kind := range []string{"journal", "idem", "feed", "records"} {
			err = loadLines(filepath.Join(s.o.StateDir, kind+".jsonl"), true, func(b []byte) error {
				events = append(events, sqlstore.LocalEvent{Stream: kind, Data: append([]byte(nil), b...)})
				return nil
			})
			if err != nil {
				db.Close()
				return err
			}
		}
		j := newJournalState()
		recs := &recordStore{ops: map[string]*recOp{}, byClient: map[string]string{}}
		for _, e := range events {
			switch e.Stream {
			case "journal":
				var r journalRec
				if err = json.Unmarshal(e.Data, &r); err != nil {
					db.Close()
					return err
				}
				j.apply(r)
			case "records":
				var r recOp
				if err = json.Unmarshal(e.Data, &r); err != nil {
					db.Close()
					return err
				}
				recs.apply(r)
			}
		}
		if len(j.NonTerminal()) > 0 {
			db.Close()
			return errors.New("local promotion requires resolved legacy food operations")
		}
		for _, op := range recs.all() {
			if !terminal(op.State) {
				db.Close()
				return errors.New("local promotion requires resolved legacy record operations")
			}
		}
		rows, err := db.ListRows(ctx, "0001-01-01", "9999-12-31")
		if err != nil {
			db.Close()
			return err
		}
		values := []sqlstore.LocalValue{}
		for _, r := range rows {
			values = append(values, sqlstore.LocalValue{ID: r.ID, Variable: l.food, Date: r.RecordDate, At: r.CreatedAt, OpID: r.OpID, RemoteID: r.VariablesValueID, Own: r.Source == sqlstore.SourceFueld, Food: true, Raw: string(r.Data)})
		}
		seen := map[string]bool{}
		for _, v := range values {
			seen[v.OpID] = true
		}
		for _, op := range j.Ops() {
			if op.State == OpDone && !seen[op.WireID()] {
				v := localValue(op, l.food)
				v.RemoteID = op.ValueID
				values = append(values, v)
			}
		}
		for _, op := range recs.all() {
			if op.State == OpDone {
				v := s.localRecordValue(op)
				v.RemoteID = op.ValueID
				values = append(values, v)
			}
		}
		// Marker is an event in the same transaction, so a crash cannot replay seed twice.
		events = append(events, sqlstore.LocalEvent{Stream: "initialized", Data: json.RawMessage(`true`)})
		prior, err := db.LocalEvents(ctx, "initialized")
		if err != nil {
			db.Close()
			return err
		}
		if len(prior) == 0 {
			if err = db.CommitLocal(ctx, events, values, false); err != nil {
				db.Close()
				return err
			}
		}
		if err = db.SetLocalMeta(ctx, "local_initialized", "true"); err != nil {
			db.Close()
			return err
		}
	}
	for _, kind := range []string{"journal", "idem", "feed", "records"} {
		events, err := db.LocalEvents(ctx, kind)
		if err != nil {
			db.Close()
			return err
		}
		for _, b := range events {
			switch kind {
			case "journal":
				var r journalRec
				err = json.Unmarshal(b, &r)
				if err == nil {
					s.journal.apply(r)
				}
			case "idem":
				var r idemRec
				err = json.Unmarshal(b, &r)
				if err == nil {
					s.idem.recs[r.ClientID] = &r
				}
			case "feed":
				var r FeedItem
				err = json.Unmarshal(b, &r)
				if err == nil {
					s.feed.items = append(s.feed.items, r)
					s.feed.index(r)
				}
			case "records":
				var r recOp
				err = json.Unmarshal(b, &r)
				if err == nil {
					s.records.apply(r)
				}
			}
			if err != nil {
				db.Close()
				return err
			}
		}
	}
	s.o.Vars = l
	s.journal.local = func(ctx context.Context, r *journalRec) error {
		var values []sqlstore.LocalValue
		for i := range r.Ops {
			op := &r.Ops[i]
			if op.State == OpFailed {
				continue
			}
			values = append(values, localValue(*op, l.food))
			op.State = OpDone
			op.ValueID = op.ID
			op.DoneAt = r.At
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return db.CommitLocal(ctx, []sqlstore.LocalEvent{{Stream: "journal", Data: b}}, values, l.export)
	}
	s.journal.publishLocal = func(r journalRec) {
		if len(r.Ops) == 0 || s.cache == nil {
			return
		}
		// Food mutations hold stateMu across commit, replay and cache publication.
		if err := s.cache.refreshLocalLocked(context.Background(), r.Ops[0].Date); err != nil {
			log.Printf("fuel: local publication: %v", err)
		}
	}
	s.idem.local = func(b []byte) error {
		return db.CommitLocal(context.Background(), []sqlstore.LocalEvent{{Stream: "idem", Data: b}}, nil, false)
	}
	s.feed.local = func(b []byte) error {
		return db.CommitLocal(context.Background(), []sqlstore.LocalEvent{{Stream: "feed", Data: b}}, nil, false)
	}
	s.records.local = func(op *recOp) error {
		v := s.localRecordValue(*op)
		op.State = OpDone
		if op.LastTry.IsZero() {
			op.LastTry = s.o.Now()
		}
		if op.ValueID == "" {
			op.ValueID = op.OpID
		}
		b, err := json.Marshal(op)
		if err != nil {
			return err
		}
		return db.CommitLocal(context.Background(), []sqlstore.LocalEvent{{Stream: "records", Data: b}}, []sqlstore.LocalValue{v}, l.export)
	}
	return nil
}
func (s *Service) localRecordValue(op recOp) sqlstore.LocalValue {
	target := s.o.BodyVar
	if op.Var == "bp" {
		target = s.o.BPVar
	}
	if op.Var == "symptom" {
		target = s.o.SymptomVar
	}
	b, _ := json.Marshal(withTitle(op.Payload))
	return sqlstore.LocalValue{ID: op.OpID, Variable: target, Date: op.RecordDate, At: op.At, OpID: op.OpID, Own: true, Raw: string(b)}
}

func (l *localStore) importOnce(ctx context.Context, date string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	list, err := l.remote.ListVariables(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{}
	allowed := map[string]bool{}
	for _, v := range l.defaults {
		if v.Name != "" {
			allowed[v.Name] = true
		}
	}
	for _, v := range list {
		names[v.ID] = v.Name
		if v.Type == "numeric" || v.Type == "number" {
			allowed[v.Name] = true
		}
	}
	var vals []Value
	if date == "" {
		vals, err = l.remote.All(ctx)
	} else {
		vals, err = l.remote.ByDate(ctx, date)
	}
	if err != nil {
		return err
	}
	var rows []sqlstore.LocalValue
	for _, v := range vals {
		name := names[v.VariableID]
		if name == "" {
			return errors.New("import value has unknown variable")
		}
		if !allowed[name] {
			continue
		}
		raw := v.Raw
		if raw == "" && v.Data != nil {
			b, e := json.Marshal(v.Data)
			if e != nil {
				return e
			}
			raw = string(b)
		}
		data := v.Data
		if data == nil {
			_ = json.Unmarshal([]byte(raw), &data)
		}
		op, _ := data["op_id"].(string)
		rows = append(rows, sqlstore.LocalValue{Variable: name, Date: v.RecordDate, At: v.CreatedAt, OpID: op, RemoteID: v.ID, Food: name == l.food, Raw: raw})
	}
	if err = l.db.ImportLocal(ctx, rows, date, now, l.export); err != nil {
		return err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return l.db.SetLocalMeta(ctx, "local_variables", string(b))
}
func (l *localStore) exportOnce(ctx context.Context, now time.Time) error {
	if !l.export {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, err := l.db.LocalExports(ctx, now)
	if err != nil || len(entries) == 0 {
		return err
	}
	list, readErr := l.remote.ListVariables(ctx)
	var vals []Value
	if readErr == nil {
		// Operation ids can collide on another date or target too. Inspect the
		// complete reconciliation result, not just the first matching row.
		vals, readErr = l.remote.All(ctx)
	}
	completed := map[string]bool{}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.DependsOn != "" && !completed[e.DependsOn] {
			continue
		}
		if err = l.db.BeginLocalExport(ctx, e, now); err != nil {
			return err
		}
		target := ""
		for _, v := range list {
			if v.Name == e.Variable {
				if target != "" {
					target = ""
					break
				}
				target = v.ID
			}
		}
		err = readErr
		if err == nil && target == "" {
			err = errors.New("export variable unresolved")
		}
		var id string
		conflict := false
		if err == nil {
			for _, v := range vals {
				data := v.Data
				if data == nil {
					_ = json.Unmarshal([]byte(v.Raw), &data)
				}
				if e.OpID == "" || data["op_id"] != e.OpID {
					continue
				}
				raw, marshalErr := json.Marshal(data)
				remote := sqlstore.LocalValue{Variable: e.Variable, Date: v.RecordDate, Raw: string(raw)}
				if marshalErr != nil || v.VariableID != target || !sqlstore.LocalPayloadEqual(e.LocalValue, remote) {
					conflict = true
					break
				}
				if id == "" {
					id = v.ID
				}
			}
		}
		if conflict {
			if err = l.db.ConflictLocalExport(ctx, e.ID); err != nil {
				return err
			}
			continue
		}
		if err == nil && id == "" {
			var retry bool
			retry, err = l.db.LocalExportMayRetry(ctx, e, now)
			if err == nil && !retry {
				continue
			}
			var data map[string]any
			if err == nil {
				err = json.Unmarshal([]byte(e.Raw), &data)
			}
			if err == nil {
				// Persist before POST, including retries. A restart must retain
				// the original uncertainty time and empty-read evidence.
				if err = l.db.UncertainLocalExport(ctx, e.ID, now); err != nil {
					return err
				}
				id, err = l.remote.Post(ctx, target, data, e.Date)
				if err == nil && id != "" {
					vals = append(vals, Value{ID: id, VariableID: target, RecordDate: e.Date, Data: data})
				}
				var rejected *PostError
				if errors.As(err, &rejected) {
					if saveErr := l.db.RejectLocalExport(context.WithoutCancel(ctx), e.ID); saveErr != nil {
						return saveErr
					}
				}
			}
		}
		if err == nil && id == "" {
			err = errors.New("export acknowledgement missing id")
		}
		if err == nil {
			err = l.db.AckLocalExport(ctx, e.ID, id, now)
		}
		if err != nil {
			if saveErr := l.db.FailLocalExport(context.WithoutCancel(ctx), e.ID, errClass(err)); saveErr != nil {
				return saveErr
			}
			continue
		}
		completed[e.ID] = true
	}
	return nil
}
func (s *Service) localSync(ctx context.Context, date string) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.local.importOnce(c, date, s.o.Now()); err != nil {
		_ = s.local.db.SetLocalMeta(context.WithoutCancel(ctx), "local_import_error", errClass(err))
		log.Printf("fuel: local import: %s", errClass(err))
		return
	}
	_ = s.local.db.SetLocalMeta(ctx, "local_import_error", "")
	if date == "" {
		_ = s.cache.RefreshBody(ctx)
		s.refreshVarList(ctx)
	}
	// Invalidate cached days so every consumer reloads imported edits/deletions.
	s.cache.mu.Lock()
	for _, d := range s.cache.days {
		d.fetched = time.Time{}
	}
	s.cache.mu.Unlock()
	s.calibDirty.Store(true)
}
func (s *Service) localLoop(ctx context.Context) {
	day := time.NewTicker(2 * time.Minute)
	full := time.NewTicker(20 * time.Minute)
	window := time.NewTicker(10 * time.Minute)
	offset := 2
	defer window.Stop()
	var exportTick <-chan time.Time
	if s.local.export {
		exporter := time.NewTicker(time.Second)
		defer exporter.Stop()
		exportTick = exporter.C
	}
	defer day.Stop()
	defer full.Stop()
	// Full import is background only, after Ready. It also reconciles historical deletions.
	s.localSync(ctx, "")
	for {
		select {
		case <-ctx.Done():
			return
		case <-day.C:
			s.localSync(ctx, s.today())
			s.localSync(ctx, dateAdd(s.today(), -1))
		case <-window.C:
			for i := 0; i < 6; i++ {
				s.localSync(ctx, dateAdd(s.today(), -offset))
				offset++
				if offset >= recentDays {
					offset = 2
				}
			}
		case <-full.C:
			s.localSync(ctx, "")
		case <-exportTick:
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.local.exportOnce(c, s.o.Now())
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("fuel: local export: %s", errClass(err))
			}
		}
	}
}

// ExportLocalLegacy reconstructs compatible rollback files from SQLite. Writers
// must be stopped. The durable backlog gate cannot be bypassed by a stale status.
func ExportLocalLegacy(ctx context.Context, path, dir string) error {
	db, err := sqlstore.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	initialized, err := db.LocalMeta(ctx, "local_initialized")
	if err != nil {
		return err
	}
	if initialized != "true" {
		return errors.New("not an initialized local store")
	}
	pending, err := db.PendingExports(ctx)
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return fmt.Errorf("rollback requires drained exporter: %d pending", len(pending))
	}
	vals, err := db.LocalValues(ctx, "")
	if err != nil {
		return err
	}
	aliases := map[string]string{}
	for _, v := range vals {
		if v.Own {
			if v.RemoteID == "" {
				return errors.New("rollback has unexported local values")
			}
			aliases[v.ID] = v.RemoteID
		}
	}
	for _, kind := range []string{"journal", "idem", "feed", "records"} {
		events, err := db.LocalEvents(ctx, kind)
		if err != nil {
			return err
		}
		var out []byte
		for _, b := range events {
			// Rewrite value_id fields only. Local item, record and op identities survive.
			var v any
			if err = json.Unmarshal(b, &v); err != nil {
				return err
			}
			rewriteLocalAliases(v, aliases)
			var done []journalRec
			if kind == "journal" {
				obj, _ := v.(map[string]any)
				ops, _ := obj["ops"].([]any)
				for _, x := range ops {
					op, _ := x.(map[string]any)
					if op["state"] == OpDone {
						id, _ := op["op_id"].(string)
						vid, _ := op["value_id"].(string)
						at, _ := op["done_at"].(string)
						when, _ := time.Parse(time.RFC3339Nano, at)
						done = append(done, journalRec{T: "state", OpID: id, State: OpDone, ValueID: vid, At: when})
						op["state"] = OpPending
						delete(op, "value_id")
						delete(op, "done_at")
					}
				}
			}
			b, err = json.Marshal(v)
			if err != nil {
				return err
			}
			out = append(out, b...)
			out = append(out, '\n')
			for _, r := range done {
				b, err = json.Marshal(r)
				if err != nil {
					return err
				}
				out = append(out, b...)
				out = append(out, '\n')
			}
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		tmp := filepath.Join(dir, kind+".jsonl.local-tmp")
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(out)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(tmp, filepath.Join(dir, kind+".jsonl")); err != nil {
			return err
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
func rewriteLocalAliases(v any, aliases map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			if k == "value_id" {
				if id, ok := y.(string); ok && aliases[id] != "" {
					x[k] = aliases[id]
				}
			} else {
				rewriteLocalAliases(y, aliases)
			}
		}
	case []any:
		for _, y := range x {
			rewriteLocalAliases(y, aliases)
		}
	}
}

// refreshLocal publishes every affected day together. A move must never expose
// its destination alongside a stale source, even on the very first read.
func (c *Cache) refreshLocal(ctx context.Context, date string) error {
	if c.publish != nil {
		c.publish.Lock()
		defer c.publish.Unlock()
	}
	return c.refreshLocalLocked(ctx, date)
}
func (c *Cache) refreshLocalLocked(ctx context.Context, date string) error {
	vals, err := c.vars.All(ctx)
	if err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	days := make(map[string]*dayCache, len(c.days))
	for d := range c.days {
		days[d] = &dayCache{fetched: now, ok: true}
	}
	if date == "" {
		date = now.Format("2006-01-02")
	}
	for i := 0; i < 35; i++ {
		days[dateAdd(date, -i)] = &dayCache{fetched: now, ok: true}
	}
	body := []Value{}
	rec := map[string][]Value{}
	from := now.AddDate(0, 0, -181).Format("2006-01-02")
	for _, v := range vals {
		dc := days[v.RecordDate]
		if dc == nil {
			dc = &dayCache{fetched: now, ok: true}
			days[v.RecordDate] = dc
		}
		if v.VariableID == c.ids.Food {
			dc.rows = append(dc.rows, v)
		}
		if v.Data == nil && v.VariableID != "" {
			dc.nums = append(dc.nums, numVal{VarID: v.VariableID, ID: v.ID, Raw: v.Raw})
			if (v.VariableID == c.ids.PushUps || v.VariableID == c.ids.PullUps) && v.Raw != "" && v.Raw != "0" {
				dc.strength = true
			}
		}
		if v.Data != nil {
			switch {
			case v.VariableID == c.ids.Body:
				body = append(body, v)
			case c.ids.BP != "" && v.VariableID == c.ids.BP && v.RecordDate >= from:
				rec["bp"] = append(rec["bp"], v)
			case c.ids.Symptom != "" && v.VariableID == c.ids.Symptom && v.RecordDate >= from:
				rec["symptom"] = append(rec["symptom"], v)
			}
		}
	}
	changed := []string{}
	for d, dc := range days {
		dc.fp = rowsFingerprint(dc.rows)
		if old := c.days[d]; old != nil && old.ok && old.fp != dc.fp {
			changed = append(changed, d)
		}
		c.gen[d]++
	}
	sort.SliceStable(body, func(i, j int) bool { return body[i].RecordDate < body[j].RecordDate })
	fp := rowsFingerprint(body)
	if !c.bodyAt.IsZero() && c.bodyFP != fp {
		changed = append(changed, "body")
	}
	c.days, c.body, c.rec, c.bodyAt, c.bodyFP = days, body, rec, now, fp
	c.mu.Unlock()
	if c.onChange != nil {
		for _, d := range changed {
			c.onChange(d)
		}
	}
	return nil
}
