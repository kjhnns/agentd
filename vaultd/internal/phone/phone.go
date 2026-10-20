// Package phone is a Go implementation of the phone side of vault v1. Tests
// use it as the client; it is also the reference for the Swift client and
// produces the test vectors.
package phone

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// Phone holds the device state. Keys are software keys here; the app uses
// Secure Enclave keys with the same wire format.
type Phone struct {
	Srv     string
	API     string // e.g. https://agent.gojoe.run
	PairURL string // e.g. http://box.taila526cc.ts.net:8791
	HTTP    *http.Client
	Name    string
	Now     func() time.Time

	Sign *vcrypto.SigningKey
	Agr  *vcrypto.AgreementKey

	Dev, Pkid, Gen string
	BsigPub        []byte
	BintPub        []byte
	Bsid, Bkid     string
	RouteToken     string

	// Pending pairing (what the app persists before hello).
	PendingT       []byte
	PendingK       []byte
	PendingR       []byte
	PendingHello   []byte
	PendingConfirm []byte
	// The self-contained resume bundle (what the app persists before confirm).
	PendingBsig, PendingBint []byte
	PendingBsid, PendingBkid string
	PendingToken             string
	PendingExp               int64
}

// clearPending wipes the pending pairing after activation.
func (p *Phone) clearPending() {
	for _, b := range [][]byte{p.PendingK, p.PendingR} {
		for i := range b {
			b[i] = 0
		}
	}
	p.PendingT, p.PendingK, p.PendingR, p.PendingHello, p.PendingConfirm = nil, nil, nil, nil, nil
	p.PendingBsig, p.PendingBint, p.PendingBsid, p.PendingBkid, p.PendingToken = nil, nil, "", "", ""
}

// New makes a phone with fresh keys.
func New(srv, api, pairURL string) *Phone {
	p := &Phone{Srv: srv, API: api, PairURL: pairURL, HTTP: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Name: "test phone", Now: time.Now, Sign: vcrypto.NewSigningKey(), Agr: vcrypto.NewAgreementKey()}
	p.Dev = vcrypto.KeyID(p.Sign.Public())
	p.Pkid = vcrypto.KeyID(p.Agr.Public())
	return p
}

func (p *Phone) post(url string, body []byte, auth bool) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+p.RouteToken)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err == nil && len(b) > 1<<20 {
		return resp.StatusCode, nil, errors.New("response larger than 1 MiB")
	}
	return resp.StatusCode, b, err
}

// ParseURI decodes agentd-vault://pair#<b64 P>.
func ParseURI(uri string) (wire.PairURI, error) {
	var P wire.PairURI
	const pre = "agentd-vault://pair#"
	if !strings.HasPrefix(uri, pre) {
		return P, errors.New("not a pairing uri")
	}
	b, err := wire.DecodeB64(strings.TrimPrefix(uri, pre), 1, 4096)
	if err != nil {
		return P, err
	}
	if err := wire.StrictDecode(b, &P); err != nil {
		return P, err
	}
	return P, nil
}

// Pair runs keys, hello and confirm (spec section 10).
func (p *Phone) Pair(uri string) error {
	if err := p.PairPrepare(uri); err != nil {
		return err
	}
	return p.Confirm()
}

