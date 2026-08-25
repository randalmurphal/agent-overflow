package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// fakeCodexProbe wires a deterministic Probe response into a test
// Session. We can't spin up a real Codex app-server in unit tests, so
// the reconcile path reaches the session through the decoder directly
// via this struct's Probe override — exercised by wrapping the App in
// a narrow test harness.
//
// (The real session type is used in integration tests under
// app_*_integration_test.go; this file keeps the reconcile logic
// isolated from the app_server binary.)
type codexProbeStub struct {
	status codex.ThreadStatusKind
	err    error
}

// TestReconcileCodexMarksLostOnSystemError covers the headline path of
// the spec: a systemError verdict flips every running-background
// tool_call row to errored/lost. This is the only branch that mutates
// persisted state, so it gets the tightest assertions.
func TestReconcileCodexMarksLostOnSystemError(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-a")
	runningID := seedRunningBackgroundTool(t, st, threadID, "tool-bg-run")

	// Also seed a *completed* background tool to guard against over-flip.
	// The reconciler must NOT touch anything that isn't `running`.
	completedID := seedCompletedBackgroundTool(t, st, threadID, "tool-bg-done")

	// And an active-turn non-background tool_call that happens to be
	// running — also MUST NOT be touched (is_background=false).
	inlineRunningID := seedRunningInlineTool(t, st, threadID, "tool-inline-run")

	installFakeCodexSession(t, a, threadID, codexProbeStub{status: codex.ThreadStatusSystemError})

	result, err := a.ReconcileCodexOnReopen(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ReconcileCodexOnReopen: %v", err)
	}

	if result.Status != codex.ThreadStatusSystemError {
		t.Fatalf("result.Status = %q, want systemError", result.Status)
	}
	if result.Running != 1 {
		t.Fatalf("result.Running = %d, want 1", result.Running)
	}
	if result.Flipped != 1 {
		t.Fatalf("result.Flipped = %d, want 1", result.Flipped)
	}
	if result.NeedsResume {
		t.Fatalf("result.NeedsResume = true on systemError, want false")
	}

	// Assertions against the store: the running background tool is now
	// errored/lost; the completed + inline rows are untouched.
	running := getItem(t, st, runningID)
	if running.Status != "errored" {
		t.Fatalf("running row status = %q, want errored", running.Status)
	}
	if running.Decision != "lost" {
		t.Fatalf("running row decision = %q, want lost", running.Decision)
	}
	completed := getItem(t, st, completedID)
	if completed.Status != "completed" {
		t.Fatalf("already-completed row touched: status = %q, want completed", completed.Status)
	}
	inline := getItem(t, st, inlineRunningID)
	if inline.Status != "running" {
		t.Fatalf("inline running row touched: status = %q, want running", inline.Status)
	}
}

// TestReconcileCodexResumesNotLoaded covers the notLoaded path: the
// adapter reports the thread isn't in memory but that's NOT the same
// as dead. The reconciler must NOT flip any rows and must signal
// `NeedsResume=true` so the caller can sequence a thread/resume.
func TestReconcileCodexResumesNotLoaded(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-b")
	runningID := seedRunningBackgroundTool(t, st, threadID, "tool-bg-run")

	installFakeCodexSession(t, a, threadID, codexProbeStub{status: codex.ThreadStatusNotLoaded})

	result, err := a.ReconcileCodexOnReopen(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ReconcileCodexOnReopen: %v", err)
	}

	if result.Status != codex.ThreadStatusNotLoaded {
		t.Fatalf("status = %q, want notLoaded", result.Status)
	}
	if !result.NeedsResume {
		t.Fatalf("NeedsResume = false, want true")
	}
	if result.Flipped != 0 {
		t.Fatalf("Flipped = %d, want 0 on notLoaded", result.Flipped)
	}

	running := getItem(t, st, runningID)
	if running.Status != "running" {
		t.Fatalf("running row flipped on notLoaded: status = %q, want running", running.Status)
	}
	if running.Decision != "" {
		t.Fatalf("running row decision set on notLoaded: %q, want empty", running.Decision)
	}
}

