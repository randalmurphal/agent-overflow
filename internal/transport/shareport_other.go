//go:build !linux

package transport

import (
	"net"
	"syscall"
)

// macOS and the BSDs admit a wildcard socket beside a more specific one on
// the same port when the new socket sets SO_REUSEADDR, which Go sets on
// every listening socket outside Windows, so nothing more is needed. The
// Windows app runs this server in WSL, under the Linux rule.

func sharePort(net.Listener) error { return nil }

func sharePortControl(string, string, syscall.RawConn) error { return nil }
