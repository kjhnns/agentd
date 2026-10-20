package vault

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/phone"
	"github.com/kjhnns/agentd/vaultd/internal/testutil"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

const canary = "CANARY-pw-7f3a9c1e-do-not-leak"

type fakeWhois struct {
	mu              sync.Mutex
	login, os, node string
}

func (f *fakeWhois) fn(_ context.Context, _ string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.login, f.os, f.node, nil
}

type env struct {
	t     *testing.T
	s     *Server
	cfg   Config
	store *passstore.GPG
	who   *fakeWhois
	pub   *httptest.Server
	pair  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, _ := testutil.Store(t)
	dir := t.TempDir()
	cfg := Config{ServerID: "agent.gojoe.run", StateDir: filepath.Join(dir, "state"), StoreRoot: store.Root,
		RouteToken: strings.Repeat("r", 40), PairLogin: "joe@example.com", PairOS: "iOS", MaxDevices: 1,
		BackupExclusion: "test", TestMode: true, AdminSock: filepath.Join(dir, "run", "admin.sock")}
	e := &env{t: t, cfg: cfg, store: store, who: &fakeWhois{login: "joe@example.com", os: "iOS", node: "nodeA"}}
	e.start()
	return e
}

func (e *env) start() {
	s, err := New(e.cfg, e.store, e.who.fn)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s = s
	// Tests read right after a start; keep the "signed before this start"
	// rule but not the 30 s skew margin (TestReadFloorSkew covers it).
	s.readFloor = s.startMs
	e.pub = httptest.NewServer(s.PublicHandler())
	e.pair = httptest.NewServer(s.PairHandler())
	e.t.Cleanup(func() { e.pub.Close(); e.pair.Close() })
}

func (e *env) restart() {
	e.pub.Close()
	e.pair.Close()
	e.start()
}

func (e *env) phone() *phone.Phone {
	p := phone.New(e.cfg.ServerID, e.pub.URL, e.pair.URL)
	return p
}

func (e *env) paired() *phone.Phone {
	e.t.Helper()
	e.s.limits = newLimiter()
	uri, err := e.s.PairStart(false)
	if err != nil {
		e.t.Fatal(err)
	}
	p := e.phone()
	if err := p.Pair(uri); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func mustOK(t *testing.T, c *phone.Call, err error) *wire.RespHeader {
	t.Helper()
	if err != nil {
		t.Fatalf("call: %v (code %d, body %s)", err, c.Code, c.RawBody)
	}
	if c.Resp.Status != "ok" {
		t.Fatalf("status %s state %s code %s", c.Resp.Status, c.Resp.State, c.Resp.Code)
	}
	return c.Resp
}

func intake(t *testing.T, p *phone.Phone, item string, pw string) *wire.RespHeader {
	t.Helper()
	h := p.Build(wire.OpIntake, item, 0, "")
	if err := p.SealSecret(&h, wire.SecretPayload{Label: "IBKR login", Login: "jklumpe", URL: "https://ibkr.com", Password: pw}); err != nil {
		t.Fatal(err)
	}
	c, err := p.Send(h)
	return mustOK(t, c, err)
}

func TestPairIntakeReplaceDelete(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	r := intake(t, p, item, canary)
	if r.State != StateDone || r.Opid == "" || r.Iv != 1 {
		t.Fatalf("intake: %+v", r)
	}
	got, err := e.store.Read(context.Background(), "vault/given/"+item)
	if err != nil || !strings.HasPrefix(string(got), canary+"\nlogin: jklumpe\n") {
		t.Fatalf("entry: %v %q", err, got)
	}
	// list
	c, err := p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	lr := mustOK(t, c, err)
	if len(lr.Entries) != 1 || lr.Entries[0].Item != item || lr.Entries[0].Iv != 1 {
		t.Fatalf("list: %+v", lr)
	}
	m, err := p.OpenMeta("given.meta", lr.Entries[0])
	if err != nil || m.Label != "IBKR login" || m.Login != "jklumpe" {
		t.Fatalf("meta: %v %+v", err, m)
	}
	// replace
	h := p.Build(wire.OpReplace, item, 1, "")
	if err := p.SealSecret(&h, wire.SecretPayload{Label: "IBKR", Login: "jk", URL: "", Password: "new-" + canary}); err != nil {
		t.Fatal(err)
	}
	c, err = p.Send(h)
	if rr := mustOK(t, c, err); rr.Iv != 2 {
		t.Fatalf("replace iv %d", rr.Iv)
	}
	// stale version
	h = p.Build(wire.OpReplace, item, 1, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "x", Password: "y"})
	c, _ = p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:version_conflict" {
		t.Fatalf("want version_conflict, got %+v", c.Resp)
	}
	// delete
	c, err = p.Send(p.Build(wire.OpDelete, item, 2, ""))
	dr := mustOK(t, c, err)
	if dr.State != StateDone {
		t.Fatalf("delete: %+v", dr)
	}
	if ok, _ := e.store.Exists("vault/given/" + item); ok {
		t.Fatal("entry still exists after delete")
	}
	// tombstone rules
	c, _ = p.Send(p.Build(wire.OpDelete, item, 2, ""))
	if c.Resp.Status != "err:tombstoned" {
		t.Fatalf("second delete: %s", c.Resp.Status)
	}
	h = p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "x", Password: "y"})
	c, _ = p.Send(h)
	if c.Resp.Status != "err:tombstoned" {
		t.Fatalf("reuse of a tombstoned id: %s", c.Resp.Status)
	}
	// op.status of the delete, with a fresh rid
	c, err = p.Send(p.Build(wire.OpStatus, "", 0, dr.Opid))
	if sr := mustOK(t, c, err); sr.State != StateDone || sr.Opid != dr.Opid {
		t.Fatalf("status: %+v", sr)
	}
	checkCanary(t, e)
}

