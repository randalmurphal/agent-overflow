//go:build !windows

package agy

import "syscall"

func openNonBlockFlag() int {
	return syscall.O_NONBLOCK
}
