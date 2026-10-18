//go:build unix

package fsutil

import "syscall"

// oNonblock keeps opening a FIFO from blocking until a writer appears.
const oNonblock = syscall.O_NONBLOCK
