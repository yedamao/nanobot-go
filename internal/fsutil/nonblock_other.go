//go:build !unix

package fsutil

// oNonblock is zero where opening special files cannot block.
const oNonblock = 0
