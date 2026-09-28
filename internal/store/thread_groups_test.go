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
	if _, err := s.CreateProject(Project{
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

// TestSetThreadGroupStripsThePin: a thread starts unpinned in a group it
// joins, from the top level or from another group. A row already in the
// destination does not change group and keeps its pin.
func TestSetThreadGroupStripsThePin(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pinned")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	other := mustCreateGroup(t, s, defaultTestProjectID, "Other")

	if _, _, err := s.PinThread("t-pinned"); err != nil {
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
		t.Errorf("joining a group left a pin behind: %+v", moved[0])
	}

	// Pinned inside the group, then named again for the same group.
	if _, _, err := s.PinThread("t-pinned"); err != nil {
		t.Fatalf("pin grouped thread: %v", err)
	}
	if _, _, err := s.SetThreadPinGroup("t-pinned", PinGroupBack); err != nil {
		t.Fatalf("move grouped thread to the back burner: %v", err)
	}
	same, err := s.SetThreadGroup([]string{"t-pinned"}, group.ID)
	if err != nil {
		t.Fatalf("repeat move into the same group: %v", err)
	}
	if same[0].PinnedAt == nil || same[0].PinGroup == nil || *same[0].PinGroup != PinGroupBack {
		t.Errorf("a move into the group the row is already in dropped its pin: %+v", same[0])
	}

	// From one group to another is leaving one and joining the other.
	across, err := s.SetThreadGroup([]string{"t-pinned"}, other.ID)
	if err != nil {
		t.Fatalf("move between groups: %v", err)
	}
	if across[0].GroupID != other.ID {
		t.Errorf("groupId = %q, want %q", across[0].GroupID, other.ID)
	}
	if across[0].PinnedAt != nil || across[0].PinGroup != nil {
		t.Errorf("moving between groups left a pin behind: %+v", across[0])
	}
}

// TestGroupedThreadPinsLikeAnyThread: a member pins, changes burner and
// unpins through the ordinary thread pin writers, and stays in its group.
func TestGroupedThreadPinsLikeAnyThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-grouped")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-grouped"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}

	pinned, changed, err := s.PinThread("t-grouped")
	if err != nil || !changed {
		t.Fatalf("PinThread on a grouped row = (%v, %v), want a pin", changed, err)
	}
	if pinned.PinnedAt == nil || pinned.PinGroup == nil || *pinned.PinGroup != PinGroupFront {
		t.Errorf("pinned row = %+v, want the front burner", pinned)
	}
	if pinned.GroupID != group.ID {
		t.Errorf("pinning dropped the group: %q", pinned.GroupID)
	}
	back, changed, err := s.SetThreadPinGroup("t-grouped", PinGroupBack)
	if err != nil || !changed {
		t.Fatalf("burner move on a grouped row = (%v, %v)", changed, err)
	}
	if back.PinGroup == nil || *back.PinGroup != PinGroupBack || back.GroupID != group.ID {
		t.Errorf("burner move = %+v, want the back burner inside the group", back)
	}
	unpinned, changed, err := s.UnpinThread("t-grouped")
	if err != nil || !changed {
		t.Fatalf("UnpinThread on a grouped row = (%v, %v)", changed, err)
	}
	if unpinned.PinnedAt != nil || unpinned.PinGroup != nil || unpinned.GroupID != group.ID {
		t.Errorf("unpinned row = %+v, want no pin inside the group", unpinned)
	}
	if _, _, err := s.PinThread("no-such-thread"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("PinThread on a missing row: error = %v, want sql.ErrNoRows", err)
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
// every selected row, grouped or not. A row leaving a group loses its pin;
// a pinned top-level row in that selection never left one and keeps it.
func TestUngroupKeepsThePinsOfUngroupedRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-pinned")
	mustCreateThread(t, s, "t-grouped")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, _, err := s.PinThread("t-pinned"); err != nil {
		t.Fatalf("pin thread: %v", err)
	}
	if _, err := s.SetThreadGroup([]string{"t-grouped"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if _, _, err := s.PinThread("t-grouped"); err != nil {
		t.Fatalf("pin grouped thread: %v", err)
	}
	if _, _, err := s.SetThreadPinGroup("t-grouped", PinGroupBack); err != nil {
		t.Fatalf("move grouped thread to the back burner: %v", err)
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
		if thread.ID == "t-grouped" && (thread.PinnedAt != nil || thread.PinGroup != nil) {
			t.Errorf("t-grouped left its group and kept its pin: %+v", thread)
		}
	}
}

// TestDeleteThreadGroupUngroupsActiveAndArchivedMembers: deleting a group
// ungroups its members, active and archived, and never deletes a thread.
// Each member leaves the group and so loses its pin; a thread outside the
// group keeps its own. The call returns every member row as it now stands.
func TestDeleteThreadGroupUngroupsActiveAndArchivedMembers(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-active")
	mustCreateThread(t, s, "t-archived")
	mustCreateThread(t, s, "t-outside")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-active", "t-archived"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	for _, id := range []string{"t-active", "t-archived", "t-outside"} {
		if _, _, err := s.PinThread(id); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	if _, _, err := s.SetThreadPinGroup("t-archived", PinGroupBack); err != nil {
		t.Fatalf("move archived member to the back burner: %v", err)
	}
	if _, _, err := s.ArchiveThread("t-archived"); err != nil {
		t.Fatalf("archive thread: %v", err)
	}

	members, err := s.DeleteThreadGroup(group.ID)
	if err != nil {
		t.Fatalf("delete thread group: %v", err)
	}
	if len(members) != 2 || members[0].ID != "t-active" || members[1].ID != "t-archived" {
		t.Fatalf("returned rows = %+v, want both members", members)
	}
	for _, member := range members {
		if member.GroupID != "" || member.PinnedAt != nil || member.PinGroup != nil {
			t.Errorf("returned member %s = %+v, want ungrouped and unpinned", member.ID, member)
		}
	}
	for _, id := range []string{"t-active", "t-archived"} {
		thread, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("thread %s did not survive the group deletion: %v", id, err)
		}
		if thread.GroupID != "" {
			t.Errorf("thread %s still names the deleted group (%q)", id, thread.GroupID)
		}
		if thread.PinnedAt != nil || thread.PinGroup != nil {
			t.Errorf("thread %s left the deleted group and kept its pin: %+v", id, thread)
		}
	}
	outside, err := s.GetThread("t-outside")
	if err != nil {
		t.Fatal(err)
	}
	if outside.PinnedAt == nil {
		t.Errorf("deleting a group unpinned a thread outside it: %+v", outside)
	}
	if _, err := s.DeleteThreadGroup(group.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second delete = %v, want sql.ErrNoRows", err)
	}
	empty := mustCreateGroup(t, s, defaultTestProjectID, "Empty")
	if members, err := s.DeleteThreadGroup(empty.ID); err != nil || len(members) != 0 {
		t.Errorf("delete of an empty group = (%+v, %v), want no rows and no error", members, err)
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
// the same group, and the copy has to survive the INSERT too. The source's
// pin is its own: the fork starts unpinned.
func TestBuildForkedThreadCopiesGroupID(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-source")
	group := mustCreateGroup(t, s, defaultTestProjectID, "Group")
	if _, err := s.SetThreadGroup([]string{"t-source"}, group.ID); err != nil {
		t.Fatalf("set thread group: %v", err)
	}
	if _, _, err := s.PinThread("t-source"); err != nil {
		t.Fatalf("pin source thread: %v", err)
	}
	source, err := s.GetThread("t-source")
	if err != nil {
		t.Fatalf("get source thread: %v", err)
	}

	fork := BuildForkedThread(source)
	if fork.GroupID != group.ID {
		t.Fatalf("fork groupId = %q, want %q", fork.GroupID, group.ID)
	}
	if fork.PinnedAt != nil || fork.PinGroup != nil {
		t.Fatalf("fork copied the source's pin: %+v", fork)
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

func TestCreateThreadValidatesGroupMembershipAtomically(t *testing.T) {
	s := newTestStore(t)
	group := mustCreateGroup(t, s, defaultTestProjectID, "Drafts")
	seedThreadGroupProject(t, s, "other-project", "/tmp/other-group-project")
	other := mustCreateGroup(t, s, "other-project", "Other")
	deleted := mustCreateGroup(t, s, defaultTestProjectID, "Deleted")
	if _, err := s.DeleteThreadGroup(deleted.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, groupID string
		allowed       bool
	}{
		{"grouped", group.ID, true},
		{"ungrouped", "", true},
		{"cross-project", other.ID, false},
		{"deleted", deleted.ID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := makeThread(tc.name, "claude")
			thread.GroupID = tc.groupID
			err := s.CreateThread(thread)
			if !tc.allowed {
				if !errors.Is(err, ErrThreadGroupGone) {
					t.Fatalf("create error = %v", err)
				}
				if _, err := s.GetThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("refused create left a thread: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, err := s.GetThread(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.GroupID != tc.groupID || !stored.IsDraft || stored.PinnedAt != nil {
				t.Fatalf("unexpected draft: %+v", stored)
			}
		})
	}
}
