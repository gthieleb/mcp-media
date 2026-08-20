package serve

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mediamime "github.com/gthieleb/mcp-media/internal/mime"
	"github.com/gthieleb/mcp-media/internal/sign"
)

// NewHandler returns an http.Handler that serves files from roots via
// signed URLs:
//
//	GET /media/{pathB64}/{filename}?exp=<unix>&d=<inline|attachment>&sig=<hex>
//
// pathB64 is the base64.RawURLEncoding of the absolute file path; filename
// is a display name used only for the Content-Disposition header (the file
// to serve is derived exclusively from the resolved path). The signature
// (see internal/sign) authenticates the tuple (pathB64, exp, disposition).
// An omitted d parameter defaults to "attachment".
//
// Responses: 200/206 on success (via http.ServeContent: Range and
// conditional requests, HEAD included), 400 for malformed requests, 403
// for invalid/expired signatures and path escapes, 404 for missing
// in-root files. Successful responses carry Content-Type from
// internal/mime, Content-Disposition, Cache-Control "private, no-store"
// and X-Content-Type-Options "nosniff".
//
// Security: client-facing errors never reveal which check failed, and
// rejection logs contain only the (decoded) file path — never the
// signature and never the full signed URL.
func NewHandler(secret []byte, roots []string) http.Handler {
	h := &mediaHandler{secret: secret, roots: roots}
	mux := http.NewServeMux()
	mux.Handle("GET /media/{pathB64}/{filename}", h)
	return mux
}

type mediaHandler struct {
	secret []byte
	roots  []string
}

func (h *mediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathB64 := r.PathValue("pathB64")
	filename := r.PathValue("filename")

	q := r.URL.Query()

	expStr := q.Get("exp")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if expStr == "" || err != nil {
		http.Error(w, "missing or invalid exp parameter", http.StatusBadRequest)
		return
	}

	sig := q.Get("sig")
	if sig == "" {
		http.Error(w, "missing sig parameter", http.StatusBadRequest)
		return
	}

	disposition := q.Get("d")
	if disposition == "" {
		disposition = "attachment"
	}
	if disposition != "inline" && disposition != "attachment" {
		http.Error(w, "invalid d parameter", http.StatusBadRequest)
		return
	}

	raw, err := base64.RawURLEncoding.DecodeString(pathB64)
	if err != nil {
		http.Error(w, "invalid path encoding", http.StatusBadRequest)
		return
	}
	path := string(raw)

	// Authenticate before touching the filesystem. The client-facing 403
	// deliberately does not say which check failed; internally we log the
	// decoded path only, never the signature or the full signed URL.
	if err := sign.Verify(h.secret, pathB64, exp, disposition, sig, time.Now()); err != nil {
		slog.Warn("media: rejected request with invalid or expired signature", "path", path, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	resolved, err := Resolve(h.roots, path)
	if err != nil {
		switch {
		case errors.Is(err, ErrEscape):
			slog.Warn("media: rejected path escape", "path", path)
			http.Error(w, "forbidden", http.StatusForbidden)
		case errors.Is(err, os.ErrNotExist):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			slog.Error("media: resolve failed", "path", path, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	// Open the RESOLVED path (never re-walk the original) to shrink the
	// TOCTOU window — see Resolve's godoc.
	f, err := os.Open(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			slog.Error("media: open failed", "path", resolved, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		slog.Error("media: stat failed", "path", resolved, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if stat.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if filename == "" {
		filename = filepath.Base(resolved)
	}

	header := w.Header()
	header.Set("Content-Type", mediamime.For(resolved))
	header.Set("Content-Disposition", disposition+"; filename=\""+sanitizeFilename(filename)+"\"")
	header.Set("Cache-Control", "private, no-store")
	header.Set("X-Content-Type-Options", "nosniff")

	http.ServeContent(w, r, filepath.Base(resolved), stat.ModTime(), f)
}

// sanitizeFilename removes characters that could break out of the quoted
// Content-Disposition filename parameter or inject headers: double quote,
// backslash, and all control characters (including CR and LF). Returns
// "file" when nothing usable remains.
func sanitizeFilename(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if clean == "" {
		return "file"
	}
	return clean
}
