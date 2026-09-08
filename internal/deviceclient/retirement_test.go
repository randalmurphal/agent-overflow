package deviceclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type heldRotationTransport struct {
	base    http.RoundTripper
	arrived chan struct{}
	resume  chan struct{}
}

func (h heldRotationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := h.base.RoundTrip(request)
	if request.URL.Path == authTokenPath {
		close(h.arrived)
		select {
		case <-h.resume:
		case <-request.Context().Done():
		}
	}
	return response, err
}

// gatedRotationTransport holds the rotation BEFORE it reaches the wire, so
// a cancellation lands mid-exchange rather than after the reply.
type gatedRotationTransport struct {
	base    http.RoundTripper
	arrived chan struct{}
	resume  chan struct{}
}

func (g gatedRotationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == authTokenPath {
		select {
		case <-g.resume: // The gate is spent; later rotations are ordinary.
		default:
			close(g.arrived)
			select {
			case <-g.resume:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
	}
	return g.base.RoundTrip(request)
}

// The leader's cancellation is its own: the exchange it started completes
// for everyone who joined it, and the leader itself returns at once.
func TestRenewalCompletesForWaitersWhenTheLeaderIsCancelled(t *testing.T) {
	backend := newBackend(t)
	client, dir := openAgainst(t, backend, nil)
	gate := gatedRotationTransport{base: client.http.Transport, arrived: make(chan struct{}), resume: make(chan struct{})}
	client.http.Transport = gate
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() { leader <- client.renew(leaderCtx) }()
	<-gate.arrived
	waiter := make(chan error, 1)
	go func() { waiter <- client.renew(t.Context()) }()
	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled leader answered %v, want its own cancellation", err)
	}
	close(gate.resume)
	if err := <-waiter; err != nil {
		t.Fatalf("waiter lost the exchange: %v", err)
	}
	stored, err := LoadSession(dir, "backend-a")
	if err != nil || stored.Credential != "ao1.issued-1" || stored.RefreshSecret == "refresh-0" {
		t.Fatalf("rotation not saved: %+v %v", stored, err)
	}
	if err := client.renew(t.Context()); err != nil || backend.rotations.Load() != 2 {
		t.Fatalf("next rotation: %v after %d rotations", err, backend.rotations.Load())
	}
}

// A lock another process holds cannot wedge this client's mutex behind a
// caller with no deadline.
func TestSessionTransactionBoundsItsLockWait(t *testing.T) {
	saved := profileWriteTimeout
	profileWriteTimeout = 40 * time.Millisecond
	t.Cleanup(func() { profileWriteTimeout = saved })
	client, dir := openAgainst(t, newBackend(t), nil)
	release, err := lockProfile(t.Context(), dir, "backend-a.json")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = client.sessionTransaction(context.Background(), func(string, *Session) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended transaction: %v", err)
	}
}

func TestRenewalCannotResurrectRemovedOrReplacedProfileAndKeepsConcurrentName(t *testing.T) {
	for _, action := range []string{"forget", "replace", "rename"} {
		t.Run(action, func(t *testing.T) {
			backend := newBackend(t)
			client, dir := openAgainst(t, backend, nil)
			held := heldRotationTransport{base: client.http.Transport, arrived: make(chan struct{}), resume: make(chan struct{})}
			client.http.Transport = held
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- client.renew(ctx) }()
			<-held.arrived
			switch action {
			case "forget":
				if err := client.Forget(); err != nil {
					t.Fatal(err)
				}
			case "replace":
				replacement := client.Session()
				replacement.Credential = "replacement-credential"
				client.Retire()
				if err := SaveSession(dir, replacement); err != nil {
					t.Fatal(err)
				}
			case "rename":
				if err := client.SetNickname("GPU workstation"); err != nil {
					t.Fatal(err)
				}
			}
			close(held.resume)
			err := <-done
			if action == "rename" && err != nil {
				t.Fatal(err)
			}
			if action != "rename" && !errors.Is(err, ErrSessionEnded) {
				t.Fatalf("late renewal: %v", err)
			}
			stored, loadErr := LoadSession(dir, "backend-a")
			switch action {
			case "forget":
				if loadErr == nil {
					t.Fatal("removed profile came back")
				}
			case "replace":
				if loadErr != nil || stored.Credential != "replacement-credential" {
					t.Fatalf("replacement lost: %+v %v", stored, loadErr)
				}
				if err := client.Forget(); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadSession(dir, "backend-a"); err != nil {
					t.Fatal("retired client deleted its replacement")
				}
			case "rename":
				if loadErr != nil || stored.Nickname != "GPU workstation" {
					t.Fatalf("rename lost: %+v %v", stored, loadErr)
				}
			}
		})
	}
}