func checkCanary(t *testing.T, e *env) {
	t.Helper()
	_ = filepath.Walk(e.cfg.StateDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(canary)) || bytes.Contains(b, []byte("CANARY")) {
			t.Errorf("canary found in %s", p)
		}
		return nil
	})
}

func TestReplayAndRid(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	h := p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	hb, _ := wire.Marshal(h)
	c1, err := p.SendBytes(h, hb)
	mustOK(t, c1, err)
	c2, err := p.SendBytes(h, hb) // exact retry
	mustOK(t, c2, err)
	if !bytes.Equal(c1.RawBody, c2.RawBody) {
		t.Fatal("retry did not return the stored answer")
	}
	h2 := h
	h2.Ts++
	c3, _ := p.Send(h2) // same rid, other bytes
	if c3.Resp == nil || c3.Resp.Status != "err:rid_conflict" {
		t.Fatalf("want rid_conflict, got %+v", c3.Resp)
	}
	// read rid is single use
	lh := p.Build(wire.OpGivenList, "", 0, "")
	lb, _ := wire.Marshal(lh)
	c, err := p.SendBytes(lh, lb)
	mustOK(t, c, err)
	c, _ = p.SendBytes(lh, lb)
	if c.Resp == nil || c.Resp.Status != "err:rid_conflict" {
		t.Fatalf("read rid reuse: %+v", c.Resp)
	}
	// stale
	sh := p.Build(wire.OpGivenList, "", 0, "")
	sh.Ts -= 200_000
	c, _ = p.Send(sh)
	if c.Resp == nil || c.Resp.Status != "err:stale" {
		t.Fatalf("stale: %+v", c.Resp)
	}
	// wrong field for op
	bh := p.Build(wire.OpGivenList, "", 3, "")
	c, _ = p.Send(bh)
	if c.Resp == nil || c.Resp.Status != "err:bad_request" {
		t.Fatalf("iv on a list: %+v", c.Resp)
	}
	// wrong token: unsigned 401
	tok := p.RouteToken
	p.RouteToken = strings.Repeat("x", 40)
	c, _ = p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	if c.Code != 401 {
		t.Fatalf("bad token: %d", c.Code)
	}
	p.RouteToken = tok
	// unknown device: unsigned 401
	q := e.phone()
	q.RouteToken, q.Gen, q.BsigPub, q.Bsid = p.RouteToken, p.Gen, p.BsigPub, p.Bsid
	c, _ = q.Send(q.Build(wire.OpGivenList, "", 0, ""))
	if c.Code != 401 {
		t.Fatalf("unknown device: %d", c.Code)
	}
}

func TestTamperedCiphertext(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	h := p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	ct, _ := wire.DecodeB64(h.C, 81, 4096)
	ct[len(ct)-1] ^= 1
	h.C = wire.EncodeB64(ct)
	c, _ := p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:decrypt_failed" || c.Resp.State != StateFailed {
		t.Fatalf("tampered: %+v", c.Resp)
	}
	// AAD bound to the item: a ciphertext sealed for another item fails.
	other := vcrypto.RandomID()
	h = p.Build(wire.OpIntake, other, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	h.Item = vcrypto.RandomID()
	c, _ = p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:decrypt_failed" {
		t.Fatalf("moved ciphertext: %+v", c.Resp)
	}
	// the failed intake left no item and no file
	if ok, _ := e.store.Exists("vault/given/" + item); ok {
		t.Fatal("file after failed intake")
	}
	c, err := p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	if r := mustOK(t, c, err); len(r.Entries) != 0 {
		t.Fatal("pending item visible")
	}
}

