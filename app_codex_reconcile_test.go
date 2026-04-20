package main

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
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
	st := newMemoryStore(t)
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
	st := newMemoryStore(t)
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
	st := newMemoryStore(t)
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
	st := newMemoryStore(t)
	a := newAppWithStore(t, st)

	threadID := seedCodexThread(t, st, "thread-no-session")

	_, err := a.ReconcileCodexOnReopen(context.Background(), threadID)
	if err == nil {
		t.Fatal("expected error when thread has no active session")
	}
}

// --- helpers ---

func newMemoryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

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

// (Unused suppression — the provider import is referenced indirectly
// via the codex subpackage for type clarity in signatures.)
var _ provider.EventKind = provider.EventInit
