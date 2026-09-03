package app

import (
	"errors"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The group bindings are pure persistence plus an emit, and the emit is
// the whole reason a second connected client stays current. These tests
// pin the frames: one whole-row thread:updated "full" per touched thread,
// one thread-group:updated per group write, and nothing on a refusal.

func newAppForThreadGroups(t *testing.T) (*App, *emitRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	rec := &emitRecorder{}
	app.testEmitHook = rec.capture
	return app, rec
}

func emittedThreadRows(rec *emitRecorder) []store.Thread {
	out := make([]store.Thread, 0)
	for _, c := range rec.snapshot() {
		if c.Channel != eventchan.ThreadUpdated.String() {
			continue
		}
		evt, ok := c.Data.(triage.ThreadUpdateEvent)
		if !ok {
			continue
		}
		if evt.Action != "full" || evt.Thread == nil {
			panic("thread:updated from a group/pin binding must be a whole-row \"full\" frame")
		}
		out = append(out, *evt.Thread)
	}
	return out
}

func emittedGroupFrames(rec *emitRecorder) []ThreadGroupUpdateEvent {
	out := make([]ThreadGroupUpdateEvent, 0)
	for _, c := range rec.snapshot() {
		if c.Channel != eventchan.ThreadGroupUpdated.String() {
			continue
		}
		if evt, ok := c.Data.(ThreadGroupUpdateEvent); ok {
			out = append(out, evt)
		}
	}
	return out
}

func TestPinBindingsEmitTheWholeRow(t *testing.T) {
	app, rec := newAppForThreadGroups(t)
	if err := app.store.CreateThread(testThread("t-pin")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	pinned, err := app.PinThread("t-pin")
	if err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	rows := emittedThreadRows(rec)
	if len(rows) != 1 || rows[0].ID != "t-pin" || rows[0].PinnedAt == nil {
		t.Fatalf("PinThread frames = %+v, want one pinned row", rows)
	}
	if pinned.PinnedAt == nil || *pinned.PinnedAt != *rows[0].PinnedAt {
		t.Errorf("returned row %+v disagrees with the emitted row %+v", pinned, rows[0])
	}

	rec.reset()
	if _, err := app.SetThreadPinGroup("t-pin", store.PinGroupBack); err != nil {
		t.Fatalf("SetThreadPinGroup: %v", err)
	}
	rows = emittedThreadRows(rec)
	if len(rows) != 1 || rows[0].PinGroup == nil || *rows[0].PinGroup != store.PinGroupBack {
		t.Fatalf("SetThreadPinGroup frames = %+v, want one back-burner row", rows)
	}

	rec.reset()
	if _, err := app.UnpinThread("t-pin"); err != nil {
		t.Fatalf("UnpinThread: %v", err)
	}
	rows = emittedThreadRows(rec)
	if len(rows) != 1 || rows[0].PinnedAt != nil || rows[0].PinGroup != nil {
		t.Fatalf("UnpinThread frames = %+v, want one unpinned row", rows)
	}
}

func TestSetThreadGroupEmitsEveryTouchedRowAndDeleteEmitsTheGroup(t *testing.T) {
	app, rec := newAppForThreadGroups(t)
	if err := app.store.CreateThread(testThread("t-root")); err != nil {
		t.Fatalf("create root: %v", err)
	}
	child := testThread("t-child")
	child.ParentThreadID = "t-root"
	if err := app.store.CreateThread(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	group, err := app.CreateThreadGroup(defaultTestProjectID, "Port work")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	frames := emittedGroupFrames(rec)
	if len(frames) != 1 || frames[0].Action != "create" || frames[0].Group.ID != group.ID {
		t.Fatalf("create frames = %+v, want one create for %s", frames, group.ID)
	}

	rec.reset()
	moved, err := app.SetThreadGroup([]string{"t-root"}, group.ID)
	if err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	rows := emittedThreadRows(rec)
	if len(rows) != 2 || len(moved) != 2 {
		t.Fatalf("SetThreadGroup emitted %d rows and returned %d, want the root and its child on both", len(rows), len(moved))
	}
	for _, row := range rows {
		if row.GroupID != group.ID {
			t.Errorf("emitted row %s groupId = %q, want %q", row.ID, row.GroupID, group.ID)
		}
	}

	// A refused move emits nothing: the second client's state was right.
	rec.reset()
	if _, err := app.SetThreadGroup([]string{"t-child"}, group.ID); !errors.Is(err, store.ErrThreadNotRoot) {
		t.Fatalf("grouping a child alone: error = %v, want ErrThreadNotRoot", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a refused move emitted %+v", got)
	}
	if _, err := app.PinThread("t-root"); !errors.Is(err, store.ErrThreadGrouped) {
		t.Fatalf("pinning a grouped row: error = %v, want ErrThreadGrouped", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a refused pin emitted %+v", got)
	}

	// Delete carries the row as it was, so the frame still names the group,
	// and emits no thread rows: the client drops the membership itself.
	if err := app.DeleteThreadGroup(group.ID); err != nil {
		t.Fatalf("DeleteThreadGroup: %v", err)
	}
	frames = emittedGroupFrames(rec)
	if len(frames) != 1 || frames[0].Action != "delete" || frames[0].Group.Name != "Port work" {
		t.Fatalf("delete frames = %+v, want one delete carrying the row", frames)
	}
	if rows := emittedThreadRows(rec); len(rows) != 0 {
		t.Fatalf("delete emitted thread rows %+v", rows)
	}
	after, err := app.store.GetThread("t-child")
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if after.GroupID != "" {
		t.Errorf("child still grouped as %q after the group was deleted", after.GroupID)
	}
}
