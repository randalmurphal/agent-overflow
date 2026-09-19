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

// TestApplyOrganizePatchRefusesTheWholePatchBeforeItWrites keeps the
// planning refusals where they are: a pin on a thread its group already
// pins is refused with nothing written, including the rename beside it.
func TestApplyOrganizePatchRefusesTheWholePatchBeforeItWrites(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	group, err := database.CreateThreadGroup("project", "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := database.SetThreadGroup([]string{thread.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}

	_, err = service.ApplyOrganizePatch(thread.ID, OrganizePatch{
		Title: patchString("Renamed"),
		Pin:   patchString(PinFront),
	})
	if !errors.Is(err, store.ErrThreadGrouped) {
		t.Fatalf("error = %v, want store.ErrThreadGrouped", err)
	}
	after, err := database.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Title == "Renamed" {
		t.Error("the refused patch renamed the thread anyway")
	}
	if after.PinnedAt != nil {
		t.Errorf("the refused patch pinned the grouped row: %+v", after)
	}
}
