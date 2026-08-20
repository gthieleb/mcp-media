// Package mint implements the internal signed-URL minting API (T1.7).
//
// The sidecar exposes this handler on its internal listener (:8091).
// Callers (the Wave-2 proxy) POST a file path and receive a signed,
// expiring URL that the public file server (:8090) later verifies using
// internal/sign. Path fencing is delegated to internal/serve BEFORE
// minting, so escaping or missing paths never produce a signed URL.
//
// Security properties:
//   - Bearer-token auth (MEDIA_INTERNAL_TOKEN) compared in constant time;
//     401 responses are generic and never hint at the failure reason.
//   - Request bodies are capped at 4 KiB and strictly decoded (unknown
//     JSON fields are rejected).
//   - TTLs are clamped to [DefaultTTL, MaxTTL]; the plan's values are
//     300s default / 900s max.
//   - The signature, the full minted URL and the bearer token are never
//     logged; the requested path may be logged.
package mint

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gthieleb/mcp-media/internal/serve"
	"github.com/gthieleb/mcp-media/internal/sign"
)

// maxRequestBytes caps the POST /mint request body (4 KiB).
const maxRequestBytes = 4 << 10

// Config holds the startup configuration for the mint handler. NewHandler
// validates it; the sidecar must refuse to start on invalid configuration
// (startup validation per the plan's security section).
type Config struct {
	// Secret is the HMAC-SHA256 key for signing (must be >= 32 bytes).
	Secret []byte
	// Token is the bearer token callers must present
	// (env MEDIA_INTERNAL_TOKEN). Must not be empty.
	Token string
	// PublicBaseURL is the externally reachable base of the file server,
	// e.g. "https://media.example.com" (http or https only). A trailing
	// slash is tolerated and trimmed.
	PublicBaseURL string
	// Roots are the media root directories used for path fencing.
	Roots []string
	// DefaultTTL applies when ttl_seconds is absent or 0 (plan: 300s).
	DefaultTTL time.Duration
	// MaxTTL clamps larger requested TTLs (plan: 900s).
	MaxTTL time.Duration
}

type handler struct {
	secret  []byte
	token   []byte
	baseURL string
	roots   []string
	defTTL  time.Duration
	maxTTL  time.Duration
}

// NewHandler validates cfg and returns the mint API handler. The handler
// serves exactly one route: POST /mint.
func NewHandler(cfg Config) (http.Handler, error) {
	if len(cfg.Secret) < 32 {
		return nil, fmt.Errorf("mint: secret must be at least 32 bytes, got %d", len(cfg.Secret))
	}
	if cfg.Token == "" {
		return nil, errors.New("mint: token must not be empty")
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("mint: invalid public base URL %q: %w", cfg.PublicBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mint: public base URL must use http or https, got %q", cfg.PublicBaseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mint: public base URL %q has no host", cfg.PublicBaseURL)
	}
	if cfg.DefaultTTL <= 0 {
		return nil, fmt.Errorf("mint: default TTL must be positive, got %s", cfg.DefaultTTL)
	}
	if cfg.MaxTTL < cfg.DefaultTTL {
		return nil, fmt.Errorf("mint: max TTL %s must be >= default TTL %s", cfg.MaxTTL, cfg.DefaultTTL)
	}
	return &handler{
		secret:  slices.Clone(cfg.Secret),
		token:   []byte(cfg.Token),
		baseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		roots:   cfg.Roots,
		defTTL:  cfg.DefaultTTL,
		maxTTL:  cfg.MaxTTL,
	}, nil
}

// mintRequest is the JSON body of POST /mint.
type mintRequest struct {
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	TTLSeconds  int64  `json:"ttl_seconds"`
}

// mintResponse is the JSON answer of POST /mint.
type mintResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mint" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorized(r) {
		// Generic 401: never reveal whether the token was absent,
		// malformed or wrong. Presented credentials are never logged.
		slog.Warn("mint: unauthorized request", "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	h.mint(w, r)
}

// authorized checks the Authorization header against the configured bearer
// token using a constant-time comparison on the raw bytes.
func (h *handler) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), h.token) == 1
}

func (h *handler) mint(w http.ResponseWriter, r *http.Request) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}

	var req mintRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		slog.Warn("mint: invalid request body", "err", err)
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Reject trailing data after the first JSON value.
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	disposition := req.Disposition
	if disposition == "" {
		disposition = "attachment"
	}
	if disposition != "inline" && disposition != "attachment" {
		writeError(w, http.StatusBadRequest, `disposition must be "inline" or "attachment"`)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if req.TTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, "ttl_seconds must not be negative")
		return
	}
	ttl := h.defTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > h.maxTTL {
			ttl = h.maxTTL
		}
	}

	// Fence BEFORE minting: never sign URLs for escaping or missing files.
	resolved, err := serve.Resolve(h.roots, req.Path)
	if err != nil {
		switch {
		case errors.Is(err, serve.ErrEscape):
			slog.Warn("mint: path escapes roots", "path", req.Path)
			writeError(w, http.StatusForbidden, "forbidden")
		case errors.Is(err, os.ErrNotExist):
			slog.Info("mint: path not found", "path", req.Path)
			writeError(w, http.StatusNotFound, "not found")
		default:
			slog.Error("mint: resolve failed", "path", req.Path, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	exp := time.Now().Add(ttl)
	pathB64 := base64.RawURLEncoding.EncodeToString([]byte(resolved))
	sig := sign.Sign(h.secret, pathB64, exp.Unix(), disposition)
	minted := h.baseURL + "/media/" + pathB64 + "/" + url.PathEscape(path.Base(resolved)) +
		"?exp=" + strconv.FormatInt(exp.Unix(), 10) + "&d=" + disposition + "&sig=" + sig

	// Never log the signature or the full minted URL.
	slog.Info("mint: issued signed url", "path", req.Path, "ttl", ttl.String(), "exp", exp.Unix())

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(mintResponse{
		URL:       minted,
		ExpiresAt: exp.UTC().Format(time.RFC3339),
	})
}

// writeError sends a JSON error body. Error responses are never cacheable.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
