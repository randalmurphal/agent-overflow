package attachedbackends

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// A peer that never answers ListOwnDevices holds a reconcile worker for one
// peer's bound: ownPeerTimeout, or the pairing bound a test set.
func TestOnePeersReconcileEndsAtItsBound(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	manager, dir := newManager(t)
	if got := manager.ownPeerBound(); got != ownPeerTimeout || ownPeerTimeout != 12*time.Second {
		t.Fatalf("a manager's peer bound = %v (ownPeerTimeout %v), want 12s", got, ownPeerTimeout)
	}
	arrived := make(chan struct{})
	server := heldOwnPeer(t, arrived)
	seed(t, dir, deviceclient.Session{
		OwnDevice: true, BackendID: "peer", Endpoint: server.URL, SessionID: "session", Credential: "credential",
		ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(),
	})
	const bound = 100 * time.Millisecond
	manager.SetPairingBound(bound)
	hooks := OwnDeviceHooks{Snapshot: func() (owndevices.List, error) { return owndevices.List{}, nil }}

	started := time.Now()
	done := make(chan bool, 1)
	go func() { done <- manager.ReconcileOwnDevices(context.Background(), hooks) }()
	var failed bool
	select {
	case failed = <-done:
	case <-time.After(testutil.WaitBudget(t)):
		t.Fatal("the pass against a peer that never answers did not end")
	}
	elapsed := time.Since(started)
	select {
	case <-arrived:
	default:
		t.Fatal("the pass ended before it asked the peer for its catalog")
	}
	if !failed {
		t.Fatal("a pass whose peer never answered reported no failure")
	}
	if elapsed >= ownPeerTimeout {
		t.Fatalf("the pass ended after %v, at ownPeerTimeout, not at the %v bound it was given", elapsed, bound)
	}
	rows, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].OwnDeviceSyncError, "deadline exceeded") {
		t.Fatalf("rows = %+v, want the peer's row to carry the bound's error", rows)
	}
}

// heldOwnPeer serves an own-device peer that mints tickets and says hello,
// then holds the first ListOwnDevices until the test ends, closing arrived
// when it comes.
func heldOwnPeer(t *testing.T, arrived chan struct{}) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/ticket" {
			_, _ = io.WriteString(w, `{"ticket":"ticket"}`)
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
		var frame transport.ClientFrame
		if wsjson.Read(ctx, conn, &frame) != nil || frame.Method != "ListOwnDevices" {
			return
		}
		once.Do(func() { close(arrived) })
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

// An address pairing's nearby exchange takes the manager's pairing bound.
func TestAnAddressPairingEndsAtThePairingBound(t *testing.T) {
	remotetest.Require(t)
	t.Parallel()
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"op":"info"`) {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { close(arrived) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	manager, _ := newManager(t)
	const bound = 100 * time.Millisecond
	manager.SetPairingBound(bound)

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Add(context.Background(), strings.TrimPrefix(server.URL, "https://"))
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(testutil.WaitBudget(t)):
		t.Fatal("an address pairing with a computer that never answers did not end")
	}
	elapsed := time.Since(started)
	select {
	case <-arrived:
	default:
		t.Fatalf("the pairing ended before its nearby exchange was sent: %v", err)
	}
	if err == nil {
		t.Fatal("an address pairing with a computer that never answered succeeded")
	}
	// pairbootstrap waits 10 s for one answer by default.
	if elapsed >= 5*time.Second {
		t.Fatalf("the pairing ended after %v, not at the %v bound it was given: %v", elapsed, bound, err)
	}
}