// TestReconcileCodexLeavesAliveUnchanged covers idle/active: the
// session is alive, so we leave every row alone. A real completion
// will arrive over the wire if/when it lands.
func TestReconcileCodexLeavesAliveUnchanged(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	for _, alive := range []codex.ThreadStatusKind{codex.ThreadStatusIdle, codex.ThreadStatusActive} {
		t.Run(string(alive), func(t *testing.T) {
			threadID := seedCodexThread(t, st, "thread-"+string(alive))
			runningID := seedRunningBackgroundTool(t, st, threadID, "tool-bg-run-"+string(alive))

			installFakeCodexSession(t, a, threadID, codexProbeStub{status: alive})

			result, err := a.ReconcileCodexOnReopen(context.Background(), threadID)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if result.Flipped != 0 {
				t.Fatalf("Flipped = %d on %s, want 0", result.Flipped, alive)
			}
			if result.NeedsResume {
				t.Fatalf("NeedsResume = true on %s, want false", alive)
			}
			running := getItem(t, st, runningID)
			if running.Status != "running" {
				t.Fatalf("row flipped on %s: status = %q", alive, running.Status)
			}
		})
	}
}

// TestReconcileCodexRejectsNoSession covers the input-validation
// contract: the reconciler only runs against an active session, so
// calling it on a thread with no session is an explicit error (not a
// silent no-op).
func TestReconcileCodexRejectsNoSession(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-no-session")

	_, err := a.ReconcileCodexOnReopen(context.Background(), threadID)
	if err == nil {
		t.Fatal("expected error when thread has no active session")
	}
}

// --- helpers ---

func newAppWithStore(t *testing.T, st *store.Store) *App {
	t.Helper()
	a := NewApp()
	a.store = st
	// ensureCodexReconcileTestProject plumbs a single project row so
	// the thread FK validates. Kept local to this test file to avoid
	// polluting app-wide helpers with bootstrap-only fixtures.
	ensureCodexReconcileTestProject(t, st)
	return a
}

const codexReconcileTestProjectID = "proj-reconcile-test"

func ensureCodexReconcileTestProject(t *testing.T, st *store.Store) {
	t.Helper()
	now := time.Now().UnixMilli()
	// Tolerate duplicate-insert from a previous subtest — tests in this
	// file share the same store instance via newAppWithStore.
	if err := st.CreateProject(store.Project{
		ID:        codexReconcileTestProjectID,
		Path:      "/tmp/reconcile",
		Name:      "Reconcile Test",
		Color:     "#888888",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		if _, getErr := st.GetProject(codexReconcileTestProjectID); getErr != nil {
			t.Fatalf("CreateProject: %v", err)
		}
	}
}

func seedCodexThread(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     codexReconcileTestProjectID,
		Title:         "Reconcile " + id,
		Provider:      "codex",
		WorkspacePath: "/tmp/reconcile/" + id,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreateThread %s: %v", id, err)
	}
	return id
}

func seedRunningBackgroundTool(t *testing.T, st *store.Store, threadID, itemID string) string {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		Summary:      "Bash: long-running script",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed running bg %s: %v", itemID, err)
	}
	return itemID
}

func seedCompletedBackgroundTool(t *testing.T, st *store.Store, threadID, itemID string) string {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		IsBackground: true,
		Summary:      "Bash: earlier bg",
		ToolName:     "Bash",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed completed bg %s: %v", itemID, err)
	}
	return itemID
}

func seedRunningInlineTool(t *testing.T, st *store.Store, threadID, itemID string) string {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID:           itemID,
		ThreadID:     threadID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: false,
		Summary:      "Read: /tmp/x",
		ToolName:     "Read",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("seed running inline %s: %v", itemID, err)
	}
	return itemID
}

// threadIDsSeededForReconcileTests lists every thread id this file
// creates via seedCodexThread, so getItem can scan ListItems on each
// without hard-coding the list in multiple places. Add to this
// constant when a new test case introduces a new thread id.
var threadIDsSeededForReconcileTests = []string{
	"thread-a",
	"thread-b",
	"thread-no-session",
	"thread-idle",
	"thread-active",
	"thread-reconcile-start",
	"thread-reconcile-alive",
	"thread-reconcile-systemerr",
	"thread-ghost-flip",
	"thread-ghost-flip-foreground",
	"thread-ghost-flip-claude",
	"thread-ghost-flip-scoping-codex",
	"thread-ghost-flip-empty",
	"thread-ghost-flip-idempotent",
}

