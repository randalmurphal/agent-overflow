//go:build noremote

package transferclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/loopback/loopbacktest"
)

// A build without remote access sends no transfer request: not to a
// reachable loopback destination, and not through a supplied dialer.
func TestNoremoteTransferClientNeverConnects(t *testing.T) {
	var reached atomic.Int32
	destination := loopbacktest.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer destination.Close()
	dial := func(context.Context, string, string) (net.Conn, error) {
		reached.Add(1)
		return nil, errors.New("supplied dialer used")
	}
	for _, endpoint := range []string{destination.URL, "https://computer.example"} {
		offer := testOffer()
		offer.Endpoint = endpoint
		client, err := New(offer, dial)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Status(context.Background())
		client.Close()
		if !errors.Is(err, buildvariant.ErrRemoteAccessUnavailable) {
			t.Errorf("%s: Status = %v, want the remote-access refusal", endpoint, err)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("a refused client reached a server or dialer %d times", reached.Load())
	}
}
