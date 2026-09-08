package app

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/network"
	"agent-overflow/internal/owndevices"
	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

type ownConnectionHost struct {
	*pairedBackend
	id      string
	profile string
}

func ownConnectionBackend(t *testing.T) ownConnectionHost {
	t.Helper()
	backend := newPairedBackend(t)
	backend.app.storeIdentity.Store(&store.Identity{BackendID: uuid.NewString(), ReplicaGeneration: uuid.NewString()})
	profile := t.TempDir()
	manager, err := attachedbackends.New(profile, "Own device", "linux")
	if err != nil {
		t.Fatal(err)
	}
	SetAttachedBackends(backend.app, manager)
	manager.SetNetwork(func() string { id, _ := BackendIdentity(backend.app); return id }, dialOwnFixture)
	id, _ := BackendIdentity(backend.app)
	return ownConnectionHost{pairedBackend: backend, id: id, profile: profile}
}

func pairOwnConnection(t *testing.T, target ownConnectionHost, source *attachedbackends.Manager, personal bool) {
	t.Helper()
	purpose := ""
	if personal {
		if err := target.app.enableOwnDevices(); err != nil {
			t.Fatal(err)
		}
		if err := target.app.ensureOwnDeviceHosting(); err != nil {
			t.Fatal(err)
		}
		purpose = "own-device"
	}
	invite, err := target.app.mintDevicePairingPurpose(string(identity.DeviceDesktop), "full", "", purpose)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = source.Add(ctx, invite.URL); err != nil {
		t.Fatal(err)
	}
	if err = target.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = source.Await(ctx, target.id); err != nil {
		t.Fatal(err)
	}
}

func reconcileOwnConnection(t *testing.T, host ownConnectionHost) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host.app.backends.ReconcileOwnDevices(ctx, host.app.ownDeviceHooks())
	rows, err := host.app.backends.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.OwnDeviceSyncError != "" {
			t.Fatalf("%s: %s", row.BackendID, row.OwnDeviceSyncError)
		}
	}
}

func TestOwnDeviceConnectionsJoinThreeHostsAndControllerWithoutPermanentHub(t *testing.T) {
	ownConnectionNetwork(t)
	a, b, c := ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	pairOwnConnection(t, b, c.app.backends, true)
	for range 3 {
		for _, host := range []ownConnectionHost{a, c, b} {
			reconcileOwnConnection(t, host)
		}
	}
	for _, host := range []ownConnectionHost{a, b, c} {
		profiles, err := host.app.backends.ConnectedOwnDeviceIDs()
		if err != nil || len(profiles) != 2 {
			t.Fatalf("host %s connections=%v: %v", host.id, profiles, err)
		}
		access, err := host.app.backends.AgentAccess()
		if err != nil || len(access) != 0 {
			t.Fatalf("membership enabled agent commands: %v %v", access, err)
		}
	}

	controller, err := attachedbackends.New(t.TempDir(), "Screen only", "linux")
	if err != nil {
		t.Fatal(err)
	}
	controller.SetNetwork(nil, dialOwnFixture)
	pairOwnConnection(t, a, controller, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hooks := controller.FrontendOwnDeviceHooks(nil)
	controller.ReconcileOwnDevices(ctx, hooks)
	if profiles, err := controller.ConnectedOwnDeviceIDs(); err != nil || len(profiles) != 3 {
		t.Fatalf("controller did not learn all hosts: %v %v", profiles, err)
	}
	// The original intermediate host can disappear. Both another execution
	// host and a frontend-only screen retain independent credentials to C.
	if err := b.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, manager := range []*attachedbackends.Manager{a.app.backends, controller} {
		var result owndevices.List
		if err := manager.CallOwnDevice(ctx, c.id, "ListOwnDevices", &result); err != nil || !result.Enabled {
			t.Fatalf("direct path depended on stopped hub: enabled=%v %v", result.Enabled, err)
		}
	}
}

func TestOwnDeviceConnectionsDoNotEnrollOrdinaryPairings(t *testing.T) {
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, false)
	reconcileOwnConnection(t, a)
	if rows := b.app.backends.Attached(); len(rows) != 0 {
		t.Fatalf("ordinary pairing created reverse grant: %+v", rows)
	}
	if snapshot, err := OwnDeviceSnapshot(a.app); err != nil || snapshot.Enabled {
		t.Fatalf("ordinary full access became own membership: %+v %v", snapshot, err)
	}
}

func TestOwnDeviceLocalRemovalSurvivesReconciliationAndRestart(t *testing.T) {
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	reconcileOwnConnection(t, a)
	if err := b.app.backends.Remove(a.id); err != nil {
		t.Fatal(err)
	}
	manager, err := attachedbackends.New(b.profile, "Restarted", "linux")
	if err != nil {
		t.Fatal(err)
	}
	SetAttachedBackends(b.app, manager)
	manager.SetNetwork(func() string { return b.id }, dialOwnFixture)
	reconcileOwnConnection(t, a)
	if _, err := deviceclient.LoadSession(b.profile, a.id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("reconciliation restored locally removed computer: %v", err)
	}
}

