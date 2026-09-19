package store

import (
	"database/sql"
	"errors"
	"testing"
)

func organizeTitle(title string) *string { return &title }

func organizeArchived(archived bool) *bool { return &archived }

// TestApplyThreadOrganizeWritesTitleGroupAndArchiveAtOnce is the landing
// case: four fields, one transaction, and the rows it wrote read back from
// inside it: the moved thread, the discussion child that travelled with
// it, and the group it had to create.
func TestApplyThreadOrganizeWritesTitleGroupAndArchiveAtOnce(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-root")
	child := makeThread("t-child", "claude")
	child.ParentThreadID = "t-root"
	if err := s.CreateThread(child); err != nil {
		t.Fatalf("create child thread: %v", err)
	}

	result, err := s.ApplyThreadOrganize("t-root", ThreadOrganizeWrite{
		Title:       organizeTitle("Release work"),
		CreateGroup: &ThreadGroupCreate{ProjectID: defaultTestProjectID, Name: "Release"},
		MoveGroup:   true,
		Archived:    organizeArchived(true),
	})
	if err != nil {
		t.Fatalf("ApplyThreadOrganize: %v", err)
	}
	if !result.Changed || !result.ArchivedChanged {
		t.Errorf("changed = %v, archivedChanged = %v, want both true", result.Changed, result.ArchivedChanged)
	}
	if result.CreatedGroup == nil || result.CreatedGroup.Name != "Release" {
		t.Fatalf("createdGroup = %+v, want the created Release group", result.CreatedGroup)
	}
	if result.Thread.Title != "Release work" {
		t.Errorf("returned title = %q, want %q", result.Thread.Title, "Release work")
	}
	if result.Thread.GroupID != result.CreatedGroup.ID {
		t.Errorf("returned groupId = %q, want %q", result.Thread.GroupID, result.CreatedGroup.ID)
	}
	if !result.Thread.Archived {
		t.Error("returned row is not archived")
	}
	if len(result.Carried) != 1 || result.Carried[0].ID != "t-child" {
		t.Fatalf("carried = %+v, want the discussion child", result.Carried)
	}
	if result.Carried[0].GroupID != result.CreatedGroup.ID {
		t.Errorf("carried child groupId = %q, want %q", result.Carried[0].GroupID, result.CreatedGroup.ID)
	}

	stored, err := s.GetThread("t-root")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if stored.Title != "Release work" || stored.GroupID != result.CreatedGroup.ID || !stored.Archived {
		t.Errorf("committed row = %+v, want the renamed, grouped, archived thread", stored)
	}
	if got := threadGroupID(t, s, "t-child"); got != result.CreatedGroup.ID {
		t.Errorf("committed child groupId = %q, want %q", got, result.CreatedGroup.ID)
	}
}

// TestApplyThreadOrganizeWritesTitlePinAndArchiveAtOnce covers the other
// half of the patch, the one a group move excludes: the pin. The back
// burner needs the two-step, and it lands in the same transaction as the
// rename and the archive.
func TestApplyThreadOrganizeWritesTitlePinAndArchiveAtOnce(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pin")

	result, err := s.ApplyThreadOrganize("t-pin", ThreadOrganizeWrite{
		Title:    organizeTitle("Back burner work"),
		Pin:      &ThreadPinWrite{Pinned: true, Burner: PinGroupBack},
		Archived: organizeArchived(true),
	})
	if err != nil {
		t.Fatalf("ApplyThreadOrganize: %v", err)
	}
	if !result.Changed || !result.ArchivedChanged {
		t.Errorf("changed = %v, archivedChanged = %v, want both true", result.Changed, result.ArchivedChanged)
	}
	if result.Thread.PinnedAt == nil {
		t.Fatal("the write left pinned_at NULL")
	}
	if result.Thread.PinGroup == nil || *result.Thread.PinGroup != PinGroupBack {
		t.Errorf("pinGroup = %v, want the back burner", result.Thread.PinGroup)
	}
	if result.Thread.Title != "Back burner work" || !result.Thread.Archived {
		t.Errorf("returned row = %+v, want it renamed and archived", result.Thread)
	}

	// none is the third tier, and it clears both pin fields.
	cleared, err := s.ApplyThreadOrganize("t-pin", ThreadOrganizeWrite{Pin: &ThreadPinWrite{}})
	if err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if !cleared.Changed {
		t.Error("unpinning a pinned row reported no change")
	}
	if cleared.Thread.PinnedAt != nil || cleared.Thread.PinGroup != nil {
		t.Errorf("unpin left latent pin state: %+v", cleared.Thread)
	}
}

