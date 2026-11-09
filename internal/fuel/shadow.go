package fuel

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

const shadowQueueSize = 1024
const shadowQueueBytes = 16 << 20

type shadowJob struct {
	kind, key string
	data      []byte // immutable, owned by the queue
	barrier   chan struct{}
}
type shadowStatus struct {
	sqlstore.Status
	Mode       string `json:"mode"`
	Available  bool   `json:"available"`
	ErrorCount uint64 `json:"error_count"` // since this process started
	QueueDepth int    `json:"queue_depth"`
}
type shadowStore struct {
	queue     chan shadowJob
	done      chan struct{}
	cancel    context.CancelFunc
	mu        sync.Mutex // only enqueue/close, never held for I/O
	closed    bool
	bytes     atomic.Int64
	errors    atomic.Uint64
	status    atomic.Pointer[shadowStatus]
	db        *sqlstore.Store // worker-owned; tests access only after a queue barrier
	journal   *Journal        // private replay reducer, never used by service reads
	replaying bool
	seeded    bool
	target    atomic.Value // resolved Variables food ID (string)
	now       func() time.Time
}
type shadowFile struct {
	kind string
	file *os.File
	size int64
}

func (s *Service) initShadow() {
	ctx, cancel := context.WithCancel(context.Background())
	sh := &shadowStore{queue: make(chan shadowJob, shadowQueueSize), done: make(chan struct{}), cancel: cancel, journal: newJournalState(), now: s.o.Now}
	sh.target.Store(s.o.FoodVar)
	s.shadow = sh
	// Pin file descriptors and lengths before requests can append. Startup replay
	// runs on the worker and cannot consume writes already queued by the hooks.
	var files []shadowFile
	for _, kind := range []string{"journal", "idem", "feed", "records"} {
		f, err := os.Open(filepath.Join(s.o.StateDir, kind+".jsonl"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			sh.errors.Add(1)
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			sh.errors.Add(1)
			continue
		}
		files = append(files, shadowFile{kind, f, st.Size()})
	}
	s.journal.shadow = func(offset int64, b []byte) {
		sh.enqueue(shadowJob{kind: "journal", key: sqlstore.JournalEventKey(offset, b), data: b})
	}
	s.idem.shadow = func(b []byte) { sh.enqueue(shadowJob{kind: "idem", data: b}) }
	s.feed.shadow = func(b []byte) { sh.enqueue(shadowJob{kind: "feed", data: b}) }
	s.records.shadow = func(b []byte) { sh.enqueue(shadowJob{kind: "records", data: b}) }
	go sh.run(ctx, s.o.StorePath, files)
}

// Enqueue does no SQLite or logging I/O. Overflow is visible in the error
// counter and is logged by the worker. Legacy JSONL and Variables permit replay.
func (sh *shadowStore) enqueue(job shadowJob) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed || sh.bytes.Load()+int64(len(job.data)) > shadowQueueBytes {
		sh.errors.Add(1)
		return
	}
	sh.bytes.Add(int64(len(job.data)))
	select {
	case sh.queue <- job:
	default:
		sh.bytes.Add(-int64(len(job.data)))
		sh.errors.Add(1)
	}
}
func (sh *shadowStore) submit(kind string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		sh.errors.Add(1)
		return
	}
	sh.enqueue(shadowJob{kind: kind, data: b})
}
func (sh *shadowStore) food(op Op) { sh.submit("food", op) }
func (sh *shadowStore) importRows(vals []Value, foodID, date string) {
	// Sort on the worker, retaining all physical aliases (no op_id dedup here).
	// A full-history read is one ordered job, bounded by the byte budget.
	rows := make([]Value, 0)
	for _, v := range vals {
		if v.VariableID == foodID && v.Data != nil {
			if v.RecordDate == "" {
				v.RecordDate = date
			}
			rows = append(rows, v)
		}
	}
	if date == "" {
		sh.submit("import_full", rows)
	} else if len(rows) > 0 {
		sh.submit("import", rows)
	}
}
func (sh *shadowStore) close() {
	sh.mu.Lock()
	if !sh.closed {
		sh.closed = true
		close(sh.queue)
	}
	sh.mu.Unlock()
	timer := time.AfterFunc(5*time.Second, sh.cancel)
	<-sh.done
	timer.Stop()
	sh.cancel()
}
func (sh *shadowStore) run(ctx context.Context, path string, files []shadowFile) {
	defer close(sh.done)
	defer func() {
		for _, f := range files {
			f.file.Close()
		}
	}()
	var err error
	sh.db, err = sqlstore.Open(path)
	if err != nil {
		sh.errors.Add(1)
		log.Printf("fuel: shadow open failed: %v", err)
	}
	if sh.db != nil {
		defer sh.db.Close()
	}
	sh.replaying = true
	for _, f := range files {
		rd := bufio.NewReader(io.LimitReader(f.file, f.size))
		var offset int64
		for ctx.Err() == nil {
			b, err := rd.ReadBytes('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				sh.errors.Add(1)
				break
			}
			offset += int64(len(b))
			b = b[:len(b)-1]
			if len(b) == 0 {
				continue
			}
			sh.process(ctx, shadowJob{kind: f.kind, key: sqlstore.JournalEventKey(offset, b), data: b})
		}
	}
	sh.replaying = false
	sh.refreshStatus(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var logged uint64
	report := func() {
		if n := sh.errors.Load(); n != logged {
			log.Printf("fuel: shadow errors=%d; replay required before promotion", n)
			logged = n
		}
	}
	defer report()
	for {
		select {
		case <-ctx.Done():
			sh.errors.Add(uint64(len(sh.queue)))
			return
		case job, ok := <-sh.queue:
			if !ok {
				return
			}
			sh.bytes.Add(-int64(len(job.data)))
			if job.barrier != nil {
				sh.refreshStatus(ctx)
				close(job.barrier)
				continue
			}
			sh.process(ctx, job)
			if len(sh.queue) == 0 {
				sh.refreshStatus(ctx)
				report()
			}
		case <-tick.C:
			sh.refreshStatus(ctx)
			report()
		}
	}
}
func (sh *shadowStore) process(parent context.Context, job shadowJob) {
	if sh.db == nil {
		sh.errors.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	writeCtx := ctx
	if job.kind == "import" || job.kind == "import_full" {
		writeCtx = parent
	} // each row gets its own deadline
	if err := sh.write(writeCtx, job); err != nil {
		sh.errors.Add(1)
		log.Printf("fuel: shadow %s failed: %v", job.kind, err)
		return
	}
	noteCtx, noteCancel := context.WithTimeout(parent, time.Second)
	defer noteCancel()
	if err := sh.db.NoteShadowWrite(noteCtx, sh.now()); err != nil {
		sh.errors.Add(1)
	}
}
func (sh *shadowStore) refreshStatus(parent context.Context) {
	st := shadowStatus{Mode: "shadow", Status: sqlstore.Status{Counts: map[string]int64{}}}
	if sh.db != nil {
		ctx, cancel := context.WithTimeout(parent, time.Second)
		status, err := sh.db.Status(ctx)
		cancel()
		if err == nil {
			st.Status = status
			st.Available = true
		} else {
			sh.errors.Add(1)
			if old := sh.status.Load(); old != nil {
				st.Status = old.Status
			}
		}
	}
	sh.status.Store(&st)
}
func (sh *shadowStore) write(ctx context.Context, job shadowJob) error {
	switch job.kind {
	case "journal":
		var r journalRec
		if err := json.Unmarshal(job.data, &r); err != nil {
			return err
		}
		sh.journal.apply(r)
		if err := sh.db.MirrorLegacy(ctx, "journal", job.key, job.data); err != nil {
			return err
		}
		ids := []string{}
		for _, op := range r.Ops {
			ids = append(ids, op.ID)
		}
		if r.OpID != "" {
			ids = append(ids, r.OpID)
		}
		for _, id := range ids {
			op, ok := sh.journal.Op(id)
			if !ok {
				continue
			}
			b, err := json.Marshal(op)
			if err != nil {
				return err
			}
			if err = sh.db.MirrorLegacy(ctx, "operation", "", b); err != nil {
				return err
			}
			if op.State == OpFailed || (!sh.replaying && (op.State == OpDone || op.State == OpUncertain)) {
				if err = sh.writeFood(ctx, op); err != nil {
					return err
				}
			}
		}
		return nil
	case "food":
		var op Op
		if err := json.Unmarshal(job.data, &op); err != nil {
			return err
		}
		return sh.writeFood(ctx, op)
	case "import", "import_full":
		var vals []Value
		if err := json.Unmarshal(job.data, &vals); err != nil {
			return err
		}
		if job.kind == "import_full" && !sh.seeded {
			sh.seeded = true
			return sh.seedFood(ctx, vals)
		}
		sort.SliceStable(vals, func(i, j int) bool {
			if !vals[i].CreatedAt.Equal(vals[j].CreatedAt) {
				return vals[i].CreatedAt.Before(vals[j].CreatedAt)
			}
			return vals[i].ID < vals[j].ID
		})
		for _, v := range vals {
			if err := ctx.Err(); err != nil {
				return err
			}
			sh.importValue(ctx, v)
		}
		return nil
	case "idem":
		var r sqlstore.IdemRecord
		if err := json.Unmarshal(job.data, &r); err != nil {
			return err
		}
		r.ExpiresAt = r.At.Add(idemRetention)
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return sh.db.MirrorLegacy(ctx, "idem", "", b)
	default:
		return sh.db.MirrorLegacy(ctx, job.kind, job.key, job.data)
	}
}

// seedFood interleaves legacy own rows and foreign rows in reducer order.
// Journal replay builds operation state first, without allocating food sequences.
// Existing SQLite identities keep their sequence on subsequent starts.
func (sh *shadowStore) seedFood(ctx context.Context, vals []Value) error {
	byValue, byWire := map[string]Op{}, map[string]Op{}
	ops := sh.journal.Ops()
	sort.Slice(ops, func(i, j int) bool {
		if !ops[i].CreatedAt.Equal(ops[j].CreatedAt) {
			return ops[i].CreatedAt.Before(ops[j].CreatedAt)
		}
		return ops[i].ID < ops[j].ID
	})
	for _, op := range ops {
		if op.State != OpDone && op.State != OpUncertain {
			continue
		}
		if old, ok := byWire[op.WireID()]; !ok || (old.ValueID == "" && op.ValueID != "") {
			byWire[op.WireID()] = op
		}
		if op.ValueID != "" {
			byValue[op.ValueID] = op
		}
	}
	type seed struct {
		value Value
		own   *Op
	}
	batch := make([]seed, 0, len(vals)+len(byWire))
	seen := map[string]bool{}
	for _, v := range vals {
		op, own := byValue[v.ID]
		if !own {
			wire, _ := v.Data["op_id"].(string)
			op, own = byWire[wire]
		}
		row := seed{value: v}
		if own && !seen[op.ID] && (op.ValueID == "" || op.ValueID == v.ID) {
			seen[op.ID] = true
			op.ValueID, op.DoneAt = v.ID, v.CreatedAt
			row.own = &op
		}
		batch = append(batch, row)
	}
	for _, op := range byWire {
		if seen[op.ID] {
			continue
		}
		at := op.DoneAt
		if at.IsZero() {
			at = op.CreatedAt
		}
		cp := op
		batch = append(batch, seed{value: Value{ID: op.ValueID, CreatedAt: at}, own: &cp})
	}
	sort.SliceStable(batch, func(i, j int) bool {
		a, b := batch[i], batch[j]
		if !a.value.CreatedAt.Equal(b.value.CreatedAt) {
			return a.value.CreatedAt.Before(b.value.CreatedAt)
		}
		aid, bid := a.value.ID, b.value.ID
		if aid == "" {
			aid = a.own.ID
		}
		if bid == "" {
			bid = b.own.ID
		}
		return aid < bid
	})
	for _, row := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.own == nil {
			sh.importValue(ctx, row.value)
			continue
		}
		rowCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := sh.writeFood(rowCtx, *row.own)
		cancel()
		if err != nil {
			sh.errors.Add(1)
			log.Printf("fuel: shadow history failed: %v", err)
		} else if row.value.Data != nil {
			// Refresh authoritative timestamps even for an already-bound row
			// whose earlier POST acknowledgement used the local clock.
			sh.importValue(ctx, row.value)
		}
	}
	return nil
}

func (sh *shadowStore) importValue(ctx context.Context, v Value) {
	data := json.RawMessage(v.Raw)
	if !json.Valid(data) {
		var err error
		data, err = json.Marshal(v.Data)
		if err != nil {
			sh.errors.Add(1)
			return
		}
	}
	opID, _ := v.Data["op_id"].(string)
	rowCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, err := sh.db.UpsertImportedRow(rowCtx, sqlstore.Row{ID: "variables:" + v.ID, Source: sqlstore.SourceVariablesImport, VariablesValueID: v.ID, OpID: opID, RecordDate: v.RecordDate, CreatedAt: v.CreatedAt, Data: data})
	cancel()
	if err != nil {
		sh.errors.Add(1)
		log.Printf("fuel: shadow import failed: %v", err)
	}
}

func (sh *shadowStore) writeFood(ctx context.Context, op Op) error {
	if op.State == OpFailed {
		return sh.db.RemoveUnconfirmedShadowRow(ctx, op.WireID())
	}
	at := op.DoneAt
	if at.IsZero() {
		at = op.CreatedAt
	}
	data, err := json.Marshal(withTitle(op.Data))
	if err != nil {
		return err
	}
	return sh.db.PutShadowRow(ctx, sqlstore.Row{ID: op.ID, Source: sqlstore.SourceFueld, VariablesValueID: op.ValueID, OpID: op.WireID(), RecordDate: op.Date, CreatedAt: at, Data: data}, sh.target.Load().(string))
}
func (s *Service) handleStoreStatus(w http.ResponseWriter, r *http.Request) {
	if s.local != nil {
		st, err := s.local.db.LocalStatus(r.Context(), s.o.Now())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"mode": "local", "available": false})
			return
		}
		writeJSON(w, http.StatusOK, struct {
			sqlstore.LocalStatus
			Mode            string `json:"mode"`
			Available       bool   `json:"available"`
			ExportVariables bool   `json:"export_variables"`
		}{st, "local", true, s.local.export})
		return
	}
	st := shadowStatus{Mode: s.o.StoreMode, Status: sqlstore.Status{Counts: map[string]int64{}}}
	if sh := s.shadow; sh != nil {
		if cached := sh.status.Load(); cached != nil {
			st = *cached
		}
		st.ErrorCount = sh.errors.Load()
		st.QueueDepth = len(sh.queue)
	}
	writeJSON(w, http.StatusOK, st)
}
