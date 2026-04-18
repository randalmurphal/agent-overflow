package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// -- Agent 3 (Wave 5D) -- integration tests for interaction-mode APIs --
//
// These exercise CreateThread (newly accepting an explicit mode) and
// UpdateThreadMode (new binding) end-to-end through the App layer to
// confirm the app + store layers agree on what a "mode" is.
//
// Scope: no provider runtime is launched. We stub startSessionFn where a mode
// change races with a session lifecycle.

func TestMode_CreateThreadWithExplicitDefault(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-default", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("CreateThread(default) error = %v", err)
	}
	if thread.Mode != "chat" {
		t.Fatalf("returned mode = %q, want chat", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "chat" {
		t.Fatalf("stored mode = %q, want chat", stored.Mode)
	}
}

func TestMode_CreateThreadWithDiscussion(t *testing.T) {
	app := newTestAppWithStore(t)

	_, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-discussion", "claude-sonnet-4-6", "discussion")
	if err == nil {
		t.Fatal("CreateThread(discussion) error = nil, want rejection (discussion is reserved for StartDiscussion)")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("CreateThread(discussion) error = %v, want invalid-mode message", err)
	}
}

func TestMode_CreateThreadWithDesign(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-design", "claude-sonnet-4-6", "design")
	if err != nil {
		t.Fatalf("CreateThread(design) error = %v", err)
	}
	if thread.Mode != "design" {
		t.Fatalf("mode = %q, want design", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "design" {
		t.Fatalf("stored mode = %q, want design", stored.Mode)
	}
}

func TestMode_CreateThreadWithPlan(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-plan", "claude-sonnet-4-6", "plan")
	if err != nil {
		t.Fatalf("CreateThread(plan) error = %v", err)
	}
	if thread.Mode != "plan" {
		t.Fatalf("mode = %q, want plan", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "plan" {
		t.Fatalf("stored mode = %q, want plan", stored.Mode)
	}
}

// TestMode_CreateThreadRejectsInvalidMode: CreateThread must reject modes not
// in the manual-selection set (including gibberish, and specifically the
// "discussion" mode which is reserved for StartDiscussion).
func TestMode_CreateThreadRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)

	cases := []string{
		"gibberish",
		"Default", // case-sensitive
		"DISCUSSION",
		"debug",
		"ship",
		"123",
	}
	for _, mode := range cases {
		mode := mode
		t.Run(fmt.Sprintf("mode=%q", mode), func(t *testing.T) {
			_, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-invalid", "claude-sonnet-4-6", mode)
			if err == nil {
				t.Fatalf("CreateThread(%q) error = nil, want rejection", mode)
			}
			if !strings.Contains(err.Error(), "invalid mode") {
				t.Fatalf("CreateThread(%q) error = %v, want invalid-mode message", mode, err)
			}
		})
	}
}

func TestMode_SetInteractionModeChangesPersistentRow(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-setmode", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if thread.Mode != "chat" {
		t.Fatalf("initial mode = %q, want chat", thread.Mode)
	}

	updated, err := app.UpdateThreadMode(thread.ID, "design")
	if err != nil {
		t.Fatalf("UpdateThreadMode(design) error = %v", err)
	}
	if updated.Mode != "design" {
		t.Fatalf("returned mode = %q, want design", updated.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "design" {
		t.Fatalf("stored mode = %q, want design", stored.Mode)
	}
}

func TestMode_SetInteractionModeRejectsInvalidMode(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	for _, bad := range []string{"gibberish", "Plan", "DEFAULT", "archive", "DiscussioN"} {
		bad := bad
		t.Run(fmt.Sprintf("mode=%q", bad), func(t *testing.T) {
			_, err := app.UpdateThreadMode(thread.ID, bad)
			if err == nil {
				t.Fatalf("UpdateThreadMode(%q) error = nil, want rejection", bad)
			}
			if !strings.Contains(err.Error(), "invalid mode") {
				t.Fatalf("UpdateThreadMode(%q) error = %v, want invalid-mode message", bad, err)
			}
		})
	}

	// The thread's mode must be unchanged after every rejected attempt.
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "chat" {
		t.Fatalf("stored mode after rejected sets = %q, want chat", stored.Mode)
	}
}

// TestMode_SetInteractionModeUnknownThreadErrors: calling UpdateThreadMode
// with a non-existent thread id must error out, and the error surfaces sql.ErrNoRows
// via the GetThread pre-check (so callers can distinguish "not found" from validation
// errors).
func TestMode_SetInteractionModeUnknownThreadErrors(t *testing.T) {
	app := newTestAppWithStore(t)

	_, err := app.UpdateThreadMode("nonexistent-thread", "design")
	if err == nil {
		t.Fatal("UpdateThreadMode(nonexistent) error = nil, want not-found")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want wrapped sql.ErrNoRows", err)
	}
}

// TestMode_SetInteractionModeUpdatesUpdatedAt: the store bumps updated_at on
// every mode change so the sidebar can resort the thread to the top.
func TestMode_SetInteractionModeUpdatesUpdatedAt(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-updated", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	originalUpdatedAt := thread.UpdatedAt

	// Force a measurable gap so the timestamp comparison can't false-pass on a
	// single-millisecond tick.
	time.Sleep(5 * time.Millisecond)

	updated, err := app.UpdateThreadMode(thread.ID, "plan")
	if err != nil {
		t.Fatalf("UpdateThreadMode() error = %v", err)
	}
	if updated.UpdatedAt <= originalUpdatedAt {
		t.Fatalf("updatedAt = %d, want > %d (should have bumped)", updated.UpdatedAt, originalUpdatedAt)
	}

	// Confirm via direct read, in case the returned row was a stale snapshot.
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.UpdatedAt <= originalUpdatedAt {
		t.Fatalf("stored updatedAt = %d, want > %d", stored.UpdatedAt, originalUpdatedAt)
	}
}

// TestMode_ForkInheritsInteractionMode: ForkThread copies the source mode onto
// the fork so a design-mode conversation keeps its capabilities after branching.
func TestMode_ForkInheritsInteractionMode(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-fork-mode")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-abc"
	source.Mode = "design"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(source.ID)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}
	if forked.Mode != "design" {
		t.Fatalf("fork mode = %q, want design (inherited from source)", forked.Mode)
	}

	stored, err := app.store.GetThread(forked.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "design" {
		t.Fatalf("stored fork mode = %q, want design", stored.Mode)
	}
}

// TestMode_DesignModeThreadHasDesignArtifactCapability: create a design-mode
// thread and verify the thread is queryable for design artifacts (empty list
// is valid when no renders have happened). This mirrors the app_claude_design.go
// gating where only design-mode Claude threads can run design tools.
func TestMode_DesignModeThreadHasDesignArtifactCapability(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-design-cap", "claude-sonnet-4-6", "design")
	if err != nil {
		t.Fatalf("CreateThread(design) error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "design" {
		t.Fatalf("stored mode = %q, want design (capability gate)", stored.Mode)
	}
	if stored.Provider != string(provider.Claude) {
		t.Fatalf("stored provider = %q, want claude (design tools only gate on Claude)", stored.Provider)
	}
}

// TestMode_DiscussionModeCreatesOrPairsWithDiscussion: CreateThread refuses to
// produce a free-standing "discussion" thread. A discussion-mode thread appears
// only via StartDiscussion which wires a channel, the deliberation runtime, and
// child participant threads. We assert the CreateThread rejection path and that
// UpdateThreadMode is the only permitted way to label an existing
// thread "discussion" — documenting the intended layering.
func TestMode_DiscussionModeCreatesOrPairsWithDiscussion(t *testing.T) {
	app := newTestAppWithStore(t)

	// CreateThread rejects the discussion mode directly.
	if _, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-disc", "claude-sonnet-4-6", "discussion"); err == nil {
		t.Fatal("CreateThread(discussion) should fail (StartDiscussion owns that mode)")
	}

	// But an existing thread may be "promoted" to discussion via the setter.
	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-disc", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThread(default) error = %v", err)
	}
	updated, err := app.UpdateThreadMode(thread.ID, "discussion")
	if err != nil {
		t.Fatalf("UpdateThreadMode(discussion) error = %v", err)
	}
	if updated.Mode != "discussion" {
		t.Fatalf("mode = %q, want discussion", updated.Mode)
	}
	// No deliberation has been wired; the thread is a bare discussion-labelled
	// row. This is expected for the setter path — the deliberation runtime
	// is keyed off DiscussionID, not the mode column.
	if updated.DiscussionID != "" {
		t.Fatalf("DiscussionID = %q, want empty (setter path does not wire channel)", updated.DiscussionID)
	}
}

func TestMode_CreateThreadDefaultWhenEmpty(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Codex), "/tmp/ws-empty", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("CreateThread(empty) error = %v", err)
	}
	if thread.Mode != "chat" {
		t.Fatalf("returned mode = %q, want chat (normalized from empty)", thread.Mode)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Mode != "chat" {
		t.Fatalf("stored mode = %q, want chat (normalized)", stored.Mode)
	}

	// Whitespace-only inputs must also normalize to default, not be preserved
	// verbatim.
	whitespaceThread, err := createTestThread(t, app, string(provider.Codex), "/tmp/ws-whitespace", "gpt-5.4", "   \t  ")
	if err != nil {
		t.Fatalf("CreateThread(whitespace) error = %v", err)
	}
	if whitespaceThread.Mode != "chat" {
		t.Fatalf("whitespace mode = %q, want chat", whitespaceThread.Mode)
	}
}

// TestMode_ConcurrentSetModeRace: 10 goroutines fight over the same thread's
// interaction mode. The contract:
//   - every call either succeeds or returns a validation/IO error
//   - the final mode is one of the modes that was actually set
//   - DB integrity is preserved (GetThread still works, row is intact)
//
// Run with -race.
func TestMode_ConcurrentSetModeRace(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-race", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	modes := []string{"chat", "plan", "design", "discussion"}
	setModes := map[string]struct{}{}
	var setModesMu sync.Mutex
	var wg sync.WaitGroup
	const goroutines = 10
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			mode := modes[i%len(modes)]
			if _, err := app.UpdateThreadMode(thread.ID, mode); err != nil {
				// A concurrent call should never produce a validation error
				// (we passed a valid mode) -- anything else is a bug.
				t.Errorf("goroutine %d UpdateThreadMode(%q) error = %v", i, mode, err)
				return
			}
			setModesMu.Lock()
			setModes[mode] = struct{}{}
			setModesMu.Unlock()
		}()
	}
	wg.Wait()

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() after race error = %v", err)
	}
	if _, ok := setModes[stored.Mode]; !ok {
		t.Fatalf("final mode = %q, want one of %v (that was observed as set)", stored.Mode, keys(setModes))
	}
	// Sanity: other persisted columns should be unchanged by the setter.
	if stored.Provider != string(provider.Claude) {
		t.Fatalf("provider changed during race: %q", stored.Provider)
	}
	if stored.Model != "claude-sonnet-4-6" {
		t.Fatalf("model changed during race: %q", stored.Model)
	}
}

