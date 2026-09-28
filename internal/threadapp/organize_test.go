package threadapp

import (
	"errors"
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

// TestApplyOrganizePatchLeavesTheThreadUntouchedWhenAWriteFails is the
// spec's rule through the service, on a refusal the plan cannot foresee: a
// discussion child is refused by the group move, and the rename planned
// beside it must not land on its own.
func TestApplyOrganizePatchLeavesTheThreadUntouchedWhenAWriteFails(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	parent, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	service.deps.NewID = func() string { return "child" }
	child, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	child.ParentThreadID = parent.ID
	if err := database.UpdateThread(child); err != nil {
		t.Fatalf("UpdateThread child parent: %v", err)
	}
	before, err := database.GetThread(child.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	_, err = service.ApplyOrganizePatch(child.ID, OrganizePatch{
		Title:    patchString("Renamed"),
		Group:    patchString("Release"),
		Archived: patchBool(true),
	})
	if !errors.Is(err, store.ErrThreadNotRoot) {
		t.Fatalf("error = %v, want store.ErrThreadNotRoot", err)
	}

	after, err := database.GetThread(child.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Title != before.Title {
		t.Errorf("title = %q, want the refused patch to leave %q", after.Title, before.Title)
	}
	if after.Archived != before.Archived {
		t.Errorf("archived = %v, want %v", after.Archived, before.Archived)
	}
	if after.GroupID != "" {
		t.Errorf("groupId = %q, want the refused move to leave it ungrouped", after.GroupID)
	}
	groups, err := database.ListThreadGroups()
	if err != nil {
		t.Fatalf("ListThreadGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %+v, want the refused patch to create none", groups)
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
