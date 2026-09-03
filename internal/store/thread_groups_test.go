package store

import (
	"database/sql"
	"errors"
	"testing"
)

// seedProject inserts a second project so the cross-project refusals below
// have somewhere to refuse a move TO.
func seedThreadGroupProject(t *testing.T, s *Store, id, path string) {
	t.Helper()
	if err := s.CreateProject(Project{
		ID: id, Path: path, Name: "Project " + id, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
}

func mustCreateGroup(t *testing.T, s *Store, projectID, name string) ThreadGroup {
	t.Helper()
	group, err := s.CreateThreadGroup(projectID, name)
	if err != nil {
		t.Fatalf("create thread group %q: %v", name, err)
	}
	return group
}

func threadGroupID(t *testing.T, s *Store, threadID string) string {
	t.Helper()
	thread, err := s.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	return thread.GroupID
}

func TestCreateThreadGroupTrimsAndRefusesBlankNames(t *testing.T) {
	s := newTestStore(t)

	group := mustCreateGroup(t, s, defaultTestProjectID, "  Release work  ")
	if group.Name != "Release work" {
		t.Errorf("name = %q, want trimmed %q", group.Name, "Release work")
	}
	if group.ID == "" {
		t.Error("create minted no id")
	}
	if group.PinnedAt != nil || group.PinGroup != nil {
		t.Errorf("a new group is unpinned, got pinnedAt=%v pinGroup=%v", group.PinnedAt, group.PinGroup)
	}

	if _, err := s.CreateThreadGroup(defaultTestProjectID, "   "); !errors.Is(err, ErrEmptyThreadGroupName) {
		t.Errorf("blank create error = %v, want ErrEmptyThreadGroupName", err)
	}
	if err := s.RenameThreadGroup(group.ID, "\t\n"); !errors.Is(err, ErrEmptyThreadGroupName) {
		t.Errorf("blank rename error = %v, want ErrEmptyThreadGroupName", err)
	}

	listed, err := s.ListThreadGroups()
	if err != nil {
		t.Fatalf("list thread groups: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != group.ID {
		t.Fatalf("list = %+v, want exactly the created group", listed)
	}
}

// TestThreadGroupPinsDoNotTouchUpdatedAt pins the same rule
// setThreadPinnedAt carries: a pin is a sidebar-presentation tweak, and
// updated_at is what an empty group sorts by.
func TestThreadGroupPinsDoNotTouchUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	group := mustCreateGroup(t, s, defaultTestProjectID, "Burner")

	if err := s.PinThreadGroup(group.ID); err != nil {
		t.Fatalf("pin thread group: %v", err)
	}
	pinned, err := s.GetThreadGroup(group.ID)
	if err != nil {
		t.Fatalf("get thread group: %v", err)
	}
	if pinned.PinnedAt == nil {
		t.Fatal("pin left pinned_at NULL")
	}
	if pinned.PinGroup == nil || *pinned.PinGroup != PinGroupFront {
		t.Errorf("pin group = %v, want front burner", pinned.PinGroup)
	}
	if pinned.UpdatedAt != group.UpdatedAt {
		t.Errorf("pin moved updated_at %d -> %d", group.UpdatedAt, pinned.UpdatedAt)
	}

	if err := s.SetThreadGroupPinGroup(group.ID, PinGroupBack); err != nil {
		t.Fatalf("set thread group pin group: %v", err)
	}
	moved, err := s.GetThreadGroup(group.ID)
	if err != nil {
		t.Fatalf("get thread group: %v", err)
	}
	if moved.PinGroup == nil || *moved.PinGroup != PinGroupBack {
		t.Errorf("pin group = %v, want back burner", moved.PinGroup)
	}
	if moved.UpdatedAt != group.UpdatedAt {
		t.Errorf("burner move touched updated_at")
	}

	if err := s.UnpinThreadGroup(group.ID); err != nil {
		t.Fatalf("unpin thread group: %v", err)
	}
	unpinned, err := s.GetThreadGroup(group.ID)
	if err != nil {
		t.Fatalf("get thread group: %v", err)
	}
	if unpinned.PinnedAt != nil || unpinned.PinGroup != nil {
		t.Errorf("unpin left latent pin state: %+v", unpinned)
	}

	// A burner move on an unpinned row is refused by the WHERE clause, not
	// by a prevalidation a future caller could skip.
	if err := s.SetThreadGroupPinGroup(group.ID, PinGroupFront); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("burner move on an unpinned group = %v, want sql.ErrNoRows", err)
	}
	if err := s.SetThreadGroupPinGroup(group.ID, 7); !errors.Is(err, ErrInvalidPinGroup) {
		t.Errorf("out-of-range burner = %v, want ErrInvalidPinGroup", err)
	}
}

// TestSetThreadGroupStripsThePin is the "one pin per visible row" rule from
// the moving side: the group carries the pin from then on.
func TestSetThreadGroupStripsThePin(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pinned")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")

	if err := s.PinThread("t-pinned"); err != nil {
		t.Fatalf("pin thread: %v", err)
	}
	moved, err := s.SetThreadGroup([]string{"t-pinned"}, group.ID)
	if err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if len(moved) != 1 || moved[0].ID != "t-pinned" {
		t.Fatalf("returned rows = %+v, want the moved thread", moved)
	}
	if moved[0].GroupID != group.ID {
		t.Errorf("groupId = %q, want %q", moved[0].GroupID, group.ID)
	}
	if moved[0].PinnedAt != nil || moved[0].PinGroup != nil {
		t.Errorf("the move left a pin behind: %+v", moved[0])
	}
}

