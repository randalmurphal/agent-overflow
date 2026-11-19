//go:build unix

package agenthome

import "syscall"

// openNonblock keeps an open of a FIFO from waiting on a writer.
const openNonblock = syscall.O_NONBLOCK