func TestGateAndKinds(t *testing.T) {
	e := newEnv(t)
	e.cfg.BackupExclusion = ""
	e.restart()
	p := e.paired()
	h := p.Build(wire.OpIntake, vcrypto.RandomID(), 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "b"})
	c, _ := p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:not_ready" {
		t.Fatalf("gate: %+v", c.Resp)
	}
	// shared item via the admin path
	if err := e.store.Write(context.Background(), "ebay/password", []byte(canary+"\nlogin: johaklu_37\nurl: https://ebay.de\nnotes: x\n")); err != nil {
		t.Fatal(err)
	}
	id, err := e.s.Share(context.Background(), AdminRequest{Path: "ebay/password", Label: "eBay"})
	if err != nil {
		t.Fatal(err)
	}
	c, err = p.Send(p.Build(wire.OpSharedGet, id, 0, ""))
	r := mustOK(t, c, err)
	v, err := p.OpenValue(r)
	if err != nil || v.Password != canary || v.Login != "johaklu_37" || v.Label != "eBay" {
		t.Fatalf("shared value: %v %+v", err, v)
	}
	// kinds: delete on a shared item is wrong_kind
	c, _ = p.Send(p.Build(wire.OpDelete, id, 1, ""))
	if c.Resp == nil || c.Resp.Status != "err:wrong_kind" {
		t.Fatalf("delete shared: %+v", c.Resp)
	}
	// list shared
	c, err = p.Send(p.Build(wire.OpSharedList, "", 0, ""))
	lr := mustOK(t, c, err)
	if len(lr.Entries) != 1 {
		t.Fatalf("shared list: %+v", lr)
	}
	m, err := p.OpenMeta("shared.meta", lr.Entries[0])
	if err != nil || m.Label != "eBay" {
		t.Fatalf("shared meta: %v %+v", err, m)
	}
	// admin paths may not touch vault/
	if _, err := e.s.Share(context.Background(), AdminRequest{Path: "vault/given/x", Label: "x"}); err == nil {
		t.Fatal("share of vault/ accepted")
	}
	if _, err := e.s.Generate(context.Background(), AdminRequest{Path: "./vault/x", Label: "x"}); err == nil {
		t.Fatal("generate into ./vault accepted")
	}
	checkCanary(t, e)
}

func TestGenerateAndSecret(t *testing.T) {
	e := newEnv(t)
	id, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Site", Login: "joe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Site"}); err == nil {
		t.Fatal("second generate without --force accepted")
	}
	v, err := e.s.Secret(context.Background(), id)
	if err != nil || len(v) != 24 {
		t.Fatalf("secret: %v len %d", err, len(v))
	}
	raw, _ := e.store.Read(context.Background(), "site/password")
	if !strings.HasPrefix(string(raw), v+"\nlogin: joe\n") {
		t.Fatal("entry layout")
	}
	b, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "state.json"))
	if bytes.Contains(b, []byte(v)) {
		t.Fatal("generated value in state.json")
	}
}

func TestPairingRules(t *testing.T) {
	e := newEnv(t)
	uri, _ := e.s.PairStart(false)
	// wrong fingerprint in the URI: phone refuses the box keys
	P, _ := phone.ParseURI(uri)
	P.Fp = wire.EncodeB64(make([]byte, 32))
	pb, _ := wire.Marshal(P)
	q := e.phone()
	if err := q.Pair("agentd-vault://pair#" + wire.EncodeB64(pb)); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("wrong fp: %v", err)
	}
	// wrong tailnet identity
	e.who.mu.Lock()
	e.who.os = "linux"
	e.who.mu.Unlock()
	if err := e.phone().Pair(uri); err == nil {
		t.Fatal("pairing from a linux node accepted")
	}
	e.who.mu.Lock()
	e.who.os = "iOS"
	e.who.mu.Unlock()
	// good pairing
	e.s.limits = newLimiter()
	p := e.phone()
	if err := p.Pair(uri); err != nil {
		t.Fatal(err)
	}
	// confirm retry with the exact bytes returns the same device
	gen := p.Gen
	if err := p.Confirm(); err != nil || p.Gen != gen {
		t.Fatalf("confirm retry: %v", err)
	}
	// the URI is single use
	if err := e.phone().Pair(uri); err == nil {
		t.Fatal("second pairing with the same URI accepted")
	}
	// a second device needs --replace
	if _, err := e.s.PairStart(false); err == nil {
		t.Fatal("pair-start without --replace accepted while paired")
	}
	e.s.limits = newLimiter()
	uri2, err := e.s.PairStart(true)
	if err != nil {
		t.Fatal(err)
	}
	p2 := e.phone()
	if err := p2.Pair(uri2); err != nil {
		t.Fatal(err)
	}
	c, _ := p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	if c.Code != 401 {
		t.Fatalf("old device after --replace: %d", c.Code)
	}
	c, err = p2.Send(p2.Build(wire.OpGivenList, "", 0, ""))
	mustOK(t, c, err)
	// expiry
	e.s.limits = newLimiter()
	uri3, _ := e.s.PairStart(true)
	e.s.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if err := e.phone().Pair(uri3); err == nil {
		t.Fatal("expired pairing accepted")
	}
	e.s.now = time.Now
}

