//go:build !unix

package builtin

// openNonblock is zero where opening special files cannot block.
const openNonblock = 0
