package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// layout creates <root>/ws and returns the symlink-free root and workspace.
func layout(t *testing.T) (root, ws string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws = filepath.Join(root, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, ws
}

func TestLookup(t *testing.T) {
	for _, name := range []string{Bwrap, Seatbelt} {
		b, err := Lookup(name)
		if err != nil || b.Name() != name {
			t.Errorf("Lookup(%q) = %v, %v", name, b, err)
		}
	}
	for _, name := range []string{"", "firejail", "Bwrap"} {
		if _, err := Lookup(name); err == nil || !strings.Contains(err.Error(), "available: bwrap, seatbelt") {
			t.Errorf("Lookup(%q) err = %v", name, err)
		}
	}
	if got := Names(); !reflect.DeepEqual(got, []string{Bwrap, Seatbelt}) {
		t.Errorf("Names() = %q", got)
	}
}

func TestNormalizeBinds(t *testing.T) {
	root, ws := layout(t)
	home := filepath.Join(root, "home")
	tools := filepath.Join(root, "tools")
	for _, d := range []string{home, tools} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(tools, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SANDBOX_TEST_TOOLS", tools)

	tests := []struct {
		name  string
		binds []string
		want  []string
	}{
		{"empty and relative dropped", []string{"", "  ", "rel/dir", "./x"}, nil},
		{"absolute kept", []string{tools}, []string{tools}},
		{"trimmed", []string{"  " + tools + "  "}, []string{tools}},
		{"tilde", []string{"~/.local/bin"}, []string{filepath.Join(home, ".local/bin")}},
		{"env var", []string{"$SANDBOX_TEST_TOOLS/bin"}, []string{filepath.Join(tools, "bin")}},
		{"unset var left as is", []string{"$SANDBOX_TEST_UNSET/bin"}, nil},
		{"symlink resolved and deduplicated", []string{link, tools, tools + "/"}, []string{tools}},
		{"workspace dropped", []string{ws, ws + "/"}, nil},
		{"workspace parent dropped", []string{root, "/"}, nil},
		{"workspace parent via dots dropped", []string{ws + "/.."}, nil},
		{"inside workspace kept", []string{filepath.Join(ws, "sub")}, []string{filepath.Join(ws, "sub")}},
		{"sibling kept", []string{filepath.Join(root, "secrets")}, []string{filepath.Join(root, "secrets")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeBinds(tt.binds, ws); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NormalizeBinds(%q) = %q, want %q", tt.binds, got, tt.want)
			}
		})
	}
}

// indexSeq returns the index of seq within argv, or -1.
func indexSeq(argv []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return i
		}
	}
	return -1
}

