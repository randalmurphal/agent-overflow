//go:build !noremote

package deviceclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// heldRedemption is a backend that holds the redemption until release
// closes or the client gives the request up, and counts the redemptions
// that reach it.
type heldRedemption struct {
	*httptest.Server
	link      Link
	arrived   chan struct{}
	release   chan struct{}
	abandoned chan struct{}
	redeemed  atomic.Int32
}

func newHeldRedemption(t *testing.T) *heldRedemption {
	t.Helper()
	be := &backend{spent: map[string]bool{}, credential: "ao1.issued-0"}
	h := &heldRedemption{arrived: make(chan struct{}), release: make(chan struct{}), abandoned: make(chan struct{})}
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authPairPath {
			h.redeemed.Add(1)
			// Read to the end so the server notices when the client hangs up.
			_, _ = io.Copy(io.Discard, r.Body)
			close(h.arrived)
			select {
			case <-h.release:
			case <-r.Context().Done():
				close(h.abandoned)
				return
			}
		}
		be.route(w, r)
	}))
	t.Cleanup(h.Close)
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	})
	h.link = Link{BackendID: "backend-a", Endpoint: h.URL, CertFingerprint: servercert.Fingerprint(h.Certificate().Raw), Token: "test-token"}
	return h
}

// A redemption is not stored until it is saved, so its owner can still
// decide not to keep it.
func TestRedeemSavesNothingUntilSaved(t *testing.T) {
	t.Parallel()
	h := newHeldRedemption(t)
	close(h.release)
	dir := t.TempDir()
	redeemed, err := Redeem(context.Background(), dir, h.link, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session before Save = %v; want none stored", err)
	}
	client, pairing, err := saveHeld(dir, redeemed, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	saved, err := LoadSession(dir, "backend-a")
	if err != nil || saved.SessionID != "session-1" || pairing.SessionID != "session-1" || client.Session().SessionID != "session-1" {
		t.Fatalf("after Save: saved %+v, %v; pairing %+v; want the redeemed session", saved, err, pairing)
	}
}

// The HTTP client's timeout bounds the redemption a caller cannot cancel: a
// person's pairing that quit lets finish, and Pair's.
func TestARedemptionIsBoundedByTheHTTPTimeout(t *testing.T) {
	t.Parallel()
	h := newHeldRedemption(t)
	close(h.release)
	redeemed, err := Redeem(context.Background(), t.TempDir(), h.link, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(redeemed.client.http.CloseIdleConnections)
	if got := redeemed.client.http.Timeout; got != pinTimeout || pinTimeout != 10*time.Second {
		t.Fatalf("the redemption's HTTP timeout = %s (pinTimeout %s); want 10s", got, pinTimeout)
	}
}

// Redeem's context ends the redemption in flight, unlike Pair's.
func TestRedeemEndsWithItsContext(t *testing.T) {
	t.Parallel()
	h := newHeldRedemption(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Redeem(ctx, dir, h.link, "test", "test")
		done <- err
	}()
	<-h.arrived
	cancel()
	select {
	case <-h.abandoned:
	case <-time.After(2 * time.Second):
		t.Fatal("the backend never saw the redemption given up")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Redeem after its context ended = %v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Redeem did not return after its context ended")
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session after a cancelled redemption = %v; want none stored", err)
	}
}

// Nothing is spent before the redemption is sent, so a redemption whose
// owner has ended is never sent.
func TestRedeemIsNotSentOnceItsOwnerEnded(t *testing.T) {
	t.Parallel()
	h := newHeldRedemption(t)
	life, end := context.WithCancel(context.Background())
	end()
	if _, err := Redeem(context.Background(), t.TempDir(), h.link, "test", "test", WithLifetime(life)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Redeem after its owner ended = %v; want context.Canceled", err)
	}
	if n := h.redeemed.Load(); n != 0 {
		t.Fatalf("%d redemptions were sent after the owner ended; want none", n)
	}
}
