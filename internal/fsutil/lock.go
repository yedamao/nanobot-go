package fsutil

import "errors"

// ErrLocked is returned by a non-waiting LockExclusive on a held lock.
var ErrLocked = errors.New("locked by another process")
