package vault

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// Bounds (spec section 8, 19 R16).
const (
	maxLivePerKind  = 500
	maxOps          = 2000
	maxTombstones   = 20000
	maxReads        = 5000
	maxHeaderBytes  = 1 << 20 // total header bytes of applying ops
	maxIv           = 1 << 31
	opRetention     = 7 * 24 * time.Hour
	readRetention   = 10 * time.Minute
	duplicateWait   = 20 * time.Second  // below the 30 s write timeout
	codeRetry       = "retry"           // internal: the side effect must run again, op stays applying
	codeUncertain   = "retry-uncertain" // internal: as codeRetry, and a write may already be in place
	admissionPrefix = "admit:"          // internal: refused before the journal, not recorded
)

func givenPath(item string) string { return "vault/given/" + item }

// opGenerate is the admin-side generate op (journaled like intake).
const opGenerate = "generate"

// opPath is the store path an op touches (its path lock).
func opPath(op *Op) string {
	if op.Op == opGenerate {
		return op.Path
	}
	return givenPath(op.Item)
}

// genAAD binds a generate blob to its item and path.
func genAAD(item, path string) []byte {
	return []byte("agentd-vault-v1 aad-gen|" + item + "|" + path)
}

// fatalf is a variable so tests can observe a crash-on-persistence-failure.
var fatalf = log.Fatalf

// opRequest rebuilds the request context of an op for its answer.
func opRequest(op *Op, dev Device) *request {
	return &request{h: wire.ReqHeader{Op: op.Op}, rh: op.Rh, dev: dev}
}

// opResp builds the signed answer for an op from its recorded outcome.
// Caller holds mu.
func (s *Server) opResp(rq *request, op *Op) []byte {
	h := s.baseResp(rq)
	if op.State == StateFailed {
		h.Status = "err:" + op.Code
	}
	h.Opid, h.State, h.Code, h.Item, h.Iv = op.Rid, op.State, op.Code, op.Item, op.ResultIv
	return s.sign(h)
}

// reservation is an in-memory claim on a rid while it is being validated
// and journaled. Duplicates wait on done.
type reservation struct {
	dev  string
	rh   []byte
	item string
	done chan struct{}
}

// mutate runs intake, replace and delete (spec section 7). Lock order:
// D (held by the caller) -> path lock -> M.
func (s *Server) mutate(ctx context.Context, rq *request) []byte {
	h := rq.h
	for {
		s.mu.Lock()
		if r, ok := s.reserved[h.Rid]; ok {
			s.mu.Unlock()
			if r.dev != rq.dev.Dev || string(r.rh) != string(rq.rh) {
				return s.errResp(rq, "rid_conflict")
			}
			// Answer "applying" at once (code r2 #37): no wait that could
			// outlive the transport; the phone asks op.status with a new rid.
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.opResp(rq, &Op{Rid: h.Rid, Item: h.Item, State: StateApplying})
		}
		if op, ok := s.st.Ops[h.Rid]; ok {
			defer s.mu.Unlock()
			if op.Dev != rq.dev.Dev || string(op.Rh) != string(rq.rh) {
				return s.errResp(rq, "rid_conflict")
			}
			if op.Response != nil {
				return op.Response
			}
			return s.opResp(rq, op)
		}
		if _, ok := s.reads[h.Rid]; ok {
			s.mu.Unlock()
			return s.errResp(rq, "rid_conflict")
		}
		break
	}
	res := &reservation{dev: rq.dev.Dev, rh: rq.rh, item: h.Item, done: make(chan struct{})}
	s.reserved[h.Rid] = res
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.reserved, h.Rid)
		s.mu.Unlock()
		close(res.done)
	}()

	unlock := s.locks.lock(givenPath(h.Item))
	defer unlock()

	s.mu.Lock()
	op := &Op{Rid: h.Rid, Dev: rq.dev.Dev, Gen: rq.dev.Gen, Rh: rq.rh, Op: h.Op,
		Item: h.Item, Iv: h.Iv, State: StateApplying, Created: s.nowMs()}
	if code := s.validateMutation(rq); code != "" {
		if len(code) > len(admissionPrefix) && code[:len(admissionPrefix)] == admissionPrefix {
			// Refused before admission: not journaled, so a full table never
			// grows (spec 19 R16). The phone retries later with a new rid.
			s.mu.Unlock()
			return s.errResp(rq, code[len(admissionPrefix):])
		}
		// A validation failure is a terminal outcome of this rid (R10).
		op.State, op.Code, op.Validation, op.Finished = StateFailed, code, true, op.Created
		if it := s.st.Items[op.Item]; it != nil && it.Kind == KindGiven {
			op.ResultIv = it.Version
		}
		s.st.Ops[op.Rid] = op
		op.Response = s.opResp(rq, op)
		if err := s.save(); err != nil {
			delete(s.st.Ops, op.Rid)
			s.mu.Unlock()
			return s.errResp(rq, "internal")
		}
		s.mu.Unlock()
		s.audit.write(s.nowMs(), h.Op, rq.dev.Dev, h.Item, code, "public")
		return op.Response
	}
	if op.Op != wire.OpDelete {
		// A delete needs no header to recover (code r2 #35): keep it compact.
		op.Header = rq.hb
	}
	undo := s.journal(op)
	if err := s.save(); err != nil {
		undo() // save failed before the rename: the old state.json is intact
		s.mu.Unlock()
		log.Printf("vaultd: journal write failed: %v", err)
		return s.errResp(rq, "internal")
	}
	s.mu.Unlock()

	meta, code := s.apply(ctx, op)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.finish(op, meta, code)
	if op.State != StateApplying {
		op.Response = s.opResp(rq, op)
		if err := s.save(); err != nil {
			// The side effect happened but its record is not durable. Never
			// answer from memory: restart and recover from durable state.
			fatalf("vaultd: state write after op %s failed: %v", op.Rid, err)
			return nil
		}
		s.audit.write(s.nowMs(), h.Op, rq.dev.Dev, h.Item, auditCode(op), "public")
		return op.Response
	}
	if op.Uncertain {
		// Persist the uncertain mark: after a restart a later error must not
		// turn this op into "failed".
		if err := s.save(); err != nil {
			log.Printf("vaultd: could not persist uncertain mark: %v", err)
		}
	}
	return s.opResp(rq, op)
}