// TestPinThreadOnAGroupedRowIsRefused is the same rule from the pinning
// side: the accessor names the refusal (ErrThreadGrouped) and the CHECK
// stands behind it for any caller that skips the accessor
// (TestMigrationV76ThreadGroupSchema writes past it).
func TestPinThreadOnAGroupedRowIsRefused(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-grouped")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-grouped"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}

	if err := s.PinThread("t-grouped"); !errors.Is(err, ErrThreadGrouped) {
		t.Fatalf("PinThread on a grouped row: error = %v, want ErrThreadGrouped", err)
	}
	if err := s.PinThread("no-such-thread"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("PinThread on a missing row: error = %v, want sql.ErrNoRows", err)
	}
	thread, err := s.GetThread("t-grouped")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if thread.PinnedAt != nil {
		t.Errorf("the refused pin still landed: %+v", thread)
	}
	if thread.GroupID != group.ID {
		t.Errorf("the refused pin dropped the group: %q", thread.GroupID)
	}
}

// TestSetThreadGroupCarriesDiscussionChildren pins the parent_thread_id
// disjunct: a discussion tree moves as a unit, and the caller learns the
// child ids from the rows it gets back.
func TestSetThreadGroupCarriesDiscussionChildren(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-root")
	child := makeThread("t-child", "claude")
	child.ParentThreadID = "t-root"
	if err := s.CreateThread(child); err != nil {
		t.Fatalf("create child thread: %v", err)
	}
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")

	moved, err := s.SetThreadGroup([]string{"t-root"}, group.ID)
	if err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved %d rows, want the root and its child: %+v", len(moved), moved)
	}
	for _, thread := range moved {
		if thread.GroupID != group.ID {
			t.Errorf("thread %s groupId = %q, want %q", thread.ID, thread.GroupID, group.ID)
		}
	}
	if got := threadGroupID(t, s, "t-child"); got != group.ID {
		t.Errorf("child groupId = %q, want %q", got, group.ID)
	}

	// "" is ungroup, and it carries the children back out the same way.
	out, err := s.SetThreadGroup([]string{"t-root"}, "")
	if err != nil {
		t.Fatalf("ungroup: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("ungroup moved %d rows, want 2", len(out))
	}
	for _, thread := range out {
		if thread.GroupID != "" {
			t.Errorf("thread %s still grouped as %q", thread.ID, thread.GroupID)
		}
	}
}

