//go:build !unix

package fsutil

import "os"

// LockExclusive is a no-op where flock is unavailable: only in-process
// mutexes serialize writers there.
func LockExclusive(*os.File, bool) error { return nil }
