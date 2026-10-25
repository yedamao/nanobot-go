//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own, so the
// terminal's Ctrl-C reaches only the launcher, which then stops the child
// itself (the gateway treats a second interrupt as "force exit").
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the process group led by p (npm's Vite child
// included).
func signalGroup(p *os.Process, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return p.Signal(sig)
	}
	return syscall.Kill(-p.Pid, s)
}
