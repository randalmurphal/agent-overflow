//go:build !noremote

package deviceclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/filelock"
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

// heldDiscoveryBackend holds the renewal-support probe of a session that
// does not yet know whether its backend supports recovery, and counts the
// exchanges that reach it.
type heldDiscoveryBackend struct {
	*httptest.Server
	arrived   chan struct{}
	abandoned chan struct{}
	exchanges atomic.Int32
}

func newHeldDiscoveryBackend(t *testing.T) *heldDiscoveryBackend {
	t.Helper()
	b := &heldDiscoveryBackend{arrived: make(chan struct{}), abandoned: make(chan struct{})}
	release := make(chan struct{})
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			b.exchanges.Add(1)
			http.NotFound(w, r)
			return
		}
		close(b.arrived)
		select {
		case <-release:
		case <-r.Context().Done():
			close(b.abandoned)
		}
	}))
	t.Cleanup(b.Close)
	t.Cleanup(func() { close(release) })
	return b
}

// openUndiscovered opens a client whose saved session has never learned
// whether b supports renewal recovery.
func openUndiscovered(t *testing.T, b *heldDiscoveryBackend, lifetime context.Context) (*Client, string) {
	t.Helper()
	seeded, dir := openAgainst(t, &backend{Server: b.Server}, func(s *Session) { s.RefreshRecovery = nil })
	client, err := Open(dir, seeded.Session(), WithLifetime(lifetime))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settled(t, client) })
	return client, dir
}

// undiscoveredUnspent fails unless the renewal spent nothing: no exchange
// was sent and the saved session is as it was.
func undiscoveredUnspent(t *testing.T, b *heldDiscoveryBackend, dir string) {
	t.Helper()
	if n := b.exchanges.Load(); n != 0 {
		t.Fatalf("%d exchanges reached the backend; want none", n)
	}
	saved, err := LoadSession(dir, "backend-a")
	if err != nil || saved.RefreshSecret != "refresh-0" || saved.PendingNextSecret != "" || saved.RefreshRecovery != nil {
		t.Fatalf("saved session = %+v, %v; want it untouched", saved, err)
	}
}

// Before a renewal learns which exchange its backend speaks it has spent
// nothing, so the owner's lifetime ends a stalled support check at once.
func TestOwnerLifetimeEndsARenewalStillCheckingSupport(t *testing.T) {
	t.Parallel()
	b := newHeldDiscoveryBackend(t)
	life, end := context.WithCancel(context.Background())
	defer end()
	client, dir := openUndiscovered(t, b, life)
	renewed := renewAsync(client)
	awaitWithin(t, "the support check reaching the backend", b.arrived)

	end()
	awaitWithin(t, "the backend seeing the support check given up", b.abandoned)
	if err := awaitWithin(t, "the renewal returning", renewed); err == nil {
		t.Fatal("a renewal cut off by its owner's lifetime reported success")
	}
	undiscoveredUnspent(t, b, dir)
}

// Retiring the owner ends a stalled support check too.
func TestRetirementEndsARenewalStillCheckingSupport(t *testing.T) {
	t.Parallel()
	b := newHeldDiscoveryBackend(t)
	client, dir := openUndiscovered(t, b, context.Background())
	renewed := renewAsync(client)
	awaitWithin(t, "the support check reaching the backend", b.arrived)

	client.Retire()
	awaitWithin(t, "the backend seeing the support check given up", b.abandoned)
	if err := awaitWithin(t, "the renewal returning", renewed); err == nil {
		t.Fatal("a retired owner's renewal reported success")
	}
	undiscoveredUnspent(t, b, dir)
}

// A legacy renewal waiting for another process's legacy exchange has spent
// nothing, so the owner's lifetime ends that wait.
func TestOwnerLifetimeEndsALegacyRenewalWaitingForItsLock(t *testing.T) {
	t.Parallel()
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	life, end := context.WithCancel(context.Background())
	defer end()
	seeded, dir := openAgainst(t, &backend{Server: server}, nil)
	client, err := Open(dir, seeded.Session(), WithLifetime(life))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settled(t, client) })
	release, err := lockProfile(t.Context(), dir, "legacy-renewal-backend-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	})

	renewed := renewAsync(client)
	end()
	if err := awaitWithin(t, "the renewal returning", renewed); !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy renewal waiting for its lock when its owner ended = %v; want context.Canceled", err)
	}
	if n := exchanges.Load(); n != 0 {
		t.Fatalf("%d exchanges were sent; want none", n)
	}
}

// A legacy renewal has spent nothing until its exchange is sent, so an owner
// lifetime that ends after the renewal took the legacy lock and before it
// sent the exchange stops it there.
func TestOwnerLifetimeEndsALegacyRenewalNotYetSent(t *testing.T) {
	// Not parallel: it watches the renewal's lock releases through
	// releaseProfileLock.
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	life, end := context.WithCancel(context.Background())
	defer end()
	seeded, dir := openAgainst(t, &backend{Server: server}, nil)
	client, err := Open(dir, seeded.Session(), WithLifetime(life))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { settled(t, client) })
	sessionFile, err := sessionPath(dir, "backend-a")
	if err != nil {
		t.Fatal(err)
	}
	sessionLock := filepath.Join(dir, "locks", filepath.Base(sessionFile)+".lock")
	legacyLock := filepath.Join(dir, "locks", "legacy-renewal-backend-a.lock")
	wait, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Another process's legacy exchange holds the legacy lock, so the
	// renewal stops after reading its session.
	legacy, err := filelock.Acquire(wait, legacyLock)
	if err != nil {
		t.Fatal(err)
	}
	firstRelease := make(chan struct{}, 1)
	saved := releaseProfileLock
	releaseProfileLock = func(f *os.File) error {
		err := releaseProfile(f)
		select {
		case firstRelease <- struct{}{}:
		default:
		}
		return err
	}
	t.Cleanup(func() { releaseProfileLock = saved })

	renewed := renewAsync(client)
	// The renewal has read the session and waits for the legacy lock.
	// Holding the session lock before handing the legacy lock over stops it
	// at the transaction it makes after taking the legacy lock.
	awaitWithin(t, "the renewal reading its session", firstRelease)
	held, err := filelock.Acquire(wait, sessionLock)
	if err != nil {
		t.Fatal(err)
	}
	if err := filelock.Release(legacy); err != nil {
		t.Fatal(err)
	}
	for {
		probe, err := filelock.Open(legacyLock)
		if err != nil {
			t.Fatal(err)
		}
		free, err := filelock.TryLock(probe)
		if err != nil {
			t.Fatal(err)
		}
		if !free {
			_ = probe.Close()
			break
		}
		if err := filelock.Release(probe); err != nil {
			t.Fatal(err)
		}
		if wait.Err() != nil {
			t.Fatal("the renewal never took the legacy lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	end()
	if err := filelock.Release(held); err != nil {
		t.Fatal(err)
	}
	if err := awaitWithin(t, "the renewal returning", renewed); !errors.Is(err, context.Canceled) {
		t.Fatalf("legacy renewal whose owner ended before it was sent = %v; want context.Canceled", err)
	}
	if n := exchanges.Load(); n != 0 {
		t.Fatalf("%d exchanges were sent after the owner's lifetime ended; want none", n)
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
