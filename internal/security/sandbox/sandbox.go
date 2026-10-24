// Package sandbox runs shell commands inside an OS process sandbox:
// bubblewrap on Linux, Seatbelt (sandbox-exec) on macOS. It is a port of
// nanobot/agent/tools/sandbox.py.
//
// Both backends expose the workspace read-write, a fixed set of system
// paths read-only and any configured extra binds, and hide the workspace's
// parent directory (in the default layout ~/.nanobot-go, which holds the
// config, keys and sessions). HOME points at the workspace. Neither backend
// restricts network access.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yedamao/nanobot-go/internal/security"
)

// Backend names, as used in tools.exec.sandbox.
const (
	Bwrap    = "bwrap"
	Seatbelt = "seatbelt"
)

// Backend turns a Plan into the argv that runs it inside a sandbox.
type Backend interface {
	// Name is the config name of the backend.
	Name() string
	// GOOS is the only operating system the backend runs on.
	GOOS() string
	// Program is the sandbox binary, looked up in PATH unless absolute.
	Program() string
	// Argv returns the argv that runs p through program.
	Argv(program string, p Plan) ([]string, error)
	// ProbeArgv returns a harmless argv that only succeeds if program can
	// start a sandboxed process on this host.
	ProbeArgv(program string) []string
}

// Plan is a normalized request: every path is absolute and symlink-free.
type Plan struct {
	// Shell runs Command as `Shell -c Command` inside the sandbox.
	Shell   string
	Command string
	// Workspace is mounted read-write; its parent is hidden.
	Workspace string
	// Cwd is the working directory inside the sandbox, within Workspace.
	Cwd string
	// ROBinds and RWBinds are extra read-only and read-write roots. None
	// of them equals or contains Workspace.
	ROBinds []string
	RWBinds []string
}

// backends is the fixed set of known backends; it is never modified.
var backends = []Backend{bwrapBackend{}, seatbeltBackend{}}

// Names returns the known backend names.
func Names() []string {
	names := make([]string, len(backends))
	for i, b := range backends {
		names[i] = b.Name()
	}
	return names
}

// Lookup returns the backend called name.
func Lookup(name string) (Backend, error) {
	for _, b := range backends {
		if b.Name() == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("unknown sandbox backend %q (available: %s)", name, strings.Join(Names(), ", "))
}

// Request describes a command to wrap.
type Request struct {
	// Program overrides the backend's binary, e.g. with the absolute path
	// Host.Check resolved.
	Program string
	// Shell is the interpreter inside the sandbox (default "sh").
	Shell   string
	Command string
	// Workspace must be an existing absolute directory.
	Workspace string
	// Cwd falls back to Workspace when it lies outside it.
	Cwd string
	// ROBinds and RWBinds are normalized with NormalizeBinds.
	ROBinds []string
	RWBinds []string
}

// Wrap returns the argv that runs r.Command inside backend b.
func Wrap(b Backend, r Request) ([]string, error) {
	if !filepath.IsAbs(r.Workspace) {
		return nil, fmt.Errorf("sandbox: workspace %q is not absolute", r.Workspace)
	}
	ws, err := filepath.EvalSymlinks(r.Workspace)
	if err != nil {
		return nil, fmt.Errorf("sandbox: workspace: %w", err)
	}
	if filepath.Dir(ws) == ws {
		return nil, errors.New("sandbox: the workspace must not be the filesystem root")
	}
	cwd := ws
	if r.Cwd != "" {
		if real, err := security.RealPath(r.Cwd); err == nil && security.IsWithin(real, ws) {
			cwd = real
		}
	}
	program, shell := r.Program, r.Shell
	if program == "" {
		program = b.Program()
	}
	if shell == "" {
		shell = "sh"
	}
	return b.Argv(program, Plan{
		Shell:     shell,
		Command:   r.Command,
		Workspace: ws,
		Cwd:       cwd,
		ROBinds:   NormalizeBinds(r.ROBinds, ws),
		RWBinds:   NormalizeBinds(r.RWBinds, ws),
	})
}

// NormalizeBinds expands ~ and $VAR in paths, drops empty and relative
// entries, resolves symlinks and removes duplicates. Paths equal to or
// containing workspace are dropped too: binding one would cover the mask
// over the workspace's parent and expose the config directory.
func NormalizeBinds(paths []string, workspace string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range paths {
		p := ExpandPath(strings.TrimSpace(raw))
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		real, err := security.RealPath(p)
		if err != nil || seen[real] || containsPath(real, workspace) {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	return out
}

// ExpandPath expands a leading ~ and $VAR / ${VAR} references. Unset
// variables are left as written, like Python's os.path.expandvars.
func ExpandPath(p string) string {
	p = os.Expand(p, func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return "${" + k + "}"
	})
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return p
}

// containsPath reports whether p is root or lies below it, either
// lexically or by file identity, so differently-cased aliases on a
// case-insensitive volume still match.
func containsPath(root, p string) bool {
	if security.IsWithin(p, root) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for cur := p; ; cur = filepath.Dir(cur) {
		if info, err := os.Stat(cur); err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

// ancestors returns every proper ancestor directory of paths, closest
// first, without duplicates.
func ancestors(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		for cur := filepath.Dir(p); ; cur = filepath.Dir(cur) {
			if !seen[cur] {
				seen[cur] = true
				out = append(out, cur)
			}
			if filepath.Dir(cur) == cur {
				break
			}
		}
	}
	return out
}

// Host is the environment backends are checked against.
type Host struct {
	GOOS     string
	LookPath func(file string) (string, error)
	// Run executes a probe argv; nil skips the probe.
	Run func(ctx context.Context, argv []string) error
}

// LocalHost returns the Host this process runs on.
func LocalHost() Host {
	return Host{GOOS: runtime.GOOS, LookPath: exec.LookPath, Run: runProbe}
}

// Check verifies that b can sandbox commands on h and returns the
// absolute path of its binary.
func (h Host) Check(ctx context.Context, b Backend) (string, error) {
	if h.GOOS != b.GOOS() {
		return "", fmt.Errorf("sandbox %q requires %s, this host runs %s", b.Name(), b.GOOS(), h.GOOS)
	}
	program, err := h.LookPath(b.Program())
	if err != nil {
		return "", fmt.Errorf("sandbox %q: %s not found: %w", b.Name(), b.Program(), err)
	}
	if program, err = filepath.Abs(program); err != nil {
		return "", fmt.Errorf("sandbox %q: %w", b.Name(), err)
	}
	if h.Run != nil {
		if err := h.Run(ctx, b.ProbeArgv(program)); err != nil {
			return "", fmt.Errorf("sandbox %q: %s cannot start a sandboxed process: %w", b.Name(), program, err)
		}
	}
	return program, nil
}

// Available reports whether the backend called name can run on this host.
func Available(ctx context.Context, name string) error {
	b, err := Lookup(name)
	if err != nil {
		return err
	}
	_, err = LocalHost().Check(ctx, b)
	return err
}

func runProbe(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(bytes.ToValidUTF8(out, nil))); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// shellQuote quotes s for a POSIX shell, like Python's shlex.quote.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
