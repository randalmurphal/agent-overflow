package app

import (
	"context"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// thread_update and thread_group organize through the sidebar's own
// bindings, so these tests watch two things at once: what the store holds
// afterwards, and what a second attached client was told. A refusal must
// leave both untouched.

type organizeFixture struct {
	*threadToolsFixture
	events *emitRecorder
}

func newOrganizeFixture(t *testing.T) *organizeFixture {
	t.Helper()
	f := newThreadToolsFixture(t)
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	return &organizeFixture{threadToolsFixture: f, events: rec}
}

// threadFrames returns the thread:updated frames, action and row per frame.
func (f *organizeFixture) threadFrames(t *testing.T) []triage.ThreadUpdateEvent {
	t.Helper()
	out := []triage.ThreadUpdateEvent{}
	for _, call := range f.events.snapshot() {
		if call.Channel != eventchan.ThreadUpdated.String() {
			continue
		}
		event, ok := call.Data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated carried %T, want triage.ThreadUpdateEvent", call.Data)
		}
		out = append(out, event)
	}
	return out
}

func (f *organizeFixture) groupFrames(t *testing.T) []ThreadGroupUpdateEvent {
	t.Helper()
	out := []ThreadGroupUpdateEvent{}
	for _, call := range f.events.snapshot() {
		if call.Channel != eventchan.ThreadGroupUpdated.String() {
			continue
		}
		event, ok := call.Data.(ThreadGroupUpdateEvent)
		if !ok {
			t.Fatalf("thread-group:updated carried %T, want ThreadGroupUpdateEvent", call.Data)
		}
		out = append(out, event)
	}
	return out
}

func (f *organizeFixture) reload(t *testing.T, threadID string) store.Thread {
	t.Helper()
	row, err := f.app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	return row
}

func caller(threadID string) threadtools.Caller {
	return threadtools.Caller{ThreadID: threadID, Title: "Caller"}
}

func updateResultFor(t *testing.T, report threadtools.UpdateReport, threadID string) threadtools.ThreadUpdateResult {
	t.Helper()
	for _, row := range report.Results {
		if row.ThreadID == threadID {
			return row
		}
	}
	t.Fatalf("no result row for %s in %+v", threadID, report.Results)
	return threadtools.ThreadUpdateResult{}
}

func stringPtr(value string) *string { return &value }
func boolPtr(value bool) *bool       { return &value }

// A grouped thread pins like any other: one call pins and renames a
// grouped and an ungrouped thread alike, and the grouped one keeps its
// group.
func TestThreadToolsUpdatePinsGroupedAndUngroupedThreads(t *testing.T) {
	f := newOrganizeFixture(t)
	plain := f.thread(t, "plain-thread")
	grouped := f.thread(t, "grouped-thread")
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{grouped.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}

	report, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{plain.ID, grouped.ID},
		Title:     stringPtr("Burner work"),
		Pin:       stringPtr(threadtools.PinBack),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}

	for _, id := range []string{plain.ID, grouped.ID} {
		if row := updateResultFor(t, report, id); !row.Updated || row.Error != "" {
			t.Fatalf("thread %s = %+v, want updated", id, row)
		}
		applied := f.reload(t, id)
		if applied.Title != "Burner work" || applied.PinnedAt == nil || applied.PinGroup == nil || *applied.PinGroup != store.PinGroupBack {
			t.Errorf("applied row = %+v, want the renamed back-burner thread", applied)
		}
	}
	if got := f.reload(t, grouped.ID); got.GroupID != group.ID {
		t.Errorf("the grouped thread left its group: %q", got.GroupID)
	}

	frames := f.threadFrames(t)
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want one full frame per thread", frames)
	}
	for _, frame := range frames {
		if frame.Thread == nil || frame.Action != triage.ThreadActionFull || frame.Thread.PinnedAt == nil {
			t.Errorf("frame = %+v, want a full frame carrying the pinned row", frame)
		}
	}
}

