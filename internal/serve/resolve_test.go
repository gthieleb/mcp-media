package serve

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// tree is a test fixture on disk:
//
//	base/
//	  media/            (root A)
//	    hello.txt
//	    sub/deep.txt
//	  media2/           (prefix-sibling of root A, NOT under it)
//	    sibling.txt
//	  other/            (root B, for multi-root tests)
//	    second.txt
//	  outside/          (never a root)
//	    secret.txt
type tree struct {
	base    string
	rootA   string
	rootA2  string
	rootB   string
	outside string
}

func newTree(t *testing.T) tree {
	t.Helper()
	base := t.TempDir()
	tr := tree{
		base:    base,
		rootA:   filepath.Join(base, "media"),
		rootA2:  filepath.Join(base, "media2"),
		rootB:   filepath.Join(base, "other"),
		outside: filepath.Join(base, "outside"),
	}
	files := map[string]string{
		filepath.Join(tr.rootA, "hello.txt"):       "hello",
		filepath.Join(tr.rootA, "sub", "deep.txt"): "deep",
		filepath.Join(tr.rootA2, "sibling.txt"):    "sibling",
		filepath.Join(tr.rootB, "second.txt"):      "second",
		filepath.Join(tr.outside, "secret.txt"):    "secret",
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(name), err)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return tr
}

func resolvedEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

func TestResolveLegitNestedFile(t *testing.T) {
	tr := newTree(t)
	for _, p := range []string{
		filepath.Join(tr.rootA, "hello.txt"),
		filepath.Join(tr.rootA, "sub", "deep.txt"),
	} {
		got, err := Resolve([]string{tr.rootA}, p)
		if err != nil {
			t.Fatalf("Resolve(%q) returned error: %v", p, err)
		}
		if want := resolvedEval(t, p); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestResolveDotDotInsideRoot(t *testing.T) {
	tr := newTree(t)
	// ".." that never leaves the root is fine after Clean.
	p := filepath.Join(tr.rootA, "sub", "..", "hello.txt")
	got, err := Resolve([]string{tr.rootA}, p)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", p, err)
	}
	if want := resolvedEval(t, filepath.Join(tr.rootA, "hello.txt")); got != want {
		t.Errorf("Resolve(%q) = %q, want %q", p, got, want)
	}
}

func TestResolveTraversalEscape(t *testing.T) {
	tr := newTree(t)
	for _, p := range []string{
		filepath.Join(tr.rootA, "..", "outside", "secret.txt"),
		filepath.Join(tr.rootA, "..", "..", "etc", "hostname"),
		filepath.Join(tr.rootA, "sub", "..", "..", "outside", "secret.txt"),
	} {
		if _, err := Resolve([]string{tr.rootA}, p); !errors.Is(err, ErrEscape) {
			t.Errorf("Resolve(%q): expected ErrEscape, got %v", p, err)
		}
	}
}

func TestResolveAbsoluteOutsideRoots(t *testing.T) {
	tr := newTree(t)
	p := filepath.Join(tr.outside, "secret.txt")
	if _, err := Resolve([]string{tr.rootA}, p); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(%q): expected ErrEscape, got %v", p, err)
	}
}

func TestResolvePrefixSiblingNotInside(t *testing.T) {
	tr := newTree(t)
	// "/.../media2/sibling.txt" shares the string prefix "/.../media" with
	// root A but is not underneath it. A naive strings.HasPrefix without a
	// path-separator boundary would wrongly accept this.
	p := filepath.Join(tr.rootA2, "sibling.txt")
	if _, err := Resolve([]string{tr.rootA}, p); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(%q): expected ErrEscape, got %v", p, err)
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	tr := newTree(t)
	link := filepath.Join(tr.rootA, "evil-link")
	if err := os.Symlink(tr.outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p := filepath.Join(link, "secret.txt")
	if _, err := Resolve([]string{tr.rootA}, p); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(%q): expected ErrEscape, got %v", p, err)
	}
}

func TestResolveSymlinkWithinRoot(t *testing.T) {
	tr := newTree(t)
	// A symlink whose target stays inside the same root is legitimate.
	link := filepath.Join(tr.rootA, "sub", "good-link.txt")
	if err := os.Symlink(filepath.Join(tr.rootA, "hello.txt"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := Resolve([]string{tr.rootA}, link)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", link, err)
	}
	if want := resolvedEval(t, filepath.Join(tr.rootA, "hello.txt")); got != want {
		t.Errorf("Resolve(%q) = %q, want %q", link, got, want)
	}
}

func TestResolveSymlinkedRoot(t *testing.T) {
	tr := newTree(t)
	// The root itself may be reached through a symlink; since roots are
	// resolved with EvalSymlinks too, this must work.
	linkRoot := filepath.Join(tr.base, "root-link")
	if err := os.Symlink(tr.rootA, linkRoot); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p := filepath.Join(linkRoot, "hello.txt")
	got, err := Resolve([]string{linkRoot}, p)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", p, err)
	}
	if want := resolvedEval(t, filepath.Join(tr.rootA, "hello.txt")); got != want {
		t.Errorf("Resolve(%q) = %q, want %q", p, got, want)
	}
}

func TestResolveMultipleRoots(t *testing.T) {
	tr := newTree(t)
	p := filepath.Join(tr.rootB, "second.txt")
	got, err := Resolve([]string{tr.rootA, tr.rootB}, p)
	if err != nil {
		t.Fatalf("Resolve(%q) returned error: %v", p, err)
	}
	if want := resolvedEval(t, p); got != want {
		t.Errorf("Resolve(%q) = %q, want %q", p, got, want)
	}
}

func TestResolveNoRoots(t *testing.T) {
	tr := newTree(t)
	p := filepath.Join(tr.rootA, "hello.txt")
	if _, err := Resolve(nil, p); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(nil, %q): expected ErrEscape, got %v", p, err)
	}
}

func TestResolveRelativePath(t *testing.T) {
	tr := newTree(t)
	// Documented contract: p must be absolute. Relative paths are rejected
	// as escape errors regardless of the current working directory.
	if _, err := Resolve([]string{tr.rootA}, "hello.txt"); !errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(relative): expected ErrEscape, got %v", err)
	}
}

func TestResolveNotExist(t *testing.T) {
	tr := newTree(t)
	p := filepath.Join(tr.rootA, "nope.txt")
	_, err := Resolve([]string{tr.rootA}, p)
	if err == nil {
		t.Fatalf("Resolve(%q): expected error, got nil", p)
	}
	// Must map to 404 (os.ErrNotExist), NOT to 403 (ErrEscape).
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Resolve(%q): expected errors.Is(err, os.ErrNotExist), got %v", p, err)
	}
	if errors.Is(err, ErrEscape) {
		t.Errorf("Resolve(%q): must not be ErrEscape, got %v", p, err)
	}
}
