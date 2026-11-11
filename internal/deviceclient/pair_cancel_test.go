//go:build !noremote

package deviceclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-overflow/internal/servercert"
)

// The redemption spends the link on the far side. A caller that stops
// waiting once it was sent still gets the session saved, so the spent link
// is not lost with nothing on disk.
func TestPairSavesTheSessionWhenTheCallerCancelsDuringRedemption(t *testing.T) {
	t.Parallel()
	be := &backend{spent: map[string]bool{}, credential: "ao1.issued-0"}
	arrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authPairPath {
			close(arrived)
			<-release
		}
		be.route(w, r)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	link := Link{BackendID: "backend-a", Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw), Token: "test-token"}
	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		client *Client
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		client, _, err := Pair(ctx, dir, link, "test", "test")
		done <- answer{client, err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the redemption never arrived")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	var got answer
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Pair did not return")
	}
	if got.err != nil {
		t.Fatalf("Pair after the caller cancelled = %v, want the redeemed session", got.err)
	}
	t.Cleanup(got.client.http.CloseIdleConnections)
	saved, err := LoadSession(dir, "backend-a")
	if err != nil || saved.SessionID != "session-1" {
		t.Fatalf("saved session = %+v, %v; want the redeemed one", saved, err)
	}
}
