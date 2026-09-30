// Package fuel is the Fuel fast path: one structured model call per food log,
// rows written straight to the Variables "Food log", and a deterministic
// snapshot of the day against targets. Contract:
// docs/specs/2026-10-fuel-api.md (section 14, the v3 addendum, wins).
package fuel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/media"
)

// Options configures a Service. Durations left zero take the spec defaults;
// tests shorten them.
type Options struct {
	Token       string
	Vars        Variables
	FoodVar     string
	BodyVar     string
	Model       Model
	ASR         media.Transcriber
	TargetsFile string
	StaplesFile string
	StateDir    string
	StravaDir   string
	TestMode    bool
	Now         func() time.Time

	RecheckAfter   time.Duration // re-read an uncertain POST after (60 s)
	FailAfter      time.Duration // an op not done after this is failed (24 h)
	ReconcileEvery time.Duration // background reconcile tick (60 s)
	Budget         time.Duration // whole request after the body is read (30 s)
	ModelTimeout   time.Duration // 20 s
	ASRTimeout     time.Duration // 15 s
	VarTimeout     time.Duration // each Variables call (5 s)
	BusyWait       time.Duration // wait for a log slot (10 s)
	ReadDeadline   time.Duration // upload read deadline (30 s)
}

// Service is the fast path. Construct with New, then Start, then Handler.
type Service struct {
	o       Options
	journal *Journal
	idem    *Idem
	feed    *Feed
	photos  *photoStore
	strava  *stravaIndex

	mu      sync.Mutex
	ids     VarIDs
	ready   bool
	cache   *Cache
	staples []Staple

	targetsMu   sync.Mutex
	targets     *Targets
	targetsErr  error
	targetsMod  time.Time
	targetsSeen bool

	itemLocks   sync.Map // item id -> *sync.Mutex
	reconcileMu sync.Mutex
	// stateMu makes a rendered response consistent: op state changes that
	// move status, rows and revision (done, failed) take it exclusively; a
	// render (status + items + snapshot) holds it shared.
	stateMu   sync.RWMutex
	coachMu   sync.Mutex
	lcMu      sync.Mutex
	closed    bool
	stop      context.CancelFunc
	workers   sync.WaitGroup
	lctx      context.Context // the lifecycle context (cancelled by Stop)
	coachSeen map[string]bool
	inflight  sync.Map // op id -> struct{}

	reqMu    sync.Mutex
	inflReq  map[string]*inflightReq // client id -> in-progress request
	logTimes []time.Time             // accepted logs, for the hourly / daily caps

	slots chan struct{} // at most 2 logs processed at once
	fails authFailures
	kick  chan struct{}
}

type inflightReq struct {
	hash string
	kind string
	done chan struct{}
}

// Validate checks the static configuration (spec 1 and 2).
func (o Options) Validate() error {
	switch {
	case o.Token == "":
		return errors.New("fuel: token resolves to empty")
	case o.Vars == nil:
		return errors.New("fuel: no Variables client")
	case o.Model == nil:
		return errors.New("fuel: no model")
	case o.FoodVar == "" || o.BodyVar == "":
		return errors.New("fuel: food_log_var and body_var are required")
	case o.StateDir == "":
		return errors.New("fuel: state_dir is empty")
	}
	if o.TestMode && strings.EqualFold(strings.TrimSpace(o.FoodVar), "Food log") {
		return errors.New(`fuel: test_mode refuses food_log_var "Food log" (the production log)`)
	}
	return nil
}

func (o *Options) defaults() {
	d := func(p *time.Duration, v time.Duration) {
		if *p == 0 {
			*p = v
		}
	}
	d(&o.RecheckAfter, 60*time.Second)
	d(&o.FailAfter, 24*time.Hour)
	d(&o.ReconcileEvery, 60*time.Second)
	d(&o.Budget, 30*time.Second)
	d(&o.ModelTimeout, 20*time.Second)
	d(&o.ASRTimeout, 15*time.Second)
	d(&o.VarTimeout, 5*time.Second)
	d(&o.BusyWait, 10*time.Second)
	d(&o.ReadDeadline, 30*time.Second)
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
}

