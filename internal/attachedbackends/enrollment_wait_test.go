//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// stillWaiting fails if ch delivers within a short wait: a call that must
// wait for an enrollment in flight answered before it ended.
func stillWaiting[T any](t *testing.T, what string, ch <-chan T) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s answered %+v while the enrollment was in flight; want it to wait", what, got)
	case <-time.After(200 * time.Millisecond):
	}
}

// A person's second pairing with a computer being enrolled is refused, and
// an introduction waits for the enrollment in flight, then decides on what
// it left. A person's pairing that ends refused, unanswered or forgotten is
// followed by the introduction's own: connected after the first two, not
// wanted after the forget, with nothing redeemed for it.
func TestAnIntroductionWaitsForAnEnrollmentInFlight(t *testing.T) {
	t.Parallel()
	for _, ending := range []struct {
		name string
		// configure makes the person's redemption, the first, end so.
		configure func(p *introducedPeer)
		// end releases the person's redemption, or forgets the computer.
		forget bool
	}{
		{name: "refused", configure: func(p *introducedPeer) { p.refusedPair = 1 }},
		// A redemption that times out ends as one whose connection dropped:
		// with no answer, as a transport error.
		{name: "unanswered", configure: func(p *introducedPeer) { p.droppedPair = 1 }},
		{name: "forgotten", forget: true},
	} {
		t.Run(ending.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, func(p *introducedPeer) {
				p.holdPair = make(chan struct{})
				if ending.configure != nil {
					ending.configure(p)
				}
			})
			m, _ := newManager(t)
			person := p.pair(m)
			within(t, "the person's redemption reaching the other computer", 5*time.Second, p.pairArrived)

			if got := within(t, "a second person's pairing", 2*time.Second, p.pair(m)); got.added || !errors.Is(got.err, errEnrollmentInProgress) {
				t.Fatalf("second person's pairing = added %t, %v; want %v", got.added, got.err, errEnrollmentInProgress)
			}
			introduction := p.introduce(context.Background(), m)
			stillWaiting(t, "the introduction", introduction)

			if ending.forget {
				if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
					t.Fatalf("Remove: %v", err)
				}
				if got := within(t, "the person's pairing", 2*time.Second, person); !errors.Is(got.err, errPairingForgotten) {
					t.Fatalf("person's pairing = %v; want forgotten", got.err)
				}
				got := within(t, "the waiting introduction", 2*time.Second, introduction)
				if got.added || got.err != nil {
					t.Fatalf("introduction that waited on a forgotten pairing = added %t, %v; want not added with no error", got.added, got.err)
				}
				if n := p.redemptions.Load(); n != 1 {
					t.Fatalf("%d redemptions; want only the person's", n)
				}
				nothingEnrolled(t, m, p.backendID)
				return
			}
			closeOnce(p.holdPair)
			if got := within(t, "the person's pairing", 5*time.Second, person); got.added || got.err == nil {
				t.Fatalf("person's pairing = added %t, %v; want it to fail", got.added, got.err)
			}
			got := within(t, "the waiting introduction", 5*time.Second, introduction)
			if !got.added || got.err != nil {
				t.Fatalf("introduction after the person's pairing failed = added %t, %v; want it connected", got.added, got.err)
			}
			enrolled(t, m, p.backendID)
			if saved, err := deviceclient.LoadSession(m.dir, p.backendID); err != nil || !saved.OwnDevice {
				t.Fatalf("saved session = %+v, %v; want the introduction's own-device pairing", saved, err)
			}
		})
	}
}

// An introduction that waits for another enrollment stops waiting when its
// caller does and when shutdown begins, while the enrollment it waited for
// goes on.
func TestAnIntroductionsWaitEndsWithItsCallerAndAtShutdown(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"caller", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair = make(chan struct{}) })
			m, _ := newManager(t)
			person := p.pair(m)
			within(t, "the person's redemption reaching the other computer", 5*time.Second, p.pairArrived)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			introduction := p.introduce(ctx, m)
			stillWaiting(t, "the introduction", introduction)
			want := context.Canceled
			if end == "caller" {
				cancel()
			} else {
				m.BeginClose()
				want = errShuttingDown
			}
			if got := within(t, "the waiting introduction", 2*time.Second, introduction); got.added || !errors.Is(got.err, want) {
				t.Fatalf("waiting introduction = added %t, %v; want %v", got.added, got.err, want)
			}
			if state, ended := enrollmentOf(m, p.backendID); state != enrollmentRedeeming || ended {
				t.Fatalf("person's pairing = state %d, context ended %t; want it still redeeming", state, ended)
			}
			closeOnce(p.holdPair)
			if got := within(t, "the person's pairing", 2*time.Second, person); !got.added || got.err != nil {
				t.Fatalf("person's pairing = added %t, %v; want it saved", got.added, got.err)
			}
			if n := p.redemptions.Load(); n != 1 {
				t.Fatalf("%d redemptions; want only the person's", n)
			}
		})
	}
}