// getItem walks every thread this file seeds looking for a row with
// the given id. The store doesn't expose a direct by-id getter
// independent of threadID; tests share a single store instance so a
// scan across known thread ids is cheap and keeps the test terse.
func getItem(t *testing.T, st *store.Store, itemID string) store.Item {
	t.Helper()
	for _, threadID := range threadIDsSeededForReconcileTests {
		items, err := st.ListItems(threadID)
		if err != nil {
			continue
		}
		for _, it := range items {
			if it.ID == itemID {
				return it
			}
		}
	}
	t.Fatalf("item %q not found via ListItems across %v", itemID, threadIDsSeededForReconcileTests)
	return store.Item{}
}

// installFakeCodexSession puts a narrow test double into the App's
// sessions map. The double only needs the Probe method — the
// reconciler never calls Send / Interrupt / Close on it.
func installFakeCodexSession(t *testing.T, a *App, threadID string, stub codexProbeStub) {
	t.Helper()
	fakeSession := codex.NewProbeOnlyTestSession(func(_ context.Context) (codex.ProbeResult, error) {
		if stub.err != nil {
			return codex.ProbeResult{}, stub.err
		}
		return codex.ProbeResult{Status: stub.status}, nil
	})
	a.mu.Lock()
	a.sessions[threadID] = session{
		provider: "codex",
		codex:    fakeSession,
	}
	a.mu.Unlock()
}

// installFakeCodexSessionWithResume is the reconcile-after-start variant
// that also wires a Resume stub. Used by the tests that exercise the
// notLoaded → thread/resume rehydration path: they need to observe
// whether Resume was called in addition to Probe.
func installFakeCodexSessionWithResume(
	t *testing.T,
	a *App,
	threadID string,
	stub codexProbeStub,
	resumeFn func(ctx context.Context) error,
) {
	t.Helper()
	fakeSession := codex.NewProbeAndResumeTestSession(
		func(_ context.Context) (codex.ProbeResult, error) {
			if stub.err != nil {
				return codex.ProbeResult{}, stub.err
			}
			return codex.ProbeResult{Status: stub.status}, nil
		},
		resumeFn,
	)
	a.mu.Lock()
	a.sessions[threadID] = session{
		provider: "codex",
		codex:    fakeSession,
	}
	a.mu.Unlock()
}

// TestStartSessionTriggersCodexReconcileResumesOnNotLoaded covers the
// post-start reconcile path: when startSessionNow seats a Codex session
// with a resume ref, reconcileCodexAfterStart runs, and if the probe
// reports notLoaded it triggers Resume to rehydrate the thread before
// the user sends another turn.
//
// We can't drive startSessionNow end-to-end in a unit test (it spawns a
// real codex process), so we exercise reconcileCodexAfterStart directly
// — that's the same code path the goroutine in startSessionNow enters.
func TestStartSessionTriggersCodexReconcileResumesOnNotLoaded(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)
	threadID := seedCodexThread(t, st, "thread-reconcile-start")

	var resumeCalls atomic.Int32
	installFakeCodexSessionWithResume(t, a, threadID,
		codexProbeStub{status: codex.ThreadStatusNotLoaded},
		func(_ context.Context) error {
			resumeCalls.Add(1)
			return nil
		},
	)

	a.reconcileCodexAfterStart(threadID)

	if got := resumeCalls.Load(); got != 1 {
		t.Fatalf("resumeCalls = %d, want 1 on notLoaded probe", got)
	}
}

// TestStartSessionTriggersCodexReconcileSkipsResumeOnAlive covers the
// alive path: an idle/active probe must NOT trigger Resume. The session
// is live; a redundant resume would be a waste of a round-trip and
// could race with real turn traffic.
func TestStartSessionTriggersCodexReconcileSkipsResumeOnAlive(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)
	threadID := seedCodexThread(t, st, "thread-reconcile-alive")

	var resumeCalls atomic.Int32
	installFakeCodexSessionWithResume(t, a, threadID,
		codexProbeStub{status: codex.ThreadStatusIdle},
		func(_ context.Context) error {
			resumeCalls.Add(1)
			return nil
		},
	)

	a.reconcileCodexAfterStart(threadID)

	if got := resumeCalls.Load(); got != 0 {
		t.Fatalf("resumeCalls = %d, want 0 on idle probe", got)
	}
}