// New validates the options and opens the stores (journal replay, idem
// compaction, feed, photos). It does no network I/O.
func New(o Options) (*Service, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	o.defaults()
	o.StateDir = expandHome(o.StateDir)
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	j, err := OpenJournal(filepath.Join(o.StateDir, "journal.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("fuel: journal: %w", err)
	}
	idem, err := OpenIdem(filepath.Join(o.StateDir, "idem.jsonl"), o.Now())
	if err != nil {
		return nil, fmt.Errorf("fuel: idem: %w", err)
	}
	feed, err := OpenFeed(filepath.Join(o.StateDir, "feed.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("fuel: feed: %w", err)
	}
	ph, err := openPhotoStore(filepath.Join(o.StateDir, "photos"), o.Now)
	if err != nil {
		return nil, fmt.Errorf("fuel: photos: %w", err)
	}
	s := &Service{
		o: o, journal: j, idem: idem, feed: feed, photos: ph,
		strava:  newStravaIndex(o.StravaDir),
		inflReq: map[string]*inflightReq{},
		slots:   make(chan struct{}, 2),
		kick:    make(chan struct{}, 1),
	}
	s.coachSeen = map[string]bool{}
	_ = loadLines(filepath.Join(o.StateDir, "coach-events.jsonl"), false, func(b []byte) error {
		var ev struct {
			EntryID string `json:"entry_id"`
		}
		if err := json.Unmarshal(b, &ev); err != nil {
			return err
		}
		s.coachSeen[ev.EntryID] = true
		return nil
	})
	s.loadTargets()
	if o.StaplesFile != "" {
		st, err := LoadStaples(o.StaplesFile)
		if err != nil {
			log.Printf("fuel: staples file %s invalid, staples ignored: %v", o.StaplesFile, err)
		} else {
			s.staples = st
			log.Printf("fuel: %d staple(s) loaded", len(st))
		}
	}
	return s, nil
}

// materializeFeed adds feed lines a crash lost between the journal txn and
// the feed append: an entry without its user line, an undo / fraction
// without its reply line, a failed op without its notice. Idempotent.
func (s *Service) materializeFeed(ctx context.Context, minAge time.Duration) {
	now := s.o.Now()
	for _, e := range s.journal.Entries() {
		if now.Sub(e.CreatedAt) < minAge {
			continue // its request may still be writing its own lines
		}
		needFeed := !s.feed.HasKey("u:"+e.ID) || !s.feed.HasKey("r:"+e.ID)
		s.coachMu.Lock()
		needCoach := e.Intent == "log" && len(e.ItemIDs) > 0 && !s.coachSeen[e.ID]
		s.coachMu.Unlock()
		needFinal := s.needsFinal(e.ClientID, e.CreatedAt, now)
		if !needFeed && !needCoach && !needFinal {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if !s.historyReady(ctx, e.Date) {
			continue // a read failed: retried on the next reconcile pass
		}
		resp, status := s.buildLogResponse(e)
		if resp.renderErr != nil {
			continue
		}
		if needFeed {
			s.appendEntryFeed(e, resp.Blocks)
		}
		if needCoach && status == StatusDone {
			s.coachEvent(e, resp.Items, resp.Snapshot)
		}
		if needFinal && status == StatusDone {
			resp.LatencyMs = map[string]int{"upload": 0, "asr": 0, "model": 0, "write": 0, "total": 0}
			s.persistFinal(idemRec{ClientID: e.ClientID, Hash: e.ReqHash, Kind: "log", At: e.CreatedAt, EntryID: e.ID, ItemIDs: e.ItemIDs}, resp)
		}
	}
	for _, op := range s.journal.Ops() {
		if op.State == OpFailed {
			s.failureNotice(op, now)
		}
		if now.Sub(op.CreatedAt) < minAge {
			continue
		}
		if op.ClientID != "" {
			s.recoverMutation(ctx, op.ClientID, op.ReqHash, op.Reason, op.EntryID, op.ItemID, op.Date, op.CreatedAt, now)
		}
	}
	for _, fc := range s.journal.Fractions() {
		it, _ := s.journal.Item(fc.ItemID)
		if now.Sub(fc.At) < minAge {
			continue
		}
		if fc.ClientID != "" {
			s.recoverMutation(ctx, fc.ClientID, fc.Hash, "fraction", it.EntryID, fc.ItemID, it.Date, fc.At, now)
		}
	}
}

// loadTargets (re)reads the targets file when its mtime changed. Missing:
// defaults, logged once. Invalid: routes answer 503 targets_invalid.
func (s *Service) loadTargets() (*Targets, error) {
	s.targetsMu.Lock()
	defer s.targetsMu.Unlock()
	var mod time.Time
	exists := false
	if s.o.TargetsFile != "" {
		if fi, err := os.Stat(expandHome(s.o.TargetsFile)); err == nil {
			mod, exists = fi.ModTime(), true
		}
	}
	if s.targetsSeen && mod.Equal(s.targetsMod) && (exists || (s.targets != nil && s.targets.isDefaults)) {
		return s.targets, s.targetsErr
	}
	path := s.o.TargetsFile
	if path == "" {
		path = filepath.Join(s.o.StateDir, "no-targets-file")
	}
	t, isDefault, err := LoadTargets(path)
	s.targetsSeen, s.targetsMod = true, mod
	s.targets, s.targetsErr = t, err
	switch {
	case err != nil:
		log.Printf("fuel: targets file invalid, routes answer 503 targets_invalid: %v", err)
	case isDefault:
		log.Printf("fuel: targets file %s missing, using built-in defaults", s.o.TargetsFile)
	}
	return t, err
}

// Close stops the loop and releases the stores.
func (s *Service) Close() {
	s.Stop()
	_ = s.journal.Close()
	_ = s.idem.Close()
	_ = s.feed.Close()
}

// Resolve maps the variable names to ids (definitive errors wrap errConfig).
func (s *Service) Resolve(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ids, err := ResolveVars(rctx, s.o.Vars, s.o.FoodVar, s.o.BodyVar)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ids = ids
	s.cache = newCache(s.o.Vars, ids, s.journal, s.o.Now)
	s.cache.readTO = s.o.VarTimeout
	s.cache.publish = &s.stateMu
	s.mu.Unlock()
	return nil
}

// IsConfigError reports a definitive misconfiguration from Resolve.
func IsConfigError(err error) bool { return errors.Is(err, errConfig) }

// Start hydrates the cache (35 days of Food log, the Body composition
// history), resumes every non-terminal op and starts the background loops.
// Resolve must have succeeded.
func (s *Service) Start(ctx context.Context) {
	// Register BEFORE any work, so Stop cancels and waits for startup too.
	lctx, cancel := context.WithCancel(ctx)
	s.lcMu.Lock()
	if s.closed {
		s.lcMu.Unlock()
		cancel()
		return
	}
	s.stop = cancel
	s.lctx = lctx
	s.workers.Add(1)
	s.lcMu.Unlock()
	defer s.workers.Done()

	s.hydrate(lctx)
	if lctx.Err() != nil {
		return
	}
	// Ops still pending from before a restart may or may not have reached
	// Variables: they become uncertain and are checked by op_id.
	for _, op := range s.journal.NonTerminal() {
		if op.State == OpPending || op.State == OpRetry {
			_ = s.journal.Append(journalRec{T: "state", OpID: op.ID, State: OpUncertain, At: op.LastTryOr()})
		}
	}
	s.materializeFeed(lctx, 0)
	if lctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		s.reconcileOnce(lctx)
		s.loop(lctx)
	}()
}

// Stop cancels startup and the background loop and waits for both; a later
// Start is a no-op.
func (s *Service) Stop() {
	s.lcMu.Lock()
	s.closed = true
	stop := s.stop
	s.stop = nil
	s.lcMu.Unlock()
	if stop != nil {
		stop()
	}
	s.workers.Wait()
}

// LastTryOr is the last attempt time, else the creation time.
func (op Op) LastTryOr() time.Time {
	if !op.LastTry.IsZero() {
		return op.LastTry
	}
	return op.CreatedAt
}

func (s *Service) today() string {
	t, _ := s.loadTargets()
	loc := time.UTC
	if t != nil {
		loc = t.loc
	}
	return s.o.Now().In(loc).Format("2006-01-02")
}

func (s *Service) hydrate(ctx context.Context) {
	today := s.today()
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := 0; i < 35; i++ {
		d := dateAdd(today, -i)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.cache.RefreshDay(ctx, d); err != nil {
				log.Printf("fuel: hydrate %s: %v", d, err)
			}
		}()
	}
	wg.Wait()
	if err := s.cache.RefreshBody(ctx); err != nil {
		log.Printf("fuel: hydrate body composition: %v", err)
	}
	log.Printf("fuel: cache hydrated (35 days of food log, body composition)")
}

func (s *Service) loop(ctx context.Context) {
	day := time.NewTicker(2 * time.Minute)
	body := time.NewTicker(20 * time.Minute)
	rec := time.NewTicker(s.o.ReconcileEvery)
	prune := time.NewTicker(time.Hour)
	defer day.Stop()
	defer body.Stop()
	defer rec.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-day.C:
			today := s.today()
			for _, d := range []string{today, dateAdd(today, -1)} {
				if err := s.cache.RefreshDay(ctx, d); err != nil {
					log.Printf("fuel: refresh %s: %v", d, err)
				}
			}
		case <-body.C:
			if err := s.cache.RefreshBody(ctx); err != nil {
				log.Printf("fuel: refresh body composition: %v", err)
			}
		case <-rec.C:
			s.reconcileOnce(ctx)
		case <-s.kick:
			s.reconcileOnce(ctx)
		case <-prune.C:
			if n := s.photos.prune(30 * 24 * time.Hour); n > 0 {
				log.Printf("fuel: pruned %d photo(s) older than 30 days", n)
			}
		}
	}
}

