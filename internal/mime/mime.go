// Package mime determines MIME types for files served by mcp-media.
//
// Import note: this package's name collides with the standard library
// "mime". Callers should import it with an alias, e.g.:
//
//	mediamime "github.com/gthieleb/mcp-media/internal/mime"
package mime

import (
	"io"
	stdmime "mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// fallbackType is returned when neither the extension nor content sniffing
// yields a type, or when the file cannot be opened/read. For never returns
// an error (signature fixed by the project plan), so callers can compare
// against this value to detect "unknown".
const fallbackType = "application/octet-stream"

// extTypes maps media-relevant extensions (lowercase, with dot) to exact
// MIME types. It intentionally takes precedence over the stdlib for these
// extensions so results are deterministic across platforms — stdlib
// mime.TypeByExtension can vary with the OS mime.types database.
var extTypes = map[string]string{
	// audio
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	".flac": "audio/flac",
	// video
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	// image
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".bmp":  "image/bmp",
	// documents / text
	".pdf":  "application/pdf",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".json": "application/json",
	".csv":  "text/csv; charset=utf-8",
	".html": "text/html; charset=utf-8",
}

// sniffLen is the number of leading bytes used for content sniffing, per
// the documented contract of http.DetectContentType.
const sniffLen = 512

// For returns the MIME type for the file at path.
//
// Resolution order:
//  1. Extension (case-insensitive): first extTypes, then stdlib
//     mime.TypeByExtension. No file I/O happens if the extension matches.
//  2. Content sniffing of the first 512 bytes via http.DetectContentType.
//  3. fallbackType ("application/octet-stream") if the file cannot be
//     opened or read — documented choice; For never returns an error.
//
// Note: an empty file yields "text/plain; charset=utf-8"
// (http.DetectContentType on zero bytes classifies as text).
func For(path string) string {
	if ext := strings.ToLower(filepath.Ext(path)); ext != "" {
		if typ, ok := extTypes[ext]; ok {
			return typ
		}
		if typ := stdmime.TypeByExtension(ext); typ != "" {
			return typ
		}
	}
	return sniff(path)
}

// sniff detects the type from file content, falling back to fallbackType
// when the file cannot be opened or read (e.g. directories, permissions).
func sniff(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return fallbackType
	}
	defer f.Close()

	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(f, buf)
	// io.EOF (empty file) and io.ErrUnexpectedEOF (<512 bytes) are fine;
	// any other error means the content is unreadable.
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fallbackType
	}
	return http.DetectContentType(buf[:n])
}
