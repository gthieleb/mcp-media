package mint

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gthieleb/mcp-media/internal/sign"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef") // 32 bytes

const (
	testToken   = "internal-test-token-0123456789abcdef"
	testBaseURL = "https://media.example.com"
)

func testConfig(roots []string) Config {
	return Config{
		Secret:        testSecret,
		Token:         testToken,
		PublicBaseURL: testBaseURL,
		Roots:         roots,
		DefaultTTL:    300 * time.Second,
		MaxTTL:        900 * time.Second,
	}
}

func newTestHandler(t *testing.T, roots []string) http.Handler {
	t.Helper()
	h, err := NewHandler(testConfig(roots))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// newRoot creates a temp root dir containing one file and returns
// (root, filePath).
func newRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "foo.ogg")
	if err := os.WriteFile(file, []byte("ogg-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return root, file
}

// postJSON issues an authenticated POST /mint with a JSON body.
func postJSON(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mint", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type mintedURL struct {
	raw         string
	pathB64     string
	filename    string
	exp         int64
	disposition string
	sig         string
}

// parseMinted decodes the mint response body and dissects the minted URL.
func parseMinted(t *testing.T, rec *httptest.ResponseRecorder) mintedURL {
	t.Helper()
	var resp struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body %q)", err, rec.Body.String())
	}
	u, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatalf("parse minted url %q: %v", resp.URL, err)
	}
	rest, ok := strings.CutPrefix(u.Path, "/media/")
	if !ok {
		t.Fatalf("minted url path %q does not start with /media/", u.Path)
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("minted url path %q: want {pathB64}/{filename}", u.Path)
	}
	q := u.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("parse exp %q: %v", q.Get("exp"), err)
	}
	// expires_at must agree with the exp query parameter.
	expAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", resp.ExpiresAt, err)
	}
	if expAt.Unix() != exp {
		t.Fatalf("expires_at (%d) disagrees with exp query (%d)", expAt.Unix(), exp)
	}
	return mintedURL{
		raw:         resp.URL,
		pathB64:     parts[0],
		filename:    parts[1],
		exp:         exp,
		disposition: q.Get("d"),
		sig:         q.Get("sig"),
	}
}

// assertExpWithin checks exp ∈ [before+ttl, after+ttl].
func assertExpWithin(t *testing.T, exp int64, before, after time.Time, ttl time.Duration) {
	t.Helper()
	lo, hi := before.Add(ttl).Unix(), after.Add(ttl).Unix()
	if exp < lo || exp > hi {
		t.Errorf("exp = %d, want within [%d, %d] (now + %s)", exp, lo, hi, ttl)
	}
}

func TestMintHappyPath(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})

	before := time.Now()
	rec := postJSON(t, h, fmt.Sprintf(`{"path": %q, "ttl_seconds": 60}`, file))
	after := time.Now()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	m := parseMinted(t, rec)

	// disposition omitted → default "attachment".
	if m.disposition != "attachment" {
		t.Errorf("disposition = %q, want default attachment", m.disposition)
	}

	// pathB64 decodes to the canonical resolved file path.
	decoded, err := base64.RawURLEncoding.DecodeString(m.pathB64)
	if err != nil {
		t.Fatalf("decode pathB64: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if string(decoded) != wantPath {
		t.Errorf("signed path = %q, want %q", decoded, wantPath)
	}
	if m.filename != "foo.ogg" {
		t.Errorf("filename = %q, want foo.ogg", m.filename)
	}

	assertExpWithin(t, m.exp, before, after, 60*time.Second)

	// The signature must verify against the tuple (pathB64, exp, disposition).
	if err := sign.Verify(testSecret, m.pathB64, m.exp, m.disposition, m.sig, time.Now()); err != nil {
		t.Errorf("sign.Verify(minted url): %v", err)
	}

	if !strings.HasPrefix(m.raw, testBaseURL+"/media/") {
		t.Errorf("url %q does not start with %q", m.raw, testBaseURL+"/media/")
	}
}

