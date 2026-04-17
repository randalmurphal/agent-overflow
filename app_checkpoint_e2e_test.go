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
	"agent-overflow/internal/provider"
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
		ID:              id,
		Title:           "e2e",
		Provider:        string(p),
		WorkspacePath:   workspace,
		Model:           "test",
		InteractionMode: "default",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// captureE2E runs a real CaptureBaseline through the App's store and
// persists the bookkeeping row, mirroring what the triage router does on
// EventTurnStart. Used by tests that don't drive the router directly.
func captureE2E(t *testing.T, app *App, threadID, workspace string, turn int) store.Checkpoint {
	t.Helper()
	ref, err := app.checkpoints.CaptureBaseline(context.Background(), workspace, threadID, turn)
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}
	record := store.Checkpoint{
		ID:            uuid.NewString(),
		ThreadID:      threadID,
		TurnIndex:     turn,
		RefName:       ref,
		CapturedAt:    time.Now().UnixMilli(),
		WorkspacePath: workspace,
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

// #25 — GetTurnDiff between adjacent captured turns returns the diff between
// their trees.
func TestAppE2E_GetTurnDiffAdjacentTurns(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	captureE2E(t, e.app, "t1", workspace, 0)
	writeE2EFile(t, workspace, "README", "turn 1 edit\n")
	captureE2E(t, e.app, "t1", workspace, 1)

	diff, err := e.app.GetTurnDiff("t1", 0)
	if err != nil {
		t.Fatalf("get turn diff: %v", err)
	}
	if !strings.Contains(diff, "+turn 1 edit") {
		t.Errorf("expected +turn 1 edit in diff; got:\n%s", diff)
	}
}

// #26 — GetTurnDiff for the latest turn falls back to checkpoint→worktree.
func TestAppE2E_GetTurnDiffLatestTurnUsesWorktree(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	captureE2E(t, e.app, "t1", workspace, 0)
	writeE2EFile(t, workspace, "README", "live edits only\n")
	// No second checkpoint: turn 0 is the latest.

	diff, err := e.app.GetTurnDiff("t1", 0)
	if err != nil {
		t.Fatalf("get turn diff: %v", err)
	}
	if !strings.Contains(diff, "+live edits only") {
		t.Errorf("expected live worktree diff against turn 0; got:\n%s", diff)
	}
}

// #27 — GetCheckpointToWorktreeDiff against an earlier captured turn shows
// *all* changes accumulated across subsequent turns + worktree drift.
func TestAppE2E_GetCheckpointToWorktreeDiffAfterManyChanges(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t1", workspace, provider.Codex)

	// Turn 0: pristine README.
	captureE2E(t, e.app, "t1", workspace, 0)
	// Turn 1: add file.
	writeE2EFile(t, workspace, "turn1.txt", "one\n")
	captureE2E(t, e.app, "t1", workspace, 1)
	// Turn 2: modify README.
	writeE2EFile(t, workspace, "README", "hello\nturn 2\n")
	captureE2E(t, e.app, "t1", workspace, 2)
	// Worktree-only change AFTER the latest turn.
	writeE2EFile(t, workspace, "live.txt", "uncommitted\n")

	diff, err := e.app.GetCheckpointToWorktreeDiff("t1", 0)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	// Must include every change made since turn 0.
	for _, token := range []string{"turn1.txt", "turn 2", "live.txt"} {
		if !strings.Contains(diff, token) {
			t.Errorf("expected %q in checkpoint→worktree diff; got:\n%s", token, diff)
		}
	}
}

// #28 — RevertToTurn(fork) creates a new thread forked from the source.
// Per the current implementation (app_thread_fork.go): the fork clones ALL
// of the source's items, not items-up-to-turn-N. The source thread is left
// untouched. The returned ID is NEW and resolvable.
//
// Flip-verification: if the fork ever silently returned the SAME thread id
// (a regression that would collapse fork into no-op), the newID == source
// assertion below would fail, as would the retrieval asserting a new row.
func TestAppE2E_RevertToTurnForkCreatesChildThread(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)

	// Claude thread with SessionRef set: exercises the "pending fork ref"
	// branch of resolveForkResumeState (no need for a real Codex binary).
	thread := seedE2EThread(t, e.app, "t-src", workspace, provider.Claude)
	thread.SessionRef = "claude-session-abc"
	if err := e.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}

	// ForkThread requires at least one item to prevent creating orphan forks.
	for i, body := range []string{"hello", "world"} {
		if err := e.app.store.InsertItem(store.Item{
			ID:        fmt.Sprintf("item-%d", i),
			ThreadID:  "t-src",
			TurnIndex: i,
			ItemIndex: 0,
			Kind:      "text",
			Role:      "user",
			Summary:   body,
			CreatedAt: time.Now().UnixMilli() + int64(i),
		}); err != nil {
			t.Fatalf("insert item %d: %v", i, err)
		}
	}

	// Capture turns 0 and 1 so the revert has a legitimate target.
	captureE2E(t, e.app, "t-src", workspace, 0)
	writeE2EFile(t, workspace, "README", "turn 1\n")
	captureE2E(t, e.app, "t-src", workspace, 1)

	newID, err := e.app.RevertToTurn("t-src", 0, "fork")
	if err != nil {
		t.Fatalf("revert fork: %v", err)
	}
	if newID == "" {
		t.Fatal("fork returned empty thread id")
	}
	if newID == "t-src" {
		t.Errorf("fork returned source id — expected a NEW thread")
	}

	// New thread exists.
	forked, err := e.app.store.GetThread(newID)
	if err != nil {
		t.Fatalf("forked thread not retrievable: %v", err)
	}
	if forked.ForkedFromThreadID != "t-src" {
		t.Errorf("expected ForkedFromThreadID=t-src, got %q", forked.ForkedFromThreadID)
	}

	// Source thread survives untouched.
	src, err := e.app.store.GetThread("t-src")
	if err != nil {
		t.Fatalf("source thread gone: %v", err)
	}
	if src.Archived {
		t.Errorf("source thread should not have been archived")
	}

	// Workspace itself must NOT have been restored. Turn 1 modified README.
	data, _ := os.ReadFile(filepath.Join(workspace, "README"))
	if string(data) != "turn 1\n" {
		t.Errorf("fork should not touch worktree; README is %q", data)
	}

	// Forked thread has items cloned from source (current contract: ALL items,
	// not "up to turn N"). Document the actual behavior so any future change
	// to item trimming is a conscious decision with a test change.
	items, err := e.app.store.ListItems(newID)
	if err != nil {
		t.Fatalf("list forked items: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected all 2 source items cloned (no turn-trim); got %d", len(items))
	}
}

