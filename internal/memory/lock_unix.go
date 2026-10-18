//go:build unix

package memory

import (
	"errors"
	"os"
	"syscall"
)

// lockFileExclusive takes an exclusive advisory lock on f, released when f
// is closed. Without wait it fails with errLocked if another open file
// description holds the lock.
func lockFileExclusive(f *os.File, wait bool) error {
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
			return errLocked
		}
		return err
	}
}
