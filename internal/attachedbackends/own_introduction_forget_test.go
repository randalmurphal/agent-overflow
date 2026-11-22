//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/servercert"
)

// introducedPeer is the other computer of an own-device introduction. It
// speaks renewal recovery, as every computer that mints introductions does,
// and each test sets how it behaves after the redemption.
type introducedPeer struct {
	backendID string
	route     computerroute.Route
	raw       string
	// gone drops every connection after the redemption, as a computer that
	// just quit does.
	gone bool
	// holdHealth, when set, holds route selection's health probe until it
	// closes; healthArrived closes when the first probe reaches it.
	holdHealth    chan struct{}
	healthArrived chan struct{}
	// holdRotation, when set, holds the first renewal until it closes or
	// the request ends; rotationArrived closes when it arrives and
	// rotationAbandoned when the client gave the request up.
	holdRotation      chan struct{}
	rotationArrived   chan struct{}
	rotationAbandoned chan struct{}
	// holdPair, when set, holds redemption number heldPair (the first when
	// zero) until it closes or the request ends; pairArrived closes when it
	// arrives and pairAbandoned when the client gave the request up.
	// refusePair refuses every redemption, refusedPair only that one, and
	// droppedPair drops that one's connection with no answer.
	holdPair      chan struct{}
	heldPair      int32
	pairArrived   chan struct{}
	pairAbandoned chan struct{}
	refusePair    atomic.Bool
	refusedPair   int32
	droppedPair   int32
	redeemed      atomic.Bool
	redemptions   atomic.Int32
	rotations     atomic.Int32
	// personRaw is a person's pairing link to this computer, where raw is
	// an introduction.
	personRaw string
}

const introducedBackendID = "6f0c8a2e-3b4d-4c5e-9f60-7a8b9c0d1e2f"

