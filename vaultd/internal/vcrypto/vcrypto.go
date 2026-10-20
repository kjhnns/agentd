// Package vcrypto holds the vault v1 primitives (spec section 3): ECDSA P-256
// with raw r||s signatures, HPKE base mode DHKEM(P-256)/HKDF-SHA256/AES-256-GCM,
// HKDF and HMAC, key ids and the domain-separation labels.
package vcrypto

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
)

// Domain-separation labels (spec section 3). No label is a prefix of another.
const (
	LabelReq         = "agentd-vault-v1 req\n"
	LabelResp        = "agentd-vault-v1 resp\n"
	LabelBoxKeys     = "agentd-vault-v1 box-keys\n"
	LabelPairKeys    = "agentd-vault-v1 pair-keys\n"
	LabelPairT       = "agentd-vault-v1 pair-t\n"
	LabelPairHello   = "agentd-vault-v1 pair-hello\n"
	LabelPairBox     = "agentd-vault-v1 pair-box\n"
	LabelPairConfirm = "agentd-vault-v1 pair-confirm\n"
	LabelPairDone    = "agentd-vault-v1 pair-done\n"
	InfoPairKey      = "agentd-vault-v1 pair-key"
	InfoP2B          = "agentd-vault-v1 p2b"
	InfoB2P          = "agentd-vault-v1 b2p"
	InfoGen          = "agentd-vault-v1 gen" // box-internal: generate journal blob
)

// B64 is base64url without padding, the encoding of every binary field.
var B64 = base64.RawURLEncoding

// Cat concatenates byte slices (label || message || ...).
func Cat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Hash is SHA-256.
func Hash(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// KeyID is the first 16 bytes of SHA-256 over an uncompressed point, as hex.
func KeyID(point []byte) string { return hex.EncodeToString(Hash(point)[:16]) }

// RandomID returns 16 random bytes as 32 lowercase hex characters.
func RandomID() string { return hex.EncodeToString(RandomBytes(16)) }

// RandomBytes returns n bytes from crypto/rand.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return b
}

// ---- signatures ----

// SigningKey is an ECDSA P-256 private key.
type SigningKey struct{ k *ecdsa.PrivateKey }

// NewSigningKey generates a fresh key.
func NewSigningKey() *SigningKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &SigningKey{k}
}

// SigningKeyFromScalar parses a 32-byte private scalar.
func SigningKeyFromScalar(d []byte) (*SigningKey, error) {
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, err
	}
	return &SigningKey{k}, nil
}

// Scalar returns the 32-byte private scalar.
func (s *SigningKey) Scalar() []byte {
	b, err := s.k.Bytes()
	if err != nil {
		panic(err)
	}
	return b
}

// Public returns the uncompressed public point.
func (s *SigningKey) Public() []byte {
	b, err := s.k.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	return b
}

// Sign returns a raw 64-byte r||s signature over SHA-256(msg).
func (s *SigningKey) Sign(msg []byte) []byte {
	r, ss, err := ecdsa.Sign(rand.Reader, s.k, Hash(msg))
	if err != nil {
		panic(err)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	ss.FillBytes(out[32:])
	return out
}

// ParseSigningPublic parses an uncompressed P-256 point for verification.
func ParseSigningPublic(point []byte) (*ecdsa.PublicKey, error) {
	if len(point) != 65 || point[0] != 4 {
		return nil, errors.New("not an uncompressed P-256 point")
	}
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
}

// Verify checks a raw r||s signature over SHA-256(msg). r and s must be in
// [1, n-1].
func Verify(point, msg, sig []byte) bool {
	pub, err := ParseSigningPublic(point)
	if err != nil || len(sig) != 64 {
		return false
	}
	n := elliptic.P256().Params().N
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if r.Sign() <= 0 || s.Sign() <= 0 || r.Cmp(n) >= 0 || s.Cmp(n) >= 0 {
		return false
	}
	return ecdsa.Verify(pub, Hash(msg), r, s)
}

// ---- HPKE ----

var (
	kem  = hpke.DHKEM(ecdh.P256())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.AES256GCM()
)

// AgreementKey is a P-256 HPKE recipient key.
type AgreementKey struct{ k hpke.PrivateKey }

// NewAgreementKey generates a fresh key.
func NewAgreementKey() *AgreementKey {
	k, err := kem.GenerateKey()
	if err != nil {
		panic(err)
	}
	return &AgreementKey{k}
}

// AgreementKeyFromScalar parses a 32-byte private scalar.
func AgreementKeyFromScalar(d []byte) (*AgreementKey, error) {
	k, err := kem.NewPrivateKey(d)
	if err != nil {
		return nil, err
	}
	return &AgreementKey{k}, nil
}

// Scalar returns the 32-byte private scalar.
func (a *AgreementKey) Scalar() []byte {
	b, err := a.k.Bytes()
	if err != nil {
		panic(err)
	}
	return b
}

// Public returns the uncompressed public point.
func (a *AgreementKey) Public() []byte { return a.k.PublicKey().Bytes() }

// ValidPoint reports whether point is a valid uncompressed P-256 point.
func ValidPoint(point []byte) bool {
	if len(point) != 65 || point[0] != 4 {
		return false
	}
	_, err := ecdh.P256().NewPublicKey(point)
	return err == nil
}

// Seal encrypts plaintext to the recipient point with a fresh sender context.
// It returns enc (65 bytes) || ciphertext with tag.
func Seal(recipient []byte, info string, aad, plaintext []byte) ([]byte, error) {
	if !ValidPoint(recipient) {
		return nil, errors.New("bad recipient point")
	}
	pk, err := kem.NewPublicKey(recipient)
	if err != nil {
		return nil, err
	}
	enc, s, err := hpke.NewSender(pk, kdf, aead, []byte(info))
	if err != nil {
		return nil, err
	}
	ct, err := s.Seal(aad, plaintext)
	if err != nil {
		return nil, err
	}
	return Cat(enc, ct), nil
}

// Open decrypts enc || ciphertext with this key.
func (a *AgreementKey) Open(info string, aad, msg []byte) ([]byte, error) {
	if len(msg) < 65+16 {
		return nil, errors.New("ciphertext too short")
	}
	r, err := hpke.NewRecipient(msg[:65], a.k, kdf, aead, []byte(info))
	if err != nil {
		return nil, err
	}
	return r.Open(aad, msg[65:])
}

// ---- HKDF / HMAC ----

// PairKey derives K = HKDF-SHA256(ikm=S, salt=pid bytes, info, 32).
func PairKey(secret []byte, pidHex string) ([]byte, error) {
	salt, err := hex.DecodeString(pidHex)
	if err != nil || len(salt) != 16 {
		return nil, errors.New("bad pid")
	}
	return hkdf.Key(sha256.New, secret, salt, InfoPairKey, 32)
}

// MAC is HMAC-SHA256.
func MAC(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// MACEqual compares two MACs in constant time.
func MACEqual(a, b []byte) bool { return hmac.Equal(a, b) }