// #29 — Restore on a Claude thread must return an error suggesting fork.
// The worktree must not be touched by the rejected call.
func TestAppE2E_RevertToTurnRestoreOnClaudeRejected(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t-claude", workspace, provider.Claude)
	captureE2E(t, e.app, "t-claude", workspace, 0)

	// Dirty the workspace AFTER the capture so we can verify restore is a no-op.
	writeE2EFile(t, workspace, "README", "user edit\n")

	_, err := e.app.RevertToTurn("t-claude", 0, "restore")
	if err == nil {
		t.Fatal("expected error: Claude threads cannot be restored in place")
	}
	if !strings.Contains(err.Error(), "fork") {
		t.Errorf("error should suggest fork mode; got: %v", err)
	}

	// Worktree unchanged.
	data, _ := os.ReadFile(filepath.Join(workspace, "README"))
	if string(data) != "user edit\n" {
		t.Errorf("worktree should be untouched on rejected restore; got %q", data)
	}
}

// #30 — Restore on a Codex thread applies the captured state to the
// worktree (destructively).
func TestAppE2E_RevertToTurnRestoreOnCodexAppliesWorkspace(t *testing.T) {
	e := newE2EApp(t)
	workspace := t.TempDir()
	initE2ERepo(t, workspace)
	seedE2EThread(t, e.app, "t-codex", workspace, provider.Codex)

	captureE2E(t, e.app, "t-codex", workspace, 0)
	writeE2EFile(t, workspace, "README", "dirty\n")
	writeE2EFile(t, workspace, "junk.txt", "added after checkpoint\n")

	if _, err := e.app.RevertToTurn("t-codex", 0, "restore"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(workspace, "README"))
	if string(got) != "hello\n" {
		t.Errorf("README not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "junk.txt")); err == nil {
		t.Errorf("junk.txt added after checkpoint should have been cleaned")
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
