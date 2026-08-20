// Package serve provides path fencing for the media file server: it maps a
// requested path to an on-disk location while guaranteeing the result stays
// inside one of the configured root directories.
package serve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is returned when a requested path does not resolve to a location
// inside any of the configured roots. Callers (HTTP layer) should map it to
// 403 Forbidden. It covers lexical traversal ("../.."), absolute paths
// outside all roots, symlink escapes, and non-absolute input.
var ErrEscape = errors.New("path escapes all configured roots")

// Resolve fences p against roots and returns the canonical absolute path.
//
// Contract:
//   - roots are absolute directories. Each root is symlink-resolved
//     (filepath.EvalSymlinks) per call, so a root reached through a symlink
//     works. Roots that cannot be resolved are skipped (fail closed).
//   - p MUST be an absolute path. Relative input is rejected with ErrEscape;
//     there is no implicit joining against roots or the working directory.
//   - p is first cleaned lexically (filepath.Clean). If the cleaned path is
//     not under any root, ErrEscape is returned without touching the disk.
//   - The candidate is then resolved with filepath.EvalSymlinks, and the
//     RESOLVED path must sit under one of the RESOLVED roots. This defeats
//     symlink escapes. Symlinks whose targets stay inside a root are fine.
//   - The file must exist. EvalSymlinks fails on missing paths; Resolve
//     returns an error wrapping os.ErrNotExist (HTTP layer: 404), which is
//     distinguishable from ErrEscape (403) via errors.Is.
//
// Return value is the fully symlink-resolved absolute path on success.
func Resolve(roots []string, p string) (string, error) {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrEscape, p)
	}

	// Cheap lexical pre-check: avoids touching the disk for obvious escapes.
	inRoot := false
	for _, root := range roots {
		if underRoot(clean, filepath.Clean(root)) {
			inRoot = true
			break
		}
	}
	if !inRoot {
		return "", fmt.Errorf("%w: %q", ErrEscape, p)
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Missing file (or dangling component): wrap so callers can use
		// errors.Is(err, os.ErrNotExist) for a 404 mapping.
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}

	for _, root := range roots {
		resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue // unresolvable root: fail closed for that root
		}
		if underRoot(resolved, resolvedRoot) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: %q resolves outside all roots", ErrEscape, p)
}

// underRoot reports whether path is root itself or lies underneath it,
// with a proper path-separator boundary ("/a/media2" is NOT under "/a/media").
func underRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}
