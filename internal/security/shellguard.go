package security

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrCommandBlocked is returned (wrapped) when ShellGuard rejects a command.
var ErrCommandBlocked = errors.New("command blocked by safety guard")

// defaultDenyPatterns is ported from nanobot/agent/tools/shell.py. Patterns
// are matched against the lower-cased command. Python's `format(?!=)\b` is
// rewritten without lookahead for RE2.
var defaultDenyPatterns = []string{
	`\brm\b[^|;&\n]*\s(?:-[a-z]*[rf][a-z]*|--(?:recursive|force))\b`, // rm -r/-f in any flag cluster or position
	`\bdel\s+/[fq]\b`,                                   // del /f, del /q
	`\brmdir\s+/s\b`,                                    // rmdir /s
	`(?:^|[;&|]\s*)format(?:$|[^\w=])`,                  // format (as standalone command only)
	`\b(?:mkfs|diskpart)\b`,                             // disk operations
	`\bdd\s+if=`,                                        // dd
	`>\s*/dev/sd`,                                       // write to disk
	`\b(?:shutdown|reboot|poweroff)\b`,                  // system power
	`:\(\)\s*\{.*\};\s*:`,                               // fork bomb
	`>>?\s*\S*(?:history\.jsonl|\.dream_cursor)`,        // redirect into memory state
	`\btee\b[^|;&<>]*(?:history\.jsonl|\.dream_cursor)`, // tee / tee -a
	`\b(?:cp|mv)\b(?:\s+[^\s|;&<>]+)+\s+\S*(?:history\.jsonl|\.dream_cursor)`, // cp/mv target
	`\bdd\b[^|;&<>]*\bof=\S*(?:history\.jsonl|\.dream_cursor)`,                // dd of=
	`\bsed\s+-i[^|;&<>]*(?:history\.jsonl|\.dream_cursor)`,                    // sed -i
}

// benignDevicePaths are safe stdio redirect targets that are never blocked.
var benignDevicePaths = map[string]bool{
	"/dev/null": true, "/dev/zero": true, "/dev/full": true, "/dev/random": true,
	"/dev/urandom": true, "/dev/stdin": true, "/dev/stdout": true, "/dev/stderr": true,
	"/dev/tty": true,
}

var (
	// parentRef matches a ".." path component: "cd ..", "../x", "a/../b".
	parentRef = regexp.MustCompile(`(?:^|[\s;&|(=<>"'/\\])\.\.(?:$|[\s;&|)/\\"'<>])`)
	// pathToken matches words that start like an absolute, home or
	// variable-based path at a shell word boundary. A backslash (`\/etc`)
	// and an opening brace (`{/etc,x}`) count as boundaries because the
	// shell strips or expands them.
	pathToken = regexp.MustCompile(`(?:^|[\s"'=<>|;&(` + "`" + `\\{])((?:/|~|\$)[^\s"'<>|;&()` + "`" + `]*)`)
	// dirStack matches tilde forms that refer to the shell directory stack
	// or another user's home; they cannot be resolved statically.
	dirStack = regexp.MustCompile(`^~(?:[+-]\d*|[A-Za-z0-9_.@-]+)(?:/|$)`)
)

// ShellGuardConfig configures a ShellGuard.
type ShellGuardConfig struct {
	// DenyPatterns are extra regular expressions added to the built-in list.
	DenyPatterns []string
	// AllowPatterns, when non-empty, form an allowlist: a command runs only
	// if every top-level segment fully matches one of them. A command that
	// is explicitly allowed skips the deny list.
	AllowPatterns []string
	// Workspace, when set with RestrictToWorkspace, confines the working
	// directory and any absolute paths in the command.
	Workspace *Workspace
}

// ShellGuard is a best-effort filter for destructive or out-of-workspace
// shell commands. It is not a sandbox.
type ShellGuard struct {
	deny      []*regexp.Regexp
	allow     []*regexp.Regexp
	workspace *Workspace
}

// NewShellGuard compiles the configured patterns.
func NewShellGuard(cfg ShellGuardConfig) (*ShellGuard, error) {
	g := &ShellGuard{workspace: cfg.Workspace}
	for _, p := range append(append([]string{}, cfg.DenyPatterns...), defaultDenyPatterns...) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("deny pattern %q: %w", p, err)
		}
		g.deny = append(g.deny, re)
	}
	for _, p := range cfg.AllowPatterns {
		re, err := regexp.Compile(`^(?:` + p + `)$`)
		if err != nil {
			return nil, fmt.Errorf("allow pattern %q: %w", p, err)
		}
		g.allow = append(g.allow, re)
	}
	return g, nil
}

