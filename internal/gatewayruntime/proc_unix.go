//go:build unix

package gatewayruntime

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
)

// detach starts cmd in a session of its own, away from the terminal (no
// SIGHUP or Ctrl-C from it) and leading its own process group.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive probes pid with signal 0.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processIdentity is "<pgid>:<start time>" (the start time where the
// platform exposes it), or empty when pid is gone.
func processIdentity(pid int) string {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return ""
	}
	id := strconv.Itoa(pgid)
	if start := processStartTime(pid); start != "" {
		id += ":" + start
	}
	return id
}

// terminateProcess sends SIGTERM (SIGKILL with force) to pid's process
// group when pid leads it, so its children (MCP servers) go too, else to
// pid alone. A gone process is not an error.
func terminateProcess(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		target = -pid
	}
	if err := syscall.Kill(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