func auditCode(op *Op) string {
	if op.State == StateFailed {
		return op.Code
	}
	return op.State
}

// headerBytes is the total size of headers held by applying ops. Caller
// holds mu.
func (s *Server) headerBytes() int {
	n := 0
	for _, op := range s.st.Ops {
		n += len(op.Header) + len(op.Blob)
	}
	return n
}

// validateMutation applies the authorization table and the bounds. Caller
// holds mu. Returns "", an error code (journaled as a terminal failure), or
// admissionPrefix+code (not journaled).
func (s *Server) validateMutation(rq *request) string {
	h := rq.h
	if h.Op != wire.OpDelete && s.cfg.BackupExclusion == "" {
		return admissionPrefix + "not_ready"
	}
	it := s.st.Items[h.Item]
	code := ""
	switch h.Op {
	case wire.OpIntake:
		_, dead := s.st.Tombstones[h.Item]
		switch {
		case dead:
			code = "tombstoned"
		case it != nil:
			code = "exists"
		case s.st.liveCount(KindGiven) >= maxLivePerKind,
			// every live given item can become a tombstone: reserve that
			len(s.st.Tombstones)+s.st.liveCount(KindGiven) >= maxTombstones:
			return admissionPrefix + "full"
		}
	case wire.OpReplace, wire.OpDelete:
		_, dead := s.st.Tombstones[h.Item]
		switch {
		case it != nil && it.Busy != "":
			code = "busy"
		case dead:
			code = "tombstoned"
		case it == nil:
			code = "not_found"
		case it.Kind != KindGiven:
			code = "wrong_kind"
		case it.Status != StatusLive || it.Version != h.Iv:
			code = "version_conflict"
		}
	}
	if len(s.st.Ops) >= maxOps {
		s.pruneOps()
	}
	if len(s.st.Ops) >= maxOps {
		// Full table: only a VALID delete of an existing item is admitted
		// (bounded by the live items); nothing else is journaled.
		if h.Op != wire.OpDelete || code != "" {
			if code == "" {
				code = "full"
			}
			return admissionPrefix + code
		}
	}
	if code == "" && h.Op != wire.OpDelete && s.headerBytes()+len(rq.hb) > maxHeaderBytes {
		return admissionPrefix + "full"
	}
	return code
}

