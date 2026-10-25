//go:build !unix

package gatewayruntime

import (
	"os"
	"os/exec"
)

// detach is a no-op where sessions are unavailable.
func detach(*exec.Cmd) {}

// processAlive reports whether pid can be found (on Windows, opened).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	p.Release()
	return true
}

// processIdentity is unknown here: a live PID is trusted.
func processIdentity(int) string { return "" }

// terminateProcess kills pid: there is no SIGTERM to send here.
func terminateProcess(pid int, _ bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	defer p.Release()
	if err := p.Kill(); err != nil && processAlive(pid) {
		return err
	}
	return nil
}
