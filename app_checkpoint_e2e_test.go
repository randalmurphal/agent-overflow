package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// e2eTestApp pairs a fresh App with a test-emitting triage router. The
// real App wires `a.emitWithReplay()`, which reaches for Wails + telemetry
// services that aren't stood up in tests; we thread our own emit func that
// collects emissions for assertion.
type e2eTestApp struct {
	app       *App
	emissions *[]testEmission
	emissLock *sync.Mutex
}

type testEmission struct {
	name string
	data any
}

// newE2EApp builds an App wired to a real checkpoint store, real SQLite,
// and a triage.Router whose emitter records events into a thread-safe slice.
func newE2EApp(t *testing.T) *e2eTestApp {
	t.Helper()

	app := newTestAppWithStore(t)
	app.checkpoints = checkpoint.NewStore()

	var emissions []testEmission
	var lock sync.Mutex
	emit := func(name string, data any) {
		lock.Lock()
		emissions = append(emissions, testEmission{name, data})
		lock.Unlock()
	}

	app.triage = triage.NewRouter(app.store, emit)
	app.triage.SetCheckpointStore(app.checkpoints)

	return &e2eTestApp{app: app, emissions: &emissions, emissLock: &lock}
}

func (e *e2eTestApp) snapshotEmissions() []testEmission {
	e.emissLock.Lock()
	defer e.emissLock.Unlock()
	out := make([]testEmission, len(*e.emissions))
	copy(out, *e.emissions)
	return out
}

// initE2ERepo creates a git repo at the given directory, matching the
// fixture used by the store-level integration tests.
func initE2ERepo(t *testing.T, dir string) {
	t.Helper()
	runE2EGit(t, dir, "init", "-q", "-b", "main")
	writeE2EFile(t, dir, "README", "hello\n")
	runE2EGit(t, dir, "add", "-A")
	runE2EGit(t, dir,
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "init")
}

func runE2EGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@test",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeE2EFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// seedE2EThread creates a thread row pointing at the given workspace.
func seedE2EThread(t *testing.T, app *App, id, workspace string, p provider.ProviderKind) store.Thread {
	t.Helper()
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "e2e",
		Provider:      string(p),
		WorkspacePath: workspace,
		Model:         "test",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// captureE2E runs a real CaptureBaseline through the App's store and
// persists the bookkeeping row, mirroring what the triage router does on
// EventTurnStart. Used by tests that don't drive the router directly.
func captureE2E(t *testing.T, app *App, threadID, workspace string, turn int, toolPaths ...string) store.Checkpoint {
	t.Helper()
	ref, err := app.checkpoints.CaptureBaseline(context.Background(), workspace, threadID, turn)
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}
	record := store.Checkpoint{
		ID:                  uuid.NewString(),
		ThreadID:            threadID,
		TurnIndex:           turn,
		CheckpointTurnCount: turn,
		RefName:             ref,
		ToolPaths:           toolPaths,
		CapturedAt:          time.Now().UnixMilli(),
		WorkspacePath:       workspace,
	}
	if err := app.store.SaveCheckpoint(record); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	return record
}

// findEmission returns the first emission matching name, or nil if absent.
func findEmission(events []testEmission, name string) *testEmission {
	for i := range events {
		if events[i].name == name {
			return &events[i]
		}
	}
	return nil
}

// countEmissionsNamed returns how many of the given emission name appear.
func countEmissionsNamed(events []testEmission, name string) int {
	n := 0
	for _, e := range events {
		if e.name == name {
			n++
		}
	}
	return n
}

// -- Tests --