// PairPrepare runs keys and hello and leaves the confirm bytes and the resume
// bundle in the pending fields. With a pending hello it resends those exact
// bytes instead of building a new transcript (resume after a lost answer).
func (p *Phone) PairPrepare(uri string) error {
	P, err := ParseURI(uri)
	if err != nil {
		return err
	}
	if P.V != 1 || !wire.IsID(P.Pid) || p.Now().UnixMilli() > P.Exp {
		return errors.New("pairing link is invalid or expired")
	}
	S, err := wire.DecodeB64Exact(P.S, 32)
	if err != nil {
		return err
	}
	fp, err := wire.DecodeB64Exact(P.Fp, 32)
	if err != nil {
		return err
	}
	// keys
	kb, _ := wire.Marshal(wire.PairKeysReq{Pid: P.Pid})
	code, body, err := p.post(p.PairURL+"/vault/v1/pair/keys", kb, false)
	if err != nil || code != 200 {
		return fmt.Errorf("pair/keys: %d %v", code, err)
	}
	var kr wire.PairKeysResp
	if err := wire.StrictDecode(body, &kr); err != nil {
		return err
	}
	bsig, err1 := wire.DecodeB64Exact(kr.BsigPub, 65)
	bint, err2 := wire.DecodeB64Exact(kr.BintPub, 65)
	ksig, err3 := wire.DecodeB64Exact(kr.Ksig, 64)
	if err1 != nil || err2 != nil || err3 != nil || !vcrypto.ValidPoint(bsig) || !vcrypto.ValidPoint(bint) ||
		vcrypto.KeyID(bsig) != kr.Bsid || vcrypto.KeyID(bint) != kr.Bkid {
		return errors.New("box keys malformed")
	}
	if !bytes.Equal(vcrypto.Hash(vcrypto.Cat([]byte(vcrypto.LabelBoxKeys), bsig, bint)), fp) {
		return errors.New("pairing failed: box keys do not match")
	}
	if !vcrypto.Verify(bsig, vcrypto.Cat([]byte(vcrypto.LabelPairKeys), []byte(P.Pid), bsig, bint), ksig) {
		return errors.New("pairing failed: keys signature")
	}
	// hello (a resume reuses the stored T and hello bytes)
	if p.PendingHello != nil && p.PendingT != nil {
		var pt wire.PairT
		if wire.StrictDecode(p.PendingT, &pt) == nil && pt.Pid == P.Pid {
			return p.helloAndBuild(P, p.PendingT, p.PendingK, p.PendingHello, bsig, bint, kr)
		}
	}
	T, err := wire.Marshal(wire.PairT{V: 1, Srv: p.Srv, Pid: P.Pid, Bsid: kr.Bsid, BsigPub: kr.BsigPub,
		Bkid: kr.Bkid, BintPub: kr.BintPub, PsigPub: wire.EncodeB64(p.Sign.Public()),
		PagrPub: wire.EncodeB64(p.Agr.Public()), Pc: wire.EncodeB64(vcrypto.RandomBytes(16)), Name: p.Name})
	if err != nil {
		return err
	}
	K, err := vcrypto.PairKey(S, P.Pid)
	if err != nil {
		return err
	}
	p.PendingT, p.PendingK = T, K
	hello, _ := wire.Marshal(wire.PairHelloReq{T: wire.EncodeB64(T),
		Sig: wire.EncodeB64(p.Sign.Sign(vcrypto.Cat([]byte(vcrypto.LabelPairT), T))),
		Mac: wire.EncodeB64(vcrypto.MAC(K, vcrypto.Cat([]byte(vcrypto.LabelPairHello), T)))})
	p.PendingHello = hello
	return p.helloAndBuild(P, T, K, hello, bsig, bint, kr)
}