// TestSetThreadGroupRefusesCrossProjectAndRollsBack: the project subquery
// is the refusal, and one bad id fails the whole call — a partial
// multi-select move is not a state the sidebar could explain.
func TestSetThreadGroupRefusesCrossProjectAndRollsBack(t *testing.T) {
	s := newTestStore(t)
	seedThreadGroupProject(t, s, "other-project", "/tmp/other")

	mustCreateThread(t, s, "t-home")
	elsewhere := makeThread("t-elsewhere", "claude")
	elsewhere.ProjectID = "other-project"
	if err := s.CreateThread(elsewhere); err != nil {
		t.Fatalf("create thread in other project: %v", err)
	}
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")

	_, err := s.SetThreadGroup([]string{"t-home", "t-elsewhere"}, group.ID)
	if !errors.Is(err, ErrThreadGroupGone) {
		t.Fatalf("cross-project move error = %v, want ErrThreadGroupGone", err)
	}
	if got := threadGroupID(t, s, "t-home"); got != "" {
		t.Errorf("the refused call left t-home grouped as %q; it did not roll back", got)
	}
	if got := threadGroupID(t, s, "t-elsewhere"); got != "" {
		t.Errorf("t-elsewhere groupId = %q, want empty", got)
	}

	// An unknown group resolves to no project, so it matches nothing and
	// fails the same way.
	_, err = s.SetThreadGroup([]string{"t-home"}, "no-such-group")
	if !errors.Is(err, ErrThreadGroupGone) {
		t.Fatalf("unknown-group move error = %v, want ErrThreadGroupGone", err)
	}
	if got := threadGroupID(t, s, "t-home"); got != "" {
		t.Errorf("the unknown-group move left t-home grouped as %q", got)
	}
	_, err = s.SetThreadGroup([]string{"no-such-thread"}, group.ID)
	if !errors.Is(err, ErrThreadGone) {
		t.Fatalf("unknown-thread move error = %v, want ErrThreadGone", err)
	}
}

// TestSetThreadGroupRefusesAChildNamedAsRoot: a discussion child travels
// with its root and is never grouped on its own, in either direction.
// Naming it BESIDE its root is fine — the root's disjunct carries it and
// the read-back dedupes.
func TestSetThreadGroupRefusesAChildNamedAsRoot(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-root")
	child := makeThread("t-child", "claude")
	child.ParentThreadID = "t-root"
	if err := s.CreateThread(child); err != nil {
		t.Fatalf("create child thread: %v", err)
	}
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")

	_, err := s.SetThreadGroup([]string{"t-child"}, group.ID)
	if !errors.Is(err, ErrThreadNotRoot) {
		t.Fatalf("grouping a child alone: error = %v, want ErrThreadNotRoot", err)
	}
	if got := threadGroupID(t, s, "t-child"); got != "" {
		t.Errorf("the refused move left t-child grouped as %q", got)
	}

	moved, err := s.SetThreadGroup([]string{"t-child", "t-root"}, group.ID)
	if err != nil {
		t.Fatalf("root and child named together: %v", err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved %d rows, want the root and its child once each: %+v", len(moved), moved)
	}
	for _, thread := range moved {
		if thread.GroupID != group.ID {
			t.Errorf("thread %s groupId = %q, want %q", thread.ID, thread.GroupID, group.ID)
		}
	}

	_, err = s.SetThreadGroup([]string{"t-child"}, "")
	if !errors.Is(err, ErrThreadNotRoot) {
		t.Fatalf("ungrouping a child alone: error = %v, want ErrThreadNotRoot", err)
	}
}

// TestUngroupKeepsThePinsOfUngroupedRows: a bulk "Remove from Group" names
// every selected row, grouped or not. Ungrouping touches only group_id, so
// a pinned top-level row in that selection keeps its pin.
func TestUngroupKeepsThePinsOfUngroupedRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pinned")
	mustCreateThread(t, s, "t-grouped")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if err := s.PinThread("t-pinned"); err != nil {
		t.Fatalf("pin thread: %v", err)
	}
	if _, err := s.SetThreadGroup([]string{"t-grouped"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}

	out, err := s.SetThreadGroup([]string{"t-pinned", "t-grouped"}, "")
	if err != nil {
		t.Fatalf("ungroup: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("ungroup returned %d rows, want 2", len(out))
	}
	for _, thread := range out {
		if thread.GroupID != "" {
			t.Errorf("thread %s still grouped as %q", thread.ID, thread.GroupID)
		}
		if thread.ID == "t-pinned" && thread.PinnedAt == nil {
			t.Errorf("ungrouping the selection stripped t-pinned's pin: %+v", thread)
		}
	}
}

// TestDeleteThreadGroupUngroupsActiveAndArchivedMembers pins the FK's ON
// DELETE SET NULL: deleting a group ungroups, and never deletes a thread.
func TestDeleteThreadGroupUngroupsActiveAndArchivedMembers(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-active")
	mustCreateThread(t, s, "t-archived")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-active", "t-archived"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if err := s.ArchiveThread("t-archived"); err != nil {
		t.Fatalf("archive thread: %v", err)
	}

	if err := s.DeleteThreadGroup(group.ID); err != nil {
		t.Fatalf("delete thread group: %v", err)
	}
	for _, id := range []string{"t-active", "t-archived"} {
		thread, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("thread %s did not survive the group deletion: %v", id, err)
		}
		if thread.GroupID != "" {
			t.Errorf("thread %s still names the deleted group (%q)", id, thread.GroupID)
		}
	}
	if err := s.DeleteThreadGroup(group.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second delete = %v, want sql.ErrNoRows", err)
	}
}

