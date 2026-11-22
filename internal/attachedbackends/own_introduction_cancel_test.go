//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/servercert"
)

// heldIntroduction is an own-device introduction whose redemption the far
// side holds until the test releases it.
type heldIntroduction struct {
	m         *Manager
	backendID string
	// cancel ends the introduction's own caller; release lets the far side
	// answer its redemption.
	cancel    context.CancelFunc
	release   chan struct{}
	released  sync.Once
	done      chan heldAnswer
	rotations *atomic.Int32
	published *atomic.Int32
}

type heldAnswer struct {
	added bool
	err   error
}

// startHeldIntroduction starts the introduction and returns once its
// redemption has reached the far side.
func startHeldIntroduction(t *testing.T, backendID string) *heldIntroduction {
	t.Helper()
	arrived := make(chan struct{})
	held := &heldIntroduction{backendID: backendID, release: make(chan struct{}), done: make(chan heldAnswer, 1),
		rotations: &atomic.Int32{}, published: &atomic.Int32{}}
	grant := func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": "session-1", "credential": "ao1.issued", "expiresAtMs": time.Now().Add(time.Hour).UnixMilli(),
			"refreshSecret": "refresh-" + time.Now().String(), "refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
			"scopes": []string{"threads:read"},
		})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"backendId": backendID})
		case "/auth/pair":
			close(arrived)
			<-held.release
			grant(w)
		case "/auth/token":
			held.rotations.Add(1)
			grant(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	m, err := New(t.TempDir(), "this computer", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	// Released before the server and the manager close, so a test that
	// failed before finish does not leave the redemption parked.
	t.Cleanup(func() { held.released.Do(func() { close(held.release) }) })
	held.m = m
	m.SetChanged(func(SetChange) { held.published.Add(1) })

	route := computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)}
	route, err = computerroute.Normalize(route)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{
		Version: deviceclient.LinkVersion, Purpose: "own-introduction", BackendID: backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "invitation",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(encoded)

	ctx, cancel := context.WithCancel(context.Background())
	held.cancel = cancel
	go func() {
		added, err := m.AcceptOwnDeviceIntroduction(ctx, raw, []computerroute.Route{route})
		held.done <- heldAnswer{added, err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the redemption never arrived")
	}
	return held
}

// finish releases the redemption and returns what the introduction ended
// with.
func (h *heldIntroduction) finish(t *testing.T) heldAnswer {
	t.Helper()
	h.released.Do(func() { close(h.release) })
	select {
	case got := <-h.done:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("AcceptOwnDeviceIntroduction did not return")
		return heldAnswer{}
	}
}

// An introduction is bound to its caller: a caller that leaves while the
// redemption waits on the far side ends the redemption, and nothing is
// saved, rotated or published. The far side's invitation is spent; the
// session it issued is revoked when that computer mints its next
// introduction.
func TestOwnIntroductionEndsWhenTheCallerLeavesDuringRedemption(t *testing.T) {
	t.Parallel()
	held := startHeldIntroduction(t, "backend-far")
	held.cancel()
	// The far side is still holding the redemption: the answer comes from
	// the caller leaving, not from the far side.
	select {
	case got := <-held.done:
		if got.added || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("introduction after the caller left = added %t, %v; want not added and the caller's cancellation", got.added, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the introduction outlived its caller")
	}
	held.released.Do(func() { close(held.release) })
	if _, err := deviceclient.LoadSession(held.m.dir, held.backendID); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("stored session after the caller left = %v; want none", err)
	}
	nothingEnrolled(t, held.m, held.backendID)
	if n := held.rotations.Load(); n != 0 {
		t.Fatalf("%d rotations; want none for a pairing that was not saved", n)
	}
	if n := held.published.Load(); n != 0 {
		t.Fatalf("%d publications; want none", n)
	}
}