// The calling thread never archives itself, and the refusal leaves the rest
// of the call alone.
func TestThreadToolsUpdateRefusesArchivingTheCaller(t *testing.T) {
	f := newOrganizeFixture(t)
	self := f.thread(t, "caller-thread")
	other := f.thread(t, "other-thread")

	report, err := f.adapter.UpdateThreads(t.Context(), caller(self.ID), threadtools.UpdateCall{
		ThreadIDs: []string{self.ID, other.ID},
		Archived:  boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	refused := updateResultFor(t, report, self.ID)
	if refused.Updated || refused.ErrorCode != threadtools.CodeIsCaller {
		t.Fatalf("caller row = %+v, want a %s refusal", refused, threadtools.CodeIsCaller)
	}
	if f.reload(t, self.ID).Archived {
		t.Error("the calling thread was archived")
	}
	if !f.reload(t, other.ID).Archived {
		t.Error("the other thread was not archived")
	}

	frames := f.threadFrames(t)
	if len(frames) != 1 || frames[0].Action != triage.ThreadActionUnlisted || frames[0].Thread.ID != other.ID {
		t.Fatalf("frames = %+v, want one unlisted frame for %s", frames, other.ID)
	}

	// Unarchiving announces the row as listed again, and a second call that
	// asks for the state the row already holds says nothing at all.
	f.events.reset()
	if _, err := f.adapter.UpdateThreads(t.Context(), caller(self.ID), threadtools.UpdateCall{
		ThreadIDs: []string{other.ID}, Archived: boolPtr(false),
	}); err != nil {
		t.Fatalf("UpdateThreads unarchive: %v", err)
	}
	if frames := f.threadFrames(t); len(frames) != 1 || frames[0].Action != triage.ThreadActionListed {
		t.Fatalf("unarchive frames = %+v, want one listed frame", frames)
	}

	f.events.reset()
	report, err = f.adapter.UpdateThreads(t.Context(), caller(self.ID), threadtools.UpdateCall{
		ThreadIDs: []string{other.ID}, Archived: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("UpdateThreads repeat: %v", err)
	}
	if row := updateResultFor(t, report, other.ID); !row.Updated {
		t.Errorf("a repeat of a change already in place = %+v, want updated", row)
	}
	if frames := f.threadFrames(t); len(frames) != 0 {
		t.Fatalf("a write that changed nothing emitted %+v", frames)
	}
}

// A patch that cannot be valid for any thread refuses the whole call, and
// an unknown id is one row's own refusal.
func TestThreadToolsUpdateValidatesThePatchAndTheIds(t *testing.T) {
	f := newOrganizeFixture(t)
	thread := f.thread(t, "patch-thread")

	blank, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{thread.ID}, Title: stringPtr("   "),
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("blank title code = %q, want %q (%+v)", code, threadtools.CodeInvalidRequest, blank)
	}
	if got := f.reload(t, thread.ID); got.Title != thread.Title {
		t.Errorf("title = %q after a refused call, want it untouched", got.Title)
	}
	if frames := f.threadFrames(t); len(frames) != 0 {
		t.Fatalf("a refused patch emitted %+v", frames)
	}

	report, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{"no-such-thread", thread.ID}, Archived: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, "no-such-thread"); row.ErrorCode != threadtools.CodeNotFound {
		t.Fatalf("unknown id = %+v, want %q", row, threadtools.CodeNotFound)
	}
	if row := updateResultFor(t, report, thread.ID); !row.Updated {
		t.Fatalf("the known id = %+v, want it applied beside the miss", row)
	}
}

// A group is per project: a name that exists in ANOTHER project is not this
// thread's group, so the patch creates one where the thread lives and the
// sidebar hears about the new row before the thread that joined it.
func TestThreadToolsUpdateCreatesTheGroupInTheThreadsOwnProject(t *testing.T) {
	f := newOrganizeFixture(t)
	other := store.Project{ID: "tt-other", Path: t.TempDir(), Name: "Other", CreatedAt: 1, UpdatedAt: 1}
	if _, err := f.app.store.CreateProject(other); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	elsewhere, err := f.app.store.CreateThreadGroup(other.ID, "Auth work")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	thread := f.thread(t, "group-thread")

	report, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{thread.ID}, Group: stringPtr("Auth work"),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, thread.ID); !row.Updated {
		t.Fatalf("group move = %+v", row)
	}

	joined := f.reload(t, thread.ID)
	if joined.GroupID == "" || joined.GroupID == elsewhere.ID {
		t.Fatalf("groupId = %q, want a new group in %s", joined.GroupID, f.project.ID)
	}
	created, err := f.app.store.GetThreadGroup(joined.GroupID)
	if err != nil {
		t.Fatalf("GetThreadGroup: %v", err)
	}
	if created.ProjectID != f.project.ID || created.Name != "Auth work" {
		t.Errorf("created group = %+v, want %q in %s", created, "Auth work", f.project.ID)
	}

	groupFrames := f.groupFrames(t)
	if len(groupFrames) != 1 || groupFrames[0].Action != "create" || groupFrames[0].Group.ID != created.ID {
		t.Fatalf("group frames = %+v, want one create for %s", groupFrames, created.ID)
	}
	threadFrames := f.threadFrames(t)
	if len(threadFrames) != 1 || threadFrames[0].Thread.GroupID != created.ID {
		t.Fatalf("thread frames = %+v, want one row carrying the new group", threadFrames)
	}

	// Naming the same group again reuses it, whatever case it is typed in,
	// and moves the second thread into the group that is already there.
	f.events.reset()
	second := f.thread(t, "second-thread")
	if _, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{second.ID}, Group: stringPtr("auth WORK"),
	}); err != nil {
		t.Fatalf("UpdateThreads second: %v", err)
	}
	if got := f.reload(t, second.ID); got.GroupID != created.ID {
		t.Errorf("second thread groupId = %q, want the existing %s", got.GroupID, created.ID)
	}
	if frames := f.groupFrames(t); len(frames) != 0 {
		t.Fatalf("reusing a group emitted %+v", frames)
	}
}

