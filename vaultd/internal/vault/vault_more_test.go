package vault

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/phone"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// faultStore injects write errors in order; nil entries call through.
type faultStore struct {
	passstore.Store
	mu   sync.Mutex
	errs []error
	last []byte // content of the last write that reached the disk
}

func (f *faultStore) Write(ctx context.Context, path string, content []byte) error {
	f.mu.Lock()
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	f.mu.Unlock()
	if err != nil {
		if errors.Is(err, passstore.ErrUncertain) {
			// The rename happened: the file is in place, durability unknown.
			if werr := f.Store.Write(ctx, path, content); werr != nil {
				panic("fault store: underlying write failed: " + werr.Error())
			}
			f.mu.Lock()
			f.last = append([]byte(nil), content...)
			f.mu.Unlock()
		}
		return err
	}
	if err := f.Store.Write(ctx, path, content); err != nil {
		return err
	}
	f.mu.Lock()
	f.last = append([]byte(nil), content...)
	f.mu.Unlock()
	return nil
}

func newFaultEnv(t *testing.T) (*env, *faultStore) {
	e := newEnv(t)
	fs := &faultStore{Store: e.store}
	e.pub.Close()
	e.pair.Close()
	s, err := New(e.cfg, fs, e.who.fn)
	if err != nil {
		t.Fatal(err)
	}
	s.readFloor = s.startMs
	e.s = s
	e.pub = newHTTP(t, s.PublicHandler())
	e.pair = newHTTP(t, s.PairHandler())
	return e, fs
}

func TestUncertainWriteThenPreRenameFailure(t *testing.T) {
	e, fs := newFaultEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	fs.errs = []error{passstore.ErrUncertain, errors.New("gpg failed, exit 2")}
	h := p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	c, err := p.Send(h)
	r := mustOK(t, c, err)
	if r.State != StateApplying {
		t.Fatalf("uncertain write must stay applying, got %s", r.State)
	}
	// Second attempt fails BEFORE the rename: still never "failed".
	e.s.runApplying(context.Background(), true)
	c, err = p.Send(p.Build(wire.OpStatus, "", 0, r.Opid))
	if sr := mustOK(t, c, err); sr.State != StateApplying {
		t.Fatalf("after a later pre-rename error: %s", sr.State)
	}
	// Third attempt succeeds.
	e.s.runApplying(context.Background(), true)
	c, err = p.Send(p.Build(wire.OpStatus, "", 0, r.Opid))
	if sr := mustOK(t, c, err); sr.State != StateDone || sr.Iv != 1 {
		t.Fatalf("final: %+v", sr)
	}
	if got, _ := e.store.Read(context.Background(), givenPath(item)); !strings.HasPrefix(string(got), canary) {
		t.Fatal("entry missing")
	}
}

func TestPreRenameFailureIsTerminal(t *testing.T) {
	e, fs := newFaultEnv(t)
	p := e.paired()
	fs.errs = []error{errors.New("gpg failed, exit 2")}
	h := p.Build(wire.OpIntake, vcrypto.RandomID(), 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	c, _ := p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:internal" || c.Resp.State != StateFailed {
		t.Fatalf("first-attempt pre-rename failure: %+v", c.Resp)
	}
}

func TestOpStatusKeepsOriginalVersion(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	first := intake(t, p, item, canary)
	h := p.Build(wire.OpReplace, item, 1, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "v2"})
	c, err := p.Send(h)
	mustOK(t, c, err)
	c, err = p.Send(p.Build(wire.OpStatus, "", 0, first.Opid))
	if r := mustOK(t, c, err); r.Iv != 1 {
		t.Fatalf("status of the intake reports iv %d, want 1", r.Iv)
	}
}

func TestFullOpTableDoesNotGrow(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	intake(t, p, item, canary)
	e.s.mu.Lock()
	now := e.s.nowMs()
	for len(e.s.st.Ops) < maxOps {
		rid := vcrypto.RandomID()
		e.s.st.Ops[rid] = &Op{Rid: rid, State: StateDone, Finished: now}
	}
	e.s.mu.Unlock()
	h := p.Build(wire.OpIntake, vcrypto.RandomID(), 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "b"})
	c, _ := p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:full" || c.Resp.Opid != "" {
		t.Fatalf("intake on a full table: %+v", c.Resp)
	}
	c, _ = p.Send(p.Build(wire.OpDelete, vcrypto.RandomID(), 1, ""))
	if c.Resp == nil || c.Resp.Status != "err:not_found" || c.Resp.Opid != "" {
		t.Fatalf("invalid delete on a full table must not be journaled: %+v", c.Resp)
	}
	e.s.mu.Lock()
	n := len(e.s.st.Ops)
	e.s.mu.Unlock()
	if n != maxOps {
		t.Fatalf("table grew to %d", n)
	}
	c, err := p.Send(p.Build(wire.OpDelete, item, 1, ""))
	if r := mustOK(t, c, err); r.State != StateDone {
		t.Fatalf("valid delete on a full table: %+v", r)
	}
}