// #21 — Turn-start on a git-backed thread lands a checkpoint row in SQLite
// and a git ref in the workspace.
func TestAppE2E_CheckpointCapturedOnTurnStart(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	evt := provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}
	if err := e.app.triage.Handle(evt); err != nil {
		t.Fatalf("handle: %v", err)
	}

	rows, err := e.app.store.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 checkpoint row, got %d", len(rows))
	}
	has, err := e.app.checkpoints.HasCheckpointRef(context.Background(), workspace, rows[0].RefName)
	if err != nil {
		t.Fatalf("has ref: %v", err)
	}
	if !has {
		t.Errorf("expected git ref %s to resolve", rows[0].RefName)
	}

	emissions := e.snapshotEmissions()
	if e := findEmission(emissions, "checkpoint:captured"); e == nil {
		t.Errorf("expected checkpoint:captured emission; got: %+v", emissions)
	}
}

// #22 — Non-git thread emits checkpoint:unavailable and doesn't capture.
func TestAppE2E_NonGitThreadEmitsUnavailable(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir() // NOT a git repo
	seedE2EThread(t, e.app, "t1", workspace, provider.Claude)

	if err := e.app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	rows, _ := e.app.store.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("expected 0 checkpoint rows on non-git workspace, got %d", len(rows))
	}

	emissions := e.snapshotEmissions()
	if e := findEmission(emissions, "checkpoint:unavailable"); e == nil {
		t.Errorf("expected checkpoint:unavailable emission; got: %+v", emissions)
	}
	if e := findEmission(emissions, "checkpoint:captured"); e != nil {
		t.Errorf("should not emit checkpoint:captured on non-git workspace")
	}
}

// e2eErroringStore wraps the real checkpoint store but forces CaptureBaseline
// to fail. Used to drive the `checkpoint:error` path without engineering a
// flaky filesystem condition.
type e2eErroringStore struct {
	inner *checkpoint.Store
}

func (s *e2eErroringStore) IsGitRepository(ctx context.Context, workspace string) bool {
	return s.inner.IsGitRepository(ctx, workspace)
}
func (s *e2eErroringStore) CaptureBaseline(ctx context.Context, workspace, threadID string, turn int) (string, error) {
	return "", errors.New("forced capture failure")
}
func (s *e2eErroringStore) DeleteRef(ctx context.Context, workspace, ref string) error {
	return s.inner.DeleteRef(ctx, workspace, ref)
}
func (s *e2eErroringStore) DiffRefToRef(ctx context.Context, workspace, fromRef, toRef string) ([]byte, error) {
	return s.inner.DiffRefToRef(ctx, workspace, fromRef, toRef)
}
func (s *e2eErroringStore) DiffRefToRefSummary(ctx context.Context, workspace, fromRef, toRef string) ([]diffsummary.File, error) {
	return s.inner.DiffRefToRefSummary(ctx, workspace, fromRef, toRef)
}

// #23 — A CaptureBaseline failure must emit checkpoint:error, must NOT
// persist a checkpoint row, and must NOT return an error from Handle (turn
// must proceed).
func TestAppE2E_CheckpointErrorEmitted(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	// Override the real store with one that forces a capture error.
	e.app.triage.SetCheckpointStore(&e2eErroringStore{inner: e.app.checkpoints})

	if err := e.app.triage.Handle(provider.ProviderEvent{
		Kind:     provider.EventTurnStart,
		ThreadID: "t1",
	}); err != nil {
		t.Errorf("Handle should swallow capture errors so the turn proceeds; got: %v", err)
	}

	rows, _ := e.app.store.ListCheckpoints("t1")
	if len(rows) != 0 {
		t.Errorf("expected no checkpoint row after forced failure, got %d", len(rows))
	}

	emissions := e.snapshotEmissions()
	if e := findEmission(emissions, "checkpoint:error"); e == nil {
		t.Errorf("expected checkpoint:error emission; got: %+v", emissions)
	}
}

// #24 — Repeated TurnStart events for the same (thread, turn) must not
// create duplicate checkpoint refs or rows.
func TestAppE2E_DuplicateCaptureDedupedPerTurn(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	for i := 0; i < 4; i++ {
		if err := e.app.triage.Handle(provider.ProviderEvent{
			Kind:     provider.EventTurnStart,
			ThreadID: "t1",
		}); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	rows, err := e.app.store.ListCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 checkpoint row after 4 turn_start events, got %d", len(rows))
	}

	refs, err := e.app.checkpoints.ListThreadRefs(context.Background(), workspace, "t1")
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("expected 1 checkpoint ref, got %d: %v", len(refs), refs)
	}
}

