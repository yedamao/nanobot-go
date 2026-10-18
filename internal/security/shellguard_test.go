package security

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestShellGuardDenyPatterns(t *testing.T) {
	g, err := NewShellGuard(ShellGuardConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cmd     string
		blocked bool
	}{
		{"ls -la", false},
		{"echo hello", false},
		{"rm file.txt", false},
		{"rm -rf /", true},
		{"RM -RF build", true},
		{"rm -fr x", true},
		{"rm -r x", true},
		{"cd x && rm -rf y", true},
		{"del /f a.txt", true},
		{"rmdir /s dir", true},
		{"format c:", true},
		{"ls; format", true},
		{"clang-format -i x.c", false},
		{"git log --format=%h", false},
		{"mkfs.ext4 /dev/sda1", true},
		{"diskpart", true},
		{"dd if=/dev/zero of=x", true},
		{"echo x > /dev/sda", true},
		{"shutdown -h now", true},
		{"sudo reboot", true},
		{"poweroff", true},
		{":(){ :|:& };:", true},
		{"echo x >> memory/history.jsonl", true},
		{"echo 3 > memory/.dream_cursor", true},
		{"echo x | tee -a memory/history.jsonl", true},
		{"cp other memory/history.jsonl", true},
		{"mv a b memory/.dream_cursor", true},
		{"sed -i s/a/b/ memory/history.jsonl", true},
		{"cat memory/history.jsonl", false},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			err := g.Check(tt.cmd, "")
			if got := err != nil; got != tt.blocked {
				t.Fatalf("Check(%q) err = %v, blocked want %v", tt.cmd, err, tt.blocked)
			}
			if err != nil && !errors.Is(err, ErrCommandBlocked) {
				t.Errorf("error does not wrap ErrCommandBlocked: %v", err)
			}
		})
	}
}

