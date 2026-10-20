package vault

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// Config is the daemon configuration.
type Config struct {
	ServerID        string   // srv, e.g. agent.gojoe.run
	PublicAddr      string   // 100.120.65.8:8790
	PairAddr        string   // 100.120.65.8:8791
	AdminSock       string   // $XDG_RUNTIME_DIR/vaultd/admin.sock
	StateDir        string   // ~/.local/state/vaultd
	StoreRoot       string   // ~/.password-store
	GPGHome         string   // tests only
	RouteToken      string   // resolved secret
	PairLogin       string   // tailnet owner login allowed to pair
	PairOS          string   // "iOS"
	MaxDevices      int      // 1
	BackupExclusion string   // clawd commit of the backup change; "" = intake not ready
	NotifyCmd       []string // argv; the notice text is appended
	TestMode        bool
}

// Whois resolves a remote address to a tailnet identity.
type Whois func(ctx context.Context, remoteAddr string) (login, os, node string, err error)

// Server is the vaultd daemon.
type Server struct {
	cfg   Config
	store passstore.Store
	whois Whois
	now   func() time.Time

	dev sync.RWMutex // D: requests read-lock; unpair, pairing completion, reset write-lock
	mu  sync.Mutex   // M: guards st, keys, reserved
	st  *State
	key *BoxKeys

	reserved map[string]*reservation // rid -> claim of a mutation being validated and journaled
	reads    map[string]*ReadRec     // rid -> (dev, rh, ts) of reads, 10 minutes, memory only
	locks    *pathLocks              // per store path
	applyRun sync.Mutex              // one runApplying at a time

	limits    *limiter
	audit     *auditLog
	startMs   int64         // start time
	readFloor int64         // reads signed at or before this are stale (read rids are memory only)
	active    chan struct{} // at most 8 public requests in work at once
}

// New loads (or creates) state and keys and runs journal recovery.
func New(cfg Config, store passstore.Store, whois Whois) (*Server, error) {
	if !wire.ValidSrv(cfg.ServerID) {
		return nil, errors.New("server_id invalid")
	}
	if cfg.MaxDevices <= 0 {
		cfg.MaxDevices = 1
	}
	if len(cfg.RouteToken) < 32 || len(cfg.RouteToken) > 128 || wire.HasControl(cfg.RouteToken) {
		return nil, errors.New("route token must be 32 to 128 printable characters")
	}
	if err := mkdirAllDurable(cfg.StateDir); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: store, whois: whois, now: time.Now,
		reserved: map[string]*reservation{}, reads: map[string]*ReadRec{}, locks: newPathLocks(), limits: newLimiter(),
		active: make(chan struct{}, 8)}
	if err := passstore.CheckConfig(); err != nil {
		return nil, err
	}
	if err := cleanStateTemps(cfg.StateDir); err != nil {
		return nil, fmt.Errorf("state temp sweep: %w", err)
	}
	st, err := loadState(s.statePath())
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	s.st = st
	if st.Bsid == "" {
		k := newBoxKeys()
		if err := saveBoxKeys(cfg.StateDir, k); err != nil {
			return nil, err
		}
		st.Bsid, st.Bkid = k.Bsid, k.Bkid
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	k, err := loadBoxKeys(cfg.StateDir, st.Bsid, st.Bkid)
	if err != nil {
		return nil, fmt.Errorf("keys: %w", err)
	}
	s.key = k
	if err := removeOtherKeys(cfg.StateDir, k.Bsid, k.Bkid); err != nil {
		return nil, fmt.Errorf("retiring old key files: %w", err)
	}
	if n, err := store.CleanTemps(); err != nil {
		return nil, fmt.Errorf("store temp sweep: %w", err)
	} else if n > 0 {
		log.Printf("vaultd: removed %d leftover store temp files", n)
	}
	a, err := openAudit(filepath.Join(cfg.StateDir, "audit.jsonl"))
	if err != nil {
		return nil, err
	}
	s.audit = a
	// Recovery before any listener starts (spec 7, R5). Every op found
	// applying may have written before the crash: it is uncertain.
	marked := false
	for _, op := range st.Ops {
		if op.State == StateApplying && !op.Uncertain {
			op.Uncertain, marked = true, true
		}
	}
	if marked {
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("persisting recovery marks: %w", err)
		}
	}
	if err := s.fixPendingShared(); err != nil {
		return nil, err
	}
	s.runApplying(context.Background(), false)
	s.startMs = s.nowMs()
	s.readFloor = s.startMs + 30_000
	return s, nil
}

