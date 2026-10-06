package transport

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Linux admits a second listening socket on a port, even a wildcard one
// beside 127.0.0.1, only when every socket on it set SO_REUSEPORT and they
// share an owner uid. The wildcard socket and the loopback socket it binds
// beside both set it, so neither has to close. A socket without the option,
// such as another instance's boot bind, still gets EADDRINUSE. Once the
// wildcard has bound, a process of the same uid that sets the option can
// bind the port too, until the loopback socket closes (a restart or a port
// change): the kernel keeps the port's reuse state while any socket holds
// it, so retiring the wildcard or clearing the option does not undo it.
// That uid can already read the launch credential from the local control
// file. When the wildcard fails to bind, unsharePort clears the option and
// the loopback socket stays exclusive.

// sharePort lets a socket bind beside ln, this server's own socket.
func sharePort(ln net.Listener) error { return setListenerReusePort(ln, 1) }

// unsharePort undoes sharePort on ln.
func unsharePort(ln net.Listener) error { return setListenerReusePort(ln, 0) }

func setListenerReusePort(ln net.Listener, value int) error {
	conn, ok := ln.(syscall.Conn)
	if !ok {
		return fmt.Errorf("listener %T exposes no socket", ln)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	return setReusePort(raw, value)
}

// sharePortControl is the net.ListenConfig.Control for the socket that
// binds beside one passed to sharePort.
func sharePortControl(_, _ string, raw syscall.RawConn) error {
	return setReusePort(raw, 1)
}

func setReusePort(raw syscall.RawConn, value int) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, value)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("set SO_REUSEPORT=%d: %w", value, sockErr)
	}
	return nil
}