// Two spawns naming one group that does not exist yet end in ONE group.
// Both plans are made from a read that found no such group; the destination
// resolves the name again inside the write's own transaction, so the second
// joins the first's group instead of inserting a row nobody could tell apart
// from it.
func TestConcurrentSpawnsNamingOneNewGroupShareIt(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "on it")
	const group = "Release sweep"

	acks := make([]threadtools.RequestAck, 2)
	errs := make([]error, len(acks))
	var wg sync.WaitGroup
	for i := range acks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acks[i], errs[i] = f.adapter().Spawn(context.Background(), f.callerIdentity(), threadtools.SpawnCall{
				Prompt: "start the sweep", Group: group,
			})
		}(i)
	}
	wg.Wait()

	groups, err := f.app.store.ListThreadGroups()
	if err != nil {
		t.Fatalf("ListThreadGroups: %v", err)
	}
	named := []store.ThreadGroup{}
	for _, candidate := range groups {
		if candidate.Name == group {
			named = append(named, candidate)
		}
	}
	if len(named) != 1 {
		t.Fatalf("groups named %q = %+v, want exactly one", group, named)
	}
	for i, ack := range acks {
		if errs[i] != nil {
			t.Fatalf("Spawn %d: %v", i, errs[i])
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", ack.ThreadID, err)
		}
		if spawned.GroupID != named[0].ID {
			t.Errorf("spawn %d joined group %q, want %s", i, spawned.GroupID, named[0].ID)
		}
	}
}

// Grouping and pinning in one call: the group move lands first and clears
// the pin of a row that changes group, and the pin then lands inside the
// new group, even when it restates the tier the row held before the move.
func TestThreadToolsUpdateGroupsAndPinsInOneCall(t *testing.T) {
	f := newOrganizeFixture(t)
	thread := f.thread(t, "group-pin-thread")
	if _, _, err := f.app.store.PinThread(thread.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}

	report, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{thread.ID}, Group: stringPtr("Release"), Pin: stringPtr(threadtools.PinFront),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, thread.ID); !row.Updated {
		t.Fatalf("group and pin = %+v", row)
	}
	got := f.reload(t, thread.ID)
	if got.GroupID == "" || got.PinnedAt == nil || (got.PinGroup != nil && *got.PinGroup != store.PinGroupFront) {
		t.Fatalf("row = %+v, want it grouped and on the front burner", got)
	}
	if frames := f.groupFrames(t); len(frames) != 1 || frames[0].Action != "create" {
		t.Fatalf("group frames = %+v, want the created group announced", frames)
	}
	frames := f.threadFrames(t)
	if len(frames) != 1 || frames[0].Thread.GroupID != got.GroupID || frames[0].Thread.PinnedAt == nil {
		t.Fatalf("thread frames = %+v, want one row grouped and pinned", frames)
	}

	// Moving to another group alone leaves the pin behind.
	f.events.reset()
	if _, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{thread.ID}, Group: stringPtr("Later"),
	}); err != nil {
		t.Fatalf("UpdateThreads move: %v", err)
	}
	moved := f.reload(t, thread.ID)
	if moved.GroupID == got.GroupID || moved.GroupID == "" || moved.PinnedAt != nil || moved.PinGroup != nil {
		t.Fatalf("row after a move between groups = %+v, want it unpinned in the new group", moved)
	}
}

