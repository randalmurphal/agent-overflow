package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// seedTaskOutputThread writes a Claude thread with one running background
// launch row and returns the output file the launch names.
func seedTaskOutputThread(t *testing.T, app *App, threadID, launchID, meta string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := app.store.CreateThread(store.Thread{
		ID:            threadID,
		Title:         "tail",
		Provider:      "claude",
		WorkspacePath: "/tmp/tail",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.store.AppendItem(store.Item{
		ID:           launchID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: make build",
		ToolName:     "Bash",
		Meta:         meta,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
}

func writeTaskOutput(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A watch is the calling connection's: another connection naming its id
// releases nothing, and the connection's own release does.
func TestWatchBackgroundTaskOutputIsOwnedByItsConnection(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	path := writeTaskOutput(t, t.TempDir(), "task-a.output", "line 1\n")
	seedTaskOutputThread(t, app, "thread-tail-owner", "launch-a", `{"task_id":"task-a","output_file":"`+path+`"}`)

	ctxA, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	ctxB, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	snap, err := app.WatchBackgroundTaskOutput(ctxA, "thread-tail-owner", "launch-a")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if snap.WatchID == "" || snap.Data != "line 1\n" {
		t.Fatalf("snapshot: %+v", snap)
	}
	tailer := app.backgroundTaskTailer()
	if err := app.UnwatchBackgroundTaskOutput(ctxB, "thread-tail-owner", snap.WatchID); err != nil {
		t.Fatalf("unwatch from another connection: %v", err)
	}
	if got := tailer.Watching(); got != 1 {
		t.Fatalf("another connection's release must not land; watching %d", got)
	}
	if err := app.UnwatchBackgroundTaskOutput(ctxA, "thread-tail-owner", snap.WatchID); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	if got := tailer.Watching(); got != 0 {
		t.Fatalf("the owner's release lands; watching %d", got)
	}
}

// A Monitor's launch names no file; the watch reads it off the task's
// newest notification row, and the launch row stays as it was written.
func TestWatchBackgroundTaskOutputResolvesMonitorFileFromItsNotification(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	path := writeTaskOutput(t, t.TempDir(), "task-w.output", "matched: build done\n")
	seedTaskOutputThread(t, app, "thread-tail-monitor", "launch-w", `{"task_id":"task-w","watch_task":true}`)
	launchBefore, _, err := app.store.GetThreadItem("thread-tail-monitor", "launch-w")
	if err != nil {
		t.Fatal(err)
	}

	ctx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	if _, err := app.WatchBackgroundTaskOutput(ctx, "thread-tail-monitor", "launch-w"); err != ErrBackgroundTaskOutputNoFile {
		t.Fatalf("before any notification: %v, want ErrBackgroundTaskOutputNoFile", err)
	}
	if _, err := app.store.AppendItem(store.Item{
		ID:        "note-w-1",
		ThreadID:  "thread-tail-monitor",
		TurnIndex: 1,
		Kind:      "notification",
		Role:      "system",
		Status:    "completed",
		Summary:   "matched: build done",
		Meta:      `{"task_id":"task-w","watch_task":true,"output_file":"` + path + `"}`,
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	snap, err := app.WatchBackgroundTaskOutput(ctx, "thread-tail-monitor", "launch-w")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if snap.Data != "matched: build done\n" {
		t.Fatalf("snapshot: %+v", snap)
	}
	launchAfter, _, err := app.store.GetThreadItem("thread-tail-monitor", "launch-w")
	if err != nil {
		t.Fatal(err)
	}
	if launchAfter.Meta != launchBefore.Meta || launchAfter.UpdatedAt != launchBefore.UpdatedAt {
		t.Fatalf("the launch row is history: %+v -> %+v", launchBefore, launchAfter)
	}
	if err := app.UnwatchBackgroundTaskOutput(ctx, "thread-tail-monitor", snap.WatchID); err != nil {
		t.Fatal(err)
	}
}

// A release that names another thread leaves the watch and its
// connection cleanup alone, so the connection's end still releases it.
func TestUnwatchOnAnotherThreadKeepsTheConnectionCleanup(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	path := writeTaskOutput(t, t.TempDir(), "task-c.output", "line 1\n")
	seedTaskOutputThread(t, app, "thread-tail-cleanup", "launch-c", `{"task_id":"task-c","output_file":"`+path+`"}`)

	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	snap, err := app.WatchBackgroundTaskOutput(ctx, "thread-tail-cleanup", "launch-c")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	tailer := app.backgroundTaskTailer()
	if err := app.UnwatchBackgroundTaskOutput(ctx, "thread-elsewhere", snap.WatchID); err != nil {
		t.Fatalf("unwatch with another thread: %v", err)
	}
	if got := tailer.Watching(); got != 1 {
		t.Fatalf("a release naming another thread releases nothing; watching %d", got)
	}
	if !state.HasCleanup(backgroundTaskOutputCleanupKey(snap.WatchID)) {
		t.Fatal("a release naming another thread leaves the connection's cleanup bound")
	}
	state.RunCleanups()
	if got := tailer.Watching(); got != 0 {
		t.Fatalf("the connection's end releases the watch; watching %d", got)
	}
}

// A task that ended took its watch with it; the page's Unwatch that
// follows still drops the connection's cleanup for it, and a page that
// never unwatches is covered by the end itself.
func TestWatchBindingsGoWithTheTaskOrTheUnwatch(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	dir := t.TempDir()
	pathA := writeTaskOutput(t, dir, "task-ea.output", "a\n")
	pathB := writeTaskOutput(t, dir, "task-eb.output", "b\n")
	seedTaskOutputThread(t, app, "thread-tail-end", "launch-ea", `{"task_id":"task-ea","output_file":"`+pathA+`"}`)
	if _, err := app.store.AppendItem(store.Item{
		ID: "launch-eb", ThreadID: "thread-tail-end", TurnIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "running", IsBackground: true, Summary: "Bash: make b", ToolName: "Bash",
		Meta: `{"task_id":"task-eb","output_file":"` + pathB + `"}`, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	tailer := app.backgroundTaskTailer()

	// Ended, then unwatched by the page.
	snapA, err := app.WatchBackgroundTaskOutput(ctx, "thread-tail-end", "launch-ea")
	if err != nil {
		t.Fatal(err)
	}
	keyA := backgroundTaskOutputCleanupKey(snapA.WatchID)
	if !state.HasCleanup(keyA) {
		t.Fatal("the watch binds a cleanup on its connection")
	}
	tailer.EndLaunch("thread-tail-end", "launch-ea")
	if err := app.UnwatchBackgroundTaskOutput(ctx, "thread-tail-end", snapA.WatchID); err != nil {
		t.Fatal(err)
	}
	if state.HasCleanup(keyA) {
		t.Fatal("after the task ended, the page's unwatch must still drop the connection's cleanup")
	}

	// Ended with no unwatch at all.
	snapB, err := app.WatchBackgroundTaskOutput(ctx, "thread-tail-end", "launch-eb")
	if err != nil {
		t.Fatal(err)
	}
	keyB := backgroundTaskOutputCleanupKey(snapB.WatchID)
	tailer.EndLaunch("thread-tail-end", "launch-eb")
	if state.HasCleanup(keyB) {
		t.Fatal("the task's end drops the cleanup of a watch it took")
	}
	app.taskOutput.mu.Lock()
	left := len(app.taskOutput.bindings)
	app.taskOutput.mu.Unlock()
	if left != 0 {
		t.Fatalf("no binding record outlives its watch; %d left", left)
	}
}