func TestHelloRetryAfterDoneAndConcurrentConfirms(t *testing.T) {
	e := newEnv(t)
	uri, _ := e.s.PairStart(false)
	p := e.phone()
	if err := p.Pair(uri); err != nil {
		t.Fatal(err)
	}
	// hello retry after done: the same answer, no attempt counted
	e.s.limits = newLimiter()
	for i := 0; i < pairMaxAttempts+1; i++ {
		resp, err := http.Post(e.pair.URL+"/vault/v1/pair/hello", "application/json", bytes.NewReader(p.PendingHello))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("hello retry %d: %v %d", i, err, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// concurrent identical confirms all get the stored answer
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(e.pair.URL+"/vault/v1/pair/confirm", "application/json", bytes.NewReader(p.PendingConfirm))
			if err == nil {
				codes[i] = resp.StatusCode
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("confirm %d: %d", i, c)
		}
	}
}

func TestReplaceRacesDelete(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	for round := 0; round < 5; round++ {
		item := vcrypto.RandomID()
		intake(t, p, item, canary)
		var wg sync.WaitGroup
		var rep, del *phone.Call
		wg.Add(2)
		go func() {
			defer wg.Done()
			h := p.Build(wire.OpReplace, item, 1, "")
			_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: "v2"})
			rep, _ = p.Send(h)
		}()
		go func() { defer wg.Done(); del, _ = p.Send(p.Build(wire.OpDelete, item, 1, "")) }()
		wg.Wait()
		repOK := rep.Resp != nil && rep.Resp.Status == "ok"
		delOK := del.Resp != nil && del.Resp.Status == "ok"
		if repOK == delOK {
			t.Fatalf("round %d: replace ok=%v delete ok=%v (want exactly one)", round, repOK, delOK)
		}
		exists, _ := e.store.Exists(givenPath(item))
		if delOK && exists {
			t.Fatalf("round %d: delete won but the file exists", round)
		}
		if repOK && !exists {
			t.Fatalf("round %d: replace won but the file is gone", round)
		}
	}
}

func TestRevokedDeviceCannotPairAgain(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	c, err := p.Send(p.Build(wire.OpUnpair, "", 0, ""))
	mustOK(t, c, err)
	uri, _ := e.s.PairStart(false)
	q := e.phone()
	q.Sign, q.Dev = p.Sign, p.Dev // the same signing key as the revoked device
	e.s.limits = newLimiter()
	if err := q.Pair(uri); err == nil {
		t.Fatal("a revoked device id paired again")
	}
}

func TestRejectedReplacementKeepsActiveDevice(t *testing.T) {
	e := newEnv(t)
	old := e.paired()
	c, err := old.Send(old.Build(wire.OpUnpair, "", 0, ""))
	mustOK(t, c, err)
	p := e.paired() // the active device
	uri, _ := e.s.PairStart(true)
	q := e.phone()
	q.Sign, q.Dev = old.Sign, old.Dev // revoked id: confirm must be refused
	e.s.limits = newLimiter()
	if err := q.Pair(uri); err == nil {
		t.Fatal("replacement with a revoked id accepted")
	}
	c, err = p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	mustOK(t, c, err)
}

func TestReadReplayAfterRestart(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	h := p.Build(wire.OpGivenList, "", 0, "")
	hb, _ := wire.Marshal(h)
	c, err := p.SendBytes(h, hb)
	mustOK(t, c, err)
	e.restart()
	p.API, p.PairURL = e.pub.URL, e.pair.URL
	c, _ = p.SendBytes(h, hb)
	if c.Resp == nil || c.Resp.Status != "err:stale" {
		t.Fatalf("read replay after restart: %+v", c.Resp)
	}
}

func TestRecoveredRetryIdenticalBytes(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	h := p.Build(wire.OpIntake, item, 0, "")
	_ = p.SealSecret(&h, wire.SecretPayload{Label: "a", Password: canary})
	hb, _ := wire.Marshal(h)
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
	c1, err := p.SendBytes(h, hb)
	mustOK(t, c1, err)
	c2, err := p.SendBytes(h, hb)
	mustOK(t, c2, err)
	if !bytes.Equal(c1.RawBody, c2.RawBody) {
		t.Fatal("recovered op answers with different bytes")
	}
}

func TestStartupSweepsTemps(t *testing.T) {
	e := newEnv(t)
	tmp := filepath.Join(e.store.Root, passstore.TempPrefix+"leftover")
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := filepath.Join(e.cfg.StateDir, ".tmp-leftover")
	if err := os.WriteFile(st, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.restart()
	for _, f := range []string{tmp, st} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived the start", f)
		}
	}
}

