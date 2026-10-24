package sandbox

import (
	"fmt"
	"path/filepath"
)

// bwrapRequired and bwrapOptional are the system paths mounted read-only,
// as in nanobot's _bwrap. Optional paths are skipped when missing.
var (
	bwrapRequired = []string{"/usr"}
	bwrapOptional = []string{
		"/bin",
		"/lib",
		"/lib64",
		"/etc/alternatives",
		"/etc/ssl/certs",
		"/etc/pki/tls/certs",
		"/etc/pki/ca-trust",
		"/etc/crypto-policies",
		"/etc/resolv.conf",
		"/etc/ld.so.cache",
	}
)

// bwrapBackend runs commands under bubblewrap (Linux). The workspace is
// bind-mounted read-write and its parent is hidden behind a fresh tmpfs.
type bwrapBackend struct{}

func (bwrapBackend) Name() string    { return Bwrap }
func (bwrapBackend) GOOS() string    { return "linux" }
func (bwrapBackend) Program() string { return "bwrap" }

func (bwrapBackend) Argv(program string, p Plan) ([]string, error) {
	parent := filepath.Dir(p.Workspace)
	if filepath.Dir(parent) == parent {
		// A tmpfs over / would hide every system mount as well.
		return nil, fmt.Errorf("sandbox %q: the workspace %s must not sit directly under /", Bwrap, p.Workspace)
	}
	args := []string{program, "--new-session", "--die-with-parent",
		"--setenv", "HOME", p.Workspace,
		// The host TMPDIR is usually not mounted; /tmp is a private tmpfs.
		"--setenv", "TMPDIR", "/tmp",
	}
	for _, d := range bwrapRequired {
		args = append(args, "--ro-bind", d, d)
	}
	for _, d := range bwrapOptional {
		args = append(args, "--ro-bind-try", d, d)
	}
	args = append(args,
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--tmpfs", parent, // mask the config directory
		"--dir", p.Workspace, // recreate the workspace mount point
		"--bind", p.Workspace, p.Workspace,
	)
	for _, d := range p.ROBinds {
		args = append(args, "--ro-bind-try", d, d)
	}
	for _, d := range p.RWBinds {
		args = append(args, "--bind-try", d, d)
	}
	return append(args, "--chdir", p.Cwd, "--", p.Shell, "-c", p.Command), nil
}

func (bwrapBackend) ProbeArgv(program string) []string {
	return []string{program, "--new-session", "--die-with-parent",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--", "true"}
}
