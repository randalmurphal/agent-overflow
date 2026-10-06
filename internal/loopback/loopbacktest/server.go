// Package loopbacktest starts test servers that loopback.Dialer reaches.
package loopbacktest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-overflow/internal/loopback"
)

// NewUnstartedServer is httptest.NewUnstartedServer listening on ::1 where
// the host has an IPv6 loopback. loopback.Dialer tries ::1 before
// 127.0.0.1, and servers in this and other test processes bind
// loopback.EphemeralIPv6, so a server on 127.0.0.1 alone can have its port
// answered on ::1 by an unrelated listener.
func NewUnstartedServer(t testing.TB, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	ln, err := net.Listen("tcp6", loopback.EphemeralIPv6)
	if err != nil {
		// No IPv6 loopback: the dialer's ::1 attempt is refused for every
		// port, so 127.0.0.1 is the address it reaches.
		return srv
	}
	if err := srv.Listener.Close(); err != nil {
		_ = ln.Close()
		t.Fatalf("close the IPv4 test listener: %v", err)
	}
	srv.Listener = ln
	return srv
}

// NewServer is httptest.NewServer on the address NewUnstartedServer picks.
func NewServer(t testing.TB, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := NewUnstartedServer(t, handler)
	srv.Start()
	return srv
}

// NewTLSServer is httptest.NewTLSServer on the address NewUnstartedServer
// picks. httptest's certificate names ::1 as well as 127.0.0.1.
func NewTLSServer(t testing.TB, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := NewUnstartedServer(t, handler)
	srv.StartTLS()
	return srv
}