func TestOwnDeviceConnectionsRetryOfflineMemberUpgradeLegacyAndPropagateRemoval(t *testing.T) {
	ownConnectionNetwork(t)
	a, b, c := ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t)
	var offline atomic.Bool
	_, aPort, _ := net.SplitHostPort(a.srv.Addr())
	for _, host := range []ownConnectionHost{b, c} {
		host.app.backends.SetNetwork(func() string { return host.id }, func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(address)
			if offline.Load() && port == aPort {
				return nil, errors.New("test computer is offline")
			}
			return dialOwnFixture(ctx, network, address)
		})
	}
	// A already has an ordinary independent grant to C. Joining the approved
	// group must replace that profile with a group session, not stay one-way.
	pairOwnConnection(t, c, a.app.backends, false)
	pairOwnConnection(t, b, a.app.backends, true)
	reconcileOwnConnection(t, a)
	legacy, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil || legacy.OwnDevice {
		t.Fatalf("ordinary profile: %+v %v", legacy, err)
	}
	offline.Store(true)
	pairOwnConnection(t, b, c.app.backends, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.app.backends.ReconcileOwnDevices(ctx, c.app.ownDeviceHooks())
	rows, err := c.app.backends.List()
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].OwnDeviceSyncError, a.id) || !strings.Contains(rows[0].OwnDeviceSyncError, "test computer is offline") {
		t.Fatalf("offline introduction was not reported with its target and cause: %+v", rows)
	}
	if _, err := deviceclient.LoadSession(c.profile, a.id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("offline target was spuriously paired: %v", err)
	}
	if rows := b.app.backends.Attached(); len(rows) != 2 {
		t.Fatalf("offline member prevented reachable reciprocal connection: %+v", rows)
	}
	offline.Store(false)
	reconcileOwnConnection(t, c)
	reconcileOwnConnection(t, a)
	upgraded, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil || !upgraded.OwnDevice || upgraded.SessionID == legacy.SessionID {
		t.Fatalf("approved group did not replace ordinary profile: %+v %v", upgraded, err)
	}

	// B removes C while C is not reconciling. A receives the removal through
	// B and drops C's group profile; stale membership cannot recreate it.
	stale, err := OwnDeviceSnapshot(c.app)
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.app.backends.OwnIdentity()
	if err != nil {
		t.Fatal(err)
	}
	member, err := b.app.store.OwnDevice(key)
	if err != nil {
		t.Fatal(err)
	}
	member.Generation++
	member.Removed = true
	if _, err := MergeOwnDevices(b.app, []owndevices.Member{member}); err != nil {
		t.Fatal(err)
	}
	reconcileOwnConnection(t, b)
	reconcileOwnConnection(t, a)
	if _, err := deviceclient.LoadSession(a.profile, c.id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("removed member retained profile: %v", err)
	}
	if _, err := MergeOwnDevices(a.app, stale.Members); err != nil {
		t.Fatal(err)
	}
	reconcileOwnConnection(t, a)
	if _, err := deviceclient.LoadSession(a.profile, c.id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("stale catalog restored removed member: %v", err)
	}
	var snapshot owndevices.List
	if err := c.app.backends.CallOwnDevice(ctx, a.id, "ListOwnDevices", &snapshot); err == nil {
		t.Fatal("removed device retained authenticated membership access")
	}
}

// Exercise actual LAN advertisement and listener rebinds without depending on
// the test machine's NICs. Only the dialer's address translation is injected;
// TLS pins, proofs, scope checks, sessions and RPC dispatch remain production.
func ownConnectionNetwork(t *testing.T) {
	t.Helper()
	interfaces, addresses := network.Interfaces, network.InterfaceAddrs
	t.Cleanup(func() { network.Interfaces, network.InterfaceAddrs = interfaces, addresses })
	network.Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "test-lan", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	network.InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.50.5"), Mask: net.CIDRMask(24, 32)}}, nil
	}
}
func dialOwnFixture(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err == nil && host == "192.168.50.5" {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func TestOwnDeviceConnectionsMergePreviouslySeparateGroups(t *testing.T) {
	ownConnectionNetwork(t)
	a, b, c, d := ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	pairOwnConnection(t, d, c.app.backends, true)
	reconcileOwnConnection(t, a)
	reconcileOwnConnection(t, c)
	pairOwnConnection(t, c, a.app.backends, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for range 3 {
		for _, host := range []ownConnectionHost{a, b, c, d} {
			// A sponsor can learn a member before its own direct connection is
			// ready. Pending introductions must converge and clear their errors.
			host.app.backends.ReconcileOwnDevices(ctx, host.app.ownDeviceHooks())
		}
	}
	for _, host := range []ownConnectionHost{a, b, c, d} {
		reconcileOwnConnection(t, host)
		profiles, err := host.app.backends.ConnectedOwnDeviceIDs()
		if err != nil || len(profiles) != 3 {
			t.Fatalf("merged group missing direct peers: %s %v %v", host.id, profiles, err)
		}
	}
}

func TestOwnDeviceConnectionsReconcileWithoutAWindowAndStopWithHost(t *testing.T) {
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); a.app.backends.WaitOwnDevices(); b.app.backends.WaitOwnDevices() })
	for _, host := range []ownConnectionHost{a, b} {
		host.app.backends.StartOwnDevices(ctx, host.app.ownDeviceHooks())
	}
	for len(b.app.backends.Attached()) != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("headless hosts did not establish reciprocal connection")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	a.app.backends.WaitOwnDevices()
	b.app.backends.WaitOwnDevices()
}