func (p *Phone) helloAndBuild(P wire.PairURI, T, K, hello, bsig, bint []byte, kr wire.PairKeysResp) error {
	code, body, err := p.post(p.PairURL+"/vault/v1/pair/hello", hello, false)
	if err != nil || code != 200 {
		return fmt.Errorf("pair/hello: %d %v", code, err)
	}
	var hr wire.PairHelloResp
	if err := wire.StrictDecode(body, &hr); err != nil {
		return err
	}
	rc, err1 := wire.DecodeB64(hr.Rc, 81, 4096)
	bs, err2 := wire.DecodeB64Exact(hr.Bsig, 64)
	bm, err3 := wire.DecodeB64Exact(hr.Bmac, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return errors.New("hello answer malformed")
	}
	msg := vcrypto.Cat([]byte(vcrypto.LabelPairBox), T, rc)
	if !vcrypto.Verify(bsig, msg, bs) || !vcrypto.MACEqual(bm, vcrypto.MAC(K, msg)) {
		return errors.New("hello answer not authentic")
	}
	aad := wire.AADB2P(p.Srv, p.Dev, P.Pid, p.Pkid, "pair.box", P.Pid, 0)
	pt, err := p.Agr.Open(vcrypto.InfoB2P, aad, rc)
	if err != nil {
		return errors.New("rc does not open")
	}
	var sec wire.PairBoxSecret
	if err := wire.StrictDecode(pt, &sec); err != nil {
		return err
	}
	R, err := wire.DecodeB64Exact(sec.R, 32)
	if err != nil {
		return err
	}
	if len(sec.RouteToken) < 32 || len(sec.RouteToken) > 128 || wire.HasControl(sec.RouteToken) {
		return errors.New("route token out of range")
	}
	p.PendingR = R
	// confirm: serialized once and kept for exact retransmission
	cmsg := vcrypto.Cat([]byte(vcrypto.LabelPairConfirm), T, R)
	confirm, _ := wire.Marshal(wire.PairConfirmReq{Th: wire.EncodeB64(vcrypto.Hash(T)),
		Csig: wire.EncodeB64(p.Sign.Sign(cmsg)), Cmac: wire.EncodeB64(vcrypto.MAC(K, cmsg))})
	p.PendingConfirm = confirm
	p.PendingBsig, p.PendingBint, p.PendingBsid, p.PendingBkid = bsig, bint, kr.Bsid, kr.Bkid
	p.PendingToken, p.PendingExp = sec.RouteToken, P.Exp
	return nil
}

// Confirm sends the stored confirm bytes (first send and every retry) and
// activates the pairing from the resume bundle.
func (p *Phone) Confirm() error {
	if p.PendingConfirm == nil {
		return errors.New("no pending pairing")
	}
	bsig, bint, bsid, bkid, routeToken := p.PendingBsig, p.PendingBint, p.PendingBsid, p.PendingBkid, p.PendingToken
	code, body, err := p.post(p.PairURL+"/vault/v1/pair/confirm", p.PendingConfirm, false)
	if err != nil || code != 200 {
		return fmt.Errorf("pair/confirm: %d %v", code, err)
	}
	var dr wire.PairDoneResp
	if err := wire.StrictDecode(body, &dr); err != nil {
		return err
	}
	ds, err := wire.DecodeB64Exact(dr.Dsig, 64)
	if err != nil || dr.Dev != p.Dev || !wire.IsID(dr.Gen) ||
		!vcrypto.Verify(bsig, vcrypto.Cat([]byte(vcrypto.LabelPairDone), p.PendingT, []byte(dr.Dev), []byte(dr.Gen)), ds) {
		return errors.New("done answer not authentic")
	}
	p.Gen, p.BsigPub, p.BintPub, p.Bsid, p.Bkid, p.RouteToken = dr.Gen, bsig, bint, bsid, bkid, routeToken
	return nil
}

// Activate clears the pending pairing once the app stored the result.
func (p *Phone) Activate() { p.clearPending() }

// Call is one signed request and its verified answer.
type Call struct {
	Header  wire.ReqHeader
	HB      []byte
	Code    int
	Resp    *wire.RespHeader
	RawBody []byte
}

// Build makes a request header for op.
func (p *Phone) Build(op, item string, iv int64, opid string) wire.ReqHeader {
	path := ""
	for k, v := range wire.Routes {
		if v == op {
			path = k
		}
	}
	return wire.ReqHeader{V: 1, Srv: p.Srv, Op: op, Path: path, Dev: p.Dev, Gen: p.Gen,
		Rid: vcrypto.RandomID(), Ts: p.Now().UnixMilli(), Item: item, Iv: iv, Opid: opid}
}