// TestApplyThreadOrganizePinDoesNotTouchUpdatedAt is setThreadPinnedAt's
// rule, kept through the organize transaction: a pin is a sidebar tweak,
// not thread activity.
func TestApplyThreadOrganizePinDoesNotTouchUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pin")
	before, err := s.GetThread("t-pin")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	result, err := s.ApplyThreadOrganize("t-pin", ThreadOrganizeWrite{
		Title: organizeTitle("Renamed"),
		Pin:   &ThreadPinWrite{Pinned: true, Burner: PinGroupFront},
	})
	if err != nil {
		t.Fatalf("ApplyThreadOrganize: %v", err)
	}
	if result.Thread.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved %d -> %d; neither a rename nor a pin is activity",
			before.UpdatedAt, result.Thread.UpdatedAt)
	}
	if result.ArchivedChanged {
		t.Error("archivedChanged is true for a patch that never touched the archive flag")
	}
}

// TestApplyThreadOrganizeRollsBackEveryWriteWhenAStepFails is the spec's
// rule: a thread is either fully updated or untouched. The pin here is
// refused by the group move that precedes it, which is exactly the shape
// four separate accessors could not undo.
func TestApplyThreadOrganizeRollsBackEveryWriteWhenAStepFails(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-roll")
	before, err := s.GetThread("t-roll")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	_, err = s.ApplyThreadOrganize("t-roll", ThreadOrganizeWrite{
		Title:       organizeTitle("Renamed"),
		CreateGroup: &ThreadGroupCreate{ProjectID: defaultTestProjectID, Name: "Release"},
		MoveGroup:   true,
		Pin:         &ThreadPinWrite{Pinned: true, Burner: PinGroupFront},
		Archived:    organizeArchived(true),
	})
	if !errors.Is(err, ErrThreadGrouped) {
		t.Fatalf("error = %v, want ErrThreadGrouped", err)
	}
	assertThreadUntouched(t, s, before)

	// The group the refused patch had to create goes with it: an empty
	// group left in the sidebar is state nobody asked for.
	groups, err := s.ListThreadGroups()
	if err != nil {
		t.Fatalf("list thread groups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want the created group rolled back", groups)
	}

	// The same rule from the other side: a move that the store refuses
	// takes the rename before it with it.
	_, err = s.ApplyThreadOrganize("t-roll", ThreadOrganizeWrite{
		Title:     organizeTitle("Renamed"),
		MoveGroup: true,
		GroupID:   "no-such-group",
	})
	if !errors.Is(err, ErrThreadGroupGone) {
		t.Fatalf("error = %v, want ErrThreadGroupGone", err)
	}
	assertThreadUntouched(t, s, before)

	// And a patch on a thread that is gone writes nothing at all.
	_, err = s.ApplyThreadOrganize("no-such-thread", ThreadOrganizeWrite{
		Title: organizeTitle("Renamed"),
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows", err)
	}
}

// TestApplyThreadOrganizeReportsNoChangeWhenNothingMoves keeps the changed
// flag honest: root emits nothing for a patch that restates what the row
// already holds, and the row still comes back.
func TestApplyThreadOrganizeReportsNoChangeWhenNothingMoves(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-still")
	before, err := s.GetThread("t-still")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	result, err := s.ApplyThreadOrganize("t-still", ThreadOrganizeWrite{
		Title:    organizeTitle(before.Title),
		Pin:      &ThreadPinWrite{},
		Archived: organizeArchived(false),
	})
	if err != nil {
		t.Fatalf("ApplyThreadOrganize: %v", err)
	}
	if result.Changed || result.ArchivedChanged {
		t.Errorf("changed = %v, archivedChanged = %v, want both false",
			result.Changed, result.ArchivedChanged)
	}
	if result.Thread.ID != "t-still" || result.Thread.Title != before.Title {
		t.Errorf("returned row = %+v, want the unchanged row", result.Thread)
	}
	if result.CreatedGroup != nil || len(result.Carried) != 0 {
		t.Errorf("a no-op patch reported group work: %+v / %+v", result.CreatedGroup, result.Carried)
	}
	assertThreadUntouched(t, s, before)
}

// assertThreadUntouched compares every field one organize patch can write.
func assertThreadUntouched(t *testing.T, s *Store, before Thread) {
	t.Helper()
	after, err := s.GetThread(before.ID)
	if err != nil {
		t.Fatalf("get thread %s: %v", before.ID, err)
	}
	if after.Title != before.Title {
		t.Errorf("title = %q, want %q", after.Title, before.Title)
	}
	if after.GroupID != before.GroupID {
		t.Errorf("groupId = %q, want %q", after.GroupID, before.GroupID)
	}
	if (after.PinnedAt == nil) != (before.PinnedAt == nil) {
		t.Errorf("pinnedAt = %v, want %v", after.PinnedAt, before.PinnedAt)
	}
	if after.Archived != before.Archived {
		t.Errorf("archived = %v, want %v", after.Archived, before.Archived)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved %d -> %d", before.UpdatedAt, after.UpdatedAt)
	}
}