// #25 — GetCheckpointRangeDiff between adjacent captured turns returns the diff between
// their trees.
func TestAppE2E_GetCheckpointRangeDiffAdjacentTurns(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	captureE2E(t, e.app, "t1", workspace, 0)
	writeE2EFile(t, workspace, "README", "turn 1 edit\n")
	captureE2E(t, e.app, "t1", workspace, 1)

	diff, err := e.app.GetCheckpointRangeDiff("t1", 0, 1)
	if err != nil {
		t.Fatalf("get checkpoint range diff: %v", err)
	}
	if !strings.Contains(diff, "+turn 1 edit") {
		t.Errorf("expected +turn 1 edit in diff; got:\n%s", diff)
	}
}

// #29 — conversation-and-files revert on a Claude thread clears the session ref and restores
// the worktree in one atomic operation. Claude has no wire-level rollback
// primitive, so the conversation side of "both" is expressed by clearing
// SessionRef — the next turn spawns a fresh session with no provider-side
// context. The old Claude session file on disk is intentionally left alone.
func TestAppE2E_RevertToCheckpointConversationAndFilesOnClaudeClearsSessionAndRestores(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	thread := seedE2EThread(t, e.app, "t-claude", workspace, provider.Claude)
	thread.SessionRef = "claude-session-before"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	// Turn 0 = baseline; turn 1 = agent edits README. The path-scoped
	// restore consults turn-1's tool_paths to decide what to roll back.
	captureE2E(t, e.app, "t-claude", workspace, 0)
	writeE2EFile(t, workspace, "README", "agent-edited\n")
	captureE2E(t, e.app, "t-claude", workspace, 1, "README")
	writeE2EFile(t, workspace, "README", "dirty\n")

	if err := e.app.RevertToCheckpoint("t-claude", 0, RevertModeConversationAndFiles); err != nil {
		t.Fatalf("revert checkpoint on Claude: %v", err)
	}

	got, _ := e.app.store.GetThread("t-claude")
	if got.SessionRef != "" {
		t.Errorf("SessionRef should be cleared after revert-both, got %q", got.SessionRef)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "README"))
	if string(data) != "hello\n" {
		t.Errorf("README should be restored; got %q", data)
	}
}

// #30 — revert-both on a Codex thread restores the worktree. The
// conversation rollback against the live provider is covered by the
// tests in internal/provider/codex; this test verifies the app-layer
// plumbing calls RestoreWorktreePaths after the provider call.
//
// Because the test doesn't spin up a real codex binary, the provider
// call is skipped by having no active session and no SessionRef — the
// app layer short-circuits out of rollbackCodexThread's temp-resume
// branch. Full provider-integrated coverage lives under the test matrix
// task (see docs for the missing-pieces backlog).
func TestAppE2E_RevertToCheckpointConversationAndFilesOnCodexPropagatesMissingSessionRef(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	captureE2E(t, e.app, "t-codex", workspace, 0)
	writeE2EFile(t, workspace, "README", "dirty\n")
	writeE2EFile(t, workspace, "junk.txt", "added after checkpoint\n")

	// Seed no items so last turn is 0 — numTurns would be 0+1-0=1, but
	// we bypass the provider-call branch because SessionRef is empty and
	// no live session is registered. rollbackCodexThread returns a
	// "missing codex thread reference" error in that case, which we
	// assert propagates cleanly.
	err := e.app.RevertToCheckpoint("t-codex", 0, RevertModeConversationAndFiles)
	if err == nil {
		t.Fatal("expected rollback error when SessionRef is empty")
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Errorf("error should mention codex; got: %v", err)
	}
	// Worktree was NOT restored because the revert aborted before the
	// code-side step. This is the desired failure ordering — don't
	// partially mutate state when the provider side fails.
	data, _ := os.ReadFile(filepath.Join(workspace, "README"))
	if string(data) != "dirty\n" {
		t.Errorf("worktree must not be restored when provider rollback fails; got %q", data)
	}
	if _, err := os.Stat(filepath.Join(workspace, "junk.txt")); err != nil {
		t.Errorf("junk.txt should still exist when revert aborted; stat err: %v", err)
	}
}

