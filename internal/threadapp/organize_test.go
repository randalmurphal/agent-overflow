package threadapp

import (
	"testing"

	"agent-overflow/internal/store"
)

func patchString(value string) *string { return &value }

func patchBool(value bool) *bool { return &value }

// TestApplyOrganizePatchLandsEveryFieldItPlanned is the whole patch through
// the service: a rename, a pin and an archive in one call.
func TestApplyOrganizePatchLandsEveryFieldItPlanned(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := service.ApplyOrganizePatch(thread.ID, OrganizePatch{
		Title:    patchString("  Release work  "),
		Pin:      patchString(PinBack),
		Archived: patchBool(true),
	})
	if err != nil {
		t.Fatalf("ApplyOrganizePatch: %v", err)
	}
	if !result.Changed || !result.ArchivedChanged {
		t.Errorf("changed = %v, archivedChanged = %v, want both true", result.Changed, result.ArchivedChanged)
	}
	if result.Thread.Title != "Release work" {
		t.Errorf("title = %q, want the trimmed title", result.Thread.Title)
	}
	if result.Thread.PinGroup == nil || *result.Thread.PinGroup != store.PinGroupBack {
		t.Errorf("pinGroup = %v, want the back burner", result.Thread.PinGroup)
	}
	if !result.Thread.Archived {
		t.Error("the patch did not archive the thread")
	}

	stored, err := database.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Title != "Release work" || !stored.Archived || stored.PinnedAt == nil {
		t.Errorf("committed row = %+v, want it renamed, pinned and archived", stored)
	}

	// A patch that restates what the row already holds moves nothing, so
	// root emits nothing for it.
	again, err := service.ApplyOrganizePatch(thread.ID, OrganizePatch{
		Title:    patchString("Release work"),
		Pin:      patchString(PinBack),
		Archived: patchBool(true),
	})
	if err != nil {
		t.Fatalf("ApplyOrganizePatch (no-op): %v", err)
	}
	if again.Changed || again.ArchivedChanged {
		t.Errorf("changed = %v, archivedChanged = %v, want both false", again.Changed, again.ArchivedChanged)
	}
	if again.Thread.ID != thread.ID {
		t.Errorf("a no-op patch returned %+v, want the current row", again.Thread)
	}
}

// TestApplyOrganizePatchPinsGroupedThreads: a grouped thread takes a pin,
// and a patch that sets a group and a pin lands the pin inside the new
// group, including the tier the row already held before the move cleared
// it. Leaving a group clears the pin.
func TestApplyOrganizePatchPinsGroupedThreads(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	grouped, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	service.deps.NewID = func() string { return "moving" }
	moving, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	group, err := database.CreateThreadGroup("project", "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := database.SetThreadGroup([]string{grouped.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}

	result, err := service.ApplyOrganizePatch(grouped.ID, OrganizePatch{
		Title: patchString("Renamed"),
		Pin:   patchString(PinBack),
	})
	if err != nil {
		t.Fatalf("pin a grouped thread: %v", err)
	}
	if result.Thread.Title != "Renamed" || result.Thread.GroupID != group.ID || currentPinTier(result.Thread) != PinBack {
		t.Errorf("grouped thread = %+v, want renamed and on the back burner inside its group", result.Thread)
	}

	// Pinned front before the move; the move clears it, so the same tier
	// in the patch is a write, not a no-op.
	if _, _, err := database.PinThread(moving.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	result, err = service.ApplyOrganizePatch(moving.ID, OrganizePatch{
		Group: patchString("Release"),
		Pin:   patchString(PinFront),
	})
	if err != nil {
		t.Fatalf("group and pin: %v", err)
	}
	if result.Thread.GroupID != group.ID || currentPinTier(result.Thread) != PinFront {
		t.Errorf("moved thread = %+v, want it pinned front inside the group", result.Thread)
	}

	result, err = service.ApplyOrganizePatch(moving.ID, OrganizePatch{Group: patchString("")})
	if err != nil {
		t.Fatalf("ungroup: %v", err)
	}
	if result.Thread.GroupID != "" || currentPinTier(result.Thread) != PinNone || !result.Changed {
		t.Errorf("ungrouped thread = %+v (changed %v), want it out of the group and unpinned", result.Thread, result.Changed)
	}
}

// spawnedChild makes a thread spawned by another one on this computer and
// nested under it, the only kind of thread a placement patch moves.
func spawnedChild(t *testing.T, service *Service, database *store.Store, id string) store.Thread {
	t.Helper()
	service.deps.NewID = func() string { return id + "-spawner" }
	spawner, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create spawner: %v", err)
	}
	service.deps.NewID = func() string { return id }
	child, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	token := id + "-token"
	if _, err := database.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: spawner.ID, Kind: store.ThreadRequestSpawn, State: store.ThreadRequestUnconfirmed,
	}, store.ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: store.ThreadRequestSpawn, SourceThreadID: spawner.ID}); err != nil {
		t.Fatalf("AcceptLocalThreadRequest: %v", err)
	}
	if err := database.NameLocalThreadRequestTarget(token, child.ID, true); err != nil {
		t.Fatalf("NameLocalThreadRequestTarget: %v", err)
	}
	child, err = database.GetThread(child.ID)
	if err != nil || child.Placement != store.PlacementNested {
		t.Fatalf("spawned child = %+v, %v; want it nested", child, err)
	}
	return child
}

// An ungroup with placement nested takes a grouped child out of its group
// and back under its spawner in one patch: the store's ungroup leaves it
// standalone, and the plan's placement write nests it.
func TestApplyOrganizePatchUngroupsAndNestsInOnePatch(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	child := spawnedChild(t, service, database, "child")
	group, err := database.CreateThreadGroup("project", "Sweep")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := database.SetThreadGroup([]string{child.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	result, err := service.ApplyOrganizePatch(child.ID, OrganizePatch{
		Group:     patchString(""),
		Placement: patchString(store.PlacementNested),
	})
	if err != nil {
		t.Fatalf("ApplyOrganizePatch: %v", err)
	}
	if result.Thread.GroupID != "" || result.Thread.Placement != store.PlacementNested {
		t.Errorf("child = %s in group %q, want nested with no group", result.Thread.Placement, result.Thread.GroupID)
	}
}

// A placement change clears the pin, so a pin named in the same patch is
// compared with no pin and written after the move: the child keeps the
// tier it held, now in its new place.
func TestApplyOrganizePatchKeepsAPinNamedWithAPlacementChange(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	child := spawnedChild(t, service, database, "child")
	if _, _, err := database.PinThread(child.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	result, err := service.ApplyOrganizePatch(child.ID, OrganizePatch{
		Placement: patchString(store.PlacementStandalone),
		Pin:       patchString(PinFront),
	})
	if err != nil {
		t.Fatalf("ApplyOrganizePatch: %v", err)
	}
	if result.Thread.Placement != store.PlacementStandalone || currentPinTier(result.Thread) != PinFront {
		t.Errorf("child = %s pinned %q, want standalone and pinned front", result.Thread.Placement, currentPinTier(result.Thread))
	}
}