func (s *Service) kickReconcile() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Service) itemLock(id string) *itemMutex {
	m, _ := s.itemLocks.LoadOrStore(id, &itemMutex{ch: make(chan struct{}, 1)})
	return m.(*itemMutex)
}

// itemMutex serializes the mutations of one item; request handlers acquire
// it within their deadline (LockCtx), background recovery waits (Lock).
type itemMutex struct{ ch chan struct{} }

func (m *itemMutex) Lock()   { m.ch <- struct{}{} }
func (m *itemMutex) Unlock() { <-m.ch }
func (m *itemMutex) LockCtx(ctx context.Context) bool {
	select {
	case m.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---- write engine ----

// rowData builds the Variables data object of an op's row (spec 14 row
// schemas). Null optional keys are omitted; only fiber_g may be null.
func originalRowData(it Item, opID string, photoRef string) map[string]any {
	d := map[string]any{
		"item":          it.Name,
		"portion_basis": it.Basis,
		"source":        "fuel",
		"op_id":         opID,
		"entry_id":      it.EntryID,
		"item_id":       it.ID,
		"eaten_at":      it.EatenAt.Format(time.RFC3339),
	}
	if it.PortionG != nil {
		d["portion_g"] = round1(*it.PortionG)
	}
	if photoRef != "" {
		d["photo_ref"] = photoRef
	}
	it.Orig.putInto(d)
	return d
}

func correctionRowData(it Item, m Macros, opID, rowItemID, reason string) map[string]any {
	d := map[string]any{
		"item":     "correction: " + it.Name,
		"source":   "fuel",
		"op_id":    opID,
		"entry_id": it.EntryID,
		"item_id":  rowItemID,
		"corrects": it.ID,
		"reason":   reason,
		"eaten_at": it.EatenAt.Format(time.RFC3339),
	}
	m.putInto(d)
	return d
}

// postOp performs one POST attempt of a journaled op and journals the
// outcome: done on 201, failed on a definitive rejection, uncertain otherwise.
// An uncertain op is NEVER blindly re-posted: the reconciler reads first.
// The attempt (count and start time) is journaled BEFORE the request unless
// the caller's txn already carried it (preJournaled), so a crash mid-POST can
// neither exceed three attempts nor shorten their spacing.
func (s *Service) postOp(ctx context.Context, op Op, preJournaled bool) string {
	s.inflight.Store(op.ID, struct{}{})
	defer s.inflight.Delete(op.ID)
	attempts := op.Attempts
	if !preJournaled {
		attempts++
		if err := s.journal.Append(journalRec{T: "attempt", OpID: op.ID, Attempts: attempts, At: s.o.Now()}); err != nil {
			log.Printf("fuel: journal attempt %s: %v; not posting", op.ID, err)
			return op.State
		}
	}
	pctx, cancel := context.WithTimeout(ctx, s.o.VarTimeout)
	vid, err := s.o.Vars.Post(pctx, s.ids.Food, op.Data, op.Date)
	cancel()
	now := s.o.Now()
	var pe *PostError
	switch {
	case err == nil:
		s.markDone(op.ID, vid, attempts, now)
		return OpDone
	case errors.As(err, &pe) && op.Reason == "compensation":
		// The obligation stays open: retried with the same op_id.
		log.Printf("fuel: compensation %s rejected by Variables (HTTP %d); will retry", op.ID, pe.Status)
		_ = s.journal.Append(journalRec{T: "state", OpID: op.ID, State: OpUncertain, Attempts: attempts, At: now})
		return OpUncertain
	case errors.As(err, &pe):
		log.Printf("fuel: op %s rejected by Variables (HTTP %d); entry %s fails", op.ID, pe.Status, op.EntryID)
		s.failOp(op, now)
		return OpFailed
	default:
		log.Printf("fuel: op %s uncertain after attempt %d (%s)", op.ID, attempts, errClass(err))
		_ = s.journal.Append(journalRec{T: "state", OpID: op.ID, State: OpUncertain, Attempts: attempts, At: now})
		return OpUncertain
	}
}

// markDone journals an op as done and merges its row into the cache in one
// step under the cache lock, so a snapshot never sees the new revision with
// the old rows (or the reverse).
func (s *Service) markDone(opID, valueID string, attempts int, now time.Time) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.cache.commit(func() {
		if err := s.journal.Append(journalRec{T: "state", OpID: opID, State: OpDone, ValueID: valueID, Attempts: attempts, At: now}); err != nil {
			log.Printf("fuel: journal done %s: %v", opID, err)
		}
		// Merge whenever the journal APPLIED the record (also when its fsync
		// failed): the row IS in Variables, and revision and rows must move
		// together. A restart without the record re-finds it by op_id.
		if done, ok := s.journal.Op(opID); ok && done.State == OpDone {
			s.cache.mergeLocked(done)
		}
	})
}

// failOp marks an op failed (which fails its entry, derived on replay too)
// and tells the feed.
func (s *Service) failOp(op Op, now time.Time) {
	s.stateMu.Lock()
	err := s.journal.Append(journalRec{T: "state", OpID: op.ID, State: OpFailed, At: now})
	s.stateMu.Unlock()
	if err != nil {
		log.Printf("fuel: journal failed %s: %v", op.ID, err)
		return
	}
	s.failureNotice(op, now)
	s.kickReconcile()
}

// failureNotice appends the feed line for a failed op once (keyed by op id,
// so the startup replay can add a line a crash lost).
func (s *Service) failureNotice(op Op, now time.Time) {
	if s.feed.HasNotice(op.ID) {
		return
	}
	it, _ := s.journal.Item(op.ItemID)
	msg := "could not save " + it.Name
	if op.Kind == "correction" {
		msg = "could not save the " + op.Reason + " of " + it.Name
	}
	eid := op.EntryID
	_, _ = s.feed.Append(FeedItem{At: now, Role: "fuel", Text: &msg, EntryID: &eid, NoticeOp: op.ID})
}

// reconcileOnce finishes or compensates every non-terminal op by op_id, then
// writes compensations for failed entries (spec 6 and 14 [C1] [C2]).
func (s *Service) reconcileOnce(ctx context.Context) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.mu.Lock()
	ready := s.cache != nil
	s.mu.Unlock()
	if !ready {
		return
	}
	now := s.o.Now()
	readDays := map[string]bool{}
	for _, op := range s.journal.NonTerminal() {
		if _, busy := s.inflight.Load(op.ID); busy {
			continue
		}
		if now.Sub(op.LastTryOr()) < s.o.RecheckAfter {
			continue
		}
		l := s.itemLock(op.ItemID)
		if !l.LockCtx(ctx) {
			return
		}
		s.reconcileOp(ctx, op.ID, now, readDays)
		l.Unlock()
	}
	s.refreshFailedDays(ctx, now, readDays)
	s.compensate(ctx, readDays)
	s.coachCatchUp(ctx, now)
	s.materializeFeed(ctx, 2*s.o.Budget)
}

