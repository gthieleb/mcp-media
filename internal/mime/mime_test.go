package mime_test

import (
	"os"
	"path/filepath"
	"testing"

	// Package name "mime" collides with the stdlib; alias per package doc.
	mediamime "github.com/gthieleb/mcp-media/internal/mime"
)

// TestFor_ExtensionMap pins every entry of the explicit extension map.
// Files do not need to exist: extension lookup happens before any I/O.
func TestFor_ExtensionMap(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		// audio
		"song.ogg":  "audio/ogg",
		"song.opus": "audio/opus",
		"song.mp3":  "audio/mpeg",
		"song.wav":  "audio/wav",
		"song.m4a":  "audio/mp4",
		"song.flac": "audio/flac",
		// video
		"movie.mp4":  "video/mp4",
		"movie.webm": "video/webm",
		"movie.mkv":  "video/x-matroska",
		"movie.mov":  "video/quicktime",
		// image
		"pic.jpg":  "image/jpeg",
		"pic.jpeg": "image/jpeg",
		"pic.png":  "image/png",
		"pic.gif":  "image/gif",
		"pic.webp": "image/webp",
		"pic.svg":  "image/svg+xml",
		"pic.bmp":  "image/bmp",
		// documents / text
		"doc.pdf":   "application/pdf",
		"notes.txt": "text/plain; charset=utf-8",
		"README.md": "text/markdown; charset=utf-8",
		"data.json": "application/json",
		"data.csv":  "text/csv; charset=utf-8",
		"page.html": "text/html; charset=utf-8",
	}
	dir := t.TempDir()
	for name, want := range cases {
		if got := mediamime.For(filepath.Join(dir, name)); got != want {
			t.Errorf("For(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestFor_ExtensionCaseInsensitive(t *testing.T) {
	t.Parallel()
	if got := mediamime.For("PHOTO.PNG"); got != "image/png" {
		t.Errorf("For(uppercase ext) = %q, want image/png", got)
	}
}

func TestFor_ExtensionWinsWithoutFile(t *testing.T) {
	t.Parallel()
	// Non-existent path, known extension: no I/O error, no octet-stream.
	if got := mediamime.For("/nonexistent/dir/clip.webm"); got != "video/webm" {
		t.Errorf("For(nonexistent .webm) = %q, want video/webm", got)
	}
}

func TestFor_StdlibExtensionFallback(t *testing.T) {
	t.Parallel()
	// ".css" is not in the explicit map; stdlib mime.TypeByExtension
	// (built-in table) knows it as text/css; charset=utf-8.
	if got := mediamime.For("style.css"); got != "text/css; charset=utf-8" {
		t.Errorf("For(.css) = %q, want text/css; charset=utf-8 (stdlib fallback)", got)
	}
}

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}

func TestFor_SniffNoExtension(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mediamime.For(p); got != "image/png" {
		t.Errorf("For(extensionless PNG) = %q, want image/png", got)
	}
}

func TestFor_SniffUnknownExtension(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "blob.mcpmediaunknown")
	if err := os.WriteFile(p, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mediamime.For(p); got != "image/png" {
		t.Errorf("For(unknown-ext PNG) = %q, want image/png", got)
	}
}

func TestFor_SniffText(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, []byte("hello world, this is plain text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mediamime.For(p); got != "text/plain; charset=utf-8" {
		t.Errorf("For(text blob) = %q, want text/plain; charset=utf-8", got)
	}
}

func TestFor_MissingFileNoExtension(t *testing.T) {
	t.Parallel()
	// Documented choice: unreadable file -> application/octet-stream,
	// no error (signature stays For(path string) string per plan).
	p := filepath.Join(t.TempDir(), "does-not-exist")
	if got := mediamime.For(p); got != "application/octet-stream" {
		t.Errorf("For(missing file) = %q, want application/octet-stream", got)
	}
}

func TestFor_DirectoryIsUnsniffable(t *testing.T) {
	t.Parallel()
	// Opening a directory succeeds but reading it fails -> octet-stream.
	if got := mediamime.For(t.TempDir()); got != "application/octet-stream" {
		t.Errorf("For(directory) = %q, want application/octet-stream", got)
	}
}

func TestFor_EmptyFile(t *testing.T) {
	t.Parallel()
	// Pinned behavior: http.DetectContentType on 0 bytes yields
	// "text/plain; charset=utf-8" (nothing binary detected -> text).
	p := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mediamime.For(p); got != "text/plain; charset=utf-8" {
		t.Errorf("For(empty file) = %q, want text/plain; charset=utf-8", got)
	}
}