func TestShellGuardAllowPatterns(t *testing.T) {
	g, err := NewShellGuard(ShellGuardConfig{AllowPatterns: []string{`rm -rf build`, `ls.*`, `echo \w+`}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cmd     string
		blocked bool
	}{
		{"rm -rf build", false}, // allow overrides deny
		{"ls -la && rm -rf build", false},
		{"rm -rf build && rm -rf /", true}, // every segment must be allowed
		{"cat x", true},                    // not in allowlist
		{"echo hi; cat secret", true},
	}
	for _, tt := range tests {
		if err := g.Check(tt.cmd, ""); (err != nil) != tt.blocked {
			t.Errorf("Check(%q) = %v, blocked want %v", tt.cmd, err, tt.blocked)
		}
	}
}

func TestShellGuardExtraDenyAndBadPatterns(t *testing.T) {
	g, err := NewShellGuard(ShellGuardConfig{DenyPatterns: []string{`\bcurl\b`}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Check("curl example.com", ""); err == nil {
		t.Error("extra deny pattern not applied")
	}
	if _, err := NewShellGuard(ShellGuardConfig{DenyPatterns: []string{"("}}); err == nil {
		t.Error("expected error for invalid deny pattern")
	}
	if _, err := NewShellGuard(ShellGuardConfig{AllowPatterns: []string{"["}}); err == nil {
		t.Error("expected error for invalid allow pattern")
	}
	if err := g.Check("   ", ""); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestShellGuardInternalURLs(t *testing.T) {
	g, _ := NewShellGuard(ShellGuardConfig{})
	tests := []struct {
		cmd     string
		blocked bool
	}{
		{"curl https://example.com/x", false},
		{"curl http://localhost:8080/", true},
		{"curl http://127.0.0.1/", true},
		{"wget http://169.254.169.254/latest/meta-data", true},
		{"curl http://10.0.0.5", true},
		{"curl 'http://[::1]:80/'", true},
		{"curl http://api.localhost/", true},
	}
	for _, tt := range tests {
		if err := g.Check(tt.cmd, ""); (err != nil) != tt.blocked {
			t.Errorf("Check(%q) = %v, blocked want %v", tt.cmd, err, tt.blocked)
		}
	}
}

func TestShellGuardWorkspaceContainment(t *testing.T) {
	ws, outside := newTestWorkspace(t, true)
	mustSymlink(t, outside, filepath.Join(ws.Root, "escape"))
	g, err := NewShellGuard(ShellGuardConfig{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	t.Setenv("NANOBOT_TEST_OUTSIDE", outside)

	tests := []struct {
		name    string
		cmd     string
		cwd     string
		blocked bool
	}{
		{"plain", "ls -la", "", false},
		{"relative path", "cat sub/a.txt", "", false},
		{"subdir cwd", "ls", filepath.Join(ws.Root, "sub"), false},
		{"abs inside", "cat " + filepath.Join(ws.Root, "sub", "a.txt"), "", false},
		{"dev null", "ls > /dev/null 2>&1", "", false},
		{"dev fd", "cat /dev/fd/0", "", false},
		{"url path", "curl https://example.com/etc/passwd", "", false},
		{"range syntax", "git diff main..HEAD", "", false},
		{"pwd var", "ls $PWD/sub", "", false},
		{"cd dotdot", "cd ..", "", true},
		{"dotdot slash", "cat ../outside/secret.txt", "", true},
		{"nested dotdot", "cat sub/../../x", "", true},
		{"etc passwd", "cat /etc/passwd", "", true},
		{"quoted abs", `cat "/etc/passwd"`, "", true},
		{"redirect outside", "echo x >/tmp/x", "", true},
		{"flag value", "tool --out=/etc/x", "", true},
		{"embedded sh -c", `sh -c "cat /etc/hosts"`, "", true},
		{"home tilde", "ls ~", "", !IsWithin(home, ws.Root)},
		{"home subdir", "cat ~/.ssh/id_rsa", "", true},
		{"other user home", "ls ~root", "", true},
		{"dir stack", "ls ~-", "", true},
		{"env var", "cat $NANOBOT_TEST_OUTSIDE/secret.txt", "", true},
		{"symlink escape", "cat " + filepath.Join(ws.Root, "escape", "secret.txt"), "", true},
		{"cwd outside", "ls", outside, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := g.Check(tt.cmd, tt.cwd)
			if (err != nil) != tt.blocked {
				t.Fatalf("Check(%q, %q) = %v, blocked want %v", tt.cmd, tt.cwd, err, tt.blocked)
			}
			if err != nil && !strings.Contains(err.Error(), "safety guard") {
				t.Errorf("unexpected error text: %v", err)
			}
		})
	}

	// Unrestricted workspaces skip path checks but keep the deny list.
	open, _ := NewShellGuard(ShellGuardConfig{Workspace: &Workspace{Root: ws.Root}})
	if err := open.Check("cat /etc/passwd && cd ..", ""); err != nil {
		t.Errorf("unrestricted guard blocked path: %v", err)
	}
	if err := open.Check("rm -rf /", ""); err == nil {
		t.Error("unrestricted guard must still apply deny patterns")
	}
}

func TestSplitShellSegments(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		{"ls", []string{"ls"}},
		{"a && b || c; d | e", []string{"a", "b", "c", "d", "e"}},
		{"sleep 1 & echo x", []string{"sleep 1 &", "echo x"}},
		{"cmd 2>&1 | tee", []string{"cmd 2>&1", "tee"}},
		{"cmd &> out", []string{"cmd &> out"}},
		{`echo "a; b" && echo 'c | d'`, []string{`echo "a; b"`, `echo 'c | d'`}},
		{`echo a\;b`, []string{`echo a\;b`}},
		{"(cd x; ls) && pwd", []string{"(cd x; ls)", "pwd"}},
		{"a\nb", []string{"a", "b"}},
		{"  ", nil},
	}
	for _, tt := range tests {
		if got := SplitShellSegments(tt.cmd); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitShellSegments(%q) = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}