// writeMockCodexRollbackBinary writes a bash script that mimics the Codex
// app-server just enough for an in-place rollback test. Handles the JSON-RPC
// methods revert-conversation/revert-both need: initialize, thread/resume,
// thread/rollback. When thread/rollback fires, the params line is appended
// to sentinel so the test can verify what the app sent.
func writeMockCodexRollbackBinary(t *testing.T, sentinel string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then continue; fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/resume"'; then
        # Echo back whatever threadId was requested so downstream calls
        # (like thread/rollback) use the client-visible id rather than a
        # synthetic server-assigned one.
        tid=$(echo "$line" | grep -o '"threadId":"[^"]*"' | head -1 | sed 's/"threadId":"//;s/"$//')
        if [ -z "$tid" ]; then tid="mock-codex-t1"; fi
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"$tid\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/rollback"'; then
        # Capture the request params for the test to inspect.
        printf '%%s\n' "$line" >> %s
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-codex-t1\",\"turns\":[]}}}"
        continue
    fi
done
`, shellQuoteForTest(sentinel))
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-rollback-mock.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}

// seedCodexItems populates a fake timeline across the given turn indices.
// Each item is a simple text/user item — enough to exercise the truncation
// logic without dragging in the full item-kind surface.
func seedCodexItems(t *testing.T, app *App, threadID string, turnIndices ...int) {
	t.Helper()
	base := time.Now().UnixMilli()
	for i, turn := range turnIndices {
		if err := app.store.InsertItem(store.Item{
			ID:        fmt.Sprintf("%s-item-%d", threadID, i),
			ThreadID:  threadID,
			TurnIndex: turn,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   fmt.Sprintf("turn-%d", turn),
			CreatedAt: base + int64(i),
		}); err != nil {
			t.Fatalf("insert item t%d: %v", turn, err)
		}
	}
}

// #31 — revert-conversation on a Codex thread must call thread/rollback on
// the provider with the correct numTurns, truncate SQLite items, and preserve
// SessionRef + worktree. Uses a bash-mock Codex binary that records the
// rollback request to a sentinel file.
func TestAppE2E_RevertToCheckpointConversationOnlyOnCodexCallsRollback(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	sentinel := filepath.Join(t.TempDir(), "rollback-params.txt")
	binary := writeMockCodexRollbackBinary(t, sentinel)
	e.app.settings = settings.NewService(t.TempDir())
	if _, err := e.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread := seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	thread.SessionRef = "stored-codex-thread-id"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	// Timeline: turns 0, 1, 2. Reverting to turn 1 drops items at 1 and 2 →
	// numTurns = lastTurn + 1 - targetTurn = 3 - 1 = 2.
	seedCodexItems(t, e.app, "t-codex", 0, 1, 2)
	captureE2E(t, e.app, "t-codex", workspace, 0)
	captureE2E(t, e.app, "t-codex", workspace, 1)

	// Dirty the workspace post-capture so the test can prove revert-conversation
	// leaves it alone.
	writeE2EFile(t, workspace, "README", "user-edit-after-capture\n")

	if err := e.app.RevertToCheckpoint("t-codex", 1, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert checkpoint: %v", err)
	}

	// Sentinel must contain the rollback request with numTurns=2.
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(data), `"numTurns":2`) {
		t.Errorf("expected numTurns=2 in rollback params; got %q", string(data))
	}
	if !strings.Contains(string(data), `"threadId":"stored-codex-thread-id"`) {
		t.Errorf("expected threadId=stored-codex-thread-id in rollback params; got %q", string(data))
	}

	// SessionRef survives — Codex keeps the session; the rollback only prunes
	// history server-side.
	got, _ := e.app.store.GetThread("t-codex")
	if got.SessionRef != "stored-codex-thread-id" {
		t.Errorf("SessionRef should survive revert-conversation; got %q", got.SessionRef)
	}

	// Items truncated to turn 0.
	items, err := e.app.store.ListItems("t-codex")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 || items[0].TurnIndex != 0 {
		t.Errorf("expected only turn 0 to survive; got %+v", items)
	}

	// Worktree NOT touched.
	if wd, _ := os.ReadFile(filepath.Join(workspace, "README")); string(wd) != "user-edit-after-capture\n" {
		t.Errorf("worktree must not be restored on revert-conversation; got %q", wd)
	}
}

// #31b — revert-both on a Codex thread calls thread/rollback AND restores
// the worktree from the checkpoint.
func TestAppE2E_RevertToCheckpointConversationAndFilesOnCodexFullRoundTrip(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	sentinel := filepath.Join(t.TempDir(), "rollback-params.txt")
	binary := writeMockCodexRollbackBinary(t, sentinel)
	e.app.settings = settings.NewService(t.TempDir())
	if _, err := e.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread := seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	thread.SessionRef = "stored-codex-id"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	seedCodexItems(t, e.app, "t-codex", 0, 1)
	// Turn 0 = baseline. Turn 1's tool_paths records README and junk.txt
	// — the agent's two edits — so the path-scoped restore knows what to
	// roll back. Turn 2 (the last seeded item turn) has no checkpoint
	// row in this test setup; numTurns rolls all the way to baseline.
	captureE2E(t, e.app, "t-codex", workspace, 0)
	writeE2EFile(t, workspace, "README", "agent-modified\n")
	writeE2EFile(t, workspace, "junk.txt", "agent-added\n")
	captureE2E(t, e.app, "t-codex", workspace, 1, "README", "junk.txt")
	writeE2EFile(t, workspace, "README", "dirty-tree\n")

	if err := e.app.RevertToCheckpoint("t-codex", 0, RevertModeConversationAndFiles); err != nil {
		t.Fatalf("revert checkpoint: %v", err)
	}

	// Rollback was issued with numTurns = 0+1 - 0 via lastTurn+1-target = 2.
	data, _ := os.ReadFile(sentinel)
	if !strings.Contains(string(data), `"numTurns":2`) {
		t.Errorf("expected numTurns=2 (drop turns 0 and 1); got %q", string(data))
	}

	// Worktree restored.
	if wd, _ := os.ReadFile(filepath.Join(workspace, "README")); string(wd) != "hello\n" {
		t.Errorf("README should be restored; got %q", wd)
	}
	if _, err := os.Stat(filepath.Join(workspace, "junk.txt")); err == nil {
		t.Errorf("junk.txt added after capture should have been removed on revert-both")
	}

	// Items gone.
	items, _ := e.app.store.ListItems("t-codex")
	if len(items) != 0 {
		t.Errorf("revert-both to turn 0 should drop all items; got %d", len(items))
	}
}

// #31c — If the Codex provider call fails, revert aborts before touching the
// worktree or truncating items. The failure ordering is deliberate: don't
// leave a half-reverted state the user can't recover from.
func TestAppE2E_RevertToCheckpointProviderFailureAbortsWorktree(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	// Binary that refuses to initialize — guaranteed to fail the temp-session
	// handshake so rollbackCodexThread returns an error.
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex-broken.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/bash\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write broken binary: %v", err)
	}
	e.app.settings = settings.NewService(t.TempDir())
	if _, err := e.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread := seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	thread.SessionRef = "stored-codex-id"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	seedCodexItems(t, e.app, "t-codex", 0, 1)
	captureE2E(t, e.app, "t-codex", workspace, 0)
	writeE2EFile(t, workspace, "README", "dirty\n")

	err := e.app.RevertToCheckpoint("t-codex", 0, RevertModeConversationAndFiles)
	if err == nil {
		t.Fatal("expected provider error to propagate")
	}

	// Worktree untouched (no restore happened).
	if wd, _ := os.ReadFile(filepath.Join(workspace, "README")); string(wd) != "dirty\n" {
		t.Errorf("worktree must not be restored when provider rollback fails; got %q", wd)
	}
	// Items untouched (truncation comes after provider call).
	items, _ := e.app.store.ListItems("t-codex")
	if len(items) != 2 {
		t.Errorf("items must survive a failed revert; got %d", len(items))
	}
}

// #31d — Revert target == last captured turn is a degenerate but valid case:
// numTurns = 1 (drop just the last turn). The provider rollback must still
// be issued.
func TestAppE2E_RevertToCheckpointDropsLastTurn(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	sentinel := filepath.Join(t.TempDir(), "rollback-params.txt")
	binary := writeMockCodexRollbackBinary(t, sentinel)
	e.app.settings = settings.NewService(t.TempDir())
	if _, err := e.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread := seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	thread.SessionRef = "stored-codex-id"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	seedCodexItems(t, e.app, "t-codex", 0, 1)
	captureE2E(t, e.app, "t-codex", workspace, 0)
	captureE2E(t, e.app, "t-codex", workspace, 1)

	// Revert to checkpoint count 1: keep turn 0, drop turn 1.
	if err := e.app.RevertToCheckpoint("t-codex", 1, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	data, _ := os.ReadFile(sentinel)
	if !strings.Contains(string(data), `"numTurns":1`) {
		t.Errorf("expected numTurns=1; got %q", string(data))
	}
	items, _ := e.app.store.ListItems("t-codex")
	if len(items) != 1 {
		t.Errorf("expected 1 item (turn 0) to survive; got %d", len(items))
	}
}

// #31e — Revert with target > last turn is a no-op for the provider (the
// server has nothing to drop) but still fires the SQL truncation path
// harmlessly (nothing to truncate).
func TestAppE2E_RevertToCheckpointBeyondLastTurnIsNoop(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	sentinel := filepath.Join(t.TempDir(), "rollback-params.txt")
	binary := writeMockCodexRollbackBinary(t, sentinel)
	e.app.settings = settings.NewService(t.TempDir())
	if _, err := e.app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread := seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)
	thread.SessionRef = "stored-codex-id"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	seedCodexItems(t, e.app, "t-codex", 0)
	captureE2E(t, e.app, "t-codex", workspace, 5) // target-future checkpoint

	// Revert to checkpoint count 5 when last turn is 0: no provider rollback
	// is needed.
	if err := e.app.RevertToCheckpoint("t-codex", 5, RevertModeConversationOnly); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		// If the sentinel was written at all, the mock got a rollback request,
		// which it should not have.
		data, _ := os.ReadFile(sentinel)
		if len(data) > 0 {
			t.Errorf("provider rollback should not be issued for out-of-range target; got: %q", data)
		}
	}
	// Items stay at turn 0.
	items, _ := e.app.store.ListItems("t-codex")
	if len(items) != 1 || items[0].TurnIndex != 0 {
		t.Errorf("expected item at turn 0 to survive; got %+v", items)
	}
}

// #31 — ListThreadCheckpoints returns rows ordered by turn_index ASC even
// when captures land out-of-order.
func TestAppE2E_ListThreadCheckpointsOrdersByTurn(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	// Insert captures out of chronological turn order.
	captureE2E(t, e.app, "t1", workspace, 2)
	writeE2EFile(t, workspace, "README", "v1\n")
	captureE2E(t, e.app, "t1", workspace, 0)
	writeE2EFile(t, workspace, "README", "v2\n")
	captureE2E(t, e.app, "t1", workspace, 1)

	list, err := e.app.ListThreadCheckpoints("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 checkpoints, got %d", len(list))
	}
	got := []int{list[0].TurnIndex, list[1].TurnIndex, list[2].TurnIndex}
	want := []int{0, 1, 2}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("list[%d].TurnIndex = %d, want %d (full: %v)", i, got[i], w, got)
		}
	}
}

// #32 — DeleteThread cleans up every checkpoint ref in the workspace AND
// drops the SQLite bookkeeping rows. Run with -race to prove there are no
// interleaving issues between ref deletion and row deletion.
//
// Flip-verification: if cleanupThreadCheckpoints ever stopped deleting refs
// (e.g. the order of delete-refs-vs-delete-thread got reversed and the
// workspace was no longer knowable after the row is gone), this test would
// fire because refs would remain.
func TestAppE2E_DeleteThreadRemovesAllCheckpointRefs(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	for i := 0; i < 3; i++ {
		if i > 0 {
			writeE2EFile(t, workspace, "README", fmt.Sprintf("turn %d\n", i))
		}
		captureE2E(t, e.app, "t1", workspace, i)
	}

	// Precondition: refs + rows both present.
	refsBefore, err := e.app.checkpoints.ListThreadRefs(context.Background(), workspace, "t1")
	if err != nil {
		t.Fatalf("list refs pre: %v", err)
	}
	if len(refsBefore) != 3 {
		t.Fatalf("expected 3 refs pre-delete, got %d", len(refsBefore))
	}
	rowsBefore, _ := e.app.store.ListCheckpoints("t1")
	if len(rowsBefore) != 3 {
		t.Fatalf("expected 3 rows pre-delete, got %d", len(rowsBefore))
	}

	if err := e.app.DeleteThread("t1"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	// Postcondition: both refs and rows are gone.
	refsAfter, err := e.app.checkpoints.ListThreadRefs(context.Background(), workspace, "t1")
	if err != nil {
		t.Fatalf("list refs post: %v", err)
	}
	if len(refsAfter) != 0 {
		t.Errorf("expected 0 refs post-delete, got %d: %v", len(refsAfter), refsAfter)
	}

	rowsAfter, _ := e.app.store.ListCheckpoints("t1")
	if len(rowsAfter) != 0 {
		t.Errorf("expected 0 rows post-delete, got %d", len(rowsAfter))
	}

	// Objects survive in git (refs are just pointers) — double-check by
	// asserting `git for-each-ref` returns nothing in our namespace. Guards
	// against a regression where CleanupThread partially succeeds.
	out, err := exec.Command("git", "-C", workspace, "for-each-ref",
		"refs/agent-overflow/checkpoints/").CombinedOutput()
	if err != nil {
		t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("expected no agent-overflow refs, got: %s", out)
	}
}

// #33 — Checkpoint capture interleaves with concurrent provider activity.
// Simulates a realistic provider by firing TurnStart events from multiple
// threads in parallel; the real triage router + real checkpoint store must
// handle all of them without corrupting state.
func TestAppE2E_CheckpointCaptureWithActiveProvider(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	const threadCount = 8
	ids := make([]string, threadCount)
	for i := 0; i < threadCount; i++ {
		ids[i] = fmt.Sprintf("t-active-%d", i)
		seedE2EThread(t, e.app, ids[i], workspace, provider.Codex)
	}

	// Inject a small amount of worktree churn to mimic an active provider
	// that's writing as captures happen.
	var mu sync.Mutex
	churn := func(tag string) {
		mu.Lock()
		defer mu.Unlock()
		writeE2EFile(t, workspace, fmt.Sprintf("churn-%s.txt", tag), tag)
	}

	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			churn(id)
			err := e.app.triage.Handle(provider.ProviderEvent{
				Kind:      provider.EventTurnStart,
				ThreadID:  id,
				Timestamp: time.Now(),
			})
			if err != nil {
				t.Errorf("handle %s: %v", id, err)
			}
		}(i, id)
	}
	wg.Wait()

	// Each thread should have exactly one checkpoint row + one ref.
	for _, id := range ids {
		rows, err := e.app.store.ListCheckpoints(id)
		if err != nil {
			t.Errorf("list rows for %s: %v", id, err)
			continue
		}
		if len(rows) != 1 {
			t.Errorf("%s: expected 1 checkpoint row, got %d", id, len(rows))
		}
		refs, err := e.app.checkpoints.ListThreadRefs(context.Background(), workspace, id)
		if err != nil {
			t.Errorf("list refs for %s: %v", id, err)
			continue
		}
		if len(refs) != 1 {
			t.Errorf("%s: expected 1 ref, got %d: %v", id, len(refs), refs)
		}
	}

	// Counts: every thread should have emitted exactly one
	// checkpoint:captured event, so the total equals threadCount.
	captured := countEmissionsNamed(e.snapshotEmissions(), "checkpoint:captured")
	if captured != threadCount {
		t.Errorf("expected %d checkpoint:captured emissions, got %d", threadCount, captured)
	}
}

// Headline test for the path-scoped revert. The agent edits one file in
// turn 1 and another in turn 2; the user manually edits an unrelated
// file between turns. After reverting to turn 0, the agent edits are
// rolled back but the manual edit survives. Mirrors the design intent:
// the revert is bounded to the agent.tool_paths set so manual workspace
// activity is preserved across turn boundaries.
func TestAppE2E_RevertConversationAndFilesPreservesManualEditsToOtherFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	thread := seedE2EThread(t, e.app, "t-paths", workspace, provider.Claude)
	thread.SessionRef = ""
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	for _, turn := range []int{0, 1, 2} {
		if err := e.app.store.InsertItem(store.Item{
			ID: "it-" + string(rune('0'+turn)), ThreadID: "t-paths",
			TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user",
			Summary: "x", CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Seed three pre-existing tracked files at baseline so each can be
	// edited and tracked independently. README ships with initE2ERepo.
	writeE2EFile(t, workspace, "a.txt", "a-baseline\n")
	writeE2EFile(t, workspace, "b.txt", "b-baseline\n")
	writeE2EFile(t, workspace, "c.txt", "c-baseline\n")
	runE2EGit(t, workspace, "add", "-A")
	runE2EGit(t, workspace,
		"-c", "user.email=tester@test", "-c", "user.name=Tester",
		"commit", "-q", "-m", "seed")

	// Turn 0 baseline.
	captureE2E(t, e.app, "t-paths", workspace, 0)

	// Turn 1: agent rewrites a.txt.
	writeE2EFile(t, workspace, "a.txt", "agent-modified-a\n")
	captureE2E(t, e.app, "t-paths", workspace, 1, "a.txt")

	// User manually edits b.txt between turns. Not tracked in any
	// tool_paths because the agent did not write it.
	writeE2EFile(t, workspace, "b.txt", "user-manual-b\n")

	// Turn 2: agent rewrites c.txt.
	writeE2EFile(t, workspace, "c.txt", "agent-modified-c\n")
	captureE2E(t, e.app, "t-paths", workspace, 2, "c.txt")

	if err := e.app.RevertToCheckpoint("t-paths", 0, RevertModeConversationAndFiles); err != nil {
		t.Fatalf("revert: %v", err)
	}

	got := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(workspace, name))
		return string(data)
	}
	if got("a.txt") != "a-baseline\n" {
		t.Errorf("a.txt should be rolled back to baseline; got %q", got("a.txt"))
	}
	if got("c.txt") != "c-baseline\n" {
		t.Errorf("c.txt should be rolled back to baseline; got %q", got("c.txt"))
	}
	if got("b.txt") != "user-manual-b\n" {
		t.Errorf("b.txt should retain the user manual edit; got %q", got("b.txt"))
	}
}

