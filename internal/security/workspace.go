// Package security holds the policy checks tools apply before touching the
// filesystem, running commands or making network requests. These are
// application-level guards, not process isolation.
package security

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrOutsideWorkspace is returned when a path resolves outside the workspace.
var ErrOutsideWorkspace = errors.New("path is outside the workspace")

// Workspace resolves tool paths relative to a root directory and, when
// RestrictToWorkspace is set, rejects paths that escape it (including via
// symlinks or "..").
type Workspace struct {
	// Root is the absolute, symlink-free workspace directory.
	Root string
	// RestrictToWorkspace confines all resolved paths to Root.
	RestrictToWorkspace bool
}

// NewWorkspace returns a Workspace rooted at root, which must be an existing
// directory.
func NewWorkspace(root string, restrict bool) (*Workspace, error) {
	abs, err := filepath.Abs(expandHome(root))
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q: not a directory", root)
	}
	return &Workspace{Root: real, RestrictToWorkspace: restrict}, nil
}

// ResolveRead resolves a path the agent wants to read.
func (w *Workspace) ResolveRead(path string) (string, error) {
	return w.resolve(path)
}

// ResolveWrite resolves a path the agent wants to create or modify. The
// file need not exist yet.
func (w *Workspace) ResolveWrite(path string) (string, error) {
	return w.resolve(path)
}

// Contains reports whether the resolved path p lies within the workspace.
func (w *Workspace) Contains(p string) bool {
	_, ok := w.relWithin(p)
	return ok
}

// Rel returns p relative to the workspace root when possible, for display.
func (w *Workspace) Rel(p string) string {
	if rel, ok := w.relWithin(p); ok {
		return rel
	}
	return p
}

// OpenRoot opens the directory through which the resolved path p must be
// accessed and returns p relative to it: the workspace root when the
// workspace is restricted, otherwise the root of p's volume. Operations on
// the returned os.Root refuse to follow symlinks or ".." out of it, so a
// symlink swapped in between resolving and opening cannot redirect them.
// The caller must close the root.
func (w *Workspace) OpenRoot(p string) (*os.Root, string, error) {
	dir, rel := w.Root, ""
	if r, ok := w.relWithin(p); ok {
		rel = r
	} else if w.RestrictToWorkspace {
		return nil, "", fmt.Errorf("%w: %s (workspace: %s)", ErrOutsideWorkspace, p, w.Root)
	} else {
		dir = filepath.VolumeName(p) + string(filepath.Separator)
		if rel, _ = filepath.Rel(dir, p); rel == "" {
			rel = "."
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// relWithin returns p relative to the workspace root if p lies within it.
// Besides the lexical check it compares each ancestor of p with the root
// by file identity, so a path spelled with different case on a
// case-insensitive filesystem (the macOS default) is still recognised.
func (w *Workspace) relWithin(p string) (string, bool) {
	if IsWithin(p, w.Root) {
		rel, err := filepath.Rel(w.Root, p)
		return rel, err == nil
	}
	rootInfo, err := os.Stat(w.Root)
	if err != nil {
		return "", false
	}
	var below []string
	for cur := filepath.Clean(p); ; {
		if info, err := os.Stat(cur); err == nil && os.SameFile(info, rootInfo) {
			slices.Reverse(below)
			return filepath.Join(append([]string{"."}, below...)...), true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		below = append(below, filepath.Base(cur))
		cur = parent
	}
}

func (w *Workspace) resolve(path string) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("invalid path %q: contains NUL byte", path)
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path must not be empty")
	}
	p := expandHome(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	real, err := RealPath(p)
	if err != nil {
		return "", err
	}
	if w.RestrictToWorkspace && !w.Contains(real) {
		return "", fmt.Errorf("%w: %s (workspace: %s)", ErrOutsideWorkspace, path, w.Root)
	}
	return real, nil
}

// RealPath returns the absolute, cleaned form of p with symlinks resolved
// in its deepest existing ancestor. Components that do not exist yet are
// appended unchanged. A component that exists but cannot be resolved (for
// example a dangling symlink) is an error, so writes cannot be redirected
// through it.
func RealPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	var missing []string
	cur := abs
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", fmt.Errorf("resolve %q: %w", p, err)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %q: %w", p, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// IsWithin reports whether p equals root or lies beneath it. Both paths
// should already be absolute and cleaned.
func IsWithin(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
