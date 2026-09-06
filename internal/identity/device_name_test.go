package identity

import (
	"reflect"
	"testing"
)

func TestDeviceNameUpdateIsBoundToLivePairedSession(t *testing.T) {
	sessions, st, _, owner, _ := newFixture(t)
	phone := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "phone")
	other := mustRedeem(t, sessions, mustMintLink(t, sessions, owner).Token, "other")
	if _, err := sessions.UpdateDeviceName(phone.Tokens.SessionID, "Pixel", "Android"); err == nil {
		t.Fatal("unconfirmed session renamed a device")
	}
	if _, err := sessions.ConfirmPairing(phone.PairingID); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetDevice(phone.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	otherBefore, err := st.GetDevice(other.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := sessions.UpdateDeviceName(phone.Tokens.SessionID, " Pixel ", "Android"); err != nil || !changed {
		t.Fatalf("rename: changed=%v err=%v", changed, err)
	}
	after, err := st.GetDevice(phone.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	before.Label, before.Platform = "Pixel", "Android"
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rename changed identity or access: before=%+v after=%+v", before, after)
	}
	otherAfter, _ := st.GetDevice(other.DeviceID)
	if !reflect.DeepEqual(otherBefore, otherAfter) {
		t.Fatal("rename touched another device")
	}
	if changed, err := sessions.UpdateDeviceName(phone.Tokens.SessionID, "Pixel", "Android"); err != nil || changed {
		t.Fatalf("unchanged name: changed=%v err=%v", changed, err)
	}
	for _, name := range []string{"", "\n", "bad\x00name"} {
		if _, err := sessions.UpdateDeviceName(phone.Tokens.SessionID, name, "Android"); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if _, err := sessions.RevokeDevice(phone.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.UpdateDeviceName(phone.Tokens.SessionID, "Changed", "Android"); err == nil {
		t.Fatal("revoked session renamed a device")
	}
	local, _, err := sessions.EnsureLocalChannelSession(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.UpdateDeviceName(local.ID, "Changed", "macOS"); err == nil {
		t.Fatal("paired-client path renamed the local channel")
	}
}
