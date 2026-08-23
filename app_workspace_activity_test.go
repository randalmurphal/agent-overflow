package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// idle reports a zero answer. The struct holds a slice, so it cannot be
// compared against WorkspaceActivity{} directly.
func (w WorkspaceActivity) idle() bool {
	return w.ActiveTurnThreads == 0 && w.RunningBackgroundTasks == 0 && len(w.BusyThreads) == 0
}

// seedRunningBackgroundLaunch writes the persisted shape of a live background
// task: a running, top-level, is_background tool_call with no completion
// sibling.
func seedRunningBackgroundLaunch(t *testing.T, a *App, threadID, itemID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := a.store.InsertItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "Bash",
		IsBackground: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed background launch %s: %v", itemID, err)
	}
}

func seedOpenTurn(t *testing.T, a *App, threadID string) {
	t.Helper()
	if err := a.store.InsertTurn(store.Turn{
		TurnID:    "turn-" + threadID,
		ThreadID:  threadID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert open turn for %s: %v", threadID, err)
	}
}

// TestGetWorkspaceActivity_AggregatesAcrossThreadsSharingAWorkspace is the
// regression the workspace-change lock was re-keyed for. Two threads in one
// worktree is first-class; asking only about the caller's own thread left the
// destructive "remove worktree" affordance live over a directory the sibling's
// agent was writing into.
func TestGetWorkspaceActivity_AggregatesAcrossThreadsSharingAWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	elsewhere := t.TempDir()

	idle, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(idle): %v", err)
	}
	busy, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(busy): %v", err)
	}
	if idle.WorkspacePath != busy.WorkspacePath {
		t.Fatalf("fixture: threads do not share a workspace (%q vs %q)", idle.WorkspacePath, busy.WorkspacePath)
	}
	seedRunningBackgroundLaunch(t, app, busy.ID, "bg-sibling")

	activity, err := app.GetWorkspaceActivity(workspace)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity: %v", err)
	}
	if activity.RunningBackgroundTasks != 1 {
		t.Errorf("RunningBackgroundTasks = %d, want 1 (the sibling thread's task)", activity.RunningBackgroundTasks)
	}
	if activity.ActiveTurnThreads != 0 {
		t.Errorf("ActiveTurnThreads = %d, want 0", activity.ActiveTurnThreads)
	}
	// The busy set names the sibling only: the idle thread is free to move
	// elsewhere even though its directory is busy.
	if want := []BusyThread{{ThreadID: busy.ID, RunningBackgroundTasks: 1}}; !slices.Equal(activity.BusyThreads, want) {
		t.Errorf("BusyThreads = %+v, want %+v", activity.BusyThreads, want)
	}

	// The count belongs to the directory, not to a project or a thread: an
	// unrelated workspace must not inherit it.
	other, err := app.GetWorkspaceActivity(elsewhere)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity(elsewhere): %v", err)
	}
	if !other.idle() {
		t.Errorf("GetWorkspaceActivity(elsewhere) = %+v, want zero", other)
	}
}

func TestGetWorkspaceActivity_SumsTasksFromEveryThreadInTheWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()

	first, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(first): %v", err)
	}
	second, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(second): %v", err)
	}
	seedRunningBackgroundLaunch(t, app, first.ID, "bg-first")
	seedRunningBackgroundLaunch(t, app, second.ID, "bg-second-a")
	if err := app.store.InsertItem(store.Item{
		ID:           "bg-second-b",
		ThreadID:     second.ID,
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		Summary:      "Bash",
		IsBackground: true,
		CreatedAt:    time.Now().UnixMilli(),
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed second background launch: %v", err)
	}

	activity, err := app.GetWorkspaceActivity(workspace)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity: %v", err)
	}
	if activity.RunningBackgroundTasks != 3 {
		t.Errorf("RunningBackgroundTasks = %d, want 3", activity.RunningBackgroundTasks)
	}
	want := []BusyThread{
		{ThreadID: first.ID, RunningBackgroundTasks: 1},
		{ThreadID: second.ID, RunningBackgroundTasks: 2},
	}
	slices.SortFunc(want, func(x, y BusyThread) int { return strings.Compare(x.ThreadID, y.ThreadID) })
	if !slices.Equal(activity.BusyThreads, want) {
		t.Errorf("BusyThreads = %+v, want %+v", activity.BusyThreads, want)
	}
}

