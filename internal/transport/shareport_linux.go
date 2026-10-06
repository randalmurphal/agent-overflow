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
// such as another instance's boot bind, still gets EADDRINUSE. A process of
// the same uid that sets the option can bind the port too; that uid can
// already read the launch credential from the local control file.

// sharePort lets a socket bind beside ln, this server's own socket.
func sharePort(ln net.Listener) error {
	conn, ok := ln.(syscall.Conn)
	if !ok {
		return fmt.Errorf("listener %T exposes no socket", ln)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	return setReusePort(raw)
}

// sharePortControl is the net.ListenConfig.Control for the socket that
// binds beside one passed to sharePort.
func sharePortControl(_, _ string, raw syscall.RawConn) error {
	return setReusePort(raw)
}

func setReusePort(raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("set SO_REUSEPORT: %w", sockErr)
	}
	return nil
}