// coachCatchUp emits the coach event of every recent log entry that became
// done after its response (spec 11: one line per log, with real macros).
func (s *Service) coachCatchUp(ctx context.Context, now time.Time) {
	for _, e := range s.journal.Entries() {
		if e.Intent != "log" || len(e.ItemIDs) == 0 || now.Sub(e.CreatedAt) > idemRetention {
			continue
		}
		s.coachMu.Lock()
		seen := s.coachSeen[e.ID]
		s.coachMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if seen || s.entryStatus(e) != StatusDone || !s.historyReady(ctx, e.Date) {
			continue
		}
		// The status that counts is the one captured with the render.
		if resp, status := s.buildLogResponse(e); resp.renderErr == nil && status == StatusDone {
			s.coachEvent(e, resp.Items, resp.Snapshot)
		}
	}
}

// refreshFailedDays re-reads the dates of failed entries (last 35 days) so a
// late row of a failed op is seen and compensated even when that date left
// the today/yesterday refresh window.
func (s *Service) refreshFailedDays(ctx context.Context, now time.Time, readDays map[string]bool) {
	for _, e := range s.journal.FailedEntries() {
		// Recovery obligations have no age limit; old ones are re-read less
		// often (every 6 h past 35 days, else every 10 min).
		every := 10 * time.Minute
		if now.Sub(e.CreatedAt) > 35*24*time.Hour {
			every = 6 * time.Hour
		}
		if readDays[e.Date] || s.cache.Age(e.Date) < every {
			continue
		}
		if err := s.cache.RefreshDay(ctx, e.Date); err == nil {
			readDays[e.Date] = true
		}
	}
}

