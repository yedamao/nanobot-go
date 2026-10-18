//go:build !unix

package memory

import "os"

// lockFileExclusive is a no-op where flock is unavailable: only the
// in-process mutex serializes history writers there.
func lockFileExclusive(*os.File, bool) error { return nil }
