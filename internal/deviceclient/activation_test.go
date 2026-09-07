package deviceclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAwaitActivationWaitsWithoutTicketsOrSuccessfulRotations(t *testing.T) {
	for _, recoverable := range []bool{false, true} {
		name := "legacy"
		if recoverable {
			name = "recoverable"
		}
		t.Run(name, func(t *testing.T) {
			var client *Client
			var rotations *atomic.Int32
			if recoverable {
				client, rotations = recoveryClient(t)
			} else {
				be := newBackend(t)
				client, _ = openAgainst(t, be, nil)
				rotations = &be.rotations
			}
			old := client.Session()
			underlying := client.http.Transport
			var approved atomic.Bool
			pending := make(chan struct{}, 1)
			var tickets atomic.Int32
			client.http.Transport = renewalTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == authTicketPath {
					tickets.Add(1)
				}
				if (r.URL.Path == authTokenPath || r.URL.Path == authTokenRecoverPath) && !approved.Load() {
					select {
					case pending <- struct{}{}:
					default:
					}
					return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"reason":"pending_confirmation"}`))}, nil
				}
				return underlying.RoundTrip(r)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- client.AwaitActivation(ctx) }()
			select {
			case <-pending:
			case <-ctx.Done():
				t.Fatal("confirmation renewal was never attempted")
			}
			if rotations.Load() != 0 || client.Session().RefreshSecret != old.RefreshSecret {
				t.Fatal("pending poll rotated credentials")
			}
			approved.Store(true)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if rotations.Load() != 1 || tickets.Load() != 0 {
				t.Fatalf("activation used %d rotations and %d tickets", rotations.Load(), tickets.Load())
			}
		})
	}
}

func TestAwaitActivationEndsOnRevocationAndForgetsOnlySession(t *testing.T) {
	be := newBackend(t)
	be.refusal = "revoked_session"
	client, dir := openAgainst(t, be, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.AwaitActivation(ctx); !errors.Is(err, ErrSessionEnded) {
		t.Fatal("revocation was not returned immediately", err)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatal("revoked session remained saved", err)
	}
	if _, err := DeviceKey(dir); err != nil {
		t.Fatal("revocation discarded the installation key", err)
	}
	if be.tickets.Load() != 0 {
		t.Fatal("activation minted a disposable socket ticket")
	}
}

func TestAwaitActivationRetriesOutageUntilCancellationWithoutLosingPairing(t *testing.T) {
	be := newBackend(t)
	be.failureStatus.Store(http.StatusServiceUnavailable)
	client, dir := openAgainst(t, be, nil)
	before := client.Session()
	underlying := client.http.Transport
	attempts := make(chan struct{}, 4)
	client.http.Transport = renewalTestTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		return underlying.RoundTrip(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.AwaitActivation(ctx) }()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("outage was not retried")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not returned", err)
	}
	after, err := LoadSession(dir, "backend-a")
	if err != nil || after.RefreshSecret != before.RefreshSecret || after.SessionID != before.SessionID {
		t.Fatal("transient outage removed pairing", err)
	}
}
