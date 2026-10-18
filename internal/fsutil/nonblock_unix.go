//go:build unix

package fsutil

import "syscall"

// oNonblock keeps opening a FIFO from blocking until a writer appears.
const oNonblock = syscall.O_NONBLOCK

// oNoFollow makes opening a symlink fail.
const oNoFollow = syscall.O_NOFOLLOW
