//go:build windows

package transport

import "syscall"

// The bind errors that mean "this PORT is unavailable" on Windows. The
// Winsock stack reports WSAEADDRINUSE (10048) / WSAEACCES (10013), which
// are distinct syscall.Errno values from the POSIX-named constants Go
// also defines for this GOOS — errors.Is against syscall.EADDRINUSE
// would never match a real bind failure here, which would turn the
// pinned-port fallback into a boot failure. Both spellings are listed
// so the set holds whichever one a future net/os layer surfaces. See
// portUnavailable in server.go.
const (
	wsaeacces     = syscall.Errno(10013)
	wsaeaddrinuse = syscall.Errno(10048)
)

// The bind errors that mean "this ADDRESS is already held" on Windows.
// Deliberately narrower than portUnavailableErrnos: WSAEACCES is a
// permission/reservation refusal (a Hyper-V excluded port range, a
// socket held with SO_EXCLUSIVEADDRUSE by another process), and no
// amount of releasing OUR OWN listener makes it bindable. See addrInUse
// in server.go.
var addrInUseErrnos = []syscall.Errno{
	wsaeaddrinuse,
	syscall.EADDRINUSE,
}

var portUnavailableErrnos = []syscall.Errno{
	wsaeaddrinuse,
	wsaeacces,
	syscall.EADDRINUSE,
	syscall.EACCES,
}
