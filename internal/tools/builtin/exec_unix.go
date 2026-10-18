//go:build unix

package builtin

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the command in its own process group and kills the
// whole group on cancellation, so background children do not linger.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