func TestMintDispositionInline(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})

	rec := postJSON(t, h, fmt.Sprintf(`{"path": %q, "disposition": "inline"}`, file))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	m := parseMinted(t, rec)
	if m.disposition != "inline" {
		t.Errorf("disposition = %q, want inline", m.disposition)
	}
	if err := sign.Verify(testSecret, m.pathB64, m.exp, "inline", m.sig, time.Now()); err != nil {
		t.Errorf("sign.Verify: %v", err)
	}
}

func TestMintTTL(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})

	t.Run("absent uses default 300s", func(t *testing.T) {
		before := time.Now()
		rec := postJSON(t, h, fmt.Sprintf(`{"path": %q}`, file))
		after := time.Now()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		assertExpWithin(t, parseMinted(t, rec).exp, before, after, 300*time.Second)
	})

	t.Run("explicit zero uses default", func(t *testing.T) {
		before := time.Now()
		rec := postJSON(t, h, fmt.Sprintf(`{"path": %q, "ttl_seconds": 0}`, file))
		after := time.Now()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		assertExpWithin(t, parseMinted(t, rec).exp, before, after, 300*time.Second)
	})

	t.Run("clamped to max 900s", func(t *testing.T) {
		before := time.Now()
		rec := postJSON(t, h, fmt.Sprintf(`{"path": %q, "ttl_seconds": 99999}`, file))
		after := time.Now()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		assertExpWithin(t, parseMinted(t, rec).exp, before, after, 900*time.Second)
	})
}

