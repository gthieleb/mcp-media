package sign

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-secret-key-0123456789abcdef")

// flipChar returns s with the character at index i replaced by a
// different character, guaranteeing the result differs from s.
func flipChar(s string, i int) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[i] == 'a' {
		b[i] = 'b'
	} else {
		b[i] = 'a'
	}
	return string(b)
}

func TestSign_DeterministicHex(t *testing.T) {
	sig1 := Sign(testSecret, "L3RtcC9oZWxsby50eHQ", 1893456000, "inline")
	sig2 := Sign(testSecret, "L3RtcC9oZWxsby50eHQ", 1893456000, "inline")
	if sig1 != sig2 {
		t.Errorf("Sign is not deterministic: %q != %q", sig1, sig2)
	}
	// HMAC-SHA256 hex-encoded must be 64 lowercase hex chars.
	if len(sig1) != 64 {
		t.Errorf("expected 64-char hex signature, got %d chars: %q", len(sig1), sig1)
	}
	for _, c := range sig1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("signature %q contains non-lowercase-hex char %q", sig1, c)
		}
	}
}

func TestSign_DependsOnSecret(t *testing.T) {
	a := Sign([]byte("secret-a"), "cGF0aA", 1893456000, "inline")
	b := Sign([]byte("secret-b"), "cGF0aA", 1893456000, "inline")
	if a == b {
		t.Errorf("different secrets produced identical signature %q", a)
	}
}

func TestVerify_Roundtrip(t *testing.T) {
	now := time.Unix(1750000000, 0)
	future := now.Add(5 * time.Minute).Unix()

	tests := []struct {
		name        string
		secret      []byte
		pathB64     string
		exp         int64
		disposition string
	}{
		{name: "inline", secret: testSecret, pathB64: "L3RtcC9oZWxsby50eHQ", exp: future, disposition: "inline"},
		{name: "attachment", secret: testSecret, pathB64: "L3RtcC9oZWxsby50eHQ", exp: future, disposition: "attachment"},
		{name: "empty disposition", secret: testSecret, pathB64: "L3RtcC9oZWxsby50eHQ", exp: future, disposition: ""},
		{name: "path with base64url chars", secret: testSecret, pathB64: "dXJsLXdpdGhfLWFuZF8", exp: future, disposition: "inline"},
		{name: "empty secret allowed", secret: []byte{}, pathB64: "cGF0aA", exp: future, disposition: "inline"},
		{name: "exp exactly now is valid", secret: testSecret, pathB64: "cGF0aA", exp: now.Unix(), disposition: "inline"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sig := Sign(tc.secret, tc.pathB64, tc.exp, tc.disposition)
			if err := Verify(tc.secret, tc.pathB64, tc.exp, tc.disposition, sig, now); err != nil {
				t.Fatalf("Verify(Sign(...)) returned error: %v", err)
			}
		})
	}
}

func TestVerify_Tamper(t *testing.T) {
	now := time.Unix(1750000000, 0)
	exp := now.Add(5 * time.Minute).Unix()
	sig := Sign(testSecret, "L3RtcC9oZWxsby50eHQ", exp, "inline")

	tests := []struct {
		name        string
		pathB64     string
		exp         int64
		disposition string
		sig         string
	}{
		{name: "flipped pathB64", pathB64: flipChar("L3RtcC9oZWxsby50eHQ", 5), exp: exp, disposition: "inline", sig: sig},
		{name: "different exp (future)", pathB64: "L3RtcC9oZWxsby50eHQ", exp: exp + 60, disposition: "inline", sig: sig},
		{name: "different disposition", pathB64: "L3RtcC9oZWxsby50eHQ", exp: exp, disposition: "attachment", sig: sig},
		{name: "flipped sig char", pathB64: "L3RtcC9oZWxsby50eHQ", exp: exp, disposition: "inline", sig: flipChar(sig, 10)},
		{name: "truncated sig", pathB64: "L3RtcC9oZWxsby50eHQ", exp: exp, disposition: "inline", sig: sig[:len(sig)-2]},
		{name: "uppercased sig", pathB64: "L3RtcC9oZWxsby50eHQ", exp: exp, disposition: "inline", sig: strings.ToUpper(sig)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(testSecret, tc.pathB64, tc.exp, tc.disposition, tc.sig, now)
			if err == nil {
				t.Fatal("Verify succeeded on tampered input")
			}
			if !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("expected ErrInvalidSignature, got %v", err)
			}
			if errors.Is(err, ErrExpired) {
				t.Errorf("tampered input must not report ErrExpired, got %v", err)
			}
		})
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	now := time.Unix(1750000000, 0)
	exp := now.Add(5 * time.Minute).Unix()
	sig := Sign([]byte("other-secret"), "cGF0aA", exp, "inline")

	err := Verify(testSecret, "cGF0aA", exp, "inline", sig, now)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature for wrong secret, got %v", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	now := time.Unix(1750000000, 0)
	past := now.Add(-1 * time.Second).Unix()
	// Valid signature, but exp lies in the past.
	sig := Sign(testSecret, "L3RtcC9oZWxsby50eHQ", past, "inline")

	err := Verify(testSecret, "L3RtcC9oZWxsby50eHQ", past, "inline", sig, now)
	if err == nil {
		t.Fatal("Verify succeeded for expired signature")
	}
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expected ErrExpired, got %v", err)
	}
	if errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expired-but-valid signature must not report ErrInvalidSignature, got %v", err)
	}
}

func TestVerify_BadHexSig(t *testing.T) {
	now := time.Unix(1750000000, 0)
	exp := now.Add(5 * time.Minute).Unix()

	tests := []struct {
		name string
		sig  string
	}{
		{name: "not hex", sig: "zz-not-hex-zz"},
		{name: "odd length", sig: "abc"},
		{name: "empty", sig: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(testSecret, "cGF0aA", exp, "inline", tc.sig, now)
			if err == nil {
				t.Fatal("Verify succeeded for malformed hex signature")
			}
			if !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("expected ErrInvalidSignature for bad hex, got %v", err)
			}
		})
	}
}

// TestVerify_TamperedExpiredExp documents the check order: the signature
// is validated before expiry. Flipping exp into the past therefore yields
// ErrInvalidSignature (the MAC no longer matches), never ErrExpired.
func TestVerify_TamperedExpiredExp(t *testing.T) {
	now := time.Unix(1750000000, 0)
	exp := now.Add(5 * time.Minute).Unix()
	sig := Sign(testSecret, "cGF0aA", exp, "inline")

	err := Verify(testSecret, "cGF0aA", now.Add(-time.Hour).Unix(), "inline", sig, now)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature, got %v", err)
	}
	if errors.Is(err, ErrExpired) {
		t.Errorf("tampered exp must not report ErrExpired, got %v", err)
	}
}

// TestSign_EmptySecret documents the chosen behavior for an empty secret:
// it is allowed (HMAC accepts empty keys per RFC 2104). Enforcing key
// strength is the caller's responsibility; Sign/Verify stay total.
func TestSign_EmptySecret(t *testing.T) {
	now := time.Unix(1750000000, 0)
	exp := now.Add(time.Minute).Unix()
	sig := Sign(nil, "cGF0aA", exp, "inline")
	if sig == "" {
		t.Fatal("Sign with nil secret returned empty signature")
	}
	if err := Verify(nil, "cGF0aA", exp, "inline", sig, now); err != nil {
		t.Errorf("Verify with nil secret failed roundtrip: %v", err)
	}
}
