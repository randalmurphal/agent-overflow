package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/servercert"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// introducingComputer is another own computer that answers an
// introduction's redemption and then either drops every connection (gone)
// or holds the rotation that acknowledges it until the request ends.
type introducingComputer struct {
	backendID       string
	invitation      string
	gone            bool
	redeemed        atomic.Bool
	rotationArrived chan struct{}
	// rotationAbandoned is set when the client gave the held rotation up.
	rotationAbandoned atomic.Bool
}

// newIntroducingComputer stands the computer up and enters it in host's
// catalog as an active own device with its route, the way a sponsor's sync
// would.
func newIntroducingComputer(t *testing.T, host ownConnectionHost, gone bool) *introducingComputer {
	t.Helper()
	c := &introducingComputer{backendID: uuid.NewString(), gone: gone, rotationArrived: make(chan struct{})}
	var arrived atomic.Bool
	released := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.gone && c.redeemed.Load() {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set(deviceclient.RefreshRecoveryHeader, "1")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"backendId": c.backendID})
		case "/auth/pair":
			c.redeemed.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionId": "session-1", "credential": "ao1.issued", "expiresAtMs": time.Now().Add(time.Hour).UnixMilli(),
				"refreshSecret": "refresh-0", "refreshExpiresAtMs": time.Now().Add(24 * time.Hour).UnixMilli(),
				"scopes": []string{"threads:read"},
			})
		case "/auth/token/recover":
			// Read to the end so the server notices when the client hangs up.
			_, _ = io.Copy(io.Discard, r.Body)
			if arrived.CompareAndSwap(false, true) {
				close(c.rotationArrived)
			}
			select {
			case <-r.Context().Done():
				c.rotationAbandoned.Store(true)
			case <-released:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	// Runs before server.Close, so a rotation still held does not wedge it.
	t.Cleanup(func() { close(released) })
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.app.enableOwnDevices(); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeOwnDevices(host.app, []owndevices.Member{{DeviceClass: "desktop", KeyThumbprint: base64.RawURLEncoding.EncodeToString(key),
		BackendID: c.backendID, Name: "Gone computer", Generation: 1, Routes: []computerroute.Route{route}}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, Purpose: "own-introduction", BackendID: c.backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "invitation"})
	if err != nil {
		t.Fatal(err)
	}
	c.invitation = deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(encoded)
	return c
}

// Quit while an introduction asked for over the wire waits for its
// acknowledging rotation, which the other computer never answers. The
// transport ends the call and Close ends the rotation, so shutdown
// finishes inside the transport's drain deadline instead of waiting on
// the other computer.
func TestQuitDuringAnIntroductionsRotationFinishesWithinTheDrain(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	host := ownConnectionBackend(t)
	other := newIntroducingComputer(t, host, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s/ws?token=launch-credential-under-test", host.srv.Addr()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	rpc := rpcclient.New(conn)
	called := make(chan error, 1)
	go func() { called <- rpc.Call(ctx, "AcceptOwnDeviceIntroduction", nil, other.invitation) }()
	select {
	case <-other.rotationArrived:
	case err := <-called:
		t.Fatalf("the introduction returned %v before its rotation reached the other computer", err)
	case <-ctx.Done():
		t.Fatal("the introduction's rotation never reached the other computer")
	}

	started := time.Now()
	shutdown := make(chan error, 1)
	go func() { shutdown <- host.app.Shutdown(context.Background()) }()
	select {
	case <-shutdown:
	case <-time.After(transportShutdownDrainTimeout + 2*time.Second):
		t.Fatalf("shutdown with an introduction's rotation in flight did not finish within %s", transportShutdownDrainTimeout+2*time.Second)
	}
	if took := time.Since(started); took > transportShutdownDrainTimeout {
		t.Fatalf("shutdown took %s, past the transport's %s drain deadline", took, transportShutdownDrainTimeout)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the introduction call was still open after shutdown")
	}
	for deadline := time.Now().Add(2 * time.Second); !other.rotationAbandoned.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the rotation was still waiting on the other computer after shutdown")
		}
	}
}

// The introduction's caller hears that the rotation failed, and the new
// connection is still announced on the own-device channel: the profile is
// saved whether or not the other computer acknowledged it.
func TestAnIntroductionWhoseRotationFailedIsStillAnnounced(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	host := ownConnectionBackend(t)
	other := newIntroducingComputer(t, host, true)
	var announced atomic.Int32
	host.app.testEmitHook = func(name string, _ any) {
		if name == string(eventchan.OwnDevicesChanged) {
			announced.Add(1)
		}
	}
	before := announced.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.app.AcceptOwnDeviceIntroduction(ctx, other.invitation) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the introduction to a computer gone after its redemption did not return")
	}
	if err == nil {
		t.Fatal("the introduction reported success though its rotation failed")
	}
	if saved, loadErr := deviceclient.LoadSession(host.profile, other.backendID); loadErr != nil || !saved.OwnDevice {
		t.Fatalf("saved pairing = %+v, %v; want the own-device pairing kept", saved, loadErr)
	}
	if announced.Load() == before {
		t.Fatal("the saved connection was not announced on own-devices:changed")
	}
}

// The rotation after an introduction is what tells the other computer the
// introduction completed. That computer revokes, at its next introduction
// of the same device, every earlier introduction session whose first
// refresh secret was never spent (store.RetireOwnIntroductions). After a
// completed introduction its session survives that, carries requests, and
// renews.
func TestTheIntroductionsRotationKeepsItsSessionThroughTheNextIntroduction(t *testing.T) {
	remotetest.Require(t)
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var catalog owndevices.List
	if err := a.app.backends.CallOwnDevice(ctx, b.id, "ListOwnDevices", &catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeOwnDeviceSource(a.app, catalog); err != nil {
		t.Fatal(err)
	}
	bkey, err := b.app.backends.OwnIdentity()
	if err != nil {
		t.Fatal(err)
	}
	invite, err := a.app.MintOwnDeviceIntroduction(context.Background(), bkey)
	if err != nil {
		t.Fatal(err)
	}
	link, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	route := computerroute.Route{Endpoint: link.Endpoint, CertFingerprint: link.CertFingerprint}
	if added, err := b.app.backends.AcceptOwnDeviceIntroduction(ctx, invite.URL, []computerroute.Route{route}); err != nil || !added {
		t.Fatalf("introduction: added=%v %v", added, err)
	}

	if _, err := a.app.MintOwnDeviceIntroduction(context.Background(), bkey); err != nil {
		t.Fatal(err)
	}
	var list owndevices.List
	if err := b.app.backends.CallOwnDevice(ctx, a.id, "ListOwnDevices", &list); err != nil || !list.Enabled {
		t.Fatalf("the introduced session after the next introduction of the same device: %+v %v", list, err)
	}
	saved, err := deviceclient.LoadSession(b.profile, a.id)
	if err != nil {
		t.Fatal(err)
	}
	renewer, err := deviceclient.Open(b.profile, saved, deviceclient.WithDialContext(dialOwnFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := renewer.Renew(ctx); err != nil {
		t.Fatalf("a later renewal of the introduced session: %v", err)
	}
	if err := b.app.backends.CallOwnDevice(ctx, a.id, "ListOwnDevices", &list); err != nil {
		t.Fatalf("the introduced session after its renewal: %v", err)
	}
}
