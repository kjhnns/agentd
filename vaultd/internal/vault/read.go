package vault

import (
	"context"
	"log"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

const pageSize = 50

// pruneReads drops read records older than the retention and caps the map.
// Caller holds mu.
func (s *Server) pruneReads() {
	cut := s.nowMs() - readRetention.Milliseconds()
	for rid, r := range s.reads {
		if r.Ts < cut {
			delete(s.reads, rid)
		}
	}
	for len(s.reads) >= maxReads {
		var oldest string
		var ts int64 = 1 << 62
		for rid, r := range s.reads {
			if r.Ts < ts {
				oldest, ts = rid, r.Ts
			}
		}
		delete(s.reads, oldest)
	}
}

// claimRead applies the rid rules to a read. Caller holds mu. Returns "" or a
// code.
func (s *Server) claimRead(rq *request) string {
	rid := rq.h.Rid
	if _, ok := s.st.Ops[rid]; ok {
		return "rid_conflict"
	}
	if _, ok := s.reserved[rid]; ok {
		return "rid_conflict"
	}
	if _, ok := s.reads[rid]; ok {
		// A read rid is single use (spec 8): the phone uses a fresh rid per
		// read, so an old signed answer can never stand in for a new one.
		return "rid_conflict"
	}
	s.pruneReads()
	s.reads[rid] = &ReadRec{Dev: rq.dev.Dev, Rh: rq.rh, Ts: s.nowMs()}
	return ""
}

// read serves given.list, shared.list, shared.get and op.status. Caller holds D.
func (s *Server) read(ctx context.Context, rq *request) []byte {
	s.mu.Lock()
	if code := s.claimRead(rq); code != "" {
		s.mu.Unlock()
		return s.errResp(rq, code)
	}
	s.mu.Unlock()
	switch rq.h.Op {
	case wire.OpGivenList:
		return s.list(rq, KindGiven, "given.meta")
	case wire.OpSharedList:
		return s.list(rq, KindShared, "shared.meta")
	case wire.OpSharedGet:
		return s.sharedGet(ctx, rq)
	case wire.OpStatus:
		return s.status(rq)
	}
	return s.errResp(rq, "bad_request")
}

func (s *Server) seal(rq *request, kind, item string, version int64, v any) (string, error) {
	pt, err := wire.Marshal(v)
	if err != nil {
		return "", err
	}
	defer wipe(pt)
	aad := wire.AADB2P(s.cfg.ServerID, rq.dev.Dev, rq.dev.Gen, rq.dev.Pkid, kind, item, version)
	ct, err := vcrypto.Seal(rq.dev.AgrPub, vcrypto.InfoB2P, aad, pt)
	if err != nil {
		return "", err
	}
	return wire.EncodeB64(ct), nil
}

func (s *Server) list(rq *request, kind, aadKind string) []byte {
	s.mu.Lock()
	all := s.st.sortedItems(kind)
	var page []Item
	more := false
	for _, it := range all {
		if rq.h.Item != "" && it.ID <= rq.h.Item {
			continue
		}
		if len(page) == pageSize {
			more = true
			break
		}
		page = append(page, *it)
	}
	s.mu.Unlock()
	h := s.baseResp(rq)
	for _, it := range page {
		version := it.Version
		if kind == KindShared {
			version = 0
		}
		c, err := s.seal(rq, aadKind, it.ID, version, wire.MetaPayload{Label: it.Label, Login: it.Login, URL: it.URL})
		if err != nil {
			log.Printf("vaultd: seal failed: %v", err)
			return s.errResp(rq, "internal")
		}
		h.Entries = append(h.Entries, wire.Entry{Item: it.ID, Iv: version, Created: it.Created,
			Updated: it.Updated, LastUse: it.LastUse, C: c})
	}
	if more && len(page) > 0 {
		h.Next = page[len(page)-1].ID
	}
	return s.sign(h)
}

func (s *Server) sharedGet(ctx context.Context, rq *request) []byte {
	s.mu.Lock()
	var path string
	if it := s.st.Items[rq.h.Item]; it != nil {
		path = it.PassPath
	}
	s.mu.Unlock()
	if path == "" {
		return s.errResp(rq, "not_found")
	}
	unlock := s.locks.lock(path)
	defer unlock()
	s.mu.Lock()
	it := s.st.Items[rq.h.Item]
	if it == nil || it.Status != StatusLive {
		s.mu.Unlock()
		return s.errResp(rq, "not_found")
	}
	if it.Kind != KindShared {
		s.mu.Unlock()
		return s.errResp(rq, "wrong_kind")
	}
	if it.PassPath != path || it.Busy != "" {
		// An applying generate may have replaced the entry without
		// durability yet (code r3 #41).
		s.mu.Unlock()
		return s.errResp(rq, "busy")
	}
	cp := *it
	s.mu.Unlock()
	raw, err := s.store.Read(ctx, cp.PassPath)
	if err != nil {
		log.Printf("vaultd: read of shared item %s failed", cp.ID)
		return s.errResp(rq, "not_found")
	}
	e, err := passstore.ParseEntry(raw)
	wipe(raw)
	if err != nil {
		return s.errResp(rq, "bad_payload")
	}
	// The value and its metadata come from the same current entry; lists show
	// the metadata recorded at share time (spec 19, code r1 #22).
	login, url := e.Login, e.URL
	c, err := s.seal(rq, "shared.value", cp.ID, 0, wire.SecretPayload{Label: cp.Label, Login: login, URL: url, Password: e.Password})
	if err != nil {
		return s.errResp(rq, "internal")
	}
	s.mu.Lock()
	if cur := s.st.Items[cp.ID]; cur != nil {
		prev := cur.LastUse
		cur.LastUse = s.nowMs()
		if err := s.save(); err != nil {
			cur.LastUse = prev
		}
	}
	s.mu.Unlock()
	s.audit.write(s.nowMs(), rq.h.Op, rq.dev.Dev, cp.ID, "ok", "public")
	h := s.baseResp(rq)
	h.Item, h.C = cp.ID, c
	return s.sign(h)
}

func (s *Server) status(rq *request) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.st.Ops[rq.h.Opid]; ok && op.Dev == rq.dev.Dev {
		return s.opResp(rq, op)
	}
	if r, ok := s.reserved[rq.h.Opid]; ok && r.dev == rq.dev.Dev {
		return s.opResp(rq, &Op{Rid: rq.h.Opid, Item: r.item, State: StateApplying})
	}
	return s.errResp(rq, "not_found")
}