// SealSecret encrypts a payload for intake (version 1) or replace (iv+1).
func (p *Phone) SealSecret(h *wire.ReqHeader, payload wire.SecretPayload) error {
	pt, err := wire.Marshal(payload)
	if err != nil {
		return err
	}
	version := int64(1)
	if h.Op == wire.OpReplace {
		version = h.Iv + 1
	}
	h.Bkid = p.Bkid
	ct, err := vcrypto.Seal(p.BintPub, vcrypto.InfoP2B, wire.AADP2B(p.Srv, p.Dev, p.Gen, p.Bkid, h.Op, h.Item, version), pt)
	if err != nil {
		return err
	}
	h.C = wire.EncodeB64(ct)
	return nil
}

// Send signs and sends a header, verifies the answer.
func (p *Phone) Send(h wire.ReqHeader) (*Call, error) {
	hb, err := wire.Marshal(h)
	if err != nil {
		return nil, err
	}
	return p.SendBytes(h, hb)
}

// SendBytes sends exact header bytes (tests use it for retries).
func (p *Phone) SendBytes(h wire.ReqHeader, hb []byte) (*Call, error) {
	sig := p.Sign.Sign(vcrypto.Cat([]byte(vcrypto.LabelReq), hb))
	env, _ := wire.Marshal(wire.Envelope{H: wire.EncodeB64(hb), S: wire.EncodeB64(sig)})
	code, body, err := p.post(p.API+h.Path, env, true)
	c := &Call{Header: h, HB: hb, Code: code, RawBody: body}
	if err != nil || code != 200 {
		return c, err
	}
	r, err := p.Verify(hb, body)
	if err != nil {
		return c, err
	}
	c.Resp = r
	return c, nil
}

