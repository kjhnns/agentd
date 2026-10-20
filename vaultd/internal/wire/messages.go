package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// Envelope is the body of every request and response on the public listener.
type Envelope struct {
	H string `json:"h"`
	S string `json:"s"`
}

// ReqHeader is the signed request header (spec section 5).
type ReqHeader struct {
	V    int64  `json:"v"`
	Srv  string `json:"srv"`
	Op   string `json:"op"`
	Path string `json:"path"`
	Dev  string `json:"dev"`
	Gen  string `json:"gen"`
	Rid  string `json:"rid"`
	Ts   int64  `json:"ts"`
	Item string `json:"item"`
	Iv   int64  `json:"iv"`
	Opid string `json:"opid"`
	Bkid string `json:"bkid"`
	C    string `json:"c"`
}

// Entry is one list entry (spec section 6).
type Entry struct {
	Item    string `json:"item"`
	Iv      int64  `json:"iv"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	LastUse int64  `json:"last_use"`
	C       string `json:"c"`
}

// RespHeader is the signed response header (spec section 6).
type RespHeader struct {
	V       int64   `json:"v"`
	Srv     string  `json:"srv"`
	Dir     string  `json:"dir"`
	Op      string  `json:"op"`
	Rh      string  `json:"rh"`
	Dev     string  `json:"dev"`
	Gen     string  `json:"gen"`
	Bsid    string  `json:"bsid"`
	Ts      int64   `json:"ts"`
	Status  string  `json:"status"`
	Opid    string  `json:"opid"`
	State   string  `json:"state"`
	Code    string  `json:"code"`
	Item    string  `json:"item"`
	Iv      int64   `json:"iv"`
	Entries []Entry `json:"entries"`
	Next    string  `json:"next"`
	C       string  `json:"c"`
}

// SecretPayload is the plaintext of intake, replace and shared.get.
type SecretPayload struct {
	Label    string `json:"label"`
	Login    string `json:"login"`
	URL      string `json:"url"`
	Password string `json:"password"`
}

// MetaPayload is the plaintext of a list entry.
type MetaPayload struct {
	Label string `json:"label"`
	Login string `json:"login"`
	URL   string `json:"url"`
}

// Validate applies the limits of spec section 7 (lengths in UTF-8 bytes, no
// character below U+0020 and no U+007F).
func (p SecretPayload) Validate() error {
	switch {
	case len(p.Label) < 1 || len(p.Label) > 100:
		return errors.New("label length")
	case len(p.Login) > 200, len(p.URL) > 500:
		return errors.New("login or url length")
	case len(p.Password) < 1 || len(p.Password) > 1024:
		return errors.New("password length")
	}
	for _, s := range []string{p.Label, p.Login, p.URL, p.Password} {
		if HasControl(s) {
			return errors.New("control character")
		}
	}
	return nil
}

// HasControl reports invalid UTF-8, a character below U+0020, U+007F, or
// U+2028/U+2029 (encoding/json would escape those two as \u, which the strict
// parser rejects).
func HasControl(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return true
		}
	}
	return false
}

// Marshal serializes v without HTML escaping and without a trailing newline,
// so the bytes pass the strict parser (no \u escapes).
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Ops and their routes (spec section 5).
const (
	OpIntake     = "intake"
	OpReplace    = "replace"
	OpDelete     = "delete"
	OpGivenList  = "given.list"
	OpSharedList = "shared.list"
	OpSharedGet  = "shared.get"
	OpStatus     = "op.status"
	OpUnpair     = "unpair"
)

// Routes maps a public path to its op.
var Routes = map[string]string{
	"/vault/v1/intake":      OpIntake,
	"/vault/v1/replace":     OpReplace,
	"/vault/v1/delete":      OpDelete,
	"/vault/v1/given/list":  OpGivenList,
	"/vault/v1/shared/list": OpSharedList,
	"/vault/v1/shared/get":  OpSharedGet,
	"/vault/v1/op/status":   OpStatus,
	"/vault/v1/unpair":      OpUnpair,
}

// IsMutation reports intake, replace and delete.
func IsMutation(op string) bool { return op == OpIntake || op == OpReplace || op == OpDelete }

var srvRe = regexp.MustCompile(`^[a-z0-9.-]{1,64}$`)

// ValidSrv checks the server id rule (no "|" can occur in an AAD field).
func ValidSrv(s string) bool { return srvRe.MatchString(s) }

// CheckFields applies the per-op field table of spec section 5: required
// fields are set, all others are empty / 0. ts window and gen are checked by
// the caller.
func (h ReqHeader) CheckFields() error {
	if h.V != 1 || !ValidSrv(h.Srv) || !IsID(h.Dev) || !IsID(h.Gen) || !IsID(h.Rid) {
		return errors.New("bad common field")
	}
	if Routes[h.Path] != h.Op {
		return errors.New("op does not match path")
	}
	need := map[string]bool{}
	switch h.Op {
	case OpIntake:
		need = map[string]bool{"item": true, "bkid": true, "c": true}
	case OpReplace:
		need = map[string]bool{"item": true, "bkid": true, "c": true, "iv": true}
	case OpDelete:
		need = map[string]bool{"item": true, "iv": true}
	case OpGivenList, OpSharedList:
		need = map[string]bool{"cursor": true}
	case OpSharedGet:
		need = map[string]bool{"item": true}
	case OpStatus:
		need = map[string]bool{"opid": true}
	case OpUnpair:
	default:
		return errors.New("unknown op")
	}
	// item: required id, optional cursor id, or empty.
	switch {
	case need["item"]:
		if !IsID(h.Item) {
			return errors.New("item")
		}
	case need["cursor"]:
		if h.Item != "" && !IsID(h.Item) {
			return errors.New("cursor")
		}
	default:
		if h.Item != "" {
			return errors.New("item not allowed")
		}
	}
	if need["iv"] != (h.Iv != 0) || h.Iv < 0 {
		return errors.New("iv")
	}
	if need["opid"] != (h.Opid != "") || (h.Opid != "" && !IsID(h.Opid)) {
		return errors.New("opid")
	}
	if need["bkid"] != (h.Bkid != "") || (h.Bkid != "" && !IsID(h.Bkid)) {
		return errors.New("bkid")
	}
	if need["c"] != (h.C != "") {
		return errors.New("c")
	}
	if h.C != "" {
		if _, err := DecodeB64(h.C, 81, 4096); err != nil {
			return errors.New("c")
		}
	}
	return nil
}

// AADP2B is the phone-to-box AAD (spec section 7).
func AADP2B(srv, dev, gen, bkid, op, item string, version int64) []byte {
	return []byte("agentd-vault-v1 aad-p2b|" + srv + "|" + dev + "|" + gen + "|" + bkid + "|" + op + "|" + item + "|" + strconv.FormatInt(version, 10))
}

// AADB2P is the box-to-phone AAD (spec section 7).
func AADB2P(srv, dev, gen, pkid, kind, item string, version int64) []byte {
	return []byte("agentd-vault-v1 aad-b2p|" + srv + "|" + dev + "|" + gen + "|" + pkid + "|" + kind + "|" + item + "|" + strconv.FormatInt(version, 10))
}

// Pairing bodies (spec section 10).

// PairURI is P, carried in agentd-vault://pair#<b64 P>.
type PairURI struct {
	V   int64  `json:"v"`
	Pid string `json:"pid"`
	S   string `json:"s"`
	Fp  string `json:"fp"`
	Exp int64  `json:"exp"`
}

// PairKeysReq is the body of /pair/keys.
type PairKeysReq struct {
	Pid string `json:"pid"`
}

// PairKeysResp is the answer of /pair/keys.
type PairKeysResp struct {
	Bsid    string `json:"bsid"`
	BsigPub string `json:"bsig_pub"`
	Bkid    string `json:"bkid"`
	BintPub string `json:"bint_pub"`
	Ksig    string `json:"ksig"`
}

// PairT is the transcript T (serialized once by the phone).
type PairT struct {
	V       int64  `json:"v"`
	Srv     string `json:"srv"`
	Pid     string `json:"pid"`
	Bsid    string `json:"bsid"`
	BsigPub string `json:"bsig_pub"`
	Bkid    string `json:"bkid"`
	BintPub string `json:"bint_pub"`
	PsigPub string `json:"psig_pub"`
	PagrPub string `json:"pagr_pub"`
	Pc      string `json:"pc"`
	Name    string `json:"name"`
}

// PairHelloReq is the body of /pair/hello.
type PairHelloReq struct {
	T   string `json:"t"`
	Sig string `json:"sig"`
	Mac string `json:"mac"`
}

// PairHelloResp is the answer of /pair/hello.
type PairHelloResp struct {
	Rc   string `json:"rc"`
	Bsig string `json:"bsig"`
	Bmac string `json:"bmac"`
}

// PairBoxSecret is the plaintext of rc.
type PairBoxSecret struct {
	R          string `json:"r"`
	RouteToken string `json:"route_token"`
}

// PairConfirmReq is the body of /pair/confirm.
type PairConfirmReq struct {
	Th   string `json:"th"`
	Csig string `json:"csig"`
	Cmac string `json:"cmac"`
}

// PairDoneResp is the answer of /pair/confirm.
type PairDoneResp struct {
	Dev  string `json:"dev"`
	Gen  string `json:"gen"`
	Dsig string `json:"dsig"`
}
