package browser

import (
	"errors"
	"testing"
)

// untiedPane is the tie of a test that detaches its mounts itself.
var untiedPane PaneTie

// TestAttachPaneTiesTheMountAsItPublishesIt: a refused tie publishes no
// mount, and the untie the tie answered runs once when the mount is
// detached. The mount is tied before anything can detach it, so there is
// no moment a published mount has no untie.
func TestAttachPaneTiesTheMountAsItPublishesIt(t *testing.T) {
	m, access, _ := newTabsManager(t, addr("a"))
	refused := errors.New("the owner is gone")
	if _, err := m.AttachPane(access.Caller(), func(MountID) (func(), error) { return nil, refused }); !errors.Is(err, refused) {
		t.Fatalf("attach with a refused tie = %v, want %v", err, refused)
	}
	if n := m.MountedPanesForTesting(); n != 0 {
		t.Fatalf("a refused tie left %d mounts", n)
	}

	var tied MountID
	untied := 0
	mount, err := m.AttachPane(access.Caller(), func(id MountID) (func(), error) {
		tied = id
		return func() { untied++ }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tied != MountID(mount.ID) {
		t.Fatalf("the tie was given %q for mount %q", tied, mount.ID)
	}
	m.DetachPane(MountID(mount.ID))
	m.DetachPane(MountID(mount.ID))
	if untied != 1 {
		t.Fatalf("the mount's untie ran %d times, want once", untied)
	}
}
