//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// One peer's part of a pass runs under its bound, asked when that part
// starts: ownPeerTimeout, which is 12 s, or what SetPairingBound answers. The
// pass's Mint hook reads the deadline it is called under, inside the part.
func TestOnePeersPassRunsUnderItsBound(t *testing.T) {
	t.Parallel()
	if ownPeerTimeout != 12*time.Second {
		t.Fatalf("ownPeerTimeout = %v, want 12s", ownPeerTimeout)
	}
	hour := func() (time.Duration, error) { return time.Hour, nil }
	for _, c := range []struct {
		name  string
		bound func() (time.Duration, error)
		want  time.Duration
	}{
		{name: "with a pairing bound", bound: hour, want: time.Hour},
		{name: "without one", want: ownPeerTimeout},
	} {
		manager, dir := newManager(t)
		manager.SetPairingBound(c.bound)
		key, err := manager.OwnIdentity()
		if err != nil {
			t.Fatal(err)
		}
		members := []owndevices.Member{
			{KeyThumbprint: "peer-key", BackendID: "peer", Name: "peer", Generation: 1},
			{KeyThumbprint: key, BackendID: "me", Name: "me", Generation: 1},
		}
		server := answeringOwnPeer(t, owndevices.List{Enabled: true, SelfKeyThumbprint: "peer-key", Members: members}, nil)
		seedOwnPeer(t, dir, server.URL)
		local := owndevices.List{Enabled: true, SelfKeyThumbprint: key, Members: members}
		stop := errors.New("stopped once the deadline was read")
		var deadline, minted time.Time
		var ok bool
		hooks := OwnDeviceHooks{
			Snapshot: func() (owndevices.List, error) { return local, nil },
			Accept:   func(owndevices.List) (bool, error) { return true, nil },
			Mint: func(ctx context.Context, _ string) (string, error) {
				deadline, ok = ctx.Deadline()
				minted = time.Now()
				return "", stop
			},
		}
		began := time.Now()
		if failed := manager.ReconcileOwnDevices(context.Background(), hooks); !failed {
			t.Fatalf("%s: the pass reported no failure, want the Mint hook's", c.name)
		}
		// Both ends are clock reads around the moment the deadline was set.
		if !ok || deadline.Before(began.Add(c.want)) || deadline.After(minted.Add(c.want)) {
			t.Fatalf("%s: Mint was called under a deadline %v after the pass began (set %t), want %v", c.name, deadline.Sub(began), ok, c.want)
		}
	}
}

// A nearby pairing's client carries the bound asked when it is built:
// pairbootstrap's own 15 s, or what SetPairingBound answers.
func TestANearbyPairingsClientCarriesItsBound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		bound func() (time.Duration, error)
		want  time.Duration
	}{
		{bound: func() (time.Duration, error) { return time.Hour, nil }, want: time.Hour},
		{want: 15 * time.Second},
	} {
		manager, _ := newManager(t)
		manager.SetPairingBound(c.bound)
		hc, err := manager.nearbyClient()
		if err != nil {
			t.Fatal(err)
		}
		if hc.Timeout != c.want {
			t.Fatalf("the nearby client's bound = %v, want %v", hc.Timeout, c.want)
		}
	}
}

// A pairing step whose bound has no time left, or answers a non-positive
// bound, fails at once with that, and sends nothing: one peer's part of a
// pass, a nearby exchange, and a redemption.
func TestAPairingStepWithNoBoundLeftFailsAtOnce(t *testing.T) {
	t.Parallel()
	exhausted := errors.New("no time left in this run")
	none := func() (time.Duration, error) { return 0, exhausted }

	t.Run("one peer's part of a pass", func(t *testing.T) {
		manager, dir := newManager(t)
		manager.SetPairingBound(none)
		var requests atomic.Int32
		server := answeringOwnPeer(t, owndevices.List{}, &requests)
		seedOwnPeer(t, dir, server.URL)
		hooks := OwnDeviceHooks{Snapshot: func() (owndevices.List, error) { return owndevices.List{}, nil }}
		if !manager.ReconcileOwnDevices(context.Background(), hooks) {
			t.Fatal("the pass reported no failure")
		}
		rows, err := manager.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || !strings.Contains(rows[0].OwnDeviceSyncError, exhausted.Error()) {
			t.Fatalf("rows = %+v, want the peer's row to carry the budget's error", rows)
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("the peer saw %d requests, want none", n)
		}
	})
	t.Run("a nearby exchange", func(t *testing.T) {
		manager, _ := newManager(t)
		manager.SetPairingBound(none)
		var requests atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.NotFound(w, r)
		}))
		t.Cleanup(server.Close)
		if _, err := manager.Add(context.Background(), strings.TrimPrefix(server.URL, "https://")); !errors.Is(err, exhausted) {
			t.Fatalf("an address pairing = %v, want the budget's error", err)
		}
		if n := requests.Load(); n != 0 {
			t.Fatalf("the computer saw %d requests, want none", n)
		}
	})
	t.Run("a redemption", func(t *testing.T) {
		p := newIntroducedPeer(t, nil)
		manager, _ := newManager(t)
		manager.SetPairingBound(none)
		if _, err := manager.Add(context.Background(), p.personRaw); !errors.Is(err, exhausted) {
			t.Fatalf("a pairing = %v, want the budget's error", err)
		}
		if n := p.redemptions.Load(); n != 0 {
			t.Fatalf("the computer saw %d redemptions, want none", n)
		}
	})
	t.Run("a non-positive bound", func(t *testing.T) {
		manager, _ := newManager(t)
		manager.SetPairingBound(func() (time.Duration, error) { return 0, nil })
		if _, err := manager.nearbyClient(); err == nil || !strings.Contains(err.Error(), "bound 0s is not positive") {
			t.Fatalf("a zero bound = %v, want it refused", err)
		}
	})
}

// seedOwnPeer saves an own-device session with the peer at endpoint.
func seedOwnPeer(t *testing.T, dir, endpoint string) {
	t.Helper()
	seed(t, dir, deviceclient.Session{
		OwnDevice: true, BackendID: "peer", Endpoint: endpoint, SessionID: "session", Credential: "credential",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(),
	})
}

// answeringOwnPeer serves an own-device peer that mints tickets, says hello
// and answers every call with catalog, counting requests when requests is
// not nil.
func answeringOwnPeer(t *testing.T, catalog owndevices.List, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests.Add(1)
		}
		if r.URL.Path == "/auth/ticket" {
			_, _ = w.Write([]byte(`{"ticket":"ticket"}`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		hello := map[string]any{"type": "hello", "backendId": "peer", "protocolVersion": transport.ProtocolVersion, "capabilities": []string{owndevices.Capability}}
		if wsjson.Write(ctx, conn, hello) != nil {
			return
		}
		for {
			var frame transport.ClientFrame
			if wsjson.Read(ctx, conn, &frame) != nil {
				return
			}
			if wsjson.Write(ctx, conn, map[string]any{"type": "rpc", "id": frame.ID, "result": catalog}) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}
