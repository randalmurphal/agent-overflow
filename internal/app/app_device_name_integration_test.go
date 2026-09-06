package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
)

func TestGoCarrierPublishesDeviceNameThroughAuthenticatedAppRPC(t *testing.T) {
	backend := newPairedBackend(t)
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))
	profile := t.TempDir()
	name := appidentity.NewDeviceName(t.TempDir())
	if err := name.Set("Original"); err != nil {
		t.Fatal(err)
	}
	manager, err := attachedbackends.New(profile, "Original", "linux")
	if err != nil {
		t.Fatal(err)
	}
	manager.SetLabelGetter(name.Get)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.Add(ctx, invite.URL); err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Await(ctx, link.BackendID); err != nil {
		t.Fatal(err)
	}
	original, err := deviceclient.LoadSession(profile, link.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.app.store.GetSession(original.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := name.Set("Renamed workhorse"); err != nil {
		t.Fatal(err)
	}
	manager.SyncDeviceName()
	for {
		device, err := backend.app.store.GetDevice(session.DeviceID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := deviceclient.LoadSession(profile, link.BackendID)
		if err != nil {
			t.Fatal(err)
		}
		if device.Label == "Renamed workhorse" && current.Label == device.Label {
			if current.SessionID != original.SessionID || current.Credential != original.Credential {
				t.Fatal("renaming replaced pairing")
			}
			if device.Platform != "linux" {
				t.Fatalf("platform=%q", device.Platform)
			}
			break
		}
		select {
		case <-ctx.Done():
			rows, _ := manager.List()
			t.Fatalf("name sync timed out: %+v", rows)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
