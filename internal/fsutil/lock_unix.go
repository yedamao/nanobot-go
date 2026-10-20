//go:build unix

package fsutil

import (
	"errors"
	"os"
	"syscall"
)

// LockExclusive takes an exclusive advisory lock (flock) on f, released
// when f is closed. Without wait it fails with ErrLocked if another open
// file description holds the lock.
func LockExclusive(f *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrLocked
		}
		return err
	}
}