func TestPairingBadMacAttempts(t *testing.T) {
	e := newEnv(t)
	uri, _ := e.s.PairStart(false)
	P, _ := phone.ParseURI(uri)
	good := P.S
	P.S = wire.EncodeB64(bytes.Repeat([]byte{7}, 32))
	pb, _ := wire.Marshal(P)
	bad := "agentd-vault://pair#" + wire.EncodeB64(pb)
	for i := 0; i < pairMaxAttempts; i++ {
		if err := e.phone().Pair(bad); err == nil {
			t.Fatal("wrong secret accepted")
		}
	}
	P.S = good
	pb, _ = wire.Marshal(P)
	e.s.limits = newLimiter()
	if err := e.phone().Pair("agentd-vault://pair#" + wire.EncodeB64(pb)); err == nil {
		t.Fatal("record survived the attempt limit")
	}
}

func TestUnpairAndReset(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	c, err := p.Send(p.Build(wire.OpUnpair, "", 0, ""))
	mustOK(t, c, err)
	c, _ = p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	if c.Code != 401 {
		t.Fatalf("after unpair: %d", c.Code)
	}
	p = e.paired()
	old := e.s.key.Bsid
	if _, err := e.s.ResetKeys(); err != nil {
		t.Fatal(err)
	}
	if e.s.key.Bsid == old {
		t.Fatal("keys not replaced")
	}
	c, _ = p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	if c.Code != 401 {
		t.Fatalf("after reset: %d", c.Code)
	}
	ents, _ := os.ReadDir(filepath.Join(e.cfg.StateDir, "keys"))
	if len(ents) != 2 {
		t.Fatalf("old key files kept: %d", len(ents))
	}
	// restart loads the new keys and a new pairing works
	e.restart()
	if e.s.key.Bsid == old {
		t.Fatal("restart loaded old keys")
	}
	e.paired()
}

func TestCrashRecovery(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	h := p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	hb, _ := wire.Marshal(h)
	// Simulate a crash right after the durable journal write (step 3).
	e.s.mu.Lock()
	op := &Op{Rid: h.Rid, Dev: p.Dev, Gen: p.Gen, Rh: vcrypto.Hash(hb), Header: hb, Op: h.Op, Item: item,
		State: StateApplying, Created: e.s.nowMs()}
	e.s.journal(op)
	if err := e.s.save(); err != nil {
		t.Fatal(err)
	}
	e.s.mu.Unlock()
	e.restart()
	p.API, p.PairURL = e.pub.URL, e.pair.URL
	got, err := e.store.Read(context.Background(), "vault/given/"+item)
	if err != nil || !strings.HasPrefix(string(got), canary) {
		t.Fatalf("recovery did not write: %v", err)
	}
	// the original request, retried, gets the finished op
	c, err := p.SendBytes(h, hb)
	if r := mustOK(t, c, err); r.State != StateDone || r.Iv != 1 {
		t.Fatalf("retry after recovery: %+v", r)
	}
	checkCanary(t, e)
}

func TestConcurrency(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	intake(t, p, item, canary)
	// same request twice in parallel: one op, the same final answer
	h := p.Build(wire.OpReplace, item, 1, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "v2"})
	hb, _ := wire.Marshal(h)
	var wg sync.WaitGroup
	res := make([]*phone.Call, 2)
	for i := range res {
		wg.Add(1)
		go func(i int) { defer wg.Done(); res[i], _ = p.SendBytes(h, hb) }(i)
	}
	wg.Wait()
	for _, c := range res {
		if c.Resp == nil || c.Resp.Status != "ok" || (c.Resp.State != StateDone && c.Resp.State != StateApplying) {
			t.Fatalf("parallel duplicate: %+v", c.Resp)
		}
	}
	c, err := p.Send(p.Build(wire.OpStatus, "", 0, h.Rid))
	if r := mustOK(t, c, err); r.State != StateDone || r.Iv != 2 {
		t.Fatalf("status after parallel duplicate: %+v", r)
	}
	// two different replaces with the same iv: exactly one wins
	wins := 0
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := p.Build(wire.OpReplace, item, 2, "")
			_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "v3"})
			c, _ := p.Send(h)
			mu.Lock()
			defer mu.Unlock()
			if c.Resp != nil && c.Resp.Status == "ok" {
				wins++
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("same-iv replaces: %d winners", wins)
	}
}

func newHTTP(t *testing.T, h http.Handler) *httptest.Server {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}