// TestDeleteProjectCascadesThreadGroups: a group belongs to one project and
// cannot outlive it.
func TestDeleteProjectCascadesThreadGroups(t *testing.T) {
	s := newTestStore(t)
	seedThreadGroupProject(t, s, "doomed-project", "/tmp/doomed")
	group := mustCreateGroup(t, s, "doomed-project", "Group")

	if err := s.DeleteProject("doomed-project"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := s.GetThreadGroup(group.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("group survived its project: %v", err)
	}
}

// TestBuildForkedThreadCopiesGroupID: a fork of a grouped thread lands in
// the same group, and the copy has to survive the INSERT too.
func TestBuildForkedThreadCopiesGroupID(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-source")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-source"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	source, err := s.GetThread("t-source")
	if err != nil {
		t.Fatalf("get source thread: %v", err)
	}

	fork := BuildForkedThread(source)
	if fork.GroupID != group.ID {
		t.Fatalf("fork groupId = %q, want %q", fork.GroupID, group.ID)
	}
	if err := s.CreateThread(fork); err != nil {
		t.Fatalf("create forked thread: %v", err)
	}
	if got := threadGroupID(t, s, fork.ID); got != group.ID {
		t.Errorf("persisted fork groupId = %q, want %q", got, group.ID)
	}
}

// TestSetThreadGroupIsIdempotentAndSkipsBlankIDs guards the two shapes a
// multi-select drag actually produces.
func TestSetThreadGroupIsIdempotentAndSkipsBlankIDs(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-one")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")

	moved, err := s.SetThreadGroup([]string{"", "  ", "t-one", "t-one"}, group.ID)
	if err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved %d rows, want 1: %+v", len(moved), moved)
	}
	if moved, err := s.SetThreadGroup(nil, group.ID); err != nil || len(moved) != 0 {
		t.Fatalf("empty id list = (%v, %v), want no rows and no error", moved, err)
	}
	if moved, err := s.SetThreadGroup([]string{"t-one"}, group.ID); err != nil || len(moved) != 1 {
		t.Fatalf("repeat move = (%v, %v), want the same single row", moved, err)
	}
}
