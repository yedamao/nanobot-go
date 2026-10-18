package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

const (
	defaultExecTimeout   = 60 * time.Second
	defaultExecMaxTime   = 10 * time.Minute
	defaultExecMaxOutput = 16000
	execWaitDelay        = 2 * time.Second
)

// ExecConfig configures the exec tool.
type ExecConfig struct {
	Disabled bool
	// Timeout is the default per-command timeout (default 60s).
	Timeout time.Duration
	// MaxTimeout caps the timeout the model may request (default 10m).
	MaxTimeout time.Duration
	// MaxOutput caps captured bytes per stream; the head and tail are kept
	// (default 16000).
	MaxOutput int
	// DenyPatterns and AllowPatterns are passed to security.ShellGuard.
	DenyPatterns  []string
	AllowPatterns []string
	// Shell is the interpreter run as `Shell -c command` (default "sh").
	Shell string
	// AllowEnv lists extra environment variables passed through to
	// commands. Everything outside baseEnvKeys and this list, notably
	// provider API keys, is withheld.
	AllowEnv []string
}

// baseEnvKeys are passed to every command when set. Unlike nanobot, PATH
// is included: commands run under `sh -c`, not a login shell that would
// rebuild it.
var baseEnvKeys = []string{
	"HOME", "PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TMPDIR", "USER", "LOGNAME", "SHELL",
	// Windows essentials.
	"SYSTEMROOT", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
}

// commandEnv builds the minimal environment commands run with.
func commandEnv(allow []string) []string {
	var env []string
	seen := map[string]bool{}
	for _, k := range slices.Concat(baseEnvKeys, allow) {
		if seen[k] {
			continue
		}
		seen[k] = true
		if v, ok := os.LookupEnv(k); ok && (v != "" || k != "TERM") {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("TERM") == "" {
		env = append(env, "TERM=dumb") // no colour codes or pagers in captured output
	}
	return env
}

// Exec implements the exec tool.
type Exec struct {
	ws    *security.Workspace
	guard *security.ShellGuard
	cfg   ExecConfig
}

// NewExec returns the exec tool.
func NewExec(ws *security.Workspace, cfg ExecConfig) (*Exec, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultExecTimeout
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = defaultExecMaxTime
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = defaultExecMaxOutput
	}
	if cfg.Shell == "" {
		cfg.Shell = "sh"
	}
	guard, err := security.NewShellGuard(security.ShellGuardConfig{
		DenyPatterns:  cfg.DenyPatterns,
		AllowPatterns: cfg.AllowPatterns,
		Workspace:     ws,
	})
	if err != nil {
		return nil, fmt.Errorf("exec tool: %w", err)
	}
	return &Exec{ws: ws, guard: guard, cfg: cfg}, nil
}

func (*Exec) Name() string { return "exec" }
func (*Exec) Description() string {
	return "Execute a shell command and return its stdout, stderr and exit code."
}
func (*Exec) ReadOnly() bool { return false }
func (e *Exec) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"command":     tools.String("Shell command to run."),
		"working_dir": tools.String("Working directory (default: workspace root)."),
		"timeout": tools.Integer(fmt.Sprintf("Timeout in seconds (default %d, max %d).",
			int(e.cfg.Timeout.Seconds()), int(e.cfg.MaxTimeout.Seconds()))),
	}, "command")
}

func (e *Exec) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Command    string `json:"command"`
		WorkingDir string `json:"working_dir"`
		Timeout    int    `json:"timeout"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}

	cwd := e.ws.Root
	if args.WorkingDir != "" {
		if cwd, err = e.ws.ResolveRead(args.WorkingDir); err != nil {
			return tools.Errorf("%v", err)
		}
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return tools.Errorf("working_dir %s is not a directory", args.WorkingDir)
		}
	}
	if err := e.guard.Check(args.Command, cwd); err != nil {
		return tools.Errorf("%v", err)
	}

	timeout := e.cfg.Timeout
	if args.Timeout > 0 {
		timeout = min(time.Duration(args.Timeout)*time.Second, e.cfg.MaxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.cfg.Shell, "-c", args.Command)
	cmd.Dir = cwd
	cmd.Env = commandEnv(e.cfg.AllowEnv)
	cmd.WaitDelay = execWaitDelay
	setProcessGroup(cmd)
	stdout := newHeadTail(e.cfg.MaxOutput)
	stderr := newHeadTail(e.cfg.MaxOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr

	runErr := cmd.Run()
	out := formatExecOutput(stdout.String(), stderr.String())

	if ctx.Err() != nil {
		reason := fmt.Sprintf("timed out after %s", timeout)
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = "was cancelled"
		}
		return tools.Errorf("command %s\n%s", reason, out)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return tools.Text(out + "\nExit code: 0")
	case errors.As(runErr, &exitErr):
		// A non-zero exit is information for the model, not a tool failure.
		return tools.Text(fmt.Sprintf("%s\nExit code: %d", out, exitErr.ExitCode()))
	default:
		return tools.Errorf("run command: %v\n%s", runErr, out)
	}
}

func formatExecOutput(stdout, stderr string) string {
	var b strings.Builder
	if stdout != "" {
		b.WriteString(strings.TrimRight(stdout, "\n"))
		b.WriteString("\n")
	}
	if stderr != "" {
		b.WriteString("STDERR:\n")
		b.WriteString(strings.TrimRight(stderr, "\n"))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "(no output)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// headTail is an io.Writer that keeps the first and last max/2 bytes of
// everything written, so huge outputs cannot exhaust memory while errors at
// the end stay visible.
type headTail struct {
	half    int
	head    []byte
	tail    []byte
	dropped int
}

func newHeadTail(max int) *headTail { return &headTail{half: max / 2} }

func (h *headTail) Write(p []byte) (int, error) {
	n := len(p)
	if room := h.half - len(h.head); room > 0 {
		k := min(room, len(p))
		h.head = append(h.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	h.tail = append(h.tail, p...)
	if over := len(h.tail) - h.half; over > 0 {
		h.dropped += over
		h.tail = append(h.tail[:0], h.tail[over:]...)
	}
	return n, nil
}

func (h *headTail) String() string {
	if h.dropped == 0 {
		return string(h.head) + string(h.tail)
	}
	return fmt.Sprintf("%s\n... (%d bytes omitted) ...\n%s", h.head, h.dropped, h.tail)
}