// The turn leg is workspace-scoped for the same reason the task leg is: a
// sibling thread mid-response is writing into the checkout, and
// removeProjectWorktree refuses on exactly this condition.
func TestGetWorkspaceActivity_CountsOpenTurnsOfSiblingThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()

	if _, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", ""); err != nil {
		t.Fatalf("createTestThread(idle): %v", err)
	}
	streaming, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread(streaming): %v", err)
	}
	seedOpenTurn(t, app, streaming.ID)

	activity, err := app.GetWorkspaceActivity(workspace)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity: %v", err)
	}
	if activity.ActiveTurnThreads != 1 {
		t.Errorf("ActiveTurnThreads = %d, want 1", activity.ActiveTurnThreads)
	}
	if activity.RunningBackgroundTasks != 0 {
		t.Errorf("RunningBackgroundTasks = %d, want 0", activity.RunningBackgroundTasks)
	}
	if want := []BusyThread{{ThreadID: streaming.ID, ActiveTurn: true}}; !slices.Equal(activity.BusyThreads, want) {
		t.Errorf("BusyThreads = %+v, want %+v", activity.BusyThreads, want)
	}

	// A settled turn releases the workspace.
	if err := app.store.UpdateTurnCompleted(
		"turn-"+streaming.ID, time.Now().UnixMilli(), "end_turn", "", "", "",
	); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
	settled, err := app.GetWorkspaceActivity(workspace)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity(after complete): %v", err)
	}
	if !settled.idle() {
		t.Errorf("GetWorkspaceActivity(after complete) = %+v, want zero", settled)
	}
}

func TestGetWorkspaceActivity_IdleWorkspaceReportsNothing(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	if _, err := createTestThread(t, app, "claude", workspace, "claude-sonnet-4-6", ""); err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	activity, err := app.GetWorkspaceActivity(workspace)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity: %v", err)
	}
	if !activity.idle() {
		t.Errorf("GetWorkspaceActivity = %+v, want zero", activity)
	}

	// A path no thread has ever occupied is the same answer, not an error:
	// the lock asks about whatever directory a pane is showing.
	unknown, err := app.GetWorkspaceActivity(filepath.Join(workspace, "never-used"))
	if err != nil {
		t.Fatalf("GetWorkspaceActivity(unknown): %v", err)
	}
	if !unknown.idle() {
		t.Errorf("GetWorkspaceActivity(unknown) = %+v, want zero", unknown)
	}
}

// A thread that cut a worktree and then switched back to the project root
// still POINTS at the worktree, and removeProjectWorktree refuses to delete a
// directory any thread references by either column. The affordance has to
// agree, or the button stays live and the backend rejects the click.
func TestGetWorkspaceActivity_MatchesThreadsByWorktreePathToo(t *testing.T) {
	app := newTestAppWithStore(t)
	root := t.TempDir()
	worktree := t.TempDir()

	thread, err := createTestThread(t, app, "claude", root, "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	thread.WorktreePath = worktree
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	seedRunningBackgroundLaunch(t, app, thread.ID, "bg-root")

	activity, err := app.GetWorkspaceActivity(worktree)
	if err != nil {
		t.Fatalf("GetWorkspaceActivity(worktree): %v", err)
	}
	if activity.RunningBackgroundTasks != 1 {
		t.Errorf("RunningBackgroundTasks = %d, want 1", activity.RunningBackgroundTasks)
	}
}

func TestGetWorkspaceActivity_RefusesAnEmptyPath(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, err := app.GetWorkspaceActivity("   "); err == nil {
		t.Fatal("GetWorkspaceActivity(\"\") = nil error, want a refusal")
	}
}