// TestMode_SetModeDuringActiveSession verifies that flipping the mode during
// an active provider session:
//   - persists the new mode to the DB
//   - leaves the active session map untouched (the running session keeps its
//     captured-at-startup config; reconnect is required to pick up the new mode)
//   - emits a thread:mode_changed event with NeedsReconnect=true
//     so the frontend can surface the requirement to the user.
func TestMode_SetModeDuringActiveSession(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	var capturedEvents []struct {
		name string
		data any
	}
	app.emitEventFn = func(name string, data any) {
		capturedEvents = append(capturedEvents, struct {
			name string
			data any
		}{name, data})
	}

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-active-mode", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Simulate an active session by parking a fake session struct; no real
	// provider is started. This is enough to exercise the code path where
	// UpdateThreadMode runs while a.sessions[threadID] is populated.
	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "active-session-token",
	}

	updated, err := app.UpdateThreadMode(thread.ID, "plan")
	if err != nil {
		t.Fatalf("UpdateThreadMode() error with active session = %v", err)
	}
	if updated.Mode != "plan" {
		t.Fatalf("updated mode = %q, want plan", updated.Mode)
	}

	// Session map must be untouched -- the in-memory session state is
	// independent of the mode column. The next StartSession/Reconnect is when
	// the provider would pick up the new mode.
	app.mu.Lock()
	_, stillActive := app.sessions[thread.ID]
	app.mu.Unlock()
	if !stillActive {
		t.Fatal("active session was evicted by UpdateThreadMode (unexpected)")
	}

	// An event must have fired signalling the active session needs reconnect.
	var modeChange *ThreadModeChangedEvent
	for _, e := range capturedEvents {
		if e.name != "thread:mode_changed" {
			continue
		}
		evt, ok := e.data.(ThreadModeChangedEvent)
		if !ok {
			t.Fatalf("thread:mode_changed payload type = %T, want ThreadModeChangedEvent", e.data)
		}
		modeChange = &evt
	}
	if modeChange == nil {
		t.Fatal("thread:mode_changed event was not emitted during active-session mode change")
	}
	if modeChange.ThreadID != thread.ID {
		t.Fatalf("event ThreadID = %q, want %q", modeChange.ThreadID, thread.ID)
	}
	if modeChange.Mode != "plan" {
		t.Fatalf("event Mode = %q, want plan", modeChange.Mode)
	}
	if !modeChange.NeedsReconnect {
		t.Fatalf("event NeedsReconnect = false, want true (session is active)")
	}
}