func TestBusyBeforeTombstone(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item := vcrypto.RandomID()
	intake(t, p, item, canary)
	e.s.mu.Lock()
	e.s.st.Items[item].Busy = "x"
	e.s.mu.Unlock()
	c, _ := p.Send(p.Build(wire.OpDelete, item, 1, ""))
	if c.Resp == nil || c.Resp.Status != "err:busy" {
		t.Fatalf("delete on a busy item: %+v", c.Resp)
	}
}

func TestReadFloorSkew(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	e.s.readFloor = e.s.startMs + 30_000
	h := p.Build(wire.OpGivenList, "", 0, "")
	h.Ts = e.s.startMs + 20_000 // future-dated within the allowed skew
	c, _ := p.Send(h)
	if c.Resp == nil || c.Resp.Status != "err:stale" {
		t.Fatalf("future-dated read inside the floor: %+v", c.Resp)
	}
}

func TestFirstConfirmRace(t *testing.T) {
	e := newEnv(t)
	uri, _ := e.s.PairStart(false)
	p := e.phone()
	if err := p.PairPrepare(uri); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Post(e.pair.URL+"/vault/v1/pair/confirm", "application/json", bytes.NewReader(p.PendingConfirm))
			if err == nil {
				codes[i] = resp.StatusCode
				resp.Body.Close()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("first confirm %d: %d", i, c)
		}
	}
	e.s.mu.Lock()
	n := e.s.st.activeCount()
	e.s.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d active devices after racing confirms", n)
	}
}

func TestGenerateUncertainThenPreRenameFailure(t *testing.T) {
	e, fs := newFaultEnv(t)
	fs.errs = []error{passstore.ErrUncertain, errors.New("gpg failed, exit 2")}
	_, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Site"})
	if err == nil || !strings.Contains(err.Error(), "not durable yet") {
		t.Fatalf("generate: %v", err)
	}
	fs.mu.Lock()
	first := append([]byte(nil), fs.last...)
	fs.mu.Unlock()
	e.s.runApplying(context.Background(), true) // pre-rename error: stays applying
	e.s.mu.Lock()
	it := e.s.sharedByPath("site/password")
	e.s.mu.Unlock()
	if it == nil || it.Busy == "" {
		t.Fatal("the item must survive a later pre-rename error")
	}
	e.s.runApplying(context.Background(), true) // succeeds with the SAME value
	got, _ := e.store.Read(context.Background(), "site/password")
	if !bytes.Equal(got, first) {
		t.Fatal("recovery wrote a different value than the first attempt")
	}
	e.s.mu.Lock()
	it = e.s.sharedByPath("site/password")
	e.s.mu.Unlock()
	if it == nil || it.Status != StatusLive || it.Busy != "" || it.Label != "Site" {
		t.Fatalf("item after recovery: %+v", it)
	}
}

func TestForceGenerateFailureKeepsMetadata(t *testing.T) {
	e, fs := newFaultEnv(t)
	id, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Old", Login: "a"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.Read(context.Background(), "site/password")
	fs.errs = []error{errors.New("gpg failed, exit 2")}
	if _, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "New", Login: "b", Force: true}); err == nil {
		t.Fatal("failed force generate reported success")
	}
	e.s.mu.Lock()
	it := e.s.st.Items[id]
	e.s.mu.Unlock()
	if it.Label != "Old" || it.Login != "a" || it.Busy != "" {
		t.Fatalf("metadata changed by a failed generate: %+v", it)
	}
	after, _ := e.store.Read(context.Background(), "site/password")
	if !bytes.Equal(before, after) {
		t.Fatal("entry changed by a failed generate")
	}
}

func TestPendingSharedLookupErrorStopsStart(t *testing.T) {
	e := newEnv(t)
	e.s.mu.Lock()
	id := vcrypto.RandomID()
	e.s.st.Items[id] = &Item{ID: id, Kind: KindShared, Status: StatusPending, PassPath: "linked/entry"}
	if err := e.s.save(); err != nil {
		t.Fatal(err)
	}
	e.s.mu.Unlock()
	if err := os.Symlink(t.TempDir(), filepath.Join(e.store.Root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(e.cfg, e.store, e.who.fn); err == nil {
		t.Fatal("start succeeded although a pending item could not be checked")
	}
}

func TestFatalOnUncertainStateWrite(t *testing.T) {
	e := newEnv(t)
	type fatal struct{}
	oldFatal, oldSync := fatalf, stateDirSync
	defer func() { fatalf, stateDirSync = oldFatal, oldSync }()
	fatalf = func(string, ...any) { panic(fatal{}) } // non-returning, like log.Fatalf
	calls := 0
	stateDirSync = func(dir string) error {
		calls++
		if calls == 2 { // the journal write succeeds, the final one is uncertain
			return errors.New("injected fsync failure")
		}
		return oldSync(dir)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("an uncertain state write did not stop the daemon")
			} else if _, ok := r.(fatal); !ok {
				panic(r)
			}
		}()
		_, _ = e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Site"})
	}()
	fatalf, stateDirSync = oldFatal, oldSync
	// The "process" died holding locks; restart from disk like systemd would.
	e.restart()
	e.s.mu.Lock()
	it := e.s.sharedByPath("site/password")
	applying := 0
	for _, op := range e.s.st.Ops {
		if op.State == StateApplying {
			applying++
		}
	}
	e.s.mu.Unlock()
	if it == nil || it.Status != StatusLive || applying != 0 {
		t.Fatalf("after restart: item %+v, applying ops %d", it, applying)
	}
	if ok, _ := e.store.Exists("site/password"); !ok {
		t.Fatal("entry missing after recovery")
	}
}

