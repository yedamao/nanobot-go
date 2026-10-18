//go:build unix

package builtin

import "syscall"

// openNonblock keeps opening a FIFO from blocking until a writer appears.
const openNonblock = syscall.O_NONBLOCK
