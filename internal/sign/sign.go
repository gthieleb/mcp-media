// Package sign implements HMAC-SHA256 request signing for media URLs.
//
// The signature authenticates the tuple (pathB64, exp, disposition):
//
//   - pathB64: base64url (raw, no padding) encoding of the file path.
//     Treated as an opaque string; Sign/Verify never decode it.
//   - exp: expiry time as Unix seconds, rendered as a decimal string.
//   - disposition: Content-Disposition hint ("inline" / "attachment").
//
// Canonical signed string (must be reproduced identically by the
// Wave-2 proxy and the media sidecar):
//
//	pathB64 + "\n" + strconv.FormatInt(exp, 10) + "\n" + disposition
//
// The signature is the lowercase hex encoding of HMAC-SHA256 over that
// canonical string.
//
// Empty secret: allowed (HMAC accepts empty keys per RFC 2104) and
// Sign/Verify remain total functions. Enforcing key strength is the
// caller's responsibility (the sidecar refuses to start without a
// configured secret).
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrInvalidSignature is returned by Verify when the signature is not
// valid hex or does not match the canonical string (tampering or wrong
// secret). Callers typically map this to HTTP 403.
var ErrInvalidSignature = errors.New("sign: invalid signature")

// ErrExpired is returned by Verify when the signature is valid but exp
// lies in the past relative to now. Callers typically map this to
// HTTP 403 as well; the distinction exists for observability/tests.
var ErrExpired = errors.New("sign: signature expired")

// canonical builds the canonical signed string documented in the
// package doc: pathB64 + "\n" + exp-as-decimal + "\n" + disposition.
// Newlines cannot appear in base64url or a decimal int64, so the
// encoding is unambiguous.
func canonical(pathB64 string, exp int64, disposition string) string {
	return pathB64 + "\n" + strconv.FormatInt(exp, 10) + "\n" + disposition
}

// mac computes HMAC-SHA256 of msg under secret.
func mac(secret []byte, msg string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// Sign returns the lowercase hex-encoded HMAC-SHA256 signature over the
// canonical form of (pathB64, exp, disposition).
func Sign(secret []byte, pathB64 string, exp int64, disposition string) string {
	return hex.EncodeToString(mac(secret, canonical(pathB64, exp, disposition)))
}

// Verify checks that sig is a valid, unexpired signature for
// (pathB64, exp, disposition) under secret.
//
// Check order: (1) sig must be valid lowercase hex in canonical form
// (as produced by Sign; hex.DecodeString also accepts uppercase, so
// canonical form is enforced explicitly), (2) the MAC is compared in
// constant time via hmac.Equal, (3) expiry is evaluated against now
// (exp == now is still valid; now.After(exp) is expired). Signature
// validity is checked before expiry so that tampered input always
// reports ErrInvalidSignature and never leaks expiry information.
//
// Returns nil on success, an error wrapping ErrInvalidSignature or
// ErrExpired otherwise (use errors.Is to distinguish).
func Verify(secret []byte, pathB64 string, exp int64, disposition, sig string, now time.Time) error {
	got, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: malformed hex: %v", ErrInvalidSignature, err)
	}
	// Enforce canonical lowercase form so each signature has exactly
	// one valid representation (unambiguous URLs, logs, cache keys).
	if hex.EncodeToString(got) != sig {
		return fmt.Errorf("%w: non-canonical hex encoding", ErrInvalidSignature)
	}
	want := mac(secret, canonical(pathB64, exp, disposition))
	if !hmac.Equal(got, want) {
		return fmt.Errorf("%w: mismatch", ErrInvalidSignature)
	}
	if now.After(time.Unix(exp, 0)) {
		return fmt.Errorf("%w: exp=%d now=%d", ErrExpired, exp, now.Unix())
	}
	return nil
}