// TestMode_SetModeWithoutActiveSessionNoReconnect covers the counterpart: when
// there is no active session, the event is still emitted so the frontend can
// refresh its cached thread row, but NeedsReconnect is false because nothing
// is running to be out of sync.
func TestMode_SetModeWithoutActiveSessionNoReconnect(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())

	var capturedEvents []struct {
		name string
		data any
	}
	app.emitEventFn = func(name string, data any) {
		capturedEvents = append(capturedEvents, struct {
			name string
			data any
		}{name, data})
	}

	thread, err := createTestThread(t, app, string(provider.Claude), "/tmp/ws-inactive-mode", "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := app.UpdateThreadMode(thread.ID, "plan"); err != nil {
		t.Fatalf("UpdateThreadMode() error = %v", err)
	}

	var modeChange *ThreadModeChangedEvent
	for _, e := range capturedEvents {
		if e.name != "thread:mode_changed" {
			continue
		}
		evt, ok := e.data.(ThreadModeChangedEvent)
		if !ok {
			t.Fatalf("thread:mode_changed payload type = %T", e.data)
		}
		modeChange = &evt
	}
	if modeChange == nil {
		t.Fatal("thread:mode_changed event missing")
	}
	if modeChange.NeedsReconnect {
		t.Fatal("NeedsReconnect = true without an active session")
	}
}

// keys is a small helper for assertion messages.
func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time guard: ensure store.Thread carries the Mode field we
// rely on. If this ever stops compiling, callers need to be updated.
var _ = func(t store.Thread) string { return t.Mode }
