//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// ownProcessGroup is a no-op where process groups are unavailable.
func ownProcessGroup(*exec.Cmd) {}

// newSession is a no-op where sessions are unavailable.
func newSession(*exec.Cmd) {}

// signalGroup signals only p where process groups are unavailable; an
// interrupt cannot be delivered there, so it kills p.
func signalGroup(p *os.Process, sig os.Signal) error {
	if sig == os.Interrupt {
		return p.Kill()
	}
	return p.Signal(sig)
}