// Check returns an error wrapping ErrCommandBlocked if command must not run
// in cwd.
func (g *ShellGuard) Check(command, cwd string) error {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return errors.New("command must not be empty")
	}
	lower := strings.ToLower(cmd)

	if !g.explicitlyAllowed(lower) {
		for _, re := range g.deny {
			if re.MatchString(lower) {
				return fmt.Errorf("%w (deny pattern filter)", ErrCommandBlocked)
			}
		}
		if len(g.allow) > 0 {
			return fmt.Errorf("%w (not in allowlist)", ErrCommandBlocked)
		}
	}

	if ContainsInternalURL(cmd) {
		return fmt.Errorf("%w (internal/private URL detected)", ErrCommandBlocked)
	}

	if g.workspace == nil || !g.workspace.RestrictToWorkspace {
		return nil
	}
	return g.checkPaths(cmd, cwd)
}

func (g *ShellGuard) explicitlyAllowed(lower string) bool {
	if len(g.allow) == 0 {
		return false
	}
	segments := SplitShellSegments(lower)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		ok := false
		for _, re := range g.allow {
			if re.MatchString(seg) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

const boundaryNote = " Commands are restricted to the workspace: " +
	"use paths inside it."

func (g *ShellGuard) checkPaths(cmd, cwd string) error {
	root := g.workspace.Root
	if cwd == "" {
		cwd = root
	}
	realCwd, err := RealPath(cwd)
	if err != nil {
		return fmt.Errorf("%w (cannot resolve working dir: %v)", ErrCommandBlocked, err)
	}
	if !g.workspace.Contains(realCwd) {
		return fmt.Errorf("%w (working dir outside workspace).%s", ErrCommandBlocked, boundaryNote)
	}
	if parentRef.MatchString(cmd) {
		return fmt.Errorf("%w (path traversal detected).%s", ErrCommandBlocked, boundaryNote)
	}

	for _, m := range pathToken.FindAllStringSubmatch(cmd, -1) {
		raw := m[1]
		if benignDevicePaths[raw] || strings.HasPrefix(raw, "/dev/fd/") {
			continue
		}
		var p string
		switch {
		case raw == "~+" || strings.HasPrefix(raw, "~+/"):
			p = filepath.Join(realCwd, strings.TrimPrefix(strings.TrimPrefix(raw, "~+"), "/"))
		case dirStack.MatchString(raw):
			return fmt.Errorf("%w (path outside working dir: %s).%s", ErrCommandBlocked, raw, boundaryNote)
		case strings.HasPrefix(raw, "~"):
			p = expandHome(raw)
		case strings.HasPrefix(raw, "$"):
			p = os.Expand(raw, func(k string) string {
				if k == "PWD" {
					return realCwd // the command runs in cwd, not our process dir
				}
				return os.Getenv(k)
			})
		default:
			p = raw
		}
		if !filepath.IsAbs(p) {
			continue // e.g. "$1" or an unset variable
		}
		real, err := RealPath(p)
		if err != nil {
			return fmt.Errorf("%w (cannot resolve path %s).%s", ErrCommandBlocked, raw, boundaryNote)
		}
		if benignDevicePaths[real] {
			continue
		}
		if !IsWithin(real, realCwd) && !g.workspace.Contains(real) {
			return fmt.Errorf("%w (path outside working dir: %s).%s", ErrCommandBlocked, raw, boundaryNote)
		}
	}
	return nil
}

// SplitShellSegments splits a command on top-level chaining operators
// (&&, ||, ;, |, &, newlines), respecting quotes, escapes and parentheses.
func SplitShellSegments(command string) []string {
	var (
		segments []string
		cur      strings.Builder
		quote    byte
		escaped  bool
		depth    int
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"' || ch == '`':
			quote = ch
		case ch == '(':
			depth++
		case ch == ')' && depth > 0:
			depth--
		case depth == 0 && (strings.HasPrefix(command[i:], "&&") || strings.HasPrefix(command[i:], "||")):
			flush()
			i++
			continue
		case depth == 0 && ch == '&' && !(i > 0 && (command[i-1] == '<' || command[i-1] == '>')) &&
			!strings.HasPrefix(command[i:], "&>"):
			cur.WriteByte(ch) // keep the background marker with its command
			flush()
			continue
		case depth == 0 && (ch == ';' || ch == '|' || ch == '\n' || ch == '\r'):
			flush()
			continue
		}
		cur.WriteByte(ch)
	}
	flush()
	return segments
}