func TestGenerateRefusedOnFullJournal(t *testing.T) {
	e := newEnv(t)
	id, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Old"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.Read(context.Background(), "site/password")
	e.s.mu.Lock()
	now := e.s.nowMs()
	for len(e.s.st.Ops) < maxOps {
		rid := vcrypto.RandomID()
		e.s.st.Ops[rid] = &Op{Rid: rid, State: StateDone, Finished: now}
	}
	e.s.mu.Unlock()
	if _, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "New", Force: true}); err == nil {
		t.Fatal("generate on a full journal accepted")
	}
	e.s.mu.Lock()
	it, n := e.s.st.Items[id], len(e.s.st.Ops)
	e.s.mu.Unlock()
	after, _ := e.store.Read(context.Background(), "site/password")
	if n != maxOps || it.Label != "Old" || it.Busy != "" || !bytes.Equal(before, after) {
		t.Fatalf("refusal changed state: ops %d item %+v", n, it)
	}
}

func TestSharedGetBusyDuringUncertainGenerate(t *testing.T) {
	e, fs := newFaultEnv(t)
	p := e.paired()
	id, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "Old"})
	if err != nil {
		t.Fatal(err)
	}
	fs.errs = []error{passstore.ErrUncertain}
	if _, err := e.s.Generate(context.Background(), AdminRequest{Path: "site/password", Label: "New", Force: true}); err == nil {
		t.Fatal("uncertain generate reported success")
	}
	c, _ := p.Send(p.Build(wire.OpSharedGet, id, 0, ""))
	if c.Resp == nil || c.Resp.Status != "err:busy" {
		t.Fatalf("shared.get during an applying generate: %+v", c.Resp)
	}
	e.s.runApplying(context.Background(), true)
	c, err = p.Send(p.Build(wire.OpSharedGet, id, 0, ""))
	r := mustOK(t, c, err)
	v, err := p.OpenValue(r)
	if err != nil || v.Label != "New" {
		t.Fatalf("after recovery: %v %+v", err, v)
	}
	fs.mu.Lock()
	last := string(fs.last)
	fs.mu.Unlock()
	if !strings.HasPrefix(last, v.Password+"\n") {
		t.Fatal("shared.get value differs from the written entry")
	}
}

func TestStatusDuringReservation(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	item, rid := vcrypto.RandomID(), vcrypto.RandomID()
	e.s.mu.Lock()
	e.s.reserved[rid] = &reservation{dev: p.Dev, item: item, done: make(chan struct{})}
	e.s.mu.Unlock()
	c, err := p.Send(p.Build(wire.OpStatus, "", 0, rid))
	if r := mustOK(t, c, err); r.State != StateApplying || r.Item != item {
		t.Fatalf("status during a reservation: %+v", r)
	}
}

func TestPagination(t *testing.T) {
	e := newEnv(t)
	p := e.paired()
	e.s.mu.Lock()
	for i := 0; i < 51; i++ {
		id := vcrypto.RandomID()
		e.s.st.Items[id] = &Item{ID: id, Kind: KindGiven, Status: StatusLive, Version: 1, PassPath: givenPath(id), Label: "x"}
	}
	e.s.mu.Unlock()
	c, err := p.Send(p.Build(wire.OpGivenList, "", 0, ""))
	r1 := mustOK(t, c, err)
	if len(r1.Entries) != 50 || r1.Next == "" {
		t.Fatalf("page 1: %d entries, next %q", len(r1.Entries), r1.Next)
	}
	c, err = p.Send(p.Build(wire.OpGivenList, r1.Next, 0, ""))
	r2 := mustOK(t, c, err)
	if len(r2.Entries) != 1 || r2.Next != "" {
		t.Fatalf("page 2: %d entries, next %q", len(r2.Entries), r2.Next)
	}
}
