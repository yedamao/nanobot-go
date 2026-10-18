//go:build !unix

package builtin

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; the
// default cancellation kills only the direct child.
func setProcessGroup(*exec.Cmd) {}
