//go:build !unix

package fsutil

// oNonblock is zero where opening special files cannot block.
const oNonblock = 0

// oNoFollow is zero where there is no O_NOFOLLOW; the regular-file check
// still applies.
const oNoFollow = 0
