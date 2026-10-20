package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

const (
	pairTTL         = 5 * time.Minute
	pairMaxAttempts = 5
)

// TailscaleWhois runs `tailscale whois --json <addr>` with a 3 s timeout.
func TailscaleWhois(ctx context.Context, remoteAddr string) (string, string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "whois", "--json", remoteAddr).Output()
	if err != nil {
		return "", "", "", err
	}
	var w struct {
		Node struct {
			StableID string
			Hostinfo struct{ OS string }
		}
		UserProfile struct{ LoginName string }
	}
	if err := json.Unmarshal(out, &w); err != nil {
		return "", "", "", err
	}
	return w.UserProfile.LoginName, w.Node.Hostinfo.OS, w.Node.StableID, nil
}

// PairHandler serves /vault/v1/pair/* on the pairing listener only.
func (s *Server) PairHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery || !jsonType(r) {
			unsigned(w, http.StatusNotFound, "not_found")
			return
		}
		var fn func(*http.Request, []byte) ([]byte, int)
		switch r.URL.Path {
		case "/vault/v1/pair/keys":
			fn = s.pairKeys
		case "/vault/v1/pair/hello":
			fn = s.pairHello
		case "/vault/v1/pair/confirm":
			fn = s.pairConfirm
		default:
			unsigned(w, http.StatusNotFound, "not_found")
			return
		}
		body, ok := readBody(w, r, 16<<10)
		if !ok {
			return
		}
		if !s.limits.allowPair(s.now()) {
			unsigned(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		out, code := fn(r, body)
		if code != http.StatusOK {
			s.audit.write(s.nowMs(), "pair"+strings.TrimPrefix(r.URL.Path, "/vault/v1/pair/"), "", "", "refused", "pair")
			unsigned(w, code, "pairing failed")
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// liveRecord returns a copy of the pairing record for pid when it exists, is
// not expired and still matches the current keys. Caller holds mu.
func (s *Server) liveRecord(pid string) *Pairing {
	rec := s.st.Pairing
	if rec == nil || rec.Pid != pid {
		return nil
	}
	if s.nowMs() > rec.Expiry {
		s.st.Pairing = nil
		_ = s.save()
		return nil
	}
	if rec.Bsid != s.key.Bsid || rec.Bkid != s.key.Bkid {
		return nil
	}
	cp := *rec
	return &cp
}

// countAttempt adds one failed attempt to the record pid (if it is still the
// current one); at the limit the record is deleted.
func (s *Server) countAttempt(pid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.st.Pairing; rec != nil && rec.Pid == pid {
		// The in-memory count stays even when the save fails: stricter than
		// disk, never looser.
		rec.Attempts++
		if rec.Attempts >= pairMaxAttempts {
			s.st.Pairing = nil
		}
		if err := s.save(); err != nil {
			log.Printf("vaultd: pairing attempt count not saved: %v", err)
		}
	}
}

func (s *Server) checkNode(r *http.Request, bound string) (string, bool) {
	if s.whois == nil {
		return "", false
	}
	login, os, node, err := s.whois(r.Context(), r.RemoteAddr)
	if err != nil || login != s.cfg.PairLogin || os != s.cfg.PairOS || node == "" {
		return "", false
	}
	if bound != "" && node != bound {
		return "", false
	}
	return node, true
}

func (s *Server) pairKeys(r *http.Request, body []byte) ([]byte, int) {
	var req wire.PairKeysReq
	if wire.StrictDecode(body, &req) != nil || !wire.IsID(req.Pid) {
		return nil, http.StatusForbidden
	}
	s.mu.Lock()
	rec := s.liveRecord(req.Pid)
	k := s.key
	s.mu.Unlock()
	if rec == nil || rec.State == PairDone {
		return nil, http.StatusNotFound
	}
	ksig := k.Sign.Sign(vcrypto.Cat([]byte(vcrypto.LabelPairKeys), []byte(req.Pid), k.Sign.Public(), k.Intake.Public()))
	out, err := wire.Marshal(wire.PairKeysResp{Bsid: k.Bsid, BsigPub: wire.EncodeB64(k.Sign.Public()),
		Bkid: k.Bkid, BintPub: wire.EncodeB64(k.Intake.Public()), Ksig: wire.EncodeB64(ksig)})
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	return out, http.StatusOK
}

// parsedT is a validated transcript.
type parsedT struct {
	t      wire.PairT
	psig   []byte
	pagr   []byte
	dev    string
	pkid   string
	pcByte []byte
}

func (s *Server) parseT(tb []byte, k *BoxKeys) (*parsedT, error) {
	var t wire.PairT
	if err := wire.StrictDecode(tb, &t); err != nil {
		return nil, err
	}
	if t.V != 1 || t.Srv != s.cfg.ServerID || !wire.IsID(t.Pid) || t.Bsid != k.Bsid || t.Bkid != k.Bkid ||
		t.BsigPub != wire.EncodeB64(k.Sign.Public()) || t.BintPub != wire.EncodeB64(k.Intake.Public()) {
		return nil, errors.New("box fields do not match")
	}
	psig, err1 := wire.DecodeB64Exact(t.PsigPub, 65)
	pagr, err2 := wire.DecodeB64Exact(t.PagrPub, 65)
	pc, err3 := wire.DecodeB64Exact(t.Pc, 16)
	if err1 != nil || err2 != nil || err3 != nil || !vcrypto.ValidPoint(psig) || !vcrypto.ValidPoint(pagr) || bytes.Equal(psig, pagr) {
		return nil, errors.New("phone keys invalid")
	}
	if len(t.Name) < 1 || len(t.Name) > 40 || wire.HasControl(t.Name) {
		return nil, errors.New("name")
	}
	return &parsedT{t: t, psig: psig, pagr: pagr, dev: vcrypto.KeyID(psig), pkid: vcrypto.KeyID(pagr), pcByte: pc}, nil
}

func (s *Server) pairHello(r *http.Request, body []byte) ([]byte, int) {
	var req wire.PairHelloReq
	if wire.StrictDecode(body, &req) != nil {
		return nil, http.StatusForbidden
	}
	tb, err1 := wire.DecodeB64(req.T, 1, 8<<10)
	sig, err2 := wire.DecodeB64Exact(req.Sig, 64)
	mac, err3 := wire.DecodeB64Exact(req.Mac, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, http.StatusForbidden
	}
	var pidOnly struct {
		Pid string `json:"pid"`
	}
	if json.Unmarshal(tb, &pidOnly) != nil || !wire.IsID(pidOnly.Pid) {
		return nil, http.StatusForbidden
	}
	pid := pidOnly.Pid
	s.mu.Lock()
	rec := s.liveRecord(pid)
	key := s.key
	s.mu.Unlock()
	if rec == nil {
		return nil, http.StatusForbidden // unknown pid: no attempt counted
	}
	node, ok := s.checkNode(r, rec.Node)
	if !ok {
		return nil, http.StatusForbidden
	}
	th := vcrypto.Hash(tb)
	if rec.State == PairAnswered || rec.State == PairDone {
		// A retry is matched on the exact bound T (S may be gone); the
		// answer holds nothing a holder of T could not already compute.
		if bytes.Equal(rec.TH, th) {
			return rec.Answer, http.StatusOK
		}
		s.mu.Lock()
		if cur := s.st.Pairing; cur != nil && cur.Pid == pid && !bytes.Equal(cur.TH, th) {
			s.st.Pairing = nil
			if err := s.save(); err != nil {
				s.st.Pairing = cur
			}
		}
		s.mu.Unlock()
		return nil, http.StatusForbidden
	}
	K, err := vcrypto.PairKey(rec.S, pid)
	if err != nil || !vcrypto.MACEqual(mac, vcrypto.MAC(K, vcrypto.Cat([]byte(vcrypto.LabelPairHello), tb))) {
		s.countAttempt(pid)
		return nil, http.StatusForbidden
	}
	pt, err := s.parseT(tb, key)
	if err != nil || pt.t.Pid != pid || !vcrypto.Verify(pt.psig, vcrypto.Cat([]byte(vcrypto.LabelPairT), tb), sig) {
		s.countAttempt(pid)
		return nil, http.StatusForbidden
	}

	// Atomic transition under M: recheck everything against the record now.
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.liveRecord(pid)
	if cur == nil || (cur.Node != "" && cur.Node != node) || s.key != key {
		return nil, http.StatusForbidden
	}
	switch cur.State {
	case PairAnswered, PairDone:
		if bytes.Equal(cur.TH, th) {
			return cur.Answer, http.StatusOK
		}
		s.st.Pairing = nil
		_ = s.save()
		return nil, http.StatusForbidden
	case PairOpen:
	default:
		return nil, http.StatusForbidden
	}
	R := vcrypto.RandomBytes(32)
	secret, err := wire.Marshal(wire.PairBoxSecret{R: wire.EncodeB64(R), RouteToken: s.cfg.RouteToken})
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	aad := wire.AADB2P(s.cfg.ServerID, pt.dev, pid, pt.pkid, "pair.box", pid, 0)
	rc, err := vcrypto.Seal(pt.pagr, vcrypto.InfoB2P, aad, secret)
	wipe(secret)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	msg := vcrypto.Cat([]byte(vcrypto.LabelPairBox), tb, rc)
	answer, err := wire.Marshal(wire.PairHelloResp{Rc: wire.EncodeB64(rc),
		Bsig: wire.EncodeB64(s.key.Sign.Sign(msg)), Bmac: wire.EncodeB64(vcrypto.MAC(K, msg))})
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	live := s.st.Pairing
	prev := *live
	live.State, live.TH, live.T, live.R, live.Node, live.Answer = PairAnswered, th, tb, R, node, answer
	if err := s.save(); err != nil {
		*live = prev // never answer from a transition that is not durable
		return nil, http.StatusInternalServerError
	}
	return answer, http.StatusOK
}

func (s *Server) pairConfirm(r *http.Request, body []byte) ([]byte, int) {
	var req wire.PairConfirmReq
	if wire.StrictDecode(body, &req) != nil {
		return nil, http.StatusForbidden
	}
	th, err1 := wire.DecodeB64Exact(req.Th, 32)
	csig, err2 := wire.DecodeB64Exact(req.Csig, 64)
	cmac, err3 := wire.DecodeB64Exact(req.Cmac, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, http.StatusForbidden
	}
	// Find the record by its bound transcript hash.
	s.mu.Lock()
	var rec *Pairing
	if p := s.st.Pairing; p != nil && bytes.Equal(p.TH, th) {
		rec = s.liveRecord(p.Pid)
	}
	key := s.key
	s.mu.Unlock()
	if rec == nil || (rec.State != PairAnswered && rec.State != PairDone) {
		return nil, http.StatusForbidden
	}
	if _, ok := s.checkNode(r, rec.Node); !ok {
		return nil, http.StatusForbidden
	}
	if rec.State == PairDone {
		// Retry of the accepted confirm: exact bytes only (S is gone).
		if bytes.Equal(body, rec.Confirm) {
			return rec.Done, http.StatusOK
		}
		return nil, http.StatusForbidden
	}
	K, err := vcrypto.PairKey(rec.S, rec.Pid)
	if err != nil {
		return nil, http.StatusForbidden
	}
	msg := vcrypto.Cat([]byte(vcrypto.LabelPairConfirm), rec.T, rec.R)
	if !vcrypto.MACEqual(cmac, vcrypto.MAC(K, msg)) {
		s.countAttempt(rec.Pid)
		return nil, http.StatusForbidden
	}
	pt, err := s.parseT(rec.T, key)
	if err != nil || !vcrypto.Verify(pt.psig, msg, csig) {
		s.countAttempt(rec.Pid)
		return nil, http.StatusForbidden
	}

	// Commit: D for writing (no request runs across the device change), then
	// M, then revalidate everything.
	done, name, code := func() ([]byte, string, int) {
		s.dev.Lock()
		defer s.dev.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.liveRecord(rec.Pid)
		if cur != nil && cur.State == PairDone && bytes.Equal(body, cur.Confirm) {
			return cur.Done, "", http.StatusOK // an identical confirm won the race
		}
		if cur == nil || cur.State != PairAnswered || !bytes.Equal(cur.TH, th) || cur.Node != rec.Node || s.key != key {
			return nil, "", http.StatusForbidden
		}
		// All rejections before any change (code r1 #4).
		if _, used := s.st.RevokedIDs[pt.dev]; used {
			return nil, "", http.StatusForbidden
		}
		for _, d := range s.st.Devices {
			if d.Dev == pt.dev {
				return nil, "", http.StatusForbidden // a device id is never active twice (9.4)
			}
		}
		replace := s.st.activeCount() >= s.cfg.MaxDevices
		if replace && !cur.Replace {
			return nil, "", http.StatusForbidden
		}
		gen := vcrypto.RandomID()
		dsig := key.Sign.Sign(vcrypto.Cat([]byte(vcrypto.LabelPairDone), rec.T, []byte(pt.dev), []byte(gen)))
		done, err := wire.Marshal(wire.PairDoneResp{Dev: pt.dev, Gen: gen, Dsig: wire.EncodeB64(dsig)})
		if err != nil {
			return nil, "", http.StatusInternalServerError
		}
		// One transaction: snapshot, change, save, roll back on a failed save.
		prevDevices := append([]*Device(nil), s.st.Devices...)
		prevRevoked := map[*Device]bool{}
		for _, d := range s.st.Devices {
			prevRevoked[d] = d.Revoked
		}
		prevPair := *s.st.Pairing
		now := s.nowMs()
		var newlyRevoked []string
		if replace {
			for _, d := range s.st.Devices {
				if !d.Revoked {
					d.Revoked = true
					s.st.RevokedIDs[d.Dev] = now
					newlyRevoked = append(newlyRevoked, d.Dev)
				}
			}
		}
		s.st.Devices = append(s.st.Devices, &Device{Dev: pt.dev, Pkid: pt.pkid, SigPub: pt.psig, AgrPub: pt.pagr,
			Name: pt.t.Name, Gen: gen, PairedAt: now})
		s.st.pruneDevices()
		live := s.st.Pairing
		live.State, live.Confirm, live.Done, live.S = PairDone, append([]byte(nil), body...), done, nil
		if err := s.save(); err != nil {
			s.st.Devices = prevDevices
			for d, r := range prevRevoked {
				d.Revoked = r
			}
			for _, id := range newlyRevoked {
				delete(s.st.RevokedIDs, id)
			}
			*s.st.Pairing = prevPair
			return nil, "", http.StatusInternalServerError
		}
		return done, pt.t.Name, http.StatusOK
	}()
	if code != http.StatusOK {
		return nil, code
	}
	if name != "" {
		// Outside every lock (code r1 #29).
		s.audit.write(s.nowMs(), "pair.confirm", pt.dev, "", "paired", "pair")
		s.notify("vault: a device paired (" + name + ")")
	}
	return done, http.StatusOK
}

// ---- admin and phone-side device control ----

// PairStart creates the single pairing record and returns the URI.
func (s *Server) PairStart(replace bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.activeCount() >= s.cfg.MaxDevices && !replace {
		return "", errors.New("a device is paired; use --replace")
	}
	S := vcrypto.RandomBytes(32)
	rec := &Pairing{Pid: vcrypto.RandomID(), S: S, Expiry: s.now().Add(pairTTL).UnixMilli(),
		State: PairOpen, Bsid: s.key.Bsid, Bkid: s.key.Bkid, Replace: replace}
	prev := s.st.Pairing
	s.st.Pairing = rec
	if err := s.save(); err != nil {
		s.st.Pairing = prev
		return "", err
	}
	p, err := wire.Marshal(wire.PairURI{V: 1, Pid: rec.Pid, S: wire.EncodeB64(S),
		Fp: wire.EncodeB64(s.key.Fingerprint()), Exp: rec.Expiry})
	if err != nil {
		return "", err
	}
	s.audit.write(s.nowMs(), "pair.start", "", "", "ok", "admin")
	return "agentd-vault://pair#" + wire.EncodeB64(p), nil
}

// unpairSelf handles /vault/v1/unpair outside the read lock: it takes D for
// writing and verifies, changes, saves and signs under it.
func (s *Server) unpairSelf(hb, sig []byte, h wire.ReqHeader, path string) []byte {
	resp, dev := func() ([]byte, string) {
		s.dev.Lock()
		defer s.dev.Unlock()
		rq, errResp := s.verify(path, wire.OpUnpair, hb, sig, h)
		if rq == nil || errResp != nil {
			return errResp, ""
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if code := s.claimRead(rq); code != "" {
			return s.errResp(rq, code), ""
		}
		d := s.st.activeDevice(rq.dev.Dev)
		if d == nil {
			return nil, ""
		}
		prevPair := s.st.Pairing
		d.Revoked = true
		s.st.RevokedIDs[d.Dev] = s.nowMs()
		s.st.Pairing = nil
		if err := s.save(); err != nil {
			d.Revoked, s.st.Pairing = false, prevPair
			delete(s.st.RevokedIDs, d.Dev)
			return s.errResp(rq, "internal"), ""
		}
		return s.sign(s.baseResp(rq)), rq.dev.Dev
	}()
	if dev != "" {
		s.audit.write(s.nowMs(), wire.OpUnpair, dev, "", "ok", "public")
		s.notify("vault: device revoked")
	}
	return resp
}

// Unpair revokes a device from the admin socket.
func (s *Server) Unpair(dev string) error {
	if err := func() error {
		s.dev.Lock()
		defer s.dev.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		d := s.st.activeDevice(dev)
		if d == nil {
			return errors.New("no such active device")
		}
		prevPair := s.st.Pairing
		d.Revoked = true
		s.st.RevokedIDs[d.Dev] = s.nowMs()
		s.st.Pairing = nil
		if err := s.save(); err != nil {
			d.Revoked, s.st.Pairing = false, prevPair
			delete(s.st.RevokedIDs, d.Dev)
			return err
		}
		return nil
	}(); err != nil {
		return err
	}
	s.audit.write(s.nowMs(), "unpair", dev, "", "ok", "admin")
	s.notify("vault: device revoked")
	return nil
}

// ResetKeys creates new box keys, revokes every device and drops the pairing
// record (spec 9.5). Refused while any op is applying (it may need the old
// intake key).
func (s *Server) ResetKeys() (string, error) {
	s.dev.Lock()
	defer s.dev.Unlock()
	s.applyRun.Lock()
	defer s.applyRun.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range s.st.Ops {
		if op.State == StateApplying {
			return "", errors.New("an operation is still applying; retry later")
		}
	}
	k := newBoxKeys()
	if err := saveBoxKeys(s.cfg.StateDir, k); err != nil {
		return "", err
	}
	old := s.key
	prevPair := s.st.Pairing
	var revoked []*Device
	now := s.nowMs()
	for _, d := range s.st.Devices {
		if !d.Revoked {
			d.Revoked = true
			s.st.RevokedIDs[d.Dev] = now
			revoked = append(revoked, d)
		}
	}
	s.st.Bsid, s.st.Bkid = k.Bsid, k.Bkid
	s.st.Pairing = nil
	if err := s.save(); err != nil {
		s.st.Bsid, s.st.Bkid, s.st.Pairing = old.Bsid, old.Bkid, prevPair
		for _, d := range revoked {
			d.Revoked = false
			delete(s.st.RevokedIDs, d.Dev)
		}
		_ = removeOtherKeys(s.cfg.StateDir, old.Bsid, old.Bkid)
		return "", err
	}
	s.key = k
	if err := removeOtherKeys(s.cfg.StateDir, k.Bsid, k.Bkid); err != nil {
		log.Printf("vaultd: old key files not removed yet (next start retries): %v", err)
	}
	s.audit.write(s.nowMs(), "reset-keys", "", "", "ok", "admin")
	return FormatFingerprint(k.Fingerprint()), nil
}

// FormatFingerprint prints all 64 hex characters in groups of 4.
func FormatFingerprint(fp []byte) string {
	const hexd = "0123456789abcdef"
	var b strings.Builder
	for i, c := range fp {
		if i > 0 && i%2 == 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}
