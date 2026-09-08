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
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// TestOwnDeviceReconcilerRetriesOnlyAfterAFailedPass — a pass in which every
// peer answered schedules nothing (propagation is push-driven), and one in
// which a peer failed is retried on a doubling ladder.
func TestOwnDeviceReconcilerRetriesOnlyAfterAFailedPass(t *testing.T) {
	manager, dir := newManager(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	legacy := false
	seed(t, dir, deviceclient.Session{
		OwnDevice: true, RefreshRecovery: &legacy,
		BackendID: "peer", Endpoint: dead.URL, SessionID: "session", Credential: "credential",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(), RefreshSecret: "refresh",
	})
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	manager.own.after = func(d time.Duration) <-chan time.Time { waits <- d; return fire }
	passes := make(chan struct{}, 8)
	hooks := OwnDeviceHooks{Snapshot: func() (owndevices.List, error) {
		passes <- struct{}{}
		return owndevices.List{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pass := func() {
		t.Helper()
		select {
		case <-passes:
		case <-time.After(10 * time.Second):
			t.Fatal("no reconcile pass")
		}
	}
	scheduled := func() time.Duration {
		t.Helper()
		select {
		case d := <-waits:
			return d
		case <-time.After(10 * time.Second):
			t.Fatal("a failed pass scheduled no retry")
			return 0
		}
	}

	manager.StartOwnDevices(ctx, hooks)
	pass()
	if d := scheduled(); d != ownRetryMin {
		t.Fatalf("first retry in %v, want %v", d, ownRetryMin)
	}
	fire <- time.Now()
	pass()
	if d := scheduled(); d != 2*ownRetryMin {
		t.Fatalf("second retry in %v, want %v", d, 2*ownRetryMin)
	}

	// With the unreachable peer gone the woken pass is healthy, and healthy
	// passes cost no timer: the next wake finds none was scheduled.
	if err := manager.Remove("peer"); err != nil {
		t.Fatal(err)
	}
	manager.WakeOwnDevices()
	pass()
	manager.WakeOwnDevices()
	pass()
	select {
	case d := <-waits:
		t.Fatalf("a healthy pass scheduled a retry in %v", d)
	default:
	}
	cancel()
	manager.WaitOwnDevices()
}

// ownPeer serves one own-device peer: a ticket mint, the hello, and the
// catalog it answers ListOwnDevices with.
func ownPeer(t *testing.T, catalog owndevices.List) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/ticket" {
			_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "ticket"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		hello := map[string]any{"type": "hello", "backendId": "peer", "protocolVersion": transport.ProtocolVersion, "capabilities": []string{owndevices.Capability}}
		if wsjson.Write(ctx, conn, hello) != nil {
			return
		}
		for {
			var frame transport.ClientFrame
			if wsjson.Read(ctx, conn, &frame) != nil {
				return
			}
			response := map[string]any{"type": "rpc", "id": frame.ID, "result": catalog}
			if frame.Method != "ListOwnDevices" {
				response["error"] = map[string]string{"code": "unexpected", "message": frame.Method}
			}
			if wsjson.Write(ctx, conn, response) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestATombstoneForThisDeviceRetiresThePeerProfile — the far side's catalog
// carrying this device as removed is its removal notice. The catalog is
// accepted like any other, and that peer's own-device profile retires even
// when a stale local generation still believes this device is a member:
// rejoining takes a fresh approval over there, never a retry from here.
func TestATombstoneForThisDeviceRetiresThePeerProfile(t *testing.T) {
	manager, dir := newManager(t)
	key, err := manager.OwnIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remote := owndevices.List{Enabled: true, SelfKeyThumbprint: "peer-key", Members: []owndevices.Member{
		{KeyThumbprint: "peer-key", BackendID: "peer", Name: "peer", Generation: 1},
		{KeyThumbprint: key, Name: "me", Generation: 2, Removed: true},
	}}
	server := ownPeer(t, remote)
	seed(t, dir, deviceclient.Session{
		OwnDevice: true, BackendID: "peer", Endpoint: server.URL, SessionID: "session", Credential: "credential",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(),
	})
	local := owndevices.List{Enabled: true, SelfKeyThumbprint: key, Members: []owndevices.Member{
		{KeyThumbprint: key, Name: "me", Generation: 1},
		{KeyThumbprint: "peer-key", BackendID: "peer", Name: "peer", Generation: 1},
	}}
	var accepted []owndevices.List
	changed := 0
	manager.SetChanged(func(change SetChange) {
		if change.Action == SetMembership {
			changed++
		}
	})
	hooks := OwnDeviceHooks{
		Snapshot: func() (owndevices.List, error) { return local, nil },
		Accept:   func(source owndevices.List) (bool, error) { accepted = append(accepted, source); return true, nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := manager.reconcileOwnPeer(ctx, "peer", hooks); err != nil {
		t.Fatalf("reconcile against a peer that removed this device: %v", err)
	}
	if len(accepted) != 1 || !accepted[0].Members[1].Removed {
		t.Errorf("accepted %+v, want the tombstoned catalog once", accepted)
	}
	if _, err := deviceclient.LoadSession(dir, "peer"); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Errorf("own-device profile after the tombstone: %v, want it retired", err)
	}
	if changed != 1 {
		t.Errorf("changed fired %d times, want once for the retired profile", changed)
	}
	if manager.Carrier("peer") != nil {
		t.Error("a retired own-device profile still has a carrier")
	}
}

// TestASessionThePeerEndedRetiresItsOwnDeviceProfile — a peer that revoked
// this device's session refuses the rotation behind every ticket mint. That
// is the same verdict a manifest reads: the profile is gone, the carrier
// with it, the observer is told, and the pass is not a failure to retry.
func TestASessionThePeerEndedRetiresItsOwnDeviceProfile(t *testing.T) {
	manager, dir := newManager(t)
	p := newPeer(t)
	p.revoke()
	seedPeer(t, dir, p, true)
	var ended []SetChange
	manager.SetChanged(func(change SetChange) {
		if change.Action == SetRemoved {
			ended = append(ended, change)
		}
	})
	hooks := OwnDeviceHooks{Snapshot: func() (owndevices.List, error) { return owndevices.List{}, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if failed := manager.ReconcileOwnDevices(ctx, hooks); failed {
		t.Fatal("a session the far side ended was reported as an outage to retry")
	}
	if ids, err := manager.ConnectedOwnDeviceIDs(); err != nil || len(ids) != 0 {
		t.Errorf("own devices after the verdict = %v (%v), want none", ids, err)
	}
	if len(ended) != 1 || ended[0].ID != "peer" || ended[0].Reason != RemovedByComputer {
		t.Errorf("observer told %v, want the one ended pairing with its reason", ended)
	}
	if manager.Carrier("peer") != nil {
		t.Error("a revoked own-device pairing still has a carrier")
	}
}
