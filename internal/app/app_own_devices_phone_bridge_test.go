package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/owndevices"
	"github.com/google/uuid"
)

func TestOwnDevicePhoneBridgesTwoPreviouslySeparateHosts(t *testing.T) {
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	// Observe the production emit funnel on both receiving hosts. An outgoing
	// profile that exists only on disk leaves each already-open desktop blind.
	var changes [2]atomic.Int32
	for i, host := range []ownConnectionHost{a, b} {
		host.app.testEmitHook = func(name string, data any) {
			if name == "backend:set-changed" && data.(BackendSetChange).Action == "membership" {
				changes[i].Add(1)
			}
		}
	}
	phone, err := attachedbackends.New(t.TempDir(), "Phone", "android")
	if err != nil {
		t.Fatal(err)
	}
	phone.SetNetwork(nil, dialOwnFixture)
	pairOwnConnection(t, a, phone, true)
	pairOwnConnection(t, b, phone, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first, second owndevices.List
	if err := phone.CallOwnDevice(ctx, a.id, "ListOwnDevices", &first); err != nil {
		t.Fatal(err)
	}
	if err := phone.CallOwnDevice(ctx, b.id, "ListOwnDevices", &second); err != nil {
		t.Fatal(err)
	}
	// Public catalogs travel through the already admitted phone; neither host
	// has an outgoing credential or a permanent coordinator at this point.
	if err := phone.CallOwnDevice(ctx, a.id, "SyncOwnDevices", &first, second.Members); err != nil {
		t.Fatal(err)
	}
	if err := phone.CallOwnDevice(ctx, b.id, "SyncOwnDevices", &second, first.Members); err != nil {
		t.Fatal(err)
	}
	if len(a.app.backends.Attached()) != 0 || len(b.app.backends.Attached()) != 0 {
		t.Fatal("catalog merge itself created a connection")
	}
	var invite PairingInvite
	if err := phone.CallOwnDevice(ctx, b.id, "MintOwnDeviceIntroduction", &invite, first.SelfKeyThumbprint); err != nil {
		t.Fatal(err)
	}
	payload, err := deviceclient.DecodeLink(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A member may relay established trust, never replace it with an unrelated
	// destination or the same destination under a different certificate.
	for _, mutation := range []struct {
		name   string
		change func(*deviceclient.Link)
	}{
		{"unknown host", func(p *deviceclient.Link) { p.BackendID = uuid.NewString() }},
		{"unknown address", func(p *deviceclient.Link) { p.Endpoint = "https://unrelated.example" }},
		{"wrong certificate", func(p *deviceclient.Link) { p.CertFingerprint = "sha256:" + strings.Repeat("a", 64) }},
		{"ordinary purpose", func(p *deviceclient.Link) { p.Purpose = "own-device" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			changed := payload
			mutation.change(&changed)
			raw, err := (identity.PairingPayload{Version: changed.Version, Purpose: changed.Purpose, BackendID: changed.BackendID, Endpoint: changed.Endpoint, Token: changed.Token, CertFingerprint: changed.CertFingerprint}).Encode()
			if err != nil {
				t.Fatal(err)
			}
			if err := phone.CallOwnDevice(ctx, a.id, "AcceptOwnDeviceIntroduction", nil, raw); err == nil {
				t.Fatal("untrusted introduction accepted")
			}
			if len(a.app.backends.Attached()) != 0 {
				t.Fatal("refused introduction changed connections")
			}
		})
	}
	for i := range changes {
		if changes[i].Load() != 0 {
			t.Fatal("refused introduction published a profile change")
		}
	}
	if err := phone.CallOwnDevice(ctx, a.id, "AcceptOwnDeviceIntroduction", nil, invite.URL); err != nil {
		t.Fatal(err)
	}
	if err := phone.CallOwnDevice(ctx, a.id, "MintOwnDeviceIntroduction", &invite, second.SelfKeyThumbprint); err != nil {
		t.Fatal(err)
	}
	if err := phone.CallOwnDevice(ctx, b.id, "AcceptOwnDeviceIntroduction", nil, invite.URL); err != nil {
		t.Fatal(err)
	}
	for i := range changes {
		if got := changes[i].Load(); got != 1 {
			t.Fatalf("host %d published %d profile changes, want 1", i, got)
		}
	}
	// A repeated introduction is idempotent and must not churn subscriptions.
	if err := phone.CallOwnDevice(ctx, b.id, "AcceptOwnDeviceIntroduction", nil, invite.URL); err != nil {
		t.Fatal(err)
	}
	if changes[1].Load() != 1 {
		t.Fatal("existing connection was republished")
	}
	for _, edge := range []struct{ from, to ownConnectionHost }{{a, b}, {b, a}} {
		var list owndevices.List
		if err := edge.from.app.backends.CallOwnDevice(ctx, edge.to.id, "ListOwnDevices", &list); err != nil || !list.Enabled {
			t.Fatalf("direct connection unavailable: %+v %v", list, err)
		}
	}
}

func TestOwnIntroductionSelectsReachableRouteBeforeRedemption(t *testing.T) {
	ownConnectionNetwork(t)
	a, b := ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, b, a.app.backends, true)
	// Initial confirmed carrier admits A and establishes both public identities.
	var catalog owndevices.List
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	available := computerroute.Route{Endpoint: link.Endpoint, CertFingerprint: link.CertFingerprint}
	// A stored LAN address is unavailable from the recipient's network. Its
	// other trusted route reaches the same real server and pinned certificate.
	unavailable := available
	unavailable.Endpoint = "https://127.0.0.1:1"
	encoded, err := (identity.PairingPayload{Version: link.Version, Purpose: link.Purpose, BackendID: link.BackendID, Endpoint: unavailable.Endpoint, Token: link.Token, CertFingerprint: link.CertFingerprint}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	added, err := b.app.backends.AcceptOwnDeviceIntroduction(ctx, encoded, []computerroute.Route{unavailable, available})
	if err != nil || !added {
		t.Fatalf("alternate introduction failed: added=%v %v", added, err)
	}
	var confirmed owndevices.List
	if err := b.app.backends.CallOwnDevice(ctx, a.id, "ListOwnDevices", &confirmed); err != nil || !confirmed.Enabled {
		t.Fatalf("selected route did not create independent access: %+v %v", confirmed, err)
	}
	saved, err := deviceclient.LoadSession(b.profile, a.id)
	if err != nil || saved.Endpoint != available.Endpoint {
		t.Fatalf("paired profile retained unreachable initial route: %+v %v", saved, err)
	}
}
