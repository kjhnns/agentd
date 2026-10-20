package wire

import (
	"encoding/base64"
	"errors"
	"regexp"
)

// B64 is base64url without padding, strict (canonical trailing bits).
var B64 = base64.RawURLEncoding.Strict()

var b64Re = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// DecodeB64 decodes s and checks the decoded length is in [min, max].
func DecodeB64(s string, min, max int) ([]byte, error) {
	if !b64Re.MatchString(s) {
		return nil, errors.New("bad b64 alphabet")
	}
	b, err := B64.DecodeString(s)
	if err != nil {
		return nil, errors.New("bad b64")
	}
	if len(b) < min || len(b) > max {
		return nil, errors.New("bad b64 length")
	}
	return b, nil
}

// DecodeB64Exact decodes s to exactly n bytes.
func DecodeB64Exact(s string, n int) ([]byte, error) { return DecodeB64(s, n, n) }

// EncodeB64 encodes b.
func EncodeB64(b []byte) string { return B64.EncodeToString(b) }

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsID reports whether s is 32 lowercase hex characters.
func IsID(s string) bool { return idRe.MatchString(s) }