func TestMintBadRequest(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})

	pad := strings.Repeat(" ", 5000) // pushes the body over the 4 KiB cap
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{"path":`},
		{"unknown field", fmt.Sprintf(`{"path": %q, "bogus": true}`, file)},
		{"negative ttl", fmt.Sprintf(`{"path": %q, "ttl_seconds": -1}`, file)},
		{"bad disposition", fmt.Sprintf(`{"path": %q, "disposition": "download"}`, file)},
		{"missing path", `{"disposition": "inline"}`},
		{"empty path", `{"path": ""}`},
		{"trailing json value", fmt.Sprintf(`{"path": %q} {}`, file)},
		{"body too large", fmt.Sprintf(`{"path": %q}`, file) + pad},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, h, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMintUnauthorized(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})
	body := fmt.Sprintf(`{"path": %q}`, file)

	tests := []struct {
		name   string
		header string // empty = no Authorization header at all
	}{
		{"no authorization header", ""},
		{"wrong token", "Bearer definitely-wrong-token"},
		{"malformed scheme", "Token " + testToken},
		{"bearer without token", "Bearer"},
		{"empty bearer token", "Bearer "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mint", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
			}
			// The 401 body must be generic: no token, no reason hints.
			if strings.Contains(rec.Body.String(), testToken) {
				t.Errorf("401 body leaks the token: %q", rec.Body.String())
			}
		})
	}
}

// TestAuthUsesConstantTimeCompare guards against regressions to a plain
// ==/strings.Equal token comparison (timing side channel).
func TestAuthUsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatalf("read handler.go: %v", err)
	}
	if !strings.Contains(string(src), "subtle.ConstantTimeCompare") {
		t.Error("handler.go must compare tokens with crypto/subtle.ConstantTimeCompare")
	}
}

func TestMintPathErrors(t *testing.T) {
	root, _ := newRoot(t)
	h := newTestHandler(t, []string{root})

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want int
	}{
		{"absolute path outside roots", outside, http.StatusForbidden},
		{"dot-dot traversal", filepath.Join(root, "..", filepath.Base(outsideDir), "secret.txt"), http.StatusForbidden},
		{"relative path", "relative/file.ogg", http.StatusForbidden},
		{"missing file inside root", filepath.Join(root, "missing.ogg"), http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, h, fmt.Sprintf(`{"path": %q}`, tc.path))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMintMethodNotAllowed(t *testing.T) {
	root, _ := newRoot(t)
	h := newTestHandler(t, []string{root})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/mint", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

func TestMintUnknownPathNotFound(t *testing.T) {
	root, _ := newRoot(t)
	h := newTestHandler(t, []string{root})
	req := httptest.NewRequest(http.MethodPost, "/other", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestMintUnsupportedMediaType(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})
	body := fmt.Sprintf(`{"path": %q}`, file)

	for _, tc := range []struct {
		name string
		ct   string // empty = no Content-Type header
	}{
		{"missing content type", ""},
		{"text plain", "text/plain"},
		{"form encoded", "application/x-www-form-urlencoded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mint", strings.NewReader(body))
			if tc.ct != "" {
				req.Header.Set("Content-Type", tc.ct)
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("status = %d, want 415", rec.Code)
			}
		})
	}

	t.Run("charset parameter accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/mint", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

func TestMintLogsNoSecrets(t *testing.T) {
	root, file := newRoot(t)
	h := newTestHandler(t, []string{root})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Successful mint: must not log the signature or the full URL.
	rec := postJSON(t, h, fmt.Sprintf(`{"path": %q}`, file))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	m := parseMinted(t, rec)

	// Failed auth with a presented token that CONTAINS the real one:
	// if the handler logs presented credentials, the real token leaks.
	req := httptest.NewRequest(http.MethodPost, "/mint", strings.NewReader(fmt.Sprintf(`{"path": %q}`, file)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken+"-forged")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec2.Code)
	}

	logs := buf.String()
	if logs == "" {
		t.Error("expected some log output")
	}
	if strings.Contains(logs, testToken) {
		t.Error("logs contain the bearer token")
	}
	if strings.Contains(logs, m.sig) {
		t.Error("logs contain the signature")
	}
	if strings.Contains(logs, m.raw) {
		t.Error("logs contain the full minted URL")
	}
}

func TestMintBaseURLTrailingSlash(t *testing.T) {
	root, file := newRoot(t)
	cfg := testConfig([]string{root})
	cfg.PublicBaseURL = testBaseURL + "/"
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rec := postJSON(t, h, fmt.Sprintf(`{"path": %q}`, file))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	m := parseMinted(t, rec)
	if !strings.HasPrefix(m.raw, testBaseURL+"/media/") || strings.Contains(m.raw, "//media/") {
		t.Errorf("unexpected minted url %q", m.raw)
	}
}

func TestMintFilenameEscaped(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a b.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, []string{root})
	rec := postJSON(t, h, fmt.Sprintf(`{"path": %q}`, file))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	m := parseMinted(t, rec)
	if !strings.Contains(m.raw, "/a%20b.txt?") {
		t.Errorf("url %q does not contain the PathEscape'd filename", m.raw)
	}
}

func TestNewHandlerValidation(t *testing.T) {
	root, _ := newRoot(t)

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"secret too short", func(c *Config) { c.Secret = testSecret[:31] }},
		{"nil secret", func(c *Config) { c.Secret = nil }},
		{"empty token", func(c *Config) { c.Token = "" }},
		{"base url wrong scheme", func(c *Config) { c.PublicBaseURL = "ftp://media.example.com" }},
		{"base url missing scheme", func(c *Config) { c.PublicBaseURL = "media.example.com" }},
		{"base url without host", func(c *Config) { c.PublicBaseURL = "https://" }},
		{"base url unparseable", func(c *Config) { c.PublicBaseURL = "://no-scheme" }},
		{"zero default ttl", func(c *Config) { c.DefaultTTL = 0 }},
		{"negative default ttl", func(c *Config) { c.DefaultTTL = -time.Second }},
		{"max below default", func(c *Config) { c.MaxTTL = c.DefaultTTL - time.Second }},
		{"zero max ttl", func(c *Config) { c.MaxTTL = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig([]string{root})
			tc.mutate(&cfg)
			if _, err := NewHandler(cfg); err == nil {
				t.Error("NewHandler: expected error, got nil")
			}
		})
	}

	t.Run("valid config", func(t *testing.T) {
		if _, err := NewHandler(testConfig([]string{root})); err != nil {
			t.Errorf("NewHandler: unexpected error: %v", err)
		}
	})
}
