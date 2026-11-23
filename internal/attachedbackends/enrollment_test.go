//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// The steps of a pairing by address before its redemption spend nothing,
// so they end when shutdown begins.
func TestShutdownEndsAPairingByAddressBeforeItsRedemption(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	m, _ := newManager(t)
	done := make(chan error, 1)
	go func() {
		_, err := m.Add(context.Background(), server.Listener.Addr().String())
		done <- err
	}()
	within(t, "the pairing's first request", 5*time.Second, arrived)
	m.BeginClose()
	if err := within(t, "the pairing by address", time.Second, done); err == nil {
		t.Fatal("a pairing by address cut off by shutdown reported success")
	}
}

// An introduction's route selection spends nothing, so it ends when
// shutdown begins.
func TestShutdownEndsAnIntroductionsRouteSelection(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdHealth = make(chan struct{}) })
	m, _ := newManager(t)
	answer := p.introduce(context.Background(), m)
	within(t, "route selection reaching the other computer", 5*time.Second, p.healthArrived)
	m.BeginClose()
	if got := within(t, "the introduction", time.Second, answer); got.added || got.err == nil {
		t.Fatalf("introduction cut off by shutdown during route selection = added %t, %v; want an error", got.added, got.err)
	}
	if n := p.redemptions.Load(); n != 0 {
		t.Fatalf("%d redemptions; want none", n)
	}
}

// pair runs a person's pairing with p on its own goroutine.
func (p *introducedPeer) pair(m *Manager) <-chan introductionAnswer {
	done := make(chan introductionAnswer, 1)
	go func() {
		_, err := m.Add(context.Background(), p.personRaw)
		done <- introductionAnswer{added: err == nil, err: err}
	}()
	return done
}

// enrollmentPurposes runs a test once for each kind of enrollment.
var enrollmentPurposes = []struct {
	name     string
	byPerson bool
	start    func(p *introducedPeer, m *Manager) <-chan introductionAnswer
}{
	{"introduction", false, func(p *introducedPeer, m *Manager) <-chan introductionAnswer {
		return p.introduce(context.Background(), m)
	}},
	{"person", true, func(p *introducedPeer, m *Manager) <-chan introductionAnswer { return p.pair(m) }},
}

// nothingEnrolled fails unless no pairing with id is saved, cached or in
// flight.
func nothingEnrolled(t *testing.T, m *Manager, id string) {
	t.Helper()
	if _, err := deviceclient.LoadSession(m.dir, id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("saved session = %v; want none", err)
	}
	m.mu.Lock()
	_, cached := m.carriers[id]
	enrolling := m.enrollments[id]
	m.mu.Unlock()
	if cached || enrolling != nil {
		t.Fatalf("carrier cached %t, enrollment %+v; want neither", cached, enrolling)
	}
	if refs := m.profiles.Refs(id); refs != 0 {
		t.Fatalf("profile lock references = %d, want none", refs)
	}
}

// enrolled fails unless a pairing with id is saved and cached, with no
// enrollment left in flight.
func enrolled(t *testing.T, m *Manager, id string) *carrier {
	t.Helper()
	if _, err := deviceclient.LoadSession(m.dir, id); err != nil {
		t.Fatalf("saved session: %v", err)
	}
	m.mu.Lock()
	held := m.carriers[id]
	enrolling := m.enrollments[id]
	m.mu.Unlock()
	if held == nil || enrolling != nil {
		t.Fatalf("carrier %v, enrollment %+v; want the carrier and no enrollment", held, enrolling)
	}
	return held
}

// enrollmentOf reads id's enrollment in flight: its state and whether its
// redemption's context has ended.
func enrollmentOf(m *Manager, id string) (enrollmentState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.enrollments[id]
	if e == nil {
		return enrollmentFailed, false
	}
	return e.state, e.ctx.Err() != nil
}

// Forget while a redemption waits on the other computer returns without
// waiting for it, ends it, and leaves nothing of the pairing.
func TestForgetBeforeARedemptionAnswers(t *testing.T) {
	t.Parallel()
	for _, purpose := range enrollmentPurposes {
		t.Run(purpose.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair = make(chan struct{}) })
			m, _ := newManager(t)
			answer := purpose.start(p, m)
			within(t, "the redemption reaching the other computer", 5*time.Second, p.pairArrived)

			if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			within(t, "the redemption ending", 2*time.Second, p.pairAbandoned)
			got := within(t, "the enrollment", 2*time.Second, answer)
			if got.added || !errors.Is(got.err, errPairingForgotten) {
				t.Fatalf("enrollment forgotten before its redemption answered = added %t, %v; want not added, forgotten", got.added, got.err)
			}
			nothingEnrolled(t, m, p.backendID)
		})
	}
}

