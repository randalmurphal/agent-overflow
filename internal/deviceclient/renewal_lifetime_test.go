//go:build !noremote

package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// heldRenewalBackend answers a renewal only once release closes, and says
// when the client gave the request up.
type heldRenewalBackend struct {
	*httptest.Server
	arrived   chan struct{}
	release   chan struct{}
	abandoned chan struct{}
}

func newHeldRenewalBackend(t *testing.T, recoverable bool) *heldRenewalBackend {
	t.Helper()
	b := &heldRenewalBackend{arrived: make(chan struct{}), release: make(chan struct{}), abandoned: make(chan struct{})}
	path := authTokenPath
	if recoverable {
		path = authTokenRecoverPath
	}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recoverable {
			w.Header().Set(RefreshRecoveryHeader, "1")
		}
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Next string `json:"nextRefreshSecret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Read to the end so the server notices when the client hangs up.
		_, _ = io.Copy(io.Discard, r.Body)
		close(b.arrived)
		select {
		case <-b.release:
		case <-r.Context().Done():
			close(b.abandoned)
			return
		}
		next := body.Next
		if next == "" {
			next = "refresh-1"
		}
		_ = json.NewEncoder(w).Encode(grant{SessionID: "session-1", Credential: "ao1.issued-1", RefreshSecret: next,
			ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(), RefreshExpiresAtMs: time.Now().Add(24 * time.Hour).UnixMilli()})
	}))
	t.Cleanup(b.Close)
	t.Cleanup(func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	})
	return b
}

// openHeld opens a client on b's session under the owner lifetime given.
func openHeld(t *testing.T, b *heldRenewalBackend, recoverable bool, lifetime context.Context) (*Client, string) {
	t.Helper()
	seeded, dir := openAgainst(t, &backend{Server: b.Server}, func(s *Session) { s.RefreshRecovery = &recoverable })
	client, err := Open(dir, seeded.Session(), WithLifetime(lifetime))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settled(t, client) })
	return client, dir
}

func renewAsync(c *Client) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.renew(context.Background()) }()
	return done
}

func awaitWithin[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen within 2s", what)
		var zero T
		return zero
	}
}

// When the owner's lifetime ends, a recoverable renewal in flight stops at
// once and keeps its proposed successor for the next rotation.
func TestOwnerLifetimeEndsARecoverableRenewalInFlight(t *testing.T) {
	t.Parallel()
	b := newHeldRenewalBackend(t, true)
	life, end := context.WithCancel(context.Background())
	defer end()
	client, dir := openHeld(t, b, true, life)
	renewed := renewAsync(client)
	awaitWithin(t, "the renewal reaching the backend", b.arrived)

	end()
	awaitWithin(t, "the backend seeing the renewal given up", b.abandoned)
	if err := awaitWithin(t, "the renewal returning", renewed); err == nil {
		t.Fatal("a renewal cut off by its owner's lifetime reported success")
	}
	saved, err := LoadSession(dir, "backend-a")
	if err != nil || saved.PendingNextSecret == "" || saved.RefreshSecret != "refresh-0" {
		t.Fatalf("saved session = %+v, %v; want the predecessor and its proposed successor kept", saved, err)
	}
}

// A legacy renewal the backend may already have committed is not cut off
// by the owner's lifetime: it finishes and saves its successor.
func TestOwnerLifetimeLeavesALegacyRenewalToFinish(t *testing.T) {
	t.Parallel()
	b := newHeldRenewalBackend(t, false)
	life, end := context.WithCancel(context.Background())
	defer end()
	client, dir := openHeld(t, b, false, life)
	renewed := renewAsync(client)
	awaitWithin(t, "the renewal reaching the backend", b.arrived)

	end()
	select {
	case err := <-renewed:
		t.Fatalf("a legacy renewal returned %v when its owner's lifetime ended; want it to finish", err)
	case <-b.abandoned:
		t.Fatal("a legacy renewal was given up when its owner's lifetime ended")
	case <-time.After(200 * time.Millisecond):
	}
	close(b.release)
	if err := awaitWithin(t, "the renewal returning", renewed); err != nil {
		t.Fatalf("legacy renewal: %v", err)
	}
	if saved, err := LoadSession(dir, "backend-a"); err != nil || saved.RefreshSecret != "refresh-1" {
		t.Fatalf("saved session = %+v, %v; want the successor saved", saved, err)
	}
}

// A retired owner never writes a renewal's answer, so retiring it ends the
// renewal in flight, legacy or not.
func TestRetirementEndsARenewalInFlight(t *testing.T) {
	t.Parallel()
	b := newHeldRenewalBackend(t, false)
	client, _ := openHeld(t, b, false, context.Background())
	renewed := renewAsync(client)
	awaitWithin(t, "the renewal reaching the backend", b.arrived)

	client.Retire()
	awaitWithin(t, "the backend seeing the renewal given up", b.abandoned)
	if err := awaitWithin(t, "the renewal returning", renewed); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("renewal of a retired owner = %v, want ErrSessionEnded", err)
	}
}