func (s *Service) reconcileOp(ctx context.Context, opID string, now time.Time, readDays map[string]bool) {
	op, ok := s.journal.Op(opID)
	if !ok || terminal(op.State) {
		return
	}
	if !readDays[op.Date] {
		if err := s.cache.RefreshDay(ctx, op.Date); err != nil {
			log.Printf("fuel: reconcile read %s: %s", op.Date, errClass(err))
			// The 24 h deadline does not depend on reads working; the op's
			// identity stays in the journal, and a late row is compensated.
			if op.Reason != "compensation" && now.Sub(op.CreatedAt) >= s.o.FailAfter {
				s.failOp(op, now)
			}
			return
		}
		readDays[op.Date] = true
	}
	rows, _, _ := s.cache.Rows(op.Date)
	for _, r := range dedupeRows(rows) {
		if id, _ := r.Data["op_id"].(string); id == op.ID {
			s.markDone(op.ID, r.ID, op.Attempts, now)
			log.Printf("fuel: op %s reconciled: found as value %s", op.ID, r.ID)
			return
		}
	}
	if op.Reason == "compensation" {
		// A compensation keeps ONE identity forever: it is re-posted with
		// the same op_id (duplicates count once) and never fails, so a late
		// row can never add to a replacement. Backoff: 60 s for the first
		// three attempts, then hourly.
		if op.Attempts >= 3 && now.Sub(op.LastTryOr()) < time.Hour {
			return
		}
	} else {
		if now.Sub(op.CreatedAt) >= s.o.FailAfter {
			log.Printf("fuel: op %s not done after %s; failed", op.ID, s.o.FailAfter)
			s.failOp(op, now)
			return
		}
		if op.Attempts >= 3 {
			return // wait for a late write to show up, until FailAfter
		}
	}
	_ = s.journal.Append(journalRec{T: "state", OpID: op.ID, State: OpRetry, At: now})
	op, _ = s.journal.Op(op.ID)
	// Same op_id: if the earlier attempt lands too, the duplicate is counted
	// once (spec 14 [C1]).
	s.postOp(context.WithoutCancel(ctx), op, false)
}