// TestStartSessionTriggersCodexReconcileSkipsResumeOnSystemError covers
// systemError: the reconciler flips persisted rows itself, and there is
// nothing to resume — the session is terminally broken. Resume would
// just error back; best to not call it.
func TestStartSessionTriggersCodexReconcileSkipsResumeOnSystemError(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)
	threadID := seedCodexThread(t, st, "thread-reconcile-systemerr")

	// Seed a running bg row so the flip has something to mutate; this is
	// the headline behaviour of ReconcileCodexOnReopen already covered
	// elsewhere, but having a row present confirms we exercised the full
	// systemError branch rather than the fast-path out.
	_ = seedRunningBackgroundTool(t, st, threadID, "tool-bg-systemerr")

	var resumeCalls atomic.Int32
	installFakeCodexSessionWithResume(t, a, threadID,
		codexProbeStub{status: codex.ThreadStatusSystemError},
		func(_ context.Context) error {
			resumeCalls.Add(1)
			return nil
		},
	)

	a.reconcileCodexAfterStart(threadID)

	if got := resumeCalls.Load(); got != 0 {
		t.Fatalf("resumeCalls = %d, want 0 on systemError probe", got)
	}
}

// TestReconcileCodexOnStart_FlipsGhostBackgroundRows covers the Phase-4
// ghost-flip contract: every persisted `is_background=1 AND
// status='running'` tool_call for a Codex thread must be flipped to
// errored/lost with a " — session ended" summary suffix on the next
// session start. This is session-start unconditional — it runs for
// every new or resumed Codex session because a prior subprocess dying
// takes its PTYs with it regardless of what the probe would report.
func TestReconcileCodexOnStart_FlipsGhostBackgroundRows(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-ghost-flip")
	runningID := seedRunningBackgroundTool(t, st, threadID, "tool-bg-ghost")

	// Observe the provider:item_event upsert. Phase-4's flip should
	// fan out one emit per flipped row so the tray subscribers update
	// without waiting for the next event cycle.
	var emitted []store.Item
	a.testEmitHook = func(name string, data any) {
		if name != "provider:item_event" {
			return
		}
		if item, ok := itemFromItemStreamEvent(data); ok {
			emitted = append(emitted, item)
		}
	}

	a.flipCodexGhostBackgroundRowsOnStart(threadID)

	running := getItem(t, st, runningID)
	if running.Status != "errored" {
		t.Fatalf("running row status = %q, want errored", running.Status)
	}
	if running.Decision != "lost" {
		t.Fatalf("running row decision = %q, want lost", running.Decision)
	}
	if !strings.HasSuffix(running.Summary, " — session ended") {
		t.Fatalf("running row summary = %q, want ' — session ended' suffix", running.Summary)
	}

	if len(emitted) != 1 {
		t.Fatalf("provider:item_event upsert emitted %d times, want 1", len(emitted))
	}
	if emitted[0].ID != runningID {
		t.Fatalf("emitted item id = %q, want %q", emitted[0].ID, runningID)
	}
	if emitted[0].Status != "errored" {
		t.Fatalf("emitted item status = %q, want errored (post-flip state)", emitted[0].Status)
	}
}

// TestReconcileCodexOnStart_LeavesForegroundRunningRowsAlone pins the
// scope of the Phase-4 ghost flip: it MUST NOT touch non-background
// running rows. Those are the existing reconciler's concern (the
// forceCloseOrphanToolCalls safety net kicks in at turn-complete time;
// the systemError probe branch of ReconcileCodexOnReopen handles the
// truly-lost case). Phase-4's flip widens the "what flips on start"
// only for backgrounded rows, not inline ones.
func TestReconcileCodexOnStart_LeavesForegroundRunningRowsAlone(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-ghost-flip-foreground")
	inlineID := seedRunningInlineTool(t, st, threadID, "tool-inline-fg-run")
	completedBgID := seedCompletedBackgroundTool(t, st, threadID, "tool-bg-fg-done")

	a.flipCodexGhostBackgroundRowsOnStart(threadID)

	inline := getItem(t, st, inlineID)
	if inline.Status != "running" {
		t.Fatalf("foreground running row flipped by Phase-4 ghost flip: status = %q", inline.Status)
	}
	if inline.Decision != "" {
		t.Fatalf("foreground running row picked up decision: %q", inline.Decision)
	}

	completed := getItem(t, st, completedBgID)
	if completed.Status != "completed" {
		t.Fatalf("already-completed bg row flipped: status = %q", completed.Status)
	}
}

