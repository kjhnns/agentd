package phone

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/vaultd/internal/vcrypto"
	"github.com/kjhnns/agentd/vaultd/internal/wire"
)

// Vectors is testdata/vectors.json, the interop contract for the Swift client
// (spec section 18). Regenerate with VAULTD_WRITE_VECTORS=1 go test ./internal/phone.
type Vectors struct {
	Note       string            `json:"note"`
	Keys       map[string]string `json:"keys"` // name -> hex scalar
	Publics    map[string]string `json:"publics"`
	KeyIDs     map[string]string `json:"key_ids"`
	Fp         string            `json:"fingerprint_b64"`
	HKDF       map[string]string `json:"hkdf"`
	HMAC       map[string]string `json:"hmac"`
	AAD        map[string]string `json:"aad"`
	HPKE       []HPKEVec         `json:"hpke"`
	Signatures []SigVec          `json:"signatures"`
}

// HPKEVec opens with the named recipient key.
type HPKEVec struct {
	Recipient string `json:"recipient"`
	Info      string `json:"info"`
	AAD       string `json:"aad"`
	CtB64     string `json:"ct_b64"`
	Plain     string `json:"plaintext"`
}

// SigVec verifies with the named key's public point.
type SigVec struct {
	Signer string `json:"signer"`
	MsgB64 string `json:"msg_b64"`
	SigB64 string `json:"sig_b64"`
}

