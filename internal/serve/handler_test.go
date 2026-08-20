package serve

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gthieleb/mcp-media/internal/sign"
)

var handlerSecret = []byte("handler-test-secret-0123456789abcdef")

// b64url encodes p the same way the mint API encodes the {pathB64} path
// segment: base64.RawURLEncoding of the absolute file path.
func b64url(p string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p))
}

// signedMediaURL builds a /media URL signed with secret over
// (pathB64, exp, d). An empty d omits the query parameter entirely; the
// signature is then computed over the server-side default "attachment".
func signedMediaURL(secret []byte, pathB64, filename string, exp int64, d string) string {
	sigDisposition := d
	if sigDisposition == "" {
		sigDisposition = "attachment"
	}
	sig := sign.Sign(secret, pathB64, exp, sigDisposition)
	u := fmt.Sprintf("/media/%s/%s?exp=%d&sig=%s", pathB64, url.PathEscape(filename), exp, sig)
	if d != "" {
		u += "&d=" + url.QueryEscape(d)
	}
	return u
}

func futureExp() int64 {
	return time.Now().Add(5 * time.Minute).Unix()
}

// do performs a request against h and returns the recorded response.
func do(t *testing.T, h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerGetOK(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	tests := []struct {
		name     string
		path     string
		filename string
		d        string
		wantBody string
		wantCD   string
	}{
		{
			name:     "default disposition is attachment",
			path:     filepath.Join(tr.rootA, "hello.txt"),
			filename: "hello.txt",
			d:        "",
			wantBody: "hello",
			wantCD:   `attachment; filename="hello.txt"`,
		},
		{
			name:     "inline disposition",
			path:     filepath.Join(tr.rootA, "hello.txt"),
			filename: "hello.txt",
			d:        "inline",
			wantBody: "hello",
			wantCD:   `inline; filename="hello.txt"`,
		},
		{
			name:     "nested file",
			path:     filepath.Join(tr.rootA, "sub", "deep.txt"),
			filename: "deep.txt",
			d:        "",
			wantBody: "deep",
			wantCD:   `attachment; filename="deep.txt"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := signedMediaURL(handlerSecret, b64url(tc.path), tc.filename, futureExp(), tc.d)
			rec := do(t, h, http.MethodGet, u, nil)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200 (body %q)", u, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", got, "text/plain; charset=utf-8")
			}
			if got := rec.Header().Get("Content-Disposition"); got != tc.wantCD {
				t.Errorf("Content-Disposition = %q, want %q", got, tc.wantCD)
			}
			if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if got := rec.Header().Get("Last-Modified"); got == "" {
				t.Error("Last-Modified header missing (ServeContent conditional support)")
			}
			if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
				t.Errorf("Accept-Ranges = %q, want %q", got, "bytes")
			}
		})
	}
}

func TestHandlerRangePartial(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	u := signedMediaURL(handlerSecret, b64url(filepath.Join(tr.rootA, "hello.txt")), "hello.txt", futureExp(), "")
	rec := do(t, h, http.MethodGet, u, map[string]string{"Range": "bytes=0-4"})

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-4/5" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 0-4/5")
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
	// Security headers also apply to partial responses.
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

func TestHandlerHead(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	u := signedMediaURL(handlerSecret, b64url(filepath.Join(tr.rootA, "hello.txt")), "hello.txt", futureExp(), "inline")
	rec := do(t, h, http.MethodHead, u, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want %q", got, "5")
	}
	if got := rec.Header().Get("Content-Disposition"); got != `inline; filename="hello.txt"` {
		t.Errorf("Content-Disposition = %q, want %q", got, `inline; filename="hello.txt"`)
	}
}

func TestHandlerForbidden(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})
	pB64 := b64url(filepath.Join(tr.rootA, "hello.txt"))
	exp := futureExp()
	sig := sign.Sign(handlerSecret, pB64, exp, "inline")

	// flipFirst returns s with its first hex character replaced by a
	// different one: a well-formed but wrong signature.
	flipFirst := func(s string) string {
		if s[0] == 'a' {
			return "b" + s[1:]
		}
		return "a" + s[1:]
	}

	mk := func(exp int64, sig string) string {
		return fmt.Sprintf("/media/%s/hello.txt?exp=%d&d=inline&sig=%s", pB64, exp, sig)
	}

	pastExp := time.Now().Add(-time.Minute).Unix()
	tests := []struct {
		name string
		url  string
	}{
		{"wrong signature", mk(exp, flipFirst(sig))},
		{"tampered exp", mk(exp+60, sig)},
		{"expired url", mk(pastExp, sign.Sign(handlerSecret, pB64, pastExp, "inline"))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tc.url, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			// The client must not learn which check failed.
			body := strings.ToLower(rec.Body.String())
			if strings.Contains(body, "expired") || strings.Contains(body, "signature") {
				t.Errorf("403 body leaks the failure reason: %q", body)
			}
		})
	}
}

func TestHandlerForbiddenEscape(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	// All paths are properly signed: fencing must hold even when the
	// minter (or an attacker who obtained a signature) targets files
	// outside the configured roots.
	tests := []struct {
		name string
		path string
	}{
		{"relative traversal", "../.."},
		{"absolute outside root", filepath.Join(tr.outside, "secret.txt")},
		{"traversal escaping root", filepath.Join(tr.rootA, "..", "outside", "secret.txt")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := signedMediaURL(handlerSecret, b64url(tc.path), "secret.txt", futureExp(), "")
			rec := do(t, h, http.MethodGet, u, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerNotFound(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	u := signedMediaURL(handlerSecret, b64url(filepath.Join(tr.rootA, "missing.txt")), "missing.txt", futureExp(), "")
	rec := do(t, h, http.MethodGet, u, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestHandlerDirectoryNotFound(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	// A signed URL to a directory resolves and opens fine, but must not
	// be served as file content.
	u := signedMediaURL(handlerSecret, b64url(filepath.Join(tr.rootA, "sub")), "sub", futureExp(), "")
	rec := do(t, h, http.MethodGet, u, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
}

func TestHandlerBadRequest(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})
	pB64 := b64url(filepath.Join(tr.rootA, "hello.txt"))
	exp := futureExp()
	sig := sign.Sign(handlerSecret, pB64, exp, "attachment")

	tests := []struct {
		name string
		url  string
	}{
		{"missing exp", fmt.Sprintf("/media/%s/hello.txt?sig=%s", pB64, sig)},
		{"invalid exp", fmt.Sprintf("/media/%s/hello.txt?exp=soon&sig=%s", pB64, sig)},
		{"missing sig", fmt.Sprintf("/media/%s/hello.txt?exp=%d", pB64, exp)},
		{"empty sig", fmt.Sprintf("/media/%s/hello.txt?exp=%d&sig=", pB64, exp)},
		{"bad disposition", signedMediaURL(handlerSecret, pB64, "hello.txt", exp, "bogus")},
		{"malformed base64", signedMediaURL(handlerSecret, "!!!not-base64!!!", "hello.txt", exp, "")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, tc.url, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})

	u := signedMediaURL(handlerSecret, b64url(filepath.Join(tr.rootA, "hello.txt")), "hello.txt", futureExp(), "")
	rec := do(t, h, http.MethodPost, u, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, http.MethodGet) {
		t.Errorf("Allow = %q, want it to contain GET", got)
	}
}

func TestHandlerContentDispositionSanitized(t *testing.T) {
	tr := newTree(t)
	h := NewHandler(handlerSecret, []string{tr.rootA})
	pB64 := b64url(filepath.Join(tr.rootA, "hello.txt"))

	// The filename path segment is attacker-controlled (it is not part of
	// the signed tuple); it must never break the Content-Disposition
	// header or inject CRLF.
	tests := []struct {
		name     string
		filename string
		wantCD   string
	}{
		{"quote stripped", `evil"name.txt`, `attachment; filename="evilname.txt"`},
		{"crlf stripped", "line\r\nbreak.txt", `attachment; filename="linebreak.txt"`},
		{"backslash stripped", `a\b.txt`, `attachment; filename="ab.txt"`},
		{"all stripped falls back", "\"\\\r\n", `attachment; filename="file"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := signedMediaURL(handlerSecret, pB64, tc.filename, futureExp(), "")
			rec := do(t, h, http.MethodGet, u, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			cd := rec.Header().Get("Content-Disposition")
			if cd != tc.wantCD {
				t.Errorf("Content-Disposition = %q, want %q", cd, tc.wantCD)
			}
			if strings.ContainsAny(cd, "\r\n") {
				t.Errorf("Content-Disposition contains CR/LF: %q", cd)
			}
		})
	}
}

func TestHandlerDoesNotLogSignature(t *testing.T) {
	tr := newTree(t)
	pB64 := b64url(filepath.Join(tr.rootA, "hello.txt"))
	exp := futureExp()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := NewHandler(handlerSecret, []string{tr.rootA})

	// Distinctive, well-formed but wrong signature.
	badSig := strings.Repeat("deadbeef", 8)
	u := fmt.Sprintf("/media/%s/hello.txt?exp=%d&d=inline&sig=%s", pB64, exp, badSig)
	rec := do(t, h, http.MethodGet, u, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("rejected request produced no log output")
	}
	if strings.Contains(out, badSig) {
		t.Errorf("log contains the signature value:\n%s", out)
	}
	if strings.Contains(out, "sig=") {
		t.Errorf("log contains the sig query parameter:\n%s", out)
	}
	if strings.Contains(out, u) {
		t.Errorf("log contains the full signed URL:\n%s", out)
	}
}