// TestReconcileCodexOnStart_ClaudeThreadsUntouched pins the provider-
// scope guard in startSessionNow: the Phase-4 ghost flip runs ONLY on
// the `t.Provider == codex` branch. Claude's background-task lifecycle
// uses `stop_task` / natural completion as the signal; a Claude
// subprocess restart doesn't invalidate running backgrounded Bash tasks
// the same way a Codex subprocess restart invalidates PTYs.
//
// The test exercises the scoping decision directly: a Claude thread
// with a seeded backgrounded row is cross-thread-verified to retain
// `status='running'` after we exercise the Codex-only flip helper for a
// DIFFERENT thread. A regression that broadened the flip's SQL scope
// beyond the supplied threadID would also flip the unrelated Claude
// row and fail this test.
func TestReconcileCodexOnStart_ClaudeThreadsUntouched(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	// Seed a Claude thread with a backgrounded Bash launch (status
	// running) — exactly what a Claude run_in_background Bash row looks
	// like between the launch and its task_updated terminal. Phase 4
	// must not flip this on any Codex session start for an UNRELATED
	// thread.
	claudeThreadID := "thread-ghost-flip-claude"
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            claudeThreadID,
		ProjectID:     codexReconcileTestProjectID,
		Title:         "Claude bg",
		Provider:      "claude",
		WorkspacePath: "/tmp/claude",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	claudeBgID := seedRunningBackgroundTool(t, st, claudeThreadID, "tool-bg-claude")

	// Run the Codex flip for a different (and in this case empty) Codex
	// thread. If the SQL scope is right (threadID-specific), the Claude
	// row is untouched; if the scope were broadened to "all rows in all
	// threads", the Claude row would flip.
	otherCodex := seedCodexThread(t, st, "thread-ghost-flip-scoping-codex")
	a.flipCodexGhostBackgroundRowsOnStart(otherCodex)

	claudeRunning := getItem(t, st, claudeBgID)
	if claudeRunning.Status != "running" {
		t.Fatalf("claude bg row status = %q, want running (flip must be scoped per-thread)", claudeRunning.Status)
	}
	if claudeRunning.Decision != "" {
		t.Fatalf("claude bg row decision = %q, want empty", claudeRunning.Decision)
	}
}

// TestReconcileCodexOnStart_IdempotentAcrossRepeatedStarts pins the
// suffix-idempotency contract. A row that already carries the
// " — session ended" suffix must not accumulate duplicates on a second
// flip (startup → flip → Codex replays item/started to running →
// subprocess crashes again → next startup → flip again).
func TestReconcileCodexOnStart_IdempotentAcrossRepeatedStarts(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-ghost-flip-idempotent")
	runningID := seedRunningBackgroundTool(t, st, threadID, "tool-bg-idempotent")

	a.flipCodexGhostBackgroundRowsOnStart(threadID)

	// Simulate the warm-reconnect re-upsert by putting the row back to
	// running with the already-suffixed summary (the real path uses
	// UpsertItem; we only need the state shape for this idempotency
	// check).
	flipped := getItem(t, st, runningID)
	flipped.Status = "running"
	flipped.Decision = ""
	flipped.UpdatedAt = time.Now().UnixMilli()
	if _, err := st.UpsertItem(flipped, nil); err != nil {
		t.Fatalf("re-upsert to running: %v", err)
	}

	a.flipCodexGhostBackgroundRowsOnStart(threadID)

	final := getItem(t, st, runningID)
	// The summary must contain the suffix exactly once.
	suffix := " — session ended"
	idx := strings.Index(final.Summary, suffix)
	if idx < 0 {
		t.Fatalf("final summary missing suffix: %q", final.Summary)
	}
	if strings.Index(final.Summary[idx+len(suffix):], suffix) >= 0 {
		t.Fatalf("final summary accumulated duplicate suffix: %q", final.Summary)
	}
}

// TestReconcileCodexOnStart_EmptyThreadNoOp pins the no-op path: a
// Codex thread with zero ghost rows must not bump threads.updated_at
// or emit anything. The WAL commit is kept cheap but the visible state
// stays put.
func TestReconcileCodexOnStart_EmptyThreadNoOp(t *testing.T) {
	st := storetest.Clone(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-ghost-flip-empty")

	var emitted []any
	a.testEmitHook = func(name string, data any) {
		emitted = append(emitted, data)
	}

	thread, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	beforeTouch := thread.UpdatedAt

	a.flipCodexGhostBackgroundRowsOnStart(threadID)

	if len(emitted) != 0 {
		t.Fatalf("empty-thread flip emitted %d events, want 0", len(emitted))
	}

	after, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread after flip: %v", err)
	}
	if after.UpdatedAt != beforeTouch {
		t.Fatalf("empty-thread flip bumped updated_at: before=%d after=%d", beforeTouch, after.UpdatedAt)
	}
}
