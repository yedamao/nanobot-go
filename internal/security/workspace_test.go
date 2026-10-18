package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestWorkspace creates root/ws (the workspace) and root/outside with a
// secret file, so escapes have somewhere real to land.
func newTestWorkspace(t *testing.T, restrict bool) (ws *Workspace, outside string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root, restrict)
	if err != nil {
		t.Fatal(err)
	}
	outside, err = filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	return ws, outside
}

func TestNewWorkspaceErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(dir, "missing"), file} {
		if _, err := NewWorkspace(root, true); err == nil {
			t.Errorf("NewWorkspace(%q) succeeded, want error", root)
		}
	}
}

func TestResolveRestricted(t *testing.T) {
	ws, outside := newTestWorkspace(t, true)
	// Symlinks inside the workspace: one escaping, one internal, one dangling.
	mustSymlink(t, outside, filepath.Join(ws.Root, "escape"))
	mustSymlink(t, filepath.Join(ws.Root, "sub"), filepath.Join(ws.Root, "inner"))
	mustSymlink(t, filepath.Join(outside, "new.txt"), filepath.Join(ws.Root, "dangling"))

	tests := []struct {
		name    string
		path    string
		want    string // relative to root; ignored when wantErr
		wantErr bool
	}{
		{"relative file", "sub/a.txt", "sub/a.txt", false},
		{"dot", ".", ".", false},
		{"new nested file", "new/dir/f.txt", "new/dir/f.txt", false},
		{"absolute inside", filepath.Join(ws.Root, "sub", "a.txt"), "sub/a.txt", false},
		{"internal symlink", "inner/a.txt", "sub/a.txt", false},
		{"dotdot inside", "sub/../sub/a.txt", "sub/a.txt", false},
		{"dotdot escape", "../outside/secret.txt", "", true},
		{"deep dotdot escape", "sub/../../outside/x", "", true},
		{"absolute outside", filepath.Join(outside, "secret.txt"), "", true},
		{"symlink escape read", "escape/secret.txt", "", true},
		{"symlink escape new file", "escape/new/file.txt", "", true},
		{"symlink dir itself", "escape", "", true},
		{"dangling symlink", "dangling", "", true},
		{"etc", "/etc/passwd", "", true},
		{"empty", "", "", true},
		{"nul byte", "a\x00b", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, resolve := range []func(string) (string, error){ws.ResolveRead, ws.ResolveWrite} {
				got, err := resolve(tt.path)
				if tt.wantErr {
					if err == nil {
						t.Fatalf("resolve(%q) = %q, want error", tt.path, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("resolve(%q): %v", tt.path, err)
				}
				if want := filepath.Join(ws.Root, tt.want); got != want {
					t.Errorf("resolve(%q) = %q, want %q", tt.path, got, want)
				}
			}
		})
	}

	if _, err := ws.ResolveRead("../outside"); !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("want ErrOutsideWorkspace, got %v", err)
	}
}

func TestResolveUnrestricted(t *testing.T) {
	ws, outside := newTestWorkspace(t, false)
	got, err := ws.ResolveRead(filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(outside, "secret.txt") {
		t.Errorf("got %q", got)
	}
	got, err = ws.ResolveWrite("rel.txt")
	if err != nil || got != filepath.Join(ws.Root, "rel.txt") {
		t.Errorf("relative paths should still be rooted in the workspace: %q, %v", got, err)
	}
}

func TestIsWithin(t *testing.T) {
	tests := []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/a/..b", "/a", true},
	}
	for _, tt := range tests {
		if got := IsWithin(tt.p, tt.root); got != tt.want {
			t.Errorf("IsWithin(%q, %q) = %v, want %v", tt.p, tt.root, got, tt.want)
		}
	}
}

func TestRel(t *testing.T) {
	ws, outside := newTestWorkspace(t, true)
	if got := ws.Rel(filepath.Join(ws.Root, "sub", "a.txt")); got != filepath.Join("sub", "a.txt") {
		t.Errorf("Rel inside = %q", got)
	}
	if got := ws.Rel(outside); got != outside {
		t.Errorf("Rel outside = %q", got)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}