func scalar(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestVectors(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "vectors.json")
	sign := map[string]*vcrypto.SigningKey{}
	agr := map[string]*vcrypto.AgreementKey{}
	var err error
	for name, b := range map[string]byte{"phone_sign": 1, "box_sign": 3} {
		if sign[name], err = vcrypto.SigningKeyFromScalar(scalar(b)); err != nil {
			t.Fatal(err)
		}
	}
	for name, b := range map[string]byte{"phone_agr": 2, "box_intake": 4} {
		if agr[name], err = vcrypto.AgreementKeyFromScalar(scalar(b)); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("VAULTD_WRITE_VECTORS") == "1" {
		v := Vectors{Note: "vault v1 interop vectors; scalars are test-only", Keys: map[string]string{},
			Publics: map[string]string{}, KeyIDs: map[string]string{}, HKDF: map[string]string{},
			HMAC: map[string]string{}, AAD: map[string]string{}}
		for n, k := range sign {
			v.Keys[n], v.Publics[n], v.KeyIDs[n] = hex.EncodeToString(k.Scalar()), wire.EncodeB64(k.Public()), vcrypto.KeyID(k.Public())
		}
		for n, k := range agr {
			v.Keys[n], v.Publics[n], v.KeyIDs[n] = hex.EncodeToString(k.Scalar()), wire.EncodeB64(k.Public()), vcrypto.KeyID(k.Public())
		}
		v.Fp = wire.EncodeB64(vcrypto.Hash(vcrypto.Cat([]byte(vcrypto.LabelBoxKeys), sign["box_sign"].Public(), agr["box_intake"].Public())))
		K, _ := vcrypto.PairKey(scalar(9), "00112233445566778899aabbccddeeff")
		v.HKDF["pair_key S=09*32 pid=00112233445566778899aabbccddeeff"] = wire.EncodeB64(K)
		v.HMAC["K above, msg 'agentd-vault-v1 pair-hello\\n{}'"] = wire.EncodeB64(vcrypto.MAC(K, []byte(vcrypto.LabelPairHello+"{}")))
		dev, pkid, bkid := v.KeyIDs["phone_sign"], v.KeyIDs["phone_agr"], v.KeyIDs["box_intake"]
		gen, item := "aabbccddeeff00112233445566778899", "0123456789abcdef0123456789abcdef"
		p2b := wire.AADP2B("agent.gojoe.run", dev, gen, bkid, "intake", item, 1)
		b2p := wire.AADB2P("agent.gojoe.run", dev, gen, pkid, "shared.value", item, 0)
		v.AAD["p2b intake v1"], v.AAD["b2p shared.value"] = string(p2b), string(b2p)
		pt1, _ := wire.Marshal(wire.SecretPayload{Label: "IBKR", Login: "joe", URL: "https://x/?a=1&b=<2>", Password: "pw-1"})
		ct1, _ := vcrypto.Seal(agr["box_intake"].Public(), vcrypto.InfoP2B, p2b, pt1)
		pt2, _ := wire.Marshal(wire.SecretPayload{Label: "eBay", Login: "j", URL: "", Password: "pw-2"})
		ct2, _ := vcrypto.Seal(agr["phone_agr"].Public(), vcrypto.InfoB2P, b2p, pt2)
		v.HPKE = []HPKEVec{
			{Recipient: "box_intake", Info: vcrypto.InfoP2B, AAD: string(p2b), CtB64: wire.EncodeB64(ct1), Plain: string(pt1)},
			{Recipient: "phone_agr", Info: vcrypto.InfoB2P, AAD: string(b2p), CtB64: wire.EncodeB64(ct2), Plain: string(pt2)},
		}
		hdr, _ := wire.Marshal(wire.ReqHeader{V: 1, Srv: "agent.gojoe.run", Op: "given.list", Path: "/vault/v1/given/list",
			Dev: dev, Gen: gen, Rid: "ffeeddccbbaa99887766554433221100", Ts: 1792000000000})
		req := vcrypto.Cat([]byte(vcrypto.LabelReq), hdr)
		resp := vcrypto.Cat([]byte(vcrypto.LabelResp), []byte(`{"v":1}`))
		v.Signatures = []SigVec{
			{Signer: "phone_sign", MsgB64: wire.EncodeB64(req), SigB64: wire.EncodeB64(sign["phone_sign"].Sign(req))},
			{Signer: "box_sign", MsgB64: wire.EncodeB64(resp), SigB64: wire.EncodeB64(sign["box_sign"].Sign(resp))},
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v Vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for n, k := range sign {
		if v.Publics[n] != wire.EncodeB64(k.Public()) || v.KeyIDs[n] != vcrypto.KeyID(k.Public()) {
			t.Fatalf("%s public or id drifted", n)
		}
	}
	for _, h := range v.HPKE {
		ct, _ := wire.DecodeB64(h.CtB64, 81, 1<<16)
		pt, err := agr[h.Recipient].Open(h.Info, []byte(h.AAD), ct)
		if err != nil || string(pt) != h.Plain {
			t.Fatalf("hpke vector for %s: %v", h.Recipient, err)
		}
	}
	for _, s := range v.Signatures {
		msg, _ := wire.DecodeB64(s.MsgB64, 1, 1<<16)
		sig, _ := wire.DecodeB64Exact(s.SigB64, 64)
		if !vcrypto.Verify(sign[s.Signer].Public(), msg, sig) {
			t.Fatalf("signature vector for %s", s.Signer)
		}
	}
	K, _ := vcrypto.PairKey(scalar(9), "00112233445566778899aabbccddeeff")
	for _, want := range v.HKDF {
		if want != wire.EncodeB64(K) {
			t.Fatal("hkdf vector drifted")
		}
	}
}

func TestCheckSemantics(t *testing.T) {
	rid, other, item := vcrypto.RandomID(), vcrypto.RandomID(), vcrypto.RandomID()
	st := wire.ReqHeader{Op: wire.OpStatus, Rid: vcrypto.RandomID(), Opid: rid}
	cases := []struct {
		h    wire.ReqHeader
		r    wire.RespHeader
		good bool
	}{
		{st, wire.RespHeader{Status: "ok", Opid: rid, State: "done", Item: item, Iv: 1}, true},
		{st, wire.RespHeader{Status: "err:internal", Opid: other, State: "failed", Code: "internal", Item: item}, false},
		{st, wire.RespHeader{Status: "err:internal", Opid: rid, State: "done", Code: "internal", Item: item}, false},
		{st, wire.RespHeader{Status: "err:not_found"}, true},
		{st, wire.RespHeader{Status: "err:not_found", State: "done"}, false},
		{wire.ReqHeader{Op: wire.OpIntake, Rid: rid, Item: item}, wire.RespHeader{Status: "ok", Opid: rid, State: "done", Item: item, Iv: 2}, false},
		{wire.ReqHeader{Op: wire.OpReplace, Rid: rid, Item: item, Iv: 3}, wire.RespHeader{Status: "ok", Opid: rid, State: "done", Item: item, Iv: 4}, true},
		{wire.ReqHeader{Op: wire.OpGivenList, Rid: rid}, wire.RespHeader{Status: "ok", Opid: rid}, false},
		{wire.ReqHeader{Op: wire.OpGivenList, Rid: rid}, wire.RespHeader{Status: "ok", C: "x"}, false},
		{wire.ReqHeader{Op: wire.OpGivenList, Rid: rid}, wire.RespHeader{Status: "ok", Next: item}, false},
		{wire.ReqHeader{Op: wire.OpSharedGet, Rid: rid, Item: item}, wire.RespHeader{Status: "err:not_found", Item: other}, false},
		{wire.ReqHeader{Op: wire.OpGivenList, Rid: rid}, wire.RespHeader{Status: "err:stale", Entries: []wire.Entry{{Item: item, Iv: 1, C: "x"}}}, false},
		{st, wire.RespHeader{Status: "ok", Opid: rid, State: "applying", Item: item}, true},
	}
	for i, c := range cases {
		if err := checkSemantics(c.h, &c.r); (err == nil) != c.good {
			t.Errorf("case %d: err=%v", i, err)
		}
	}
}