func (s *Server) statePath() string { return filepath.Join(s.cfg.StateDir, "state.json") }

// save writes state.json durably. Caller holds mu. A plain error means the
// old file is intact and the caller rolls its in-memory change back. An
// uncertain outcome (renamed, dir fsync failed) stops the daemon: memory and
// disk may disagree, so it restarts from durable state (spec 19 R2).
func (s *Server) save() error {
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	err = writeFileDurable(s.statePath(), b)
	if errors.Is(err, errStateUncertain) {
		fatalf("vaultd: state.json write outcome uncertain; restarting from durable state")
	}
	return err
}

func (s *Server) nowMs() int64 { return s.now().UnixMilli() }

// ---- HTTP plumbing ----

func writeJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func unsigned(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, []byte(`{"error":"`+msg+`"}`))
}

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		unsigned(w, http.StatusRequestEntityTooLarge, "too_large")
		return nil, false
	}
	return b, true
}

// unauthorized counts a failure and answers 401, or 429 during a block.
func (s *Server) unauthorized(w http.ResponseWriter) {
	if s.limits.failure(s.now()) {
		unsigned(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	unsigned(w, http.StatusUnauthorized, "unauthorized")
}

// jsonType requires Content-Type application/json (parameters allowed).
func jsonType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// PublicHandler serves /vault/v1/* except pairing.
func (s *Server) PublicHandler() http.Handler {
	return http.HandlerFunc(s.servePublic)
}

func (s *Server) servePublic(w http.ResponseWriter, r *http.Request) {
	op, ok := wire.Routes[r.URL.Path]
	if !ok {
		unsigned(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		unsigned(w, http.StatusMethodNotAllowed, "method")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		unsigned(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !jsonType(r) {
		unsigned(w, http.StatusUnsupportedMediaType, "content_type")
		return
	}
	body, ok := readBody(w, r, 64<<10)
	if !ok {
		return
	}
	if !s.limits.allowPublic(s.now()) {
		unsigned(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	case <-time.After(5 * time.Second):
		unsigned(w, http.StatusServiceUnavailable, "busy")
		return
	}
	tok := r.Header.Get("Authorization")
	want := "Bearer " + s.cfg.RouteToken
	if subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
		if s.limits.failure(s.now()) {
			unsigned(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		unsigned(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var env wire.Envelope
	if err := wire.StrictDecode(body, &env); err != nil {
		s.unauthorized(w)
		return
	}
	hb, err1 := wire.DecodeB64(env.H, 1, 64<<10)
	sig, err2 := wire.DecodeB64Exact(env.S, 64)
	var h wire.ReqHeader
	var err3 error
	if err1 == nil {
		err3 = wire.StrictDecode(hb, &h)
	}
	if err1 != nil || err2 != nil || err3 != nil {
		s.unauthorized(w)
		return
	}

	if op == wire.OpUnpair {
		// Unpair takes D for writing itself, so it runs outside the read lock.
		s.reply(w, nil, s.unpairSelf(hb, sig, h, r.URL.Path))
		return
	}
	s.dev.RLock()
	resp := s.handleVerified(r.Context(), r.URL.Path, op, hb, sig, h)
	s.dev.RUnlock()
	if resp == nil {
		if s.limits.blocked(s.now()) {
			unsigned(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		unsigned(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// verify checks the device and signature and the semantic rules. It returns
// the request, or (nil, nil) for an unsigned 401, or (rq, resp) for a signed
// error. Caller holds D (read or write).
func (s *Server) verify(path, op string, hb, sig []byte, h wire.ReqHeader) (*request, []byte) {
	s.mu.Lock()
	d := s.st.activeDevice(h.Dev)
	var dev Device
	if d != nil {
		dev = *d
	}
	s.mu.Unlock()
	if d == nil || !vcrypto.Verify(dev.SigPub, vcrypto.Cat([]byte(vcrypto.LabelReq), hb), sig) {
		s.limits.failure(s.now())
		return nil, nil
	}
	rq := &request{h: h, hb: hb, rh: vcrypto.Hash(hb), dev: dev}
	if h.Path != path || h.Op != op || h.Srv != s.cfg.ServerID || h.Gen != dev.Gen || h.CheckFields() != nil {
		return rq, s.errResp(rq, "bad_request")
	}
	if h.Iv > maxIv || (wire.IsMutation(op) && h.Op != wire.OpDelete && h.Bkid != s.key.Bkid) {
		return rq, s.errResp(rq, "bad_request")
	}
	now := s.nowMs()
	if h.Ts < now-120_000 || h.Ts > now+30_000 {
		return rq, s.errResp(rq, "stale")
	}
	if !wire.IsMutation(op) && h.Ts <= s.readFloor {
		// Read rids are memory only. A read signed before this start, or up
		// to the 30 s of allowed future skew after it, could have been seen
		// by the previous process: refuse it (code r2 #34). Cost: reads
		// fail as stale for about 30 s after a restart.
		return rq, s.errResp(rq, "stale")
	}
	if !s.limits.allowDevice(dev.Dev, wire.IsMutation(op), s.now()) {
		return rq, s.errResp(rq, "rate_limited")
	}
	return rq, nil
}

func (s *Server) handleVerified(ctx context.Context, path, op string, hb, sig []byte, h wire.ReqHeader) []byte {
	rq, errResp := s.verify(path, op, hb, sig, h)
	if rq == nil {
		return nil
	}
	if errResp != nil {
		return errResp
	}
	var resp []byte
	if wire.IsMutation(op) {
		resp = s.mutate(ctx, rq)
	} else {
		resp = s.read(ctx, rq)
	}
	if resp == nil {
		resp = s.errResp(rq, "internal")
	}
	return resp
}

// request is one verified request.
type request struct {
	h   wire.ReqHeader
	hb  []byte
	rh  []byte
	dev Device
}

func (s *Server) reply(w http.ResponseWriter, _ *request, resp []byte) {
	if resp == nil && s.limits.blocked(s.now()) {
		unsigned(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if resp == nil {
		unsigned(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// baseResp fills the common response fields.
func (s *Server) baseResp(rq *request) wire.RespHeader {
	return wire.RespHeader{V: 1, Srv: s.cfg.ServerID, Dir: "resp", Op: rq.h.Op,
		Rh: wire.EncodeB64(rq.rh), Dev: rq.dev.Dev, Gen: rq.dev.Gen, Bsid: s.key.Bsid,
		Ts: s.nowMs(), Status: "ok", Entries: []wire.Entry{}}
}

// sign serializes and signs a response header into an envelope.
func (s *Server) sign(h wire.RespHeader) []byte {
	if h.Entries == nil {
		h.Entries = []wire.Entry{}
	}
	hb, err := wire.Marshal(h)
	if err != nil {
		log.Printf("vaultd: marshal response: %v", err)
		return nil
	}
	sig := s.key.Sign.Sign(vcrypto.Cat([]byte(vcrypto.LabelResp), hb))
	env, err := wire.Marshal(wire.Envelope{H: wire.EncodeB64(hb), S: wire.EncodeB64(sig)})
	if err != nil {
		return nil
	}
	return env
}

func (s *Server) errResp(rq *request, code string) []byte {
	h := s.baseResp(rq)
	h.Status = "err:" + code
	return s.sign(h)
}

// notify runs notify_cmd with a fixed text. Best effort, never blocks a request.
func (s *Server) notify(text string) {
	if len(s.cfg.NotifyCmd) == 0 {
		return
	}
	argv := append(append([]string(nil), s.cfg.NotifyCmd...), text)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		if err := cmd.Run(); err != nil {
			log.Printf("vaultd: notify failed: %v", err)
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
}

// Serve runs the public and pairing listeners and the admin socket until ctx
// ends.
func (s *Server) Serve(ctx context.Context) error {
	mk := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
			MaxHeaderBytes: 8 << 10}
	}
	pub := mk(s.cfg.PublicAddr, s.PublicHandler())
	pair := mk(s.cfg.PairAddr, s.PairHandler())
	errc := make(chan error, 3)
	go func() { errc <- pub.ListenAndServe() }()
	go func() { errc <- pair.ListenAndServe() }()
	adm, err := s.listenAdmin()
	if err != nil {
		return err
	}
	go func() { errc <- s.serveAdmin(ctx, adm) }()
	go s.retryLoop(ctx)
	log.Printf("vaultd: public %s, pairing %s, admin %s", s.cfg.PublicAddr, s.cfg.PairAddr, s.cfg.AdminSock)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	_ = pub.Shutdown(sctx)
	_ = pair.Shutdown(sctx)
	_ = adm.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
