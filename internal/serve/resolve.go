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
//   - p may be absolute or relative. Relative input is resolved against the
//     FIRST root (Wave 6 agent-pod convention: the agent names files
//     relative to its output volume and never needs pod-internal mount
//     paths). With multiple roots only the first is the relative base; a
//     relative path that then escapes it is ErrEscape. An absolute path
//     must sit under one of the roots as before.
//   - p is first cleaned lexically (filepath.Clean). If the cleaned path is
//     not under any root, ErrEscape is returned without touching the disk.
//   - The candidate is then resolved with filepath.EvalSymlinks, and the
//     RESOLVED path must sit under one of the RESOLVED roots. This defeats
//     symlink escapes. Symlinks whose targets stay inside a root are fine.
//   - The file must exist. EvalSymlinks fails on missing paths; Resolve
//     returns an error wrapping os.ErrNotExist (HTTP layer: 404), which is
//     distinguishable from ErrEscape (403) via errors.Is.
//
// TOCTOU note: the validation above is point-in-time. Callers MUST open
// the returned resolved path (never re-walk the original input) to shrink
// the race window; under the read-only-volume threat model the residual
// race (e.g. a symlink swap between Resolve and Open) is acceptable.
//
// Return value is the fully symlink-resolved absolute path on success.
func Resolve(roots []string, p string) (string, error) {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		// Relative path: join against the first root and re-fence with the
		// standard absolute logic (Clean already applied to the join).
		if len(roots) == 0 {
			return "", fmt.Errorf("%w: %q is not an absolute path and no roots are configured", ErrEscape, p)
		}
		return Resolve([]string{roots[0]}, filepath.Join(filepath.Clean(roots[0]), clean))
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