// Ungrouping and pinning in one call: leaving the group clears the pin the
// row held inside it, and the pin in the call then lands on the top level.
func TestThreadToolsUpdateUngroupsBeforeItPins(t *testing.T) {
	f := newOrganizeFixture(t)
	thread := f.thread(t, "regroup-thread")
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{thread.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	if _, _, err := f.app.store.PinThread(thread.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	if _, _, err := f.app.store.SetThreadPinGroup(thread.ID, store.PinGroupBack); err != nil {
		t.Fatalf("SetThreadPinGroup: %v", err)
	}

	report, err := f.adapter.UpdateThreads(t.Context(), caller("caller-thread"), threadtools.UpdateCall{
		ThreadIDs: []string{thread.ID}, Group: stringPtr(""), Pin: stringPtr(threadtools.PinFront),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, thread.ID); !row.Updated {
		t.Fatalf("ungroup and pin = %+v", row)
	}
	got := f.reload(t, thread.ID)
	if got.GroupID != "" || got.PinnedAt == nil || (got.PinGroup != nil && *got.PinGroup != store.PinGroupFront) {
		t.Fatalf("row = %+v, want it ungrouped and on the front burner", got)
	}
	if frames := f.threadFrames(t); len(frames) != 1 {
		t.Fatalf("frames = %+v, want one frame for one thread", frames)
	}
}

// thread_group renames and deletes, and a delete ungroups its members
// rather than deleting them, clearing the pins they held inside it.
func TestThreadToolsGroupRenamesAndDeletes(t *testing.T) {
	f := newOrganizeFixture(t)
	callerThread := f.thread(t, "caller-thread")
	member := f.thread(t, "member-thread")
	archivedMember := f.thread(t, "archived-member")
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{member.ID, archivedMember.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	for _, id := range []string{member.ID, archivedMember.ID} {
		if _, _, err := f.app.store.PinThread(id); err != nil {
			t.Fatalf("PinThread(%s): %v", id, err)
		}
	}
	if _, _, err := f.app.store.ArchiveThread(archivedMember.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	// Named, with no project_id: the caller's own project answers.
	f.events.reset()
	renamed, err := f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		Group: "release", Rename: "Ship it",
	})
	if err != nil {
		t.Fatalf("UpdateGroup rename: %v", err)
	}
	if renamed.Action != "renamed" || renamed.Group != "Ship it" || renamed.GroupID != group.ID {
		t.Fatalf("rename report = %+v", renamed)
	}
	frames := f.groupFrames(t)
	if len(frames) != 1 || frames[0].Action != "patch" || frames[0].Group.Name != "Ship it" {
		t.Fatalf("rename frames = %+v, want one patch carrying the new name", frames)
	}

	// Delete counts the members it ungroups, archived ones included, and
	// leaves every thread in place.
	f.events.reset()
	deleted, err := f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		GroupID: group.ID, Delete: true,
	})
	if err != nil {
		t.Fatalf("UpdateGroup delete: %v", err)
	}
	if deleted.Action != "deleted" || deleted.Ungrouped != 2 {
		t.Fatalf("delete report = %+v, want two ungrouped threads", deleted)
	}
	frames = f.groupFrames(t)
	if len(frames) != 1 || frames[0].Action != "delete" || frames[0].Group.Name != "Ship it" {
		t.Fatalf("delete frames = %+v, want one delete carrying the row", frames)
	}
	for _, id := range []string{member.ID, archivedMember.ID} {
		if got := f.reload(t, id); got.GroupID != "" || got.PinnedAt != nil || got.PinGroup != nil {
			t.Errorf("thread %s = group %q pin %v after the delete, want ungrouped and unpinned", id, got.GroupID, got.PinnedAt)
		}
	}
}

// A group call that names nothing this computer has, or asks for two
// actions at once, is refused with a code the model can act on.
func TestThreadToolsGroupRefusals(t *testing.T) {
	f := newOrganizeFixture(t)
	callerThread := f.thread(t, "caller-thread")

	_, err := f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		Group: "Nothing here", Rename: "Later",
	})
	if code := publicCode(t, err); code != threadtools.CodeNotFound {
		t.Errorf("missing group code = %q, want %q", code, threadtools.CodeNotFound)
	}

	_, err = f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		GroupID: "g-missing", Delete: true,
	})
	if code := publicCode(t, err); code != threadtools.CodeNotFound {
		t.Errorf("missing group id code = %q, want %q", code, threadtools.CodeNotFound)
	}

	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	_, err = f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		GroupID: group.ID, Rename: "Later", Delete: true,
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Errorf("two actions code = %q, want %q", code, threadtools.CodeInvalidRequest)
	}
	_, err = f.adapter.UpdateGroup(t.Context(), caller(callerThread.ID), threadtools.GroupCall{
		GroupID: group.ID,
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Errorf("no action code = %q, want %q", code, threadtools.CodeInvalidRequest)
	}
	if frames := f.groupFrames(t); len(frames) != 0 {
		t.Fatalf("a refused group call emitted %+v", frames)
	}
}