func newIntroducedPeer(t *testing.T, configure func(*introducedPeer)) *introducedPeer {
	t.Helper()
	p := &introducedPeer{backendID: introducedBackendID, healthArrived: make(chan struct{}),
		rotationArrived: make(chan struct{}), rotationAbandoned: make(chan struct{}),
		pairArrived: make(chan struct{}), pairAbandoned: make(chan struct{})}
	if configure != nil {
		configure(p)
	}
	if p.heldPair == 0 {
		p.heldPair = 1
	}
	var healthOnce, arrivedOnce atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.gone && p.redeemed.Load() {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set(deviceclient.RefreshRecoveryHeader, "1")
		switch r.URL.Path {
		case "/healthz":
			if p.holdHealth != nil && healthOnce.CompareAndSwap(false, true) {
				close(p.healthArrived)
				select {
				case <-p.holdHealth:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"backendId": p.backendID})
		case "/auth/pair":
			number := p.redemptions.Add(1)
			// Read to the end so the server notices when the client hangs up.
			_, _ = io.Copy(io.Discard, r.Body)
			if p.holdPair != nil && number == p.heldPair {
				close(p.pairArrived)
				select {
				case <-p.holdPair:
				case <-r.Context().Done():
					close(p.pairAbandoned)
					return
				}
			}
			if number == p.droppedPair {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			if p.refusePair.Load() || number == p.refusedPair {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"reason": "unknown_credential"})
				return
			}
			p.redeemed.Store(true)
			introducedGrant(w, "refresh-0")
		case "/auth/token/recover":
			p.rotations.Add(1)
			var body struct {
				Next string `json:"nextRefreshSecret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Read to the end so the server notices when the client hangs up.
			_, _ = io.Copy(io.Discard, r.Body)
			if p.holdRotation != nil && arrivedOnce.CompareAndSwap(false, true) {
				close(p.rotationArrived)
				select {
				case <-p.holdRotation:
				case <-r.Context().Done():
					close(p.rotationAbandoned)
					return
				}
			}
			introducedGrant(w, body.Next)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if p.holdHealth != nil {
		t.Cleanup(func() { closeOnce(p.holdHealth) })
	}
	if p.holdRotation != nil {
		t.Cleanup(func() { closeOnce(p.holdRotation) })
	}
	if p.holdPair != nil {
		t.Cleanup(func() { closeOnce(p.holdPair) })
	}
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, Purpose: "own-introduction", BackendID: p.backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "invitation"})
	if err != nil {
		t.Fatal(err)
	}
	p.route, p.raw = route, deviceclient.LinkFragmentPrefix+base64.RawURLEncoding.EncodeToString(encoded)
	person, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: p.backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "pairing"})
	if err != nil {
		t.Fatal(err)
	}
	p.personRaw = deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(person)
	return p
}

// introducedVerificationNumber is the number every grant of the peer carries.
const introducedVerificationNumber = "314159"

func introducedGrant(w http.ResponseWriter, refresh string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionId": "session-1", "credential": "ao1.issued-" + refresh, "expiresAtMs": time.Now().Add(time.Hour).UnixMilli(),
		"refreshSecret": refresh, "refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
		"scopes": []string{"threads:read"}, "verificationNumber": introducedVerificationNumber,
	})
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

type introductionAnswer struct {
	added bool
	err   error
}

// introduce runs the introduction on its own goroutine.
func (p *introducedPeer) introduce(ctx context.Context, m *Manager) <-chan introductionAnswer {
	done := make(chan introductionAnswer, 1)
	go func() {
		added, err := m.AcceptOwnDeviceIntroduction(ctx, p.raw, []computerroute.Route{p.route})
		done <- introductionAnswer{added, err}
	}()
	return done
}

func within[T any](t *testing.T, what string, bound time.Duration, ch <-chan T) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(bound):
		t.Fatalf("%s did not finish within %s", what, bound)
		var zero T
		return zero
	}
}

func removeAsync(m *Manager, id string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- m.Remove(id) }()
	return done
}

// forgotten fails unless nothing of the computer's pairing is left here.
func forgotten(t *testing.T, m *Manager, id string) {
	t.Helper()
	if _, paired, err := m.Lookup(id); err != nil || paired {
		t.Fatalf("Lookup after Forget = paired %t, %v; want no pairing", paired, err)
	}
	if rows := m.Attached(); len(rows) != 0 {
		t.Fatalf("Attached after Forget = %+v; want none", rows)
	}
	m.mu.Lock()
	_, cached := m.carriers[id]
	m.mu.Unlock()
	if cached {
		t.Fatal("the forgotten computer's carrier is still cached")
	}
	if refs := m.profiles.Refs(id); refs != 0 {
		t.Fatalf("the forgotten computer's profile lock has %d holders or waiters", refs)
	}
	access, err := m.AgentAccess()
	if err != nil || access[id] {
		t.Fatalf("agent access after Forget = %v, %v; want it gone", access, err)
	}
}

// A computer that redeems the introduction and then goes away leaves a
// saved pairing whose acknowledging rotation failed. The introduction
// reports that at once, and nothing for that computer waits on it: a
// carried request finds the carrier, agent access can be turned on, and
// Forget returns with nothing of the pairing left.
func TestForgetAnIntroducedComputerThatWentAwayAfterItsRedemption(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.gone = true })
	m, _ := newManager(t)
	var published atomic.Int32
	m.SetChanged(func(change SetChange) {
		if change.Action == SetMembership {
			published.Add(1)
		}
	})

	got := within(t, "the introduction", 5*time.Second, p.introduce(context.Background(), m))
	if !got.added || got.err == nil {
		t.Fatalf("introduction to a computer gone after its redemption = added %t, %v; want added with the rotation's failure", got.added, got.err)
	}
	if published.Load() == 0 {
		t.Fatal("the saved pairing was not published")
	}
	lookup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if m.Carrier(lookup, p.backendID) == nil {
		t.Fatal("a carried request found no carrier for the saved pairing")
	}
	access := make(chan error, 1)
	go func() { access <- m.SetAgentAccess(p.backendID, true) }()
	if err := within(t, "enabling agent access", 2*time.Second, access); err != nil {
		t.Fatalf("SetAgentAccess: %v", err)
	}
	if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	forgotten(t, m, p.backendID)
}

// Forget while the introduction's rotation is still unanswered returns
// without waiting for it, the rotation ends with it, and nothing of the
// pairing is left.
func TestForgetDuringTheIntroductionsRotation(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdRotation = make(chan struct{}) })
	m, _ := newManager(t)
	answer := p.introduce(context.Background(), m)
	within(t, "the rotation reaching the other computer", 5*time.Second, p.rotationArrived)

	if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	within(t, "the rotation ending", 2*time.Second, p.rotationAbandoned)
	got := within(t, "the introduction", 2*time.Second, answer)
	if !got.added || !errors.Is(got.err, deviceclient.ErrSessionEnded) {
		t.Fatalf("introduction whose pairing was forgotten mid-rotation = added %t, %v; want added and the session ended", got.added, got.err)
	}
	forgotten(t, m, p.backendID)
}

// Quit during the introduction's rotation: the caller's context ends first
// (the transport cancels its calls), then Close. Neither waits for the
// other computer to answer, and the pairing stays with its proposed
// successor saved, which the next launch's rotation completes.
func TestQuitDuringTheIntroductionsRotation(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdRotation = make(chan struct{}) })
	m, dir := newManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := p.introduce(ctx, m)
	within(t, "the rotation reaching the other computer", 5*time.Second, p.rotationArrived)

	cancel()
	got := within(t, "the introduction after its caller ended", 2*time.Second, answer)
	if !got.added || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("introduction whose caller ended mid-rotation = added %t, %v; want added and the caller's cancellation", got.added, got.err)
	}
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	within(t, "Close", 2*time.Second, closed)
	within(t, "the rotation ending", 2*time.Second, p.rotationAbandoned)
	saved, err := deviceclient.LoadSession(dir, p.backendID)
	if err != nil || !saved.OwnDevice || saved.PendingNextSecret == "" || saved.RefreshSecret != "refresh-0" {
		t.Fatalf("pairing after quit = %+v, %v; want it kept with its proposed successor", saved, err)
	}
}

// Forget that lands after the introduction's first check and before its
// profile lock wins: the check under the lock sees the exclusion, nothing
// is redeemed, and nothing is saved.
func TestForgetBetweenTheIntroductionsCheckAndItsLock(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdHealth = make(chan struct{}) })
	m, _ := newManager(t)
	answer := p.introduce(context.Background(), m)
	within(t, "route selection reaching the other computer", 5*time.Second, p.healthArrived)

	if err := within(t, "Forget", 2*time.Second, removeAsync(m, p.backendID)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	close(p.holdHealth)
	got := within(t, "the introduction", 5*time.Second, answer)
	if got.added || got.err != nil {
		t.Fatalf("introduction after Forget = added %t, %v; want it refused quietly", got.added, got.err)
	}
	if p.redeemed.Load() {
		t.Fatal("the introduction was redeemed after the computer was forgotten")
	}
	forgotten(t, m, p.backendID)
	if excluded, err := m.ownDeviceExcluded(p.backendID); err != nil || !excluded {
		t.Fatalf("exclusion after Forget = %t, %v; want it recorded", excluded, err)
	}
}