// Verify checks a response against the request header bytes (spec 6).
func (p *Phone) Verify(hb, body []byte) (*wire.RespHeader, error) {
	var env wire.Envelope
	if err := wire.StrictDecode(body, &env); err != nil {
		return nil, err
	}
	rb, err1 := wire.DecodeB64(env.H, 1, 1<<20)
	sig, err2 := wire.DecodeB64Exact(env.S, 64)
	if err1 != nil || err2 != nil {
		return nil, errors.New("envelope")
	}
	if !vcrypto.Verify(p.BsigPub, vcrypto.Cat([]byte(vcrypto.LabelResp), rb), sig) {
		return nil, errors.New("response signature")
	}
	var r wire.RespHeader
	if err := wire.StrictDecode(rb, &r); err != nil {
		return nil, err
	}
	var h wire.ReqHeader
	_ = json.Unmarshal(hb, &h)
	if r.V != 1 || r.Dir != "resp" || r.Srv != p.Srv || r.Op != h.Op || r.Dev != p.Dev || r.Gen != p.Gen ||
		r.Bsid != p.Bsid || r.Rh != wire.EncodeB64(vcrypto.Hash(hb)) {
		return nil, errors.New("response does not match the request")
	}
	if err := checkSemantics(h, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// checkSemantics is the op-specific response check (spec 6, code r1 #18,
// r3/r4 #18): every field is either required for the op or must be empty.
func checkSemantics(h wire.ReqHeader, r *wire.RespHeader) error {
	bad := func(what string) error { return errors.New("response field: " + what) }
	ok := r.Status == "ok"
	if !ok && (len(r.Status) < 5 || r.Status[:4] != "err:") {
		return bad("status")
	}
	isList := h.Op == wire.OpGivenList || h.Op == wire.OpSharedList
	journaled := wire.IsMutation(h.Op) || h.Op == wire.OpStatus
	// Unused fields.
	if !isList && (len(r.Entries) != 0 || r.Next != "") {
		return bad("entries")
	}
	if !journaled && (r.Opid != "" || r.State != "" || r.Code != "" || r.Iv != 0) {
		return bad("op fields")
	}
	if (h.Op == wire.OpSharedGet && ok) != (r.C != "") {
		return bad("c")
	}
	if !journaled && (h.Op != wire.OpSharedGet || !ok) && r.Item != "" {
		return bad("item")
	}
	if !ok && !journaled && (len(r.Entries) != 0 || r.Next != "" || r.C != "") {
		return bad("error fields")
	}
	// Lists.
	if len(r.Entries) > 50 || (r.Next != "" && !wire.IsID(r.Next)) {
		return bad("page")
	}
	prev := h.Item // entries ascend strictly after the cursor
	for _, e := range r.Entries {
		if !wire.IsID(e.Item) || e.Item <= prev || e.C == "" ||
			(h.Op == wire.OpGivenList && e.Iv < 1) || (h.Op == wire.OpSharedList && e.Iv != 0) {
			return bad("entry")
		}
		prev = e.Item
	}
	// The cursor is the last entry of a full page, or empty (spec 8).
	if r.Next != "" && (len(r.Entries) != 50 || r.Next != r.Entries[len(r.Entries)-1].Item) {
		return bad("next")
	}
	if h.Op == wire.OpSharedGet && ok && r.Item != h.Item {
		return bad("item")
	}
	if !journaled {
		return nil
	}
	// Journal outcomes (mutations and op.status): a plain refusal carries no
	// op fields; an outcome is bound to its op id with a valid state.
	if r.Opid == "" {
		if ok || r.State != "" || r.Code != "" || r.Item != "" || r.Iv != 0 {
			return bad("refusal")
		}
		return nil
	}
	want := h.Rid
	if h.Op == wire.OpStatus {
		want = h.Opid
	}
	if r.Opid != want || !wire.IsID(r.Item) || r.Iv < 0 {
		return bad("opid/item")
	}
	if wire.IsMutation(h.Op) && r.Item != h.Item {
		return bad("item")
	}
	switch r.State {
	case "applying":
		if !ok || r.Code != "" {
			return bad("applying")
		}
	case "done":
		if !ok || r.Code != "" {
			return bad("done")
		}
		switch h.Op {
		case wire.OpIntake:
			if r.Iv != 1 {
				return bad("intake version")
			}
		case wire.OpReplace:
			if r.Iv != h.Iv+1 {
				return bad("replace version")
			}
		case wire.OpDelete:
			if r.Iv != 0 {
				return bad("delete version")
			}
		}
	case "failed":
		if ok || r.Code == "" || r.Status != "err:"+r.Code {
			return bad("failed")
		}
	default:
		return bad("state")
	}
	return nil
}

// OpenMeta decrypts a list entry.
func (p *Phone) OpenMeta(kind string, e wire.Entry) (wire.MetaPayload, error) {
	var m wire.MetaPayload
	c, err := wire.DecodeB64(e.C, 81, 1<<20)
	if err != nil {
		return m, err
	}
	pt, err := p.Agr.Open(vcrypto.InfoB2P, wire.AADB2P(p.Srv, p.Dev, p.Gen, p.Pkid, kind, e.Item, e.Iv), c)
	if err != nil {
		return m, err
	}
	if err := wire.StrictDecode(pt, &m); err != nil {
		return m, err
	}
	if len(m.Label) > 100 || len(m.Login) > 200 || len(m.URL) > 500 || wire.HasControl(m.Label+m.Login+m.URL) {
		return wire.MetaPayload{}, errors.New("metadata out of range")
	}
	return m, nil
}

// OpenValue decrypts a shared.get answer.
func (p *Phone) OpenValue(r *wire.RespHeader) (wire.SecretPayload, error) {
	var v wire.SecretPayload
	c, err := wire.DecodeB64(r.C, 81, 1<<20)
	if err != nil {
		return v, err
	}
	pt, err := p.Agr.Open(vcrypto.InfoB2P, wire.AADB2P(p.Srv, p.Dev, p.Gen, p.Pkid, "shared.value", r.Item, 0), c)
	if err != nil {
		return v, err
	}
	if err := wire.StrictDecode(pt, &v); err != nil {
		return v, err
	}
	if err := v.Validate(); err != nil {
		return wire.SecretPayload{}, err
	}
	return v, nil
}
