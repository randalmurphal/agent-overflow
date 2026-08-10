//go:build !windows

package transport

import "syscall"

// The bind errors that mean "this ADDRESS is already held" on POSIX
// hosts. See addrInUse in server.go.
var addrInUseErrnos = []syscall.Errno{
	syscall.EADDRINUSE,
}

// The bind errors that mean "this PORT is unavailable" on POSIX hosts.
// See portUnavailable in server.go.
var portUnavailableErrnos = []syscall.Errno{
	syscall.EADDRINUSE,
	syscall.EACCES,
}