// journal applies the "applying" state changes and returns an undo for a
// failed save. Caller holds mu.
func (s *Server) journal(op *Op) func() {
	s.st.Ops[op.Rid] = op
	switch op.Op {
	case wire.OpIntake:
		s.st.Items[op.Item] = &Item{ID: op.Item, Kind: KindGiven, Status: StatusPending,
			Created: op.Created, PassPath: givenPath(op.Item), Busy: op.Rid}
		return func() { delete(s.st.Ops, op.Rid); delete(s.st.Items, op.Item) }
	case wire.OpDelete:
		it := s.st.Items[op.Item]
		prev := it.Status
		it.Status, it.Busy = StatusTombstoned, op.Rid
		s.st.Tombstones[op.Item] = op.Created
		return func() {
			delete(s.st.Ops, op.Rid)
			it.Status, it.Busy = prev, ""
			delete(s.st.Tombstones, op.Item)
		}
	default:
		it := s.st.Items[op.Item]
		it.Busy = op.Rid
		return func() { delete(s.st.Ops, op.Rid); it.Busy = "" }
	}
}

// apply runs the side effect of an applying op from its stored header. The
// caller holds the op's path lock and not mu; it only reads op fields that
// change under that path lock.
func (s *Server) apply(ctx context.Context, op *Op) (*wire.MetaPayload, string) {
	switch op.Op {
	case wire.OpDelete:
		gone, err := s.store.Remove(givenPath(op.Item))
		if err != nil || !gone {
			log.Printf("vaultd: removal of item %s pending", op.Item)
			return nil, codeRetry
		}
		return nil, ""
	case opGenerate:
		content, err := s.key.Intake.Open(vcrypto.InfoGen, genAAD(op.Item, op.Path), op.Blob)
		if err != nil {
			return nil, "decrypt_failed"
		}
		defer wipe(content)
		return nil, s.writeEntry(ctx, op, op.Path, content)
	}
	var h wire.ReqHeader
	if err := wire.StrictDecode(op.Header, &h); err != nil {
		return nil, "internal"
	}
	// Decryption and payload checks are deterministic: they fail the same
	// way on every attempt, before any write, so they are terminal.
	if h.Bkid != s.key.Bkid {
		return nil, "decrypt_failed"
	}
	c, err := wire.DecodeB64(h.C, 81, 4096)
	if err != nil {
		return nil, "decrypt_failed"
	}
	version := int64(1)
	if op.Op == wire.OpReplace {
		version = op.Iv + 1
	}
	aad := wire.AADP2B(h.Srv, h.Dev, h.Gen, h.Bkid, h.Op, h.Item, version)
	pt, err := s.key.Intake.Open(vcrypto.InfoP2B, aad, c)
	if err != nil {
		return nil, "decrypt_failed"
	}
	defer wipe(pt)
	var p wire.SecretPayload
	if err := wire.StrictDecode(pt, &p); err != nil || p.Validate() != nil {
		return nil, "bad_payload"
	}
	content := passstore.FormatEntry(p.Password, p.Login, p.URL, p.Label)
	defer wipe(content)
	if code := s.writeEntry(ctx, op, givenPath(op.Item), content); code != "" {
		return nil, code
	}
	return &wire.MetaPayload{Label: p.Label, Login: p.Login, URL: p.URL}, ""
}

// writeEntry writes content and classifies the outcome: "" done,
// codeUncertain (a write may be in place: retry the same content forever),
// "internal" (first attempt failed before the rename: the old entry is intact).
func (s *Server) writeEntry(ctx context.Context, op *Op, path string, content []byte) string {
	err := s.store.Write(ctx, path, content)
	if err == nil {
		return ""
	}
	if errors.Is(err, passstore.ErrUncertain) || op.Uncertain {
		// Once a write may be in place, a later error proves nothing about
		// the file: keep retrying the same content (code r1 #1).
		log.Printf("vaultd: write of item %s not yet durable, retrying", op.Item)
		return codeUncertain
	}
	log.Printf("vaultd: write of item %s failed (old entry intact)", op.Item)
	return "internal"
}

