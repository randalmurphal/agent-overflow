//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
// side holds: the manager holds the destination's profile lock until the
// test releases the redemption.
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

// An accepted introduction spends the far side's single-use invitation. A
// caller that stops waiting once the redemption was sent still gets the
// pairing saved, activated and published.
func TestOwnIntroductionFinishesWhenTheCallerCancelsDuringRedemption(t *testing.T) {
	t.Parallel()
	held := startHeldIntroduction(t, "backend-far")
	held.cancel()
	time.Sleep(50 * time.Millisecond)
	got := held.finish(t)
	if got.err != nil || !got.added {
		t.Fatalf("introduction after the caller cancelled = added %t, %v; want it enrolled", got.added, got.err)
	}
	if saved, err := deviceclient.LoadSession(held.m.dir, held.backendID); err != nil || !saved.OwnDevice {
		t.Fatalf("saved session = %+v, %v; want the own-device pairing", saved, err)
	}
	if held.rotations.Load() == 0 {
		t.Fatal("the pairing was never activated")
	}
	if held.published.Load() == 0 {
		t.Fatal("the new pairing was not published")
	}
}