// Forget that lands after the redemption answered and before its save: the
// answer is not saved.
func TestForgetBetweenARedemptionsAnswerAndItsSave(t *testing.T) {
	t.Parallel()
	for _, purpose := range enrollmentPurposes {
		t.Run(purpose.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, _ := newManager(t)
			// What a commit of another change to the profile holds.
			unlock := m.profiles.Lock(p.backendID)
			answer := purpose.start(p, m)
			// The redemption answered; the save waits for the lock.
			waitForProfileRefs(t, m, p.backendID, 2)
			removed := removeAsync(m, p.backendID)
			waitForProfileRefs(t, m, p.backendID, 3)
			if state, ended := enrollmentOf(m, p.backendID); state != enrollmentCancelled || !ended {
				t.Fatalf("enrollment after Forget = state %d, context ended %t; want cancelled", state, ended)
			}
			unlock()
			if err := within(t, "Forget", 2*time.Second, removed); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			got := within(t, "the enrollment", 2*time.Second, answer)
			if got.added || !errors.Is(got.err, errPairingForgotten) {
				t.Fatalf("enrollment forgotten before its save = added %t, %v; want not added, forgotten", got.added, got.err)
			}
			if n := p.redemptions.Load(); n != 1 {
				t.Fatalf("%d redemptions; want the one", n)
			}
			nothingEnrolled(t, m, p.backendID)
		})
	}
}

// When shutdown begins while a redemption waits on the other computer, an
// introduction ends and saves nothing, and a person's pairing goes on and
// is saved once the other computer answers.
func TestShutdownBeforeARedemptionAnswers(t *testing.T) {
	t.Parallel()
	for _, purpose := range enrollmentPurposes {
		t.Run(purpose.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair = make(chan struct{}) })
			m, _ := newManager(t)
			answer := purpose.start(p, m)
			within(t, "the redemption reaching the other computer", 5*time.Second, p.pairArrived)

			begun := make(chan struct{})
			go func() { m.BeginClose(); close(begun) }()
			within(t, "BeginClose", 2*time.Second, begun)
			if !purpose.byPerson {
				within(t, "the redemption ending", 2*time.Second, p.pairAbandoned)
				got := within(t, "the introduction", 2*time.Second, answer)
				if got.added || !errors.Is(got.err, errShuttingDown) {
					t.Fatalf("introduction when shutdown began = added %t, %v; want not added, shutting down", got.added, got.err)
				}
				nothingEnrolled(t, m, p.backendID)
				return
			}
			if state, ended := enrollmentOf(m, p.backendID); state != enrollmentRedeeming || ended {
				t.Fatalf("person's pairing when shutdown began = state %d, context ended %t; want it still redeeming", state, ended)
			}
			closeOnce(p.holdPair)
			got := within(t, "the pairing", 2*time.Second, answer)
			if !got.added || got.err != nil {
				t.Fatalf("person's pairing whose redemption was sent before shutdown = added %t, %v; want it saved", got.added, got.err)
			}
			enrolled(t, m, p.backendID)
		})
	}
}

// When shutdown begins after a redemption answered and before its save, an
// introduction's answer is not saved and a person's is.
func TestShutdownBetweenARedemptionsAnswerAndItsSave(t *testing.T) {
	t.Parallel()
	for _, purpose := range enrollmentPurposes {
		t.Run(purpose.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, _ := newManager(t)
			unlock := m.profiles.Lock(p.backendID)
			answer := purpose.start(p, m)
			waitForProfileRefs(t, m, p.backendID, 2)
			m.BeginClose()
			unlock()
			got := within(t, "the enrollment", 2*time.Second, answer)
			if purpose.byPerson {
				if !got.added || got.err != nil {
					t.Fatalf("person's pairing answered before shutdown = added %t, %v; want it saved", got.added, got.err)
				}
				enrolled(t, m, p.backendID)
				return
			}
			if got.added || !errors.Is(got.err, errShuttingDown) {
				t.Fatalf("introduction answered before shutdown = added %t, %v; want not added, shutting down", got.added, got.err)
			}
			nothingEnrolled(t, m, p.backendID)
		})
	}
}

