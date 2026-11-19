//go:build !unix

package agenthome

// openNonblock is unix's O_NONBLOCK; elsewhere a home holds no FIFO to wait
// on.
const openNonblock = 0