// A rename or an address repair during a re-pairing waits for it and
// applies to the replacement, which would otherwise drop it. A name given
// before the re-pairing began is not carried.
func TestARenameAndARepairDuringARePairingApplyToTheReplacement(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair, p.heldPair = make(chan struct{}), 2 })
	alternative := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"backendId": p.backendID})
	}))
	t.Cleanup(alternative.Close)
	m, dir := newManager(t)
	if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("first pairing = added %t, %v", got.added, got.err)
	}
	if err := m.Rename(p.backendID, "before the re-pairing"); err != nil {
		t.Fatal(err)
	}
	again := p.pair(m)
	within(t, "the re-pairing's redemption reaching the other computer", 5*time.Second, p.pairArrived)

	renamed := make(chan error, 1)
	go func() { renamed <- m.Rename(p.backendID, "GPU computer") }()
	repaired := make(chan error, 1)
	go func() {
		_, err := m.RepairAddress(context.Background(), p.backendID, alternative.URL)
		repaired <- err
	}()
	stillWaiting(t, "the rename", renamed)
	stillWaiting(t, "the address repair", repaired)
	closeOnce(p.holdPair)
	if got := within(t, "the re-pairing", 2*time.Second, again); !got.added || got.err != nil {
		t.Fatalf("re-pairing = added %t, %v", got.added, got.err)
	}
	if err := within(t, "the rename", 2*time.Second, renamed); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := within(t, "the address repair", 5*time.Second, repaired); err != nil {
		t.Fatalf("RepairAddress: %v", err)
	}
	saved, err := deviceclient.LoadSession(dir, p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Nickname != "GPU computer" {
		t.Fatalf("nickname after the re-pairing = %q; want the rename made during it", saved.Nickname)
	}
	if len(saved.Routes) != 1 || saved.Routes[0].Endpoint != alternative.URL {
		t.Fatalf("routes after the re-pairing = %+v; want the address repaired during it", saved.Routes)
	}
}

// A name given before a re-pairing began is not carried to it.
func TestARePairingStartsWithoutTheNickname(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("first pairing = added %t, %v", got.added, got.err)
	}
	if err := m.Rename(p.backendID, "GPU computer"); err != nil {
		t.Fatal(err)
	}
	if got := within(t, "the re-pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("re-pairing = added %t, %v", got.added, got.err)
	}
	if saved, err := deviceclient.LoadSession(dir, p.backendID); err != nil || saved.Nickname != "" {
		t.Fatalf("after the re-pairing = %+v, %v; want no nickname", saved, err)
	}
}

// A rename waiting on an enrollment ends when that enrollment is forgotten
// and when shutdown begins.
func TestARenameWaitingOnAnEnrollmentEnds(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"forget", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair = make(chan struct{}) })
			m, _ := newManager(t)
			person := p.pair(m)
			within(t, "the redemption reaching the other computer", 5*time.Second, p.pairArrived)
			renamed := make(chan error, 1)
			go func() { renamed <- m.Rename(p.backendID, "GPU computer") }()
			stillWaiting(t, "the rename", renamed)
			want := deviceclient.ErrNoSession
			if end == "forget" {
				if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			} else {
				m.BeginClose()
				want = errShuttingDown
			}
			if err := within(t, "the waiting rename", 2*time.Second, renamed); !errors.Is(err, want) {
				t.Fatalf("waiting rename = %v; want %v", err, want)
			}
			closeOnce(p.holdPair)
			within(t, "the person's pairing", 2*time.Second, person)
		})
	}
}
