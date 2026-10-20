package vault

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/passstore"
	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// AdminRequest is one line on the admin socket.
type AdminRequest struct {
	Cmd     string `json:"cmd"`
	Replace bool   `json:"replace,omitempty"`
	Dev     string `json:"dev,omitempty"`
	Item    string `json:"item,omitempty"`
	Path    string `json:"path,omitempty"`
	Label   string `json:"label,omitempty"`
	Login   string `json:"login,omitempty"`
	URL     string `json:"url,omitempty"`
	Length  int    `json:"length,omitempty"`
	NoSym   bool   `json:"no_symbols,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// AdminResponse is the one-line answer. Secret is set only for "secret".
type AdminResponse struct {
	OK     bool   `json:"ok"`
	Out    string `json:"out,omitempty"`
	Err    string `json:"err,omitempty"`
	Secret string `json:"secret,omitempty"`
}

func (s *Server) listenAdmin() (*net.UnixListener, error) {
	dir := filepath.Dir(s.cfg.AdminSock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(s.cfg.AdminSock)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.cfg.AdminSock, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.cfg.AdminSock, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Server) serveAdmin(ctx context.Context, l *net.UnixListener) error {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.adminConn(ctx, c)
	}
}

func (s *Server) adminConn(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil || uid != os.Getuid() {
		return
	}
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	line, err := bufio.NewReader(&limitedReader{c, 16 << 10}).ReadBytes('\n')
	if err != nil {
		return
	}
	var req AdminRequest
	dec := json.NewDecoder(strings.NewReader(string(line)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeAdmin(c, AdminResponse{Err: "bad request"})
		return
	}
	writeAdmin(c, s.Admin(ctx, req))
}

type limitedReader struct {
	c *net.UnixConn
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("request too large")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.c.Read(p)
	l.n -= n
	return n, err
}

func writeAdmin(c *net.UnixConn, r AdminResponse) {
	b, _ := json.Marshal(r)
	_, _ = c.Write(append(b, '\n'))
}

// Admin runs one admin command (spec section 14).
func (s *Server) Admin(ctx context.Context, req AdminRequest) AdminResponse {
	fail := func(err error) AdminResponse { return AdminResponse{Err: err.Error()} }
	switch req.Cmd {
	case "pair-start":
		uri, err := s.PairStart(req.Replace)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Out: uri}
	case "devices":
		s.mu.Lock()
		var b strings.Builder
		for _, d := range s.st.Devices {
			st := "active"
			if d.Revoked {
				st = "revoked"
			}
			fmt.Fprintf(&b, "%s  %-12s  %s  %s\n", d.Dev, d.Name, time.UnixMilli(d.PairedAt).UTC().Format(time.RFC3339), st)
		}
		s.mu.Unlock()
		return AdminResponse{OK: true, Out: b.String()}
	case "unpair":
		if err := s.Unpair(req.Dev); err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Out: "ok"}
	case "reset-keys":
		fp, err := s.ResetKeys()
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Out: "ok, new fingerprint " + fp}
	case "generate":
		id, err := s.Generate(ctx, req)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Out: "ok item " + id}
	case "share":
		id, err := s.Share(ctx, req)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Out: "ok item " + id}
	case "list":
		return AdminResponse{OK: true, Out: s.listText()}
	case "secret":
		v, err := s.Secret(ctx, req.Item)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Secret: v}
	case "status":
		return AdminResponse{OK: true, Out: s.statusText()}
	}
	return fail(errors.New("unknown command"))
}

func checkMeta(label, login, url string) error {
	switch {
	case len(label) < 1 || len(label) > 100:
		return errors.New("label must be 1 to 100 bytes")
	case len(login) > 200 || len(url) > 500:
		return errors.New("login or url too long")
	}
	for _, v := range []string{label, login, url} {
		if wire.HasControl(v) {
			return errors.New("control character in metadata")
		}
	}
	return nil
}

const symbols = "-_.!@#%+="
const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func generatePassword(n int, noSym bool) string {
	alpha := alnum
	if !noSym {
		alpha += symbols
	}
	max := big.NewInt(int64(len(alpha)))
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, max) // uniform (rejection sampling inside)
		if err != nil {
			panic(err)
		}
		out[i] = alpha[v.Int64()]
	}
	return string(out)
}

// Generate writes a new password to an agent path and registers it as a
// shared item. The value never leaves vaultd. It is a journaled op like
// intake (code r2 #31, #32): under the path lock and M, the item is reserved
// (new: pending; existing: unchanged) and the op is written durably with the
// entry sealed to the box intake key; then the write runs; the new metadata
// is published only when the op is done. An uncertain write stays applying
// and the retry runner finishes the SAME value.
func (s *Server) Generate(ctx context.Context, req AdminRequest) (string, error) {
	if err := passstore.ValidAgentPath(req.Path); err != nil {
		return "", err
	}
	if err := checkMeta(req.Label, req.Login, req.URL); err != nil {
		return "", err
	}
	n := req.Length
	if n == 0 {
		n = 24
	}
	if n < 12 || n > 128 {
		return "", errors.New("length must be 12 to 128")
	}
	unlock := s.locks.lock(req.Path)
	defer unlock()
	exists, err := s.store.Exists(req.Path)
	if err != nil {
		return "", err
	}
	if exists && !req.Force {
		return "", errors.New("entry exists; use --force to overwrite it")
	}
	pw := generatePassword(n, req.NoSym)
	content := passstore.FormatEntry(pw, req.Login, req.URL, req.Label)
	defer wipe(content)

	s.mu.Lock()
	// Admission like a public mutation (code r3 #40): refuse before any
	// change when the op table or the applying-byte budget is full.
	if len(s.st.Ops) >= maxOps {
		s.pruneOps()
	}
	if len(s.st.Ops) >= maxOps || s.headerBytes()+len(content)+200 > maxHeaderBytes {
		s.mu.Unlock()
		return "", errors.New("journal is full; retry later")
	}
	it := s.sharedByPath(req.Path)
	var id string
	created := false
	switch {
	case it != nil && it.Busy != "":
		s.mu.Unlock()
		return "", errors.New("item is busy; retry later")
	case it != nil:
		id = it.ID
	default:
		if s.st.liveCount(KindShared) >= maxLivePerKind {
			s.mu.Unlock()
			return "", errors.New("too many shared items")
		}
		for {
			id = vcrypto.RandomID()
			_, dead := s.st.Tombstones[id]
			if s.st.Items[id] == nil && !dead {
				break
			}
		}
		created = true
	}
	blob, err := vcrypto.Seal(s.key.Intake.Public(), vcrypto.InfoGen, genAAD(id, req.Path), content)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	now := s.nowMs()
	op := &Op{Rid: vcrypto.RandomID(), Op: opGenerate, Item: id, Path: req.Path, State: StateApplying,
		Created: now, Blob: blob, Meta: &GenerateMeta{Label: req.Label, Login: req.Login, URL: req.URL}}
	if created {
		s.st.Items[id] = &Item{ID: id, Kind: KindShared, Status: StatusPending, Created: now, Updated: now,
			PassPath: req.Path, Label: req.Label, Login: req.Login, URL: req.URL}
	}
	s.st.Items[id].Busy = op.Rid
	s.st.Ops[op.Rid] = op
	if err := s.save(); err != nil {
		delete(s.st.Ops, op.Rid)
		if created {
			delete(s.st.Items, id)
		} else {
			s.st.Items[id].Busy = ""
		}
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()

	_, code := s.apply(ctx, op)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.finish(op, nil, code)
	if err := s.save(); err != nil {
		fatalf("vaultd: state write after generate failed: %v", err)
		return "", err
	}
	switch op.State {
	case StateDone:
		s.audit.write(s.nowMs(), "generate", "", id, "ok", "admin")
		return id, nil
	case StateApplying:
		return "", fmt.Errorf("item %s: the write is not durable yet; vaultd finishes the same value by itself (check with vaultd list)", id)
	}
	return "", errors.New("write failed (old entry intact)")
}

// Share registers an existing entry as a shared item.
func (s *Server) Share(ctx context.Context, req AdminRequest) (string, error) {
	if err := passstore.ValidAgentPath(req.Path); err != nil {
		return "", err
	}
	if err := checkMeta(req.Label, "", ""); err != nil {
		return "", err
	}
	unlock := s.locks.lock(req.Path)
	defer unlock()
	s.mu.Lock()
	if it := s.sharedByPath(req.Path); it != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("already shared as item %s", it.ID)
	}
	s.mu.Unlock()
	raw, err := s.store.Read(ctx, req.Path)
	if err != nil {
		return "", errors.New("entry not readable")
	}
	e, err := passstore.ParseEntry(raw)
	wipe(raw)
	if err != nil {
		return "", err
	}
	id, err := s.reserveShared(req.Path, req.Label, e.Login, e.URL)
	if err != nil {
		return "", err
	}
	if err := s.markShared(id, StatusLive); err != nil {
		return "", err
	}
	s.audit.write(s.nowMs(), "share", "", id, "ok", "admin")
	return id, nil
}

// sharedByPath returns the live or pending shared item of a path. Caller
// holds mu.
func (s *Server) sharedByPath(path string) *Item {
	for _, it := range s.st.Items {
		if it.Kind == KindShared && it.Status != StatusTombstoned && it.PassPath == path {
			return it
		}
	}
	return nil
}

// reserveShared creates (status pending) or reuses (force) the shared item
// of path, durably, with the capacity check under M.
func (s *Server) reserveShared(path, label, login, url string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowMs()
	if it := s.sharedByPath(path); it != nil {
		return "", fmt.Errorf("path is already shared as item %s", it.ID)
	}
	if s.st.liveCount(KindShared) >= maxLivePerKind {
		return "", errors.New("too many shared items")
	}
	var id string
	for {
		id = vcrypto.RandomID()
		_, dead := s.st.Tombstones[id]
		if s.st.Items[id] == nil && !dead {
			break
		}
	}
	s.st.Items[id] = &Item{ID: id, Kind: KindShared, Status: StatusPending, Created: now, Updated: now,
		PassPath: path, Label: label, Login: login, URL: url}
	if err := s.save(); err != nil {
		delete(s.st.Items, id)
		return "", err
	}
	return id, nil
}

func (s *Server) markShared(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.st.Items[id]
	if it == nil {
		return errors.New("item vanished")
	}
	if it.Status == status {
		return nil
	}
	prev := it.Status
	it.Status = status
	if err := s.save(); err != nil {
		it.Status = prev
		return err
	}
	return nil
}

func (s *Server) dropPendingShared(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.st.Items[id]
	if it == nil || it.Status != StatusPending {
		return nil
	}
	delete(s.st.Items, id)
	if err := s.save(); err != nil {
		s.st.Items[id] = it
		return err
	}
	return nil
}

// fixPendingShared resolves shared items left pending by a crash in Share:
// the entry exists -> live; Exists says absent -> dropped; a lookup error or
// a failed save stops the start (code r2 #33). Items with an applying
// generate op are left to the op. Runs in New, before any listener.
func (s *Server) fixPendingShared() error {
	changed := false
	for id, it := range s.st.Items {
		if it.Kind != KindShared || it.Status != StatusPending || it.Busy != "" {
			continue
		}
		ok, err := s.store.Exists(it.PassPath)
		if err != nil {
			return fmt.Errorf("pending shared item %s: %w", id, err)
		}
		if ok {
			it.Status = StatusLive
		} else {
			delete(s.st.Items, id)
		}
		changed = true
	}
	if changed {
		if err := s.save(); err != nil {
			return fmt.Errorf("saving resolved shared items: %w", err)
		}
	}
	return nil
}

// Secret returns line 1 of a live item's entry for `vaultd exec`. The value
// goes to the CLI process over the admin socket only.
func (s *Server) Secret(ctx context.Context, item string) (string, error) {
	if !wire.IsID(item) {
		return "", errors.New("bad item id")
	}
	s.mu.Lock()
	it := s.st.Items[item]
	var path string
	if it != nil && it.Status == StatusLive {
		path = it.PassPath
	}
	s.mu.Unlock()
	if path == "" {
		return "", errors.New("no such live item")
	}
	unlock := s.locks.lock(path)
	defer unlock()
	s.mu.Lock()
	it = s.st.Items[item]
	ok := it != nil && it.Status == StatusLive && it.PassPath == path && it.Busy == ""
	s.mu.Unlock()
	if !ok {
		return "", errors.New("item changed; retry")
	}
	raw, err := s.store.Read(ctx, path)
	if err != nil {
		return "", errors.New("entry not readable")
	}
	e, err := passstore.ParseEntry(raw)
	wipe(raw)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if cur := s.st.Items[item]; cur != nil {
		prev := cur.LastUse
		cur.LastUse = s.nowMs()
		if err := s.save(); err != nil {
			cur.LastUse = prev
			log.Printf("vaultd: state write after exec read failed: %v", err)
		}
	}
	s.mu.Unlock()
	s.audit.write(s.nowMs(), "exec", "", item, "ok", "admin")
	return e.Password, nil
}

func (s *Server) listText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []*Item
	for _, it := range s.st.Items {
		if it.Status == StatusLive {
			items = append(items, it)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created < items[j].Created })
	var b strings.Builder
	for _, it := range items {
		lu := "-"
		if it.LastUse > 0 {
			lu = time.UnixMilli(it.LastUse).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%s  %-6s  %-30s  created %s  last_use %s\n", it.ID, it.Kind, it.Label,
			time.UnixMilli(it.Created).UTC().Format(time.RFC3339), lu)
	}
	return b.String()
}

func (s *Server) statusText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	applying := 0
	for _, op := range s.st.Ops {
		if op.State == StateApplying {
			applying++
		}
	}
	gate := "intake ready (backup exclusion " + s.cfg.BackupExclusion + ")"
	if s.cfg.BackupExclusion == "" {
		gate = "intake NOT ready: backup_exclusion is not set"
	}
	return fmt.Sprintf("public %s\npairing %s\nfingerprint %s\nactive devices %d\ngiven items %d\nshared items %d\nops applying %d\n%s\n",
		s.cfg.PublicAddr, s.cfg.PairAddr, FormatFingerprint(s.key.Fingerprint()), s.st.activeCount(),
		s.st.liveCount(KindGiven), s.st.liveCount(KindShared), applying, gate)
}