// compensate cancels what a failed entry left in Variables. Per item, once
// no op of it is outstanding, it reads the item's AUTHORITATIVE rows (a fresh
// read: external edits and deletions win, late rows are included) and writes
// one compensation for the residual. A compensation is never replaced (see
// reconcileOp), so the residual converges to zero without double counting.
func (s *Service) compensate(ctx context.Context, readDays map[string]bool) {
	now := s.o.Now()
	for _, e := range s.journal.FailedEntries() {
		if !s.cache.Loaded(e.Date) {
			continue // no view of the day yet: refreshFailedDays reads it
		}
		for _, itemID := range e.ItemIDs {
			l := s.itemLock(itemID)
			if !l.LockCtx(ctx) {
				return
			}
			outstanding := false
			for _, op := range s.journal.ItemOps(itemID) {
				outstanding = outstanding || !terminal(op.State)
			}
			residual := func() (Macros, bool) {
				rows, _, _ := s.cache.Rows(e.Date)
				c, _, found := itemContrib(rows, itemID)
				return c.cancel(), found && c.residual()
			}
			res, need := residual()
			if !outstanding && need && !readDays[e.Date] && s.cache.Age(e.Date) >= 60*time.Second {
				// About to write: re-read so the amount is authoritative.
				if err := s.cache.RefreshDay(ctx, e.Date); err != nil {
					l.Unlock()
					continue
				}
				readDays[e.Date] = true
				res, need = residual()
			}
			if outstanding || !need {
				l.Unlock()
				continue
			}
			it, _ := s.journal.Item(itemID)
			c := s.newCorrectionOp(it, requiredKnown(res), "compensation", nil)
			c.Attempts, c.LastTry = 1, now
			err := s.journal.Append(journalRec{T: "txn", Ops: []Op{c}})
			l.Unlock()
			if err == nil {
				s.postOp(context.WithoutCancel(ctx), c, true)
			}
		}
	}
}

// rowPresent reports whether a row with the op's op_id is in the cache.
func (s *Service) rowPresent(op Op) bool {
	rows, _, _ := s.cache.Rows(op.Date)
	for _, r := range rows {
		if id, _ := r.Data["op_id"].(string); id == op.ID {
			return true
		}
	}
	return false
}

func nonZero(m Macros) bool {
	for _, k := range MacroKeys {
		if v := m.Get(k); v.OK && v.V != 0 {
			return true
		}
	}
	return false
}

// errClass names an upstream error without its body: logs never carry
// upstream response text (it can echo secrets or model output).
func errClass(err error) string {
	var pe *PostError
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pe):
		return fmt.Sprintf("HTTP %d", pe.Status)
	}
	var se *statusError
	if errors.As(err, &se) {
		return fmt.Sprintf("HTTP %d", se.Status)
	}
	return "transport error"
}

// doneContribution sums the done ops of an item; any reports whether some
// known field is non-zero (else nothing needs compensating).
func doneContribution(ops []Op) (Macros, bool) {
	var sum Macros
	init := false
	for _, op := range ops {
		if op.State != OpDone {
			continue
		}
		if !init {
			sum, init = op.Macros, true
		} else {
			sum = sum.Add(op.Macros)
		}
	}
	if !init {
		return Macros{}, false
	}
	for _, k := range MacroKeys {
		if v := sum.Get(k); v.OK && v.V != 0 {
			return sum, true
		}
	}
	return sum, false
}

func (s *Service) newCorrectionOp(it Item, m Macros, reason string, fraction *float64) Op {
	opID := newID("op_")
	rowItem := newID("it_")
	return Op{
		ID: opID, Kind: "correction", EntryID: it.EntryID, ItemID: it.ID, RowItemID: rowItem,
		Reason: reason, Fraction: fraction, Date: it.Date, Macros: m,
		Data:      correctionRowData(it, m, opID, rowItem, reason),
		CreatedAt: s.o.Now(), State: OpPending,
	}
}

// ---- item state ----

// ItemState is the wire shape of one item (spec section 10).
type ItemState struct {
	ItemID       string   `json:"item_id"`
	ValueID      *string  `json:"value_id"`
	Item         string   `json:"item"`
	PortionG     *float64 `json:"portion_g"`
	PortionBasis string   `json:"portion_basis"`
	Macros
	NeedsFraction bool     `json:"needs_fraction"`
	Fraction      *float64 `json:"fraction"`
	Undone        bool     `json:"undone"`
	Effective     Macros   `json:"effective"`
	Actions       []string `json:"actions"`
}

type itemView struct {
	state    ItemState
	pending  bool // a non-terminal op exists
	failed   bool // the entry failed
	origDone bool
}