// finish records the outcome in memory. Caller holds mu and saves.
func (s *Server) finish(op *Op, meta *wire.MetaPayload, code string) {
	if code == codeUncertain {
		op.Uncertain = true
		return
	}
	if code == codeRetry {
		return // stays applying with its header; the retry runner runs it again
	}
	now := s.nowMs()
	it := s.st.Items[op.Item]
	if it != nil && it.Busy == op.Rid {
		it.Busy = ""
	}
	op.Uncertain = false
	op.Blob = nil
	if code == "" {
		op.State = StateDone
		switch op.Op {
		case opGenerate:
			it.Status, it.Updated = StatusLive, now
			it.Label, it.Login, it.URL = op.Meta.Label, op.Meta.Login, op.Meta.URL
		case wire.OpIntake:
			it.Status, it.Version, it.Updated = StatusLive, 1, now
			it.Label, it.Login, it.URL = meta.Label, meta.Login, meta.URL
			op.ResultIv = 1
		case wire.OpReplace:
			it.Version, it.Updated = op.Iv+1, now
			it.Label, it.Login, it.URL = meta.Label, meta.Login, meta.URL
			op.ResultIv = op.Iv + 1
		case wire.OpDelete:
			delete(s.st.Items, op.Item)
		}
	} else {
		op.State, op.Code = StateFailed, code
		if (op.Op == wire.OpIntake || op.Op == opGenerate) && it != nil && it.Status == StatusPending {
			delete(s.st.Items, op.Item)
		}
		if op.Op == wire.OpReplace && it != nil {
			op.ResultIv = it.Version
		}
	}
	op.Header = nil
	op.Finished = now
}

// pruneOps drops finished ops older than the retention. Caller holds mu.
func (s *Server) pruneOps() {
	cut := s.nowMs() - opRetention.Milliseconds()
	for rid, op := range s.st.Ops {
		if op.State != StateApplying && op.Finished < cut {
			delete(s.st.Ops, rid)
		}
	}
}

// runApplying applies and finishes every op still applying, one op per lock
// round: D (read, when withD) -> applyRun -> path lock -> M, all released
// before the next op, so a revocation waits for one op at most.
func (s *Server) runApplying(ctx context.Context, withD bool) {
	s.mu.Lock()
	var rids []string
	for rid, op := range s.st.Ops {
		if op.State == StateApplying {
			rids = append(rids, rid)
		}
	}
	s.mu.Unlock()
	for _, rid := range rids {
		if ctx.Err() != nil {
			return
		}
		s.runOne(ctx, rid, withD)
	}
}

func (s *Server) runOne(ctx context.Context, rid string, withD bool) {
	if withD {
		s.dev.RLock()
		defer s.dev.RUnlock()
	}
	s.applyRun.Lock()
	defer s.applyRun.Unlock()
	s.mu.Lock()
	op := s.st.Ops[rid]
	s.mu.Unlock()
	if op == nil {
		return
	}
	unlock := s.locks.lock(opPath(op))
	defer unlock()
	s.mu.Lock()
	still := op.State == StateApplying && s.st.Ops[rid] == op
	s.mu.Unlock()
	if !still {
		return
	}
	meta, code := s.apply(ctx, op)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finish(op, meta, code)
	if op.State == StateApplying {
		if op.Uncertain {
			// Saved on every retry until it sticks (code r3 #38).
			if err := s.save(); err != nil {
				log.Printf("vaultd: could not persist uncertain mark: %v", err)
			}
		}
		return
	}
	// Persist the answer now, so a retry of the original request gets the
	// same bytes every time (phone ops only; generate is admin-side).
	if op.Dev != "" {
		op.Response = s.opResp(opRequest(op, Device{Dev: op.Dev, Gen: op.Gen}), op)
	}
	if err := s.save(); err != nil {
		fatalf("vaultd: state write after recovery failed: %v", err)
		return
	}
	s.audit.write(s.nowMs(), op.Op, op.Dev, op.Item, auditCode(op), "recovery")
}

// retryLoop reruns pending ops and prunes every minute.
func (s *Server) retryLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runApplying(ctx, true)
			s.mu.Lock()
			s.pruneReads()
			s.pruneOps()
			if p := s.st.Pairing; p != nil && s.nowMs() > p.Expiry {
				s.st.Pairing = nil
				if err := s.save(); err != nil {
					s.st.Pairing = p
				}
			}
			s.mu.Unlock()
		}
	}
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
