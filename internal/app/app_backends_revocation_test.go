package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/owndevices"
)

// observedSetChanges collects what one manager announces on
// backend:set-changed, the way both desktops' observers would.
func observedSetChanges(manager *attachedbackends.Manager) func() []attachedbackends.SetChange {
	var mu sync.Mutex
	var changes []attachedbackends.SetChange
	manager.SetChanged(func(change attachedbackends.SetChange) {
		mu.Lock()
		changes = append(changes, change)
		mu.Unlock()
	})
	return func() []attachedbackends.SetChange {
		mu.Lock()
		defer mu.Unlock()
		return append([]attachedbackends.SetChange(nil), changes...)
	}
}

// revokeDeviceLabelled is the far owner revoking this installation from
// their own settings pane.
func revokeDeviceLabelled(t *testing.T, host ownConnectionHost, label string) {
	t.Helper()
	overview, err := host.app.GetAccessOverview()
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	for _, device := range overview.Devices {
		if device.Label != label {
			continue
		}
		if _, err := host.app.RevokeAccessDevice(device.ID); err != nil {
			t.Fatal(err)
		}
		revoked++
	}
	if revoked != 1 {
		t.Fatalf("the overview holds %d devices labelled %q, want the one that paired", revoked, label)
	}
}

// TestAFarSideRevocationRemovesTheProfileAndSaysWho is the other end of
// TestARevokedDeviceIsRefusedAndTheClientNamesTheRemedy: the same verdict,
// observed from the attached-backends set. The client forgets its profile,
// the manager drops the retired carrier, and the one frame it announces
// carries the reason — which a local removal, the only removal the page
// ever asked for, deliberately does not.
func TestAFarSideRevocationRemovesTheProfileAndSaysWho(t *testing.T) {
	ownConnectionNetwork(t)
	target := ownConnectionBackend(t)
	source, err := attachedbackends.New(t.TempDir(), "Revoked desk", "linux")
	if err != nil {
		t.Fatal(err)
	}
	source.SetNetwork(nil, dialOwnFixture)
	changes := observedSetChanges(source)
	pairOwnConnection(t, target, source, false)
	revokeDeviceLabelled(t, target, "Revoked desk")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var result owndevices.List
	if err := source.CallOwnDevice(ctx, target.id, "ListOwnDevices", &result); !errors.Is(err, deviceclient.ErrSessionEnded) {
		t.Fatalf("a revoked device's call = %v, want ErrSessionEnded", err)
	}
	if rows := source.Attached(); len(rows) != 0 {
		t.Fatalf("the ended pairing is still listed: %+v", rows)
	}
	if source.Carrier(target.id) != nil {
		t.Fatal("the retired carrier is still served")
	}
	want := []attachedbackends.SetChange{{Action: attachedbackends.SetRemoved, ID: target.id, Reason: attachedbackends.RemovedByComputer}}
	if got := changes(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("announced %+v, want %+v", got, want)
	}
	// The verdict is final on this side too: nothing retries it.
	if err := source.CallOwnDevice(ctx, target.id, "ListOwnDevices", &result); err == nil {
		t.Fatal("a forgotten pairing still answered")
	}
	if got := changes(); len(got) != 1 {
		t.Fatalf("the ended pairing was announced again: %+v", got)
	}

	// A removal this installation makes itself names no reason: the page
	// asked for it, so there is nothing to explain.
	local, err := attachedbackends.New(t.TempDir(), "Kept desk", "linux")
	if err != nil {
		t.Fatal(err)
	}
	local.SetNetwork(nil, dialOwnFixture)
	localChanges := observedSetChanges(local)
	pairOwnConnection(t, target, local, false)
	if err := local.Remove(target.id); err != nil {
		t.Fatal(err)
	}
	want = []attachedbackends.SetChange{{Action: attachedbackends.SetRemoved, ID: target.id}}
	if got := localChanges(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("a local removal announced %+v, want %+v", got, want)
	}
}