func (s *Service) viewItem(it Item) itemView {
	ops := s.journal.ItemOps(it.ID)
	e, _ := s.journal.Entry(it.EntryID)
	v := itemView{failed: e.Failed}
	st := ItemState{ItemID: it.ID, Item: it.Name, PortionG: it.PortionG, PortionBasis: it.Basis,
		Macros: it.Orig, NeedsFraction: it.NeedsFraction, Actions: []string{}}
	var eff Macros
	init := false
	for _, op := range ops {
		if !terminal(op.State) {
			v.pending = true
		}
		if op.Kind == "original" {
			if op.State == OpDone {
				vid := op.ValueID
				st.ValueID = &vid
				v.origDone = true
			}
		}
		if op.Kind == "correction" && op.State != OpFailed && (op.Reason == "undo" || op.Reason == "compensation") {
			st.Undone = true
		}
		if op.State == OpFailed {
			continue
		}
		if !init {
			eff, init = op.Macros, true
		} else {
			eff = eff.Add(op.Macros)
		}
	}
	if f, ok := s.journal.Fraction(it.ID); ok {
		st.Fraction = &f
	}
	if !init {
		eff = zeroMacros()
	}
	// The effective contribution comes from the same reducer over the same
	// cached rows as the snapshot totals (own rows are merged on 201, so a
	// failed read still shows what is known to be written). Pending and
	// uncertain ops show in actions, never here.
	rows, _, _ := s.cache.Rows(it.Date)
	if c, _, found := itemContrib(rows, it.ID); found {
		eff = c.effective()
	} else {
		eff = zeroMacros()
	}
	st.Effective = eff
	if !v.pending && !v.failed && v.origDone && !st.Undone {
		st.Actions = append(st.Actions, "undo")
		if it.NeedsFraction && st.Fraction == nil {
			st.Actions = append(st.Actions, "fraction")
		}
	}
	v.state = st
	return v
}

// entryStatus: failed if the entry failed, pending while any op is not
// terminal, else done.
func (s *Service) entryStatus(e Entry) string {
	if e.Failed {
		return StatusFailed
	}
	for _, id := range e.ItemIDs {
		for _, op := range s.journal.ItemOps(id) {
			if !terminal(op.State) {
				return StatusPending
			}
		}
	}
	return StatusDone
}

func (s *Service) itemStates(e Entry) []ItemState {
	out := []ItemState{}
	for _, id := range e.ItemIDs {
		if it, ok := s.journal.Item(id); ok {
			out = append(out, s.viewItem(it).state)
		}
	}
	return out
}

// ---- snapshot ----

// snapshotFor computes the snapshot of a date from the cache. Rows and the
// revision are captured under the cache lock, so they always match.
func (s *Service) snapshotFor(date string) (Snapshot, error) {
	t, err := s.loadTargets()
	if err != nil {
		return Snapshot{}, err
	}
	var acts []Activity
	var stravaAt time.Time
	var body []Value
	rows := map[string][]Value{}
	have := map[string]bool{}
	strength := map[string]bool{}
	var fetched time.Time
	var rev int
	var asOf time.Time
	s.cache.View(func() {
		asOf = s.o.Now() // captured with the rows, the revision and the body data
		body = s.cache.body
		acts, stravaAt = s.strava.Load()
		for i := 0; i <= 31; i++ {
			d := dateAdd(date, -i)
			r, _, ok := s.cache.rowsLocked(d)
			rows[d], have[d] = r, ok
			if i <= 7 {
				strength[d] = s.cache.strengthLocked(d)
			}
		}
		_, fetched, _ = s.cache.rowsLocked(date)
		rev = s.journal.Revision(date)
	})
	in := snapInput{
		date: date, now: asOf, targets: t,
		rowsFor:  func(d string) ([]Value, bool) { return rows[d], have[d] },
		strength: func(d string) bool { return strength[d] },
		dataAsOf: fetched,
		revision: rev,
		body:     body,
		acts:     acts,
		stravaAt: stravaAt,
	}
	return computeSnapshot(in), nil
}

// mutationKey keys an undo / fraction reply line by the request's durable
// GENERATION (its op id, or the fraction choice time for fraction 1), so a
// client_id reused after expiry gets its own line.
func mutationKey(clientID string, j *Journal) string {
	id, ok := j.Ident(clientID)
	if !ok {
		return "m:" + clientID
	}
	if id.OpID != "" {
		return "m:" + id.OpID
	}
	return fmt.Sprintf("m:%s:%d", clientID, id.At.UnixNano())
}

// needsFinal reports whether a request identity still lacks its stored
// final response (within the idempotency retention).
func (s *Service) needsFinal(clientID string, at, now time.Time) bool {
	if clientID == "" || now.Sub(at) > idemRetention {
		return false
	}
	rec, ok := s.idem.Get(clientID)
	return !ok || len(rec.Response) == 0
}

// persistFinal stores a final (done, fully rendered) response once; an
// existing final response is never overwritten.
func (s *Service) persistFinal(rec idemRec, resp any) {
	rec.Status = http.StatusOK
	s.storeFinalOnce(rec, resp)
}

// storeFinalOnce atomically stores resp as the final response unless one
// exists, and returns the stored record (the existing one if it won).
func (s *Service) storeFinalOnce(rec idemRec, resp any) (idemRec, bool) {
	b, err := json.Marshal(resp)
	if err != nil {
		return idemRec{}, false
	}
	rec.Response = b
	if rec.Status == 0 {
		rec.Status = http.StatusOK
	}
	return s.idem.PutIfNoResponse(rec)
}

