package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestUpsertPushTokenKeepsOneRowPerDevice(t *testing.T) {
	s, _, device := seedOwnerDevice(t)

	if err := s.UpsertPushToken(device.ID, "android", "token-one", 100); err != nil {
		t.Fatalf("UpsertPushToken: %v", err)
	}
	if err := s.UpsertPushToken(device.ID, "android", "token-two", 200); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	live, err := s.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live tokens = %d, want 1: a device holds one registration, and a second row is one duplicate notification per stale token", len(live))
	}
	if live[0].Token != "token-two" || live[0].UpdatedAt != 200 {
		t.Fatalf("live[0] = %+v, want the newest registration", live[0])
	}
	if live[0].DeviceClass != device.Class {
		t.Fatalf("DeviceClass = %q, want %q: the fan-out reads that device's own preferences and needs its class", live[0].DeviceClass, device.Class)
	}
}

func TestUpsertPushTokenRefusesAnIncompleteRegistration(t *testing.T) {
	s, _, device := seedOwnerDevice(t)
	if err := s.UpsertPushToken("", "android", "token", 1); err == nil {
		t.Error("a registration naming no device was accepted")
	}
	if err := s.UpsertPushToken(device.ID, "android", "", 1); err == nil {
		t.Error("an empty registration token was accepted")
	}
}

// The join is the safety property, not a convenience: revoking a device is
// what makes its phone stop being woken, and reading the tokens without it
// would keep a revoked phone on the list.
func TestARevokedDeviceIsNeverSentTo(t *testing.T) {
	s, owner, first := seedOwnerDevice(t)
	second, err := s.CreateDevice(owner.ID, "Phone", "phone", "android")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	for _, device := range []Device{first, second} {
		if err := s.UpsertPushToken(device.ID, "android", "token-"+device.ID, 100); err != nil {
			t.Fatalf("UpsertPushToken: %v", err)
		}
	}

	if _, err := s.RevokeDevice(second.ID, 500); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	live, err := s.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 1 || live[0].DeviceID != first.ID {
		t.Fatalf("live tokens = %+v, want only the unrevoked device", live)
	}

	// Restoring puts it back, because the row was never deleted — the
	// revocation is a filter and not a destruction.
	if _, err := s.RestoreDevice(second.ID); err != nil {
		t.Fatalf("RestoreDevice: %v", err)
	}
	live, err = s.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens after restore: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("live tokens after restore = %d, want 2", len(live))
	}
}

// Deleting a device takes its registration with it, so a forgotten device
// leaves nothing behind that could be sent to.
func TestDeletingADeviceCascadesToItsPushToken(t *testing.T) {
	s, _, device := seedOwnerDevice(t)
	if err := s.UpsertPushToken(device.ID, "android", "token", 100); err != nil {
		t.Fatalf("UpsertPushToken: %v", err)
	}
	// Deletion only reaches a revoked device, so this is the tail of the
	// revoke-then-forget path rather than a second door.
	if _, err := s.RevokeDevice(device.ID, 500); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	deleted, err := s.DeleteDevice(device.ID)
	if err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if !deleted {
		t.Fatal("DeleteDevice removed nothing")
	}

	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM push_tokens WHERE device_id = ?`, device.ID).Scan(&rows); err != nil {
		t.Fatalf("count push tokens: %v", err)
	}
	if rows != 0 {
		t.Fatalf("push_tokens rows = %d, want 0: the cascade must take the registration with the device", rows)
	}
	live, err := s.LivePushTokens()
	if err != nil {
		t.Fatalf("LivePushTokens: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live tokens = %+v, want none after the device was forgotten", live)
	}
}

func TestDeletePushTokenReportsWhetherOneWasThere(t *testing.T) {
	s, _, device := seedOwnerDevice(t)
	removed, err := s.DeletePushToken(device.ID)
	if err != nil {
		t.Fatalf("DeletePushToken: %v", err)
	}
	if removed {
		t.Error("DeletePushToken reported a removal for a device that never registered")
	}
	if err := s.UpsertPushToken(device.ID, "android", "token", 100); err != nil {
		t.Fatalf("UpsertPushToken: %v", err)
	}
	removed, err = s.DeletePushToken(device.ID)
	if err != nil {
		t.Fatalf("DeletePushToken: %v", err)
	}
	if !removed {
		t.Error("DeletePushToken did not report removing a registration that was there")
	}
}

func TestThePushSenderIsOneRowThatRoundTrips(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetPushSender(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPushSender on a fresh store = %v, want sql.ErrNoRows: sending nothing is the resting state", err)
	}

	first := PushSenderCredential{
		ProjectID: "p-1", ClientEmail: "a@p-1.iam", CredentialJSON: `{"type":"service_account"}`,
		UpdatedAt: 100,
	}
	if err := s.SetPushSender(first); err != nil {
		t.Fatalf("SetPushSender: %v", err)
	}
	got, err := s.GetPushSender()
	if err != nil {
		t.Fatalf("GetPushSender: %v", err)
	}
	if got != first {
		t.Fatalf("GetPushSender = %+v, want %+v", got, first)
	}

	second := PushSenderCredential{
		ProjectID: "p-2", ClientEmail: "b@p-2.iam", CredentialJSON: `{"type":"service_account","x":1}`,
		UpdatedAt: 200,
	}
	if err := s.SetPushSender(second); err != nil {
		t.Fatalf("replace SetPushSender: %v", err)
	}
	got, err = s.GetPushSender()
	if err != nil {
		t.Fatalf("GetPushSender after replace: %v", err)
	}
	if got != second {
		t.Fatalf("GetPushSender = %+v, want the replacement %+v: one backend sends as one project", got, second)
	}

	cleared, err := s.ClearPushSender()
	if err != nil {
		t.Fatalf("ClearPushSender: %v", err)
	}
	if !cleared {
		t.Error("ClearPushSender did not report clearing a credential that was there")
	}
	if _, err := s.GetPushSender(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPushSender after clear = %v, want sql.ErrNoRows", err)
	}
	cleared, err = s.ClearPushSender()
	if err != nil {
		t.Fatalf("second ClearPushSender: %v", err)
	}
	if cleared {
		t.Error("ClearPushSender reported clearing nothing as a removal")
	}
}

func TestSetPushSenderRefusesAnIncompleteCredential(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetPushSender(PushSenderCredential{CredentialJSON: "{}"}); err == nil {
		t.Error("a sender credential naming no project was accepted")
	}
	if err := s.SetPushSender(PushSenderCredential{ProjectID: "p"}); err == nil {
		t.Error("a sender credential with no key document was accepted")
	}
}
