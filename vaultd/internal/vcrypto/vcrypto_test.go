package vcrypto

import (
	"bytes"
	"testing"
)

func TestSignVerify(t *testing.T) {
	k := NewSigningKey()
	msg := []byte(LabelReq + "{}")
	sig := k.Sign(msg)
	if !Verify(k.Public(), msg, sig) {
		t.Fatal("verify failed")
	}
	if Verify(k.Public(), []byte(LabelResp+"{}"), sig) {
		t.Fatal("verified under another label")
	}
	bad := append([]byte(nil), sig...)
	bad[10] ^= 1
	if Verify(k.Public(), msg, bad) {
		t.Fatal("tampered sig verified")
	}
	if Verify(k.Public(), msg, make([]byte, 64)) {
		t.Fatal("zero sig verified")
	}
	k2, err := SigningKeyFromScalar(k.Scalar())
	if err != nil || !bytes.Equal(k2.Public(), k.Public()) {
		t.Fatal("scalar round trip")
	}
}

func TestHPKE(t *testing.T) {
	k := NewAgreementKey()
	aad := []byte("agentd-vault-v1|p2b|x")
	ct, err := Seal(k.Public(), InfoP2B, aad, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.Open(InfoP2B, aad, ct)
	if err != nil || string(pt) != "secret" {
		t.Fatalf("open: %v %q", err, pt)
	}
	if _, err := k.Open(InfoB2P, aad, ct); err == nil {
		t.Fatal("opened with the wrong info")
	}
	if _, err := k.Open(InfoP2B, []byte("other"), ct); err == nil {
		t.Fatal("opened with the wrong aad")
	}
	other := NewAgreementKey()
	if _, err := other.Open(InfoP2B, aad, ct); err == nil {
		t.Fatal("opened with the wrong key")
	}
	k2, err := AgreementKeyFromScalar(k.Scalar())
	if err != nil || !bytes.Equal(k2.Public(), k.Public()) {
		t.Fatal("scalar round trip")
	}
	if ValidPoint([]byte{4, 1, 2}) || !ValidPoint(k.Public()) {
		t.Fatal("ValidPoint")
	}
}

func TestPairKey(t *testing.T) {
	k1, err := PairKey(bytes.Repeat([]byte{1}, 32), "00112233445566778899aabbccddeeff")
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, _ := PairKey(bytes.Repeat([]byte{1}, 32), "00112233445566778899aabbccddeef0")
	if bytes.Equal(k1, k2) {
		t.Fatal("salt ignored")
	}
	if _, err := PairKey(nil, "zz"); err == nil {
		t.Fatal("bad pid accepted")
	}
}