// recoverMutation adds the feed line and the final response of an undo /
// fraction that a crash (or an unready first render) left without them.
func (s *Service) recoverMutation(ctx context.Context, clientID, hash, kind, entryID, itemID, date string, at, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if id, ok := s.journal.Ident(clientID); !ok || id.ItemID != itemID {
		return // the client_id was reused later; that request owns it
	}
	key := mutationKey(clientID, s.journal)
	needFeed := !s.feed.HasKey(key)
	needFinal := s.needsFinal(clientID, at, now)
	if !needFeed && !needFinal {
		return
	}
	if !s.historyReady(ctx, date) {
		return
	}
	resp, ok := s.rebuildMutation(clientID)
	if !ok || resp.renderErr != nil {
		return
	}
	if needFeed {
		s.appendMutationFeed(key, entryID, resp.Blocks)
	}
	if needFinal && resp.Status == StatusDone {
		s.persistFinal(idemRec{ClientID: clientID, Hash: hash, Kind: kind, At: at, EntryID: entryID, ItemID: itemID}, resp)
	}
}

// historyReady loads a date and its history (outside the render lock) and
// reports whether every day is now loaded, so recovery never persists a
// widget or a coach event computed from unread days.
func (s *Service) historyReady(parent context.Context, date string) bool {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	return s.renderReady(ctx, date)
}

// renderReady: the date is FRESH (read within 60 s, or re-read now), its
// history and the body data are loaded, and the budget holds. Only then may
// a render be persisted (final response, feed widget, coach event).
func (s *Service) renderReady(ctx context.Context, date string) bool {
	fresh := s.freshen(ctx, date)
	return fresh && s.ensureHistory(ctx, date) && ctx.Err() == nil
}

// ensureHistory loads (once) every day a snapshot of date depends on (31
// days back for streaks and the ISO week) that the startup hydration did not
// cover, so an older snapshot never treats an unread day as empty. Call it
// BEFORE taking the render lock.
func (s *Service) ensureHistory(ctx context.Context, date string) bool {
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := 0; i <= 31; i++ {
		d := dateAdd(date, -i)
		if s.cache.Loaded(d) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.cache.RefreshDay(ctx, d); err != nil {
				log.Printf("fuel: load %s: %s", d, errClass(err))
			}
		}()
	}
	wg.Wait()
	// Ready = every day loaded AND the Body composition history read once
	// (else weight and body-fat widgets would be empty, not unknown).
	if !s.cache.BodyLoaded() {
		bctx, cancel := context.WithTimeout(ctx, s.o.VarTimeout)
		err := s.cache.RefreshBody(bctx)
		cancel()
		if err != nil {
			return false
		}
	}
	for i := 0; i <= 31; i++ {
		if !s.cache.Loaded(dateAdd(date, -i)) {
			return false
		}
	}
	return true
}

// freshen re-reads a day if its cache is older than 60 s (spec 5).
// It reports whether the day is now fresh.
func (s *Service) freshen(ctx context.Context, date string) bool {
	if s.cache.Age(date) < 60*time.Second {
		return true
	}
	if err := s.cache.RefreshDay(ctx, date); err != nil {
		log.Printf("fuel: refresh %s: %s", date, errClass(err))
		return false
	}
	return true
}

// ---- blocks ----

// Block is one reply element: text, or a widget carrying snapshot data.
type Block struct {
	Type     string     `json:"type"`
	Text     string     `json:"text,omitempty"`
	Widget   string     `json:"widget,omitempty"`
	AsOf     *time.Time `json:"as_of,omitempty"`
	Date     string     `json:"date,omitempty"`
	Revision *int       `json:"revision,omitempty"`
	Data     any        `json:"data,omitempty"`
}

func textBlock(t string) Block { return Block{Type: "text", Text: t} }

func widgetBlock(name string, snap Snapshot, added map[string]*float64) (Block, bool) {
	asOf, rev := snap.AsOf, snap.Revision
	b := Block{Type: "widget", Widget: name, AsOf: &asOf, Date: snap.Date, Revision: &rev}
	switch name {
	case "macros_today":
		if added == nil {
			added = map[string]*float64{}
		}
		b.Data = map[string]any{"macros": snap.Macros, "added": added}
	case "next_action":
		b.Data = snap.NextAction
	case "weight_trend":
		b.Data = snap.Weight
	case "body_fat_trend":
		b.Data = snap.BodyFat
	case "week":
		b.Data = snap.Week
	case "streaks":
		b.Data = snap.Streaks
	default:
		return b, false
	}
	return b, true
}

// addedOf sums the effective contributions per snapshot macro key.
func addedOf(ms []Macros) map[string]*float64 {
	out := map[string]*float64{}
	for _, d := range macroDefs {
		var sum tenth
		for i, m := range ms {
			if i == 0 {
				sum = m.Get(d.key)
			} else {
				sum = sum.add(m.Get(d.key))
			}
		}
		if len(ms) == 0 {
			sum = known(0)
		}
		out[d.key] = sum.ptr()
	}
	return out
}

// stripDigits removes every digit from model text (numbers come only from
// widgets, spec 8) and reports whether any was removed.
func stripDigits(s string) (string, bool) {
	var b strings.Builder
	stripped := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			stripped = true
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	return out, stripped
}

// sortedKeys is a helper for deterministic logs.
func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = json.Marshal