// Once shutdown has begun, nothing new is redeemed.
func TestShutdownRefusesANewEnrollment(t *testing.T) {
	t.Parallel()
	for _, purpose := range enrollmentPurposes {
		t.Run(purpose.name, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, _ := newManager(t)
			m.BeginClose()
			got := within(t, "the enrollment", 2*time.Second, purpose.start(p, m))
			if got.added || got.err == nil {
				t.Fatalf("enrollment after shutdown began = added %t, %v; want it refused", got.added, got.err)
			}
			if n := p.redemptions.Load(); n != 0 {
				t.Fatalf("%d redemptions after shutdown began; want none", n)
			}
			nothingEnrolled(t, m, p.backendID)
		})
	}
}

// After an enrollment committed, an introduction to the same computer is
// not wanted and redeems nothing, while a person may pair it again.
func TestAnEnrollmentAfterOneCommitted(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, _ := newManager(t)
	if got := within(t, "the introduction", 5*time.Second, p.introduce(context.Background(), m)); !got.added || got.err != nil {
		t.Fatalf("introduction = added %t, %v; want it saved", got.added, got.err)
	}
	got := within(t, "a second introduction", 2*time.Second, p.introduce(context.Background(), m))
	if got.added || got.err != nil || p.redemptions.Load() != 1 {
		t.Fatalf("second introduction = added %t, %v after %d redemptions; want not added, nothing redeemed", got.added, got.err, p.redemptions.Load())
	}
	if got := within(t, "a person's pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil || p.redemptions.Load() != 2 {
		t.Fatalf("person's pairing after an introduction = added %t, %v after %d redemptions; want it saved", got.added, got.err, p.redemptions.Load())
	}
	enrolled(t, m, p.backendID)
}

// A re-pairing leaves the old pairing working while its redemption is in
// flight, nothing waits for that redemption, and the old carrier is
// replaced only when the new session is saved.
func TestARePairingSwapsTheOldCarrierAtTheSave(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair, p.heldPair = make(chan struct{}), 2 })
	m, _ := newManager(t)
	if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("first pairing = added %t, %v", got.added, got.err)
	}
	old := enrolled(t, m, p.backendID)
	again := p.pair(m)
	within(t, "the second redemption reaching the other computer", 5*time.Second, p.pairArrived)

	lookup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if held, err := m.carrier(lookup, p.backendID); err != nil || held != old || old.client.Retired() {
		t.Fatalf("carrier during the re-pairing's redemption = %p (old %p, retired %t), %v; want the old one, working", held, old, old.client.Retired(), err)
	}
	access := make(chan error, 1)
	go func() { access <- m.SetAgentAccess(p.backendID, true) }()
	if err := within(t, "enabling agent access", 2*time.Second, access); err != nil {
		t.Fatalf("SetAgentAccess: %v", err)
	}
	closeOnce(p.holdPair)
	if got := within(t, "the re-pairing", 2*time.Second, again); !got.added || got.err != nil {
		t.Fatalf("re-pairing = added %t, %v", got.added, got.err)
	}
	if replaced := enrolled(t, m, p.backendID); replaced == old || !old.client.Retired() {
		t.Fatalf("after the re-pairing's save the old carrier is cached %t, retired %t; want it replaced", replaced == old, old.client.Retired())
	}
	if access, err := m.AgentAccess(); err != nil || access[p.backendID] {
		t.Fatalf("agent access after a person's re-pairing = %v, %v; want it off, as a new grant", access, err)
	}
}

// A redemption the other computer refuses leaves the pairing it would have
// replaced as it was, still cached and working.
func TestAFailedRedemptionLeavesTheOldCarrierWorking(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, _ := newManager(t)
	if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
		t.Fatalf("first pairing = added %t, %v", got.added, got.err)
	}
	old := enrolled(t, m, p.backendID)
	before, err := deviceclient.LoadSession(m.dir, p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	p.refusePair.Store(true)
	got := within(t, "the refused re-pairing", 5*time.Second, p.pair(m))
	var refusal *deviceclient.Refusal
	if got.added || !errors.As(got.err, &refusal) {
		t.Fatalf("refused re-pairing = added %t, %v; want the refusal", got.added, got.err)
	}
	if held := enrolled(t, m, p.backendID); held != old || old.client.Retired() {
		t.Fatalf("carrier after a refused re-pairing is the old one %t, retired %t; want the old one, working", held == old, old.client.Retired())
	}
	if after, err := deviceclient.LoadSession(m.dir, p.backendID); err != nil || after.Credential != before.Credential || after.RefreshSecret != before.RefreshSecret {
		t.Fatalf("saved session after a refused re-pairing = %+v, %v; want %+v", after, err, before)
	}
}
