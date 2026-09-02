//go:build !linux && !darwin

package devscan

// Every platform with no enumerator. In practice that is a NATIVE
// Windows or BSD build and nothing that ships: the Windows deployment is
// the WSL launcher plus a Linux payload, so a Windows install's backend
// runs enumerate_linux.go like every other Linux one.
//
// It answers ErrUnsupported rather than an empty list. "Nothing is
// listening" and "this build cannot look" are different sentences on
// screen, and a caller handed an empty slice would print the first one
// forever. The caller (internal/app's preview reconciler) surfaces the
// error and stops polling — no amount of re-asking makes a platform
// supported.

// listener is one LISTEN socket. Declared here so the shared code
// compiles on a platform that can never produce one.
type listener struct {
	Port int
	PID  int
	PPID int
	PGID int
	Comm string
}

func enumerateListeners(_ string) ([]listener, map[int]int, error) {
	return nil, nil, ErrUnsupported
}
