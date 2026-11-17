package app

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// reachableDevice pairs a new device with s's computer through one live
// session, the reach a spawn link needs to be written, and returns its id.
func reachableDevice(t *testing.T, s *store.Store) string {
	t.Helper()
	owner, err := s.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.CreateDevice(owner.ID, "Paired computer", "backend-peer", "linux")
	if err != nil {
		t.Fatal(err)
	}
	key := store.SigningKey{ID: uuid.NewString(), Secret: secret.New([]byte("0123456789abcdef")), CreatedAt: 1}
	if err := s.InsertSigningKey(key); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(store.Session{
		ID: uuid.NewString(), UserID: owner.ID, DeviceID: device.ID, BindingClass: "device-bound",
		Scopes: []string{"threads:read"}, SigningKeyID: key.ID, CreatedAt: 1, ExpiresAt: 1 << 50, ActivatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return device.ID
}

// A worker whose paired owner has been silent past the retention floor is
// released by the retention pass, which announces its new sidebar row.
func TestTheRetentionPassAnnouncesAReleasedWorker(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	worker := f.thread(t, "worker")
	withMessage(t, f.app, worker.ID)
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: "remote-spawn", OwnerDeviceID: reachableDevice(t, f.app.store), SourceComputerID: "desk", SourceThreadID: "desk-lead",
		Kind: store.ThreadRequestSpawn, SpawnLink: true, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := f.app.store.SetThreadReceiptTarget("remote-spawn", worker.ID, true); err != nil || !named {
		t.Fatalf("name the remote spawn's thread: %v, %v", named, err)
	}
	if got := f.reload(t, worker.ID).Placement; got != store.PlacementOwned {
		t.Fatalf("a linked worker = %s, want owned", got)
	}
	f.events.reset()
	f.app.expireThreadRequests(time.Now())
	if got := f.reload(t, worker.ID).Placement; got != store.PlacementStandalone {
		t.Fatalf("a worker whose owner went silent = %s, want standalone", got)
	}
	if got := lastFrameFor(f.threadFrames(t), triage.ThreadActionListed, worker.ID); got == nil || got.Placement != store.PlacementStandalone {
		t.Fatalf("the release announced %+v, want the worker listed standalone", got)
	}
}
