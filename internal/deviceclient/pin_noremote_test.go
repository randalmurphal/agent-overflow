//go:build noremote

package deviceclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-overflow/internal/buildvariant"
)

// Every connection to another computer goes through a pinned transport. A
// build without remote access refuses the dial, so not even a reachable
// server, nor an application-supplied dialer, receives a connection.
func TestNoremotePinnedTransportNeverConnects(t *testing.T) {
	reached := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	defer server.Close()
	custom := func(ctx context.Context, network, address string) (net.Conn, error) {
		reached++
		return nil, errors.New("custom dialer used")
	}
	for name, transport := range map[string]*http.Transport{
		"default dialer": NewPinnedTransport("sha256:00"),
		"custom dialer":  NewPinnedTransport("sha256:00", WithDialContext(custom)),
	} {
		_, err := (&http.Client{Transport: transport}).Get(server.URL)
		if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
			t.Errorf("%s: GET = %v, want the remote-access refusal", name, err)
		}
	}
	if reached != 0 {
		t.Fatalf("a refused transport reached a server or dialer %d times", reached)
	}
}
