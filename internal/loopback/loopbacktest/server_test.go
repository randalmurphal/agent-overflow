package loopbacktest

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/loopback"
)

// TestServerHoldsThePortLoopbackDialerTriesFirst: no other listener can take
// the server's port on ::1, so loopback.Dialer reaches this server.
func TestServerHoldsThePortLoopbackDialerTriesFirst(t *testing.T) {
	probe, err := net.Listen("tcp6", loopback.EphemeralIPv6)
	if err != nil {
		t.Skip("this host has no IPv6 loopback")
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "this server")
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if other, err := net.Listen("tcp6", net.JoinHostPort("::1", port)); err == nil {
		_ = other.Close()
		t.Fatalf("another listener bound [::1]:%s beside the test server", port)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: loopback.Dialer(time.Second)}}
	res, err := client.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "this server" {
		t.Fatalf("loopback.Dialer reached %q", body)
	}
}