func TestWrapBwrap(t *testing.T) {
	root, ws := layout(t)
	ro, rw := filepath.Join(root, "ro"), filepath.Join(root, "rw")
	b, _ := Lookup(Bwrap)

	tests := []struct {
		name    string
		cwd     string
		wantCwd string
	}{
		{"default cwd", "", ws},
		{"cwd inside", filepath.Join(ws, "sub"), filepath.Join(ws, "sub")},
		{"cwd outside falls back", root, ws},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, err := Wrap(b, Request{
				Command: "echo hi; cat x", Workspace: ws, Cwd: tt.cwd,
				ROBinds: []string{ro, root}, RWBinds: []string{rw, ws},
			})
			if err != nil {
				t.Fatal(err)
			}
			if argv[0] != "bwrap" {
				t.Errorf("program = %q", argv[0])
			}
			want := [][]string{
				{"--new-session", "--die-with-parent"},
				{"--setenv", "HOME", ws},
				{"--ro-bind", "/usr", "/usr"},
				{"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf"},
				{"--tmpfs", root, "--dir", ws, "--bind", ws, ws},
				{"--ro-bind-try", ro, ro},
				{"--bind-try", rw, rw},
				{"--chdir", tt.wantCwd, "--", "sh", "-c", "echo hi; cat x"},
			}
			last := -1
			for _, seq := range want {
				i := indexSeq(argv, seq...)
				if i < 0 || i < last {
					t.Fatalf("argv %q: %q missing or out of order", argv, seq)
				}
				last = i
			}
			if !strings.HasSuffix(strings.Join(argv, " "), "-c echo hi; cat x") {
				t.Errorf("command is not last: %q", argv)
			}
			// Binds containing the workspace would uncover the masked parent.
			if indexSeq(argv, "--ro-bind-try", root, root) >= 0 || indexSeq(argv, "--bind-try", ws, ws) >= 0 {
				t.Errorf("workspace-containing bind kept: %q", argv)
			}
		})
	}

	t.Run("program and shell override", func(t *testing.T) {
		argv, err := Wrap(b, Request{Program: "/opt/bin/bwrap", Shell: "bash", Command: "true", Workspace: ws})
		if err != nil {
			t.Fatal(err)
		}
		if argv[0] != "/opt/bin/bwrap" || indexSeq(argv, "--", "bash", "-c", "true") < 0 {
			t.Errorf("argv = %q", argv)
		}
	})

	t.Run("workspace directly under root", func(t *testing.T) {
		_, err := b.Argv("bwrap", Plan{Shell: "sh", Workspace: "/ws", Cwd: "/ws"})
		if err == nil || !strings.Contains(err.Error(), "directly under /") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestWrapSeatbelt(t *testing.T) {
	root, ws := layout(t)
	ro, rw := filepath.Join(root, "ro"), filepath.Join(root, "rw")
	b, _ := Lookup(Seatbelt)

	argv, err := Wrap(b, Request{
		Command: "echo hi", Workspace: ws, Cwd: filepath.Join(ws, "sub"),
		ROBinds: []string{ro, root}, RWBinds: []string{rw},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 9 || argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-p" {
		t.Fatalf("argv = %q", argv)
	}
	wantTail := []string{"/usr/bin/env", "HOME=" + ws, "TMPDIR=" + ws, "sh", "-c",
		"cd " + filepath.Join(ws, "sub") + " || exit 1\necho hi"}
	if !slices.Equal(argv[3:], wantTail) {
		t.Errorf("argv tail = %q, want %q", argv[3:], wantTail)
	}

	profile := argv[2]
	rules := strings.Split(profile, "\n")
	ruleIndex := func(rule string) int { return slices.Index(rules, rule) }
	q := sbplQuote
	denyParent := ruleIndex("(deny file-read* file-write* (subpath " + q(root) + "))")
	allowWS := ruleIndex("(allow file-read* file-write* (subpath " + q(ws) + "))")
	if denyParent < 0 || allowWS < 0 || denyParent > allowWS {
		t.Errorf("parent deny (%d) must precede workspace allow (%d):\n%s", denyParent, allowWS, profile)
	}
	if !slices.Equal(rules[:3], []string{"(version 1)", "(deny default)", "(allow process*)"}) {
		t.Errorf("profile head = %q", rules[:3])
	}
	if ruleIndex("(allow network*)") < 0 {
		t.Errorf("network must stay unrestricted:\n%s", profile)
	}
	// Bind rules come after the workspace allow so they win over the
	// parent deny.
	for _, rule := range []string{
		"(allow file-read* (subpath " + q(ro) + "))",
		"(deny file-write* (subpath " + q(ro) + "))",
		"(allow file-read* file-write* (subpath " + q(rw) + "))",
	} {
		if ruleIndex(rule) < allowWS {
			t.Errorf("rule %q missing or before the workspace allow:\n%s", rule, profile)
		}
	}
	// The masked parent stays traversable (metadata only) and / is listed.
	meta := rules[slices.IndexFunc(rules, func(r string) bool { return strings.HasPrefix(r, "(allow file-read-metadata") })]
	for _, lit := range []string{"/", "/private", root} {
		if !strings.Contains(meta, "(literal "+q(lit)+")") {
			t.Errorf("metadata rule lacks %s: %s", lit, meta)
		}
	}
	if strings.Contains(profile, "(allow file-read* (subpath "+q(root)+"))") {
		t.Errorf("workspace parent bind was not dropped:\n%s", profile)
	}
	unlink := rules[len(rules)-1]
	if !strings.HasPrefix(unlink, "(deny file-write-unlink") || !strings.Contains(unlink, "(literal "+q(root)+")") {
		t.Errorf("last rule = %s", unlink)
	}
}

func TestSeatbeltRWBindOverridesROProtection(t *testing.T) {
	root, ws := layout(t)
	ro := filepath.Join(root, "tools", "bin")
	b, _ := Lookup(Seatbelt)
	argv, err := Wrap(b, Request{Command: "true", Workspace: ws,
		ROBinds: []string{ro}, RWBinds: []string{filepath.Join(root, "tools")}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(argv[2], "file-write-unlink") {
		t.Errorf("RW bind covering the RO root should drop its unlink protection:\n%s", argv[2])
	}
}

func TestWrapQuoting(t *testing.T) {
	root, _ := layout(t)
	ws := filepath.Join(root, `it's "odd"\dir`)
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := Lookup(Seatbelt)
	argv, err := Wrap(b, Request{Command: "pwd", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if want := `(subpath "` + root + `/it's \"odd\"\\dir")`; !strings.Contains(argv[2], want) {
		t.Errorf("profile lacks %s:\n%s", want, argv[2])
	}
	if want := `cd '` + root + `/it'"'"'s "odd"\dir' || exit 1` + "\npwd"; argv[8] != want {
		t.Errorf("script = %q, want %q", argv[8], want)
	}
}

func TestWrapErrors(t *testing.T) {
	_, ws := layout(t)
	b, _ := Lookup(Seatbelt)
	tests := []struct {
		name, ws, want string
	}{
		{"relative", "ws", "not absolute"},
		{"missing", filepath.Join(ws, "missing"), "workspace"},
		{"root", "/", "filesystem root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Wrap(b, Request{Command: "true", Workspace: tt.ws}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestHostCheck(t *testing.T) {
	bw, _ := Lookup(Bwrap)
	sb, _ := Lookup(Seatbelt)
	found := func(string) (string, error) { return "/usr/bin/bwrap", nil }
	missing := func(string) (string, error) { return "", errors.New("executable file not found in $PATH") }
	ok := func(context.Context, []string) error { return nil }
	fail := func(context.Context, []string) error { return errors.New("setting up uid map: Permission denied") }

	tests := []struct {
		name    string
		backend Backend
		host    Host
		want    string
	}{
		{"wrong os", bw, Host{GOOS: "darwin", LookPath: found, Run: ok}, `"bwrap" requires linux, this host runs darwin`},
		{"seatbelt on linux", sb, Host{GOOS: "linux", LookPath: found, Run: ok}, "requires darwin"},
		{"missing binary", bw, Host{GOOS: "linux", LookPath: missing, Run: ok}, "bwrap not found"},
		{"probe fails", bw, Host{GOOS: "linux", LookPath: found, Run: fail}, "cannot start a sandboxed process: setting up uid map"},
		{"ok", bw, Host{GOOS: "linux", LookPath: found, Run: ok}, ""},
		{"ok without probe", bw, Host{GOOS: "linux", LookPath: found}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prog, err := tt.host.Check(context.Background(), tt.backend)
			if tt.want == "" {
				if err != nil || prog != "/usr/bin/bwrap" {
					t.Errorf("Check = %q, %v", prog, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}

	var probed []string
	h := Host{GOOS: "linux", LookPath: found, Run: func(_ context.Context, argv []string) error {
		probed = argv
		return nil
	}}
	if _, err := h.Check(context.Background(), bw); err != nil {
		t.Fatal(err)
	}
	if len(probed) == 0 || probed[0] != "/usr/bin/bwrap" || probed[len(probed)-1] != "true" {
		t.Errorf("probe argv = %q", probed)
	}
}

func TestAvailableUnknown(t *testing.T) {
	if err := Available(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "unknown sandbox backend") {
		t.Errorf("err = %v", err)
	}
}
