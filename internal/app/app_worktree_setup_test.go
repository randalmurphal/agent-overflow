package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/worktreesetup"

	"github.com/google/uuid"
)

// --- fixtures ---

type setupEventRecorder struct {
	mu     sync.Mutex
	events []WorktreeSetupEvent
	other  []string
}

func (r *setupEventRecorder) record(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name != eventchan.WorktreeSetup.String() {
		r.other = append(r.other, name)
		return
	}
	evt, ok := data.(WorktreeSetupEvent)
	if !ok {
		panic(fmt.Sprintf("worktree:setup payload type = %T, want WorktreeSetupEvent", data))
	}
	r.events = append(r.events, evt)
}

func (r *setupEventRecorder) snapshot() []WorktreeSetupEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WorktreeSetupEvent(nil), r.events...)
}

func (r *setupEventRecorder) phases() []string {
	phases := []string{}
	for _, evt := range r.snapshot() {
		phases = append(phases, evt.Phase)
	}
	return phases
}

func (r *setupEventRecorder) output() string {
	var builder strings.Builder
	for _, evt := range r.snapshot() {
		if evt.Phase == worktreeSetupPhaseOutput {
			builder.WriteString(evt.Chunk)
		}
	}
	return builder.String()
}

func (r *setupEventRecorder) terminal(t *testing.T) WorktreeSetupEvent {
	t.Helper()
	events := r.snapshot()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Phase == worktreeSetupPhaseFinished {
			return events[i]
		}
	}
	t.Fatalf("no finished frame emitted; phases = %v", r.phases())
	return WorktreeSetupEvent{}
}

// newWorktreeSetupTestApp builds an App with a store, one project whose recipe
// is `config`, and one thread already working in a worktree directory on disk.
func newWorktreeSetupTestApp(t *testing.T, config *worktreesetup.Config) (*App, store.Thread, *setupEventRecorder) {
	t.Helper()
	app := newTestAppWithStore(t)
	recorder := &setupEventRecorder{}
	app.emitEventFn = recorder.record

	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	worktreePath := filepath.Join(root, "worktree")
	for _, dir := range []string{projectPath, worktreePath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	now := time.Now().UnixMilli()
	projectID := uuid.New().String()
	if _, err := app.store.CreateProject(store.Project{
		ID:        projectID,
		Path:      projectPath,
		Name:      "Worktree Setup Test",
		Color:     "#888888",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if config != nil {
		if _, _, err := app.store.UpdateProjectWorktreeSetup(projectID, config); err != nil {
			t.Fatalf("UpdateProjectWorktreeSetup: %v", err)
		}
	}

	thread := store.Thread{
		ID:            uuid.New().String(),
		ProjectID:     projectID,
		ProjectPath:   projectPath,
		Title:         "setup",
		Provider:      "claude",
		WorkspacePath: worktreePath,
		WorktreePath:  worktreePath,
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return app, thread, recorder
}

func waitForSetupState(t *testing.T, app *App, threadID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := app.store.GetThread(threadID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		if got.WorktreeSetupState == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := app.store.GetThread(threadID)
	t.Fatalf("worktree setup state = %q, want %q", got.WorktreeSetupState, want)
}

func joinSetupRun(t *testing.T, app *App, threadID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !app.worktreeSetupService().WaitThread(ctx, threadID) {
		t.Fatal("timed out waiting for the worktree setup run to settle")
	}
}

// --- success ---

// The happy path: every step reported in order, the durable state returns to
// empty, and the record is dropped so a later snapshot is idle. A success card
// is a transient acknowledgement — retaining it would replay it at every pane
// mount for the rest of the session.
func TestWorktreeSetupSuccessClearsEverything(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{
			{"/bin/sh", "-c", "echo first"},
			{"/bin/sh", "-c", "echo second"},
		},
	})

	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateNone)

	want := []string{
		worktreeSetupPhaseStarted,
		worktreeSetupPhaseStepStarted, worktreeSetupPhaseOutput, worktreeSetupPhaseStepFinished,
		worktreeSetupPhaseStepStarted, worktreeSetupPhaseOutput, worktreeSetupPhaseStepFinished,
		worktreeSetupPhaseFinished,
	}
	if got := recorder.phases(); !equalStrings(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	if got := recorder.output(); got != "first\nsecond\n" {
		t.Fatalf("streamed output = %q, want %q", got, "first\nsecond\n")
	}
	if terminal := recorder.terminal(t); terminal.State != worktreeSetupRunSucceeded {
		t.Fatalf("terminal state = %q, want succeeded", terminal.State)
	}

	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != worktreeSetupRunIdle {
		t.Fatalf("snapshot state after success = %q, want idle", snapshot.State)
	}
}

// Output chunks carry a strictly increasing sequence starting at 1. It is what
// lets a client fold a snapshot and the live stream together without
// double-appending, so a regression here is silently corrupted output.
func TestWorktreeSetupOutputSequenceIsMonotonic(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "printf 'a\\nb\\nc\\n'"}},
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)

	var previous uint64
	seen := 0
	for _, evt := range recorder.snapshot() {
		if evt.Phase != worktreeSetupPhaseOutput {
			continue
		}
		seen++
		if evt.Seq != previous+1 {
			t.Fatalf("output seq = %d after %d, want strictly consecutive", evt.Seq, previous)
		}
		previous = evt.Seq
	}
	if seen == 0 {
		t.Fatal("no output frames emitted")
	}
}

// --- failure ---

// A failed recipe leaves the worktree in place, persists `failed`, retains the
// run so the panel can offer Retry, and keeps the failing command's output.
func TestWorktreeSetupFailureIsVisibleAndRetained(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{
			{"/bin/sh", "-c", "echo ok"},
			{"/bin/sh", "-c", "echo diagnosis >&2; exit 3"},
			{"/bin/sh", "-c", "echo never"},
		},
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateFailed)

	terminal := recorder.terminal(t)
	if terminal.State != worktreeSetupRunFailed {
		t.Fatalf("terminal state = %q, want failed", terminal.State)
	}
	if !strings.Contains(terminal.Error, "diagnosis") {
		t.Fatalf("terminal error = %q, want the command's output tail", terminal.Error)
	}
	if strings.Contains(recorder.output(), "never") {
		t.Fatal("a step after the failing one produced output; the sequence must stop at the first failure")
	}

	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != worktreeSetupRunFailed {
		t.Fatalf("snapshot state = %q, want failed", snapshot.State)
	}
	if len(snapshot.StepStatuses) != 3 {
		t.Fatalf("snapshot step statuses = %v, want 3 entries", snapshot.StepStatuses)
	}
	want := []string{worktreeSetupStepSucceeded, worktreeSetupStepFailed, worktreeSetupStepPending}
	if !equalStrings(snapshot.StepStatuses, want) {
		t.Fatalf("snapshot step statuses = %v, want %v", snapshot.StepStatuses, want)
	}
	if !strings.Contains(snapshot.Output, "diagnosis") {
		t.Fatalf("snapshot output = %q, want the failing command's stderr", snapshot.Output)
	}
	if snapshot.OutputSeq == 0 {
		t.Fatal("snapshot outputSeq = 0; a client cannot tell which live chunks it already has")
	}
}

// A recipe that cannot be read is a setup FAILURE, not a reason to skip setup:
// a worktree provisioned without what its project declared breaks later, in
// ways that only surface mid-turn. The store refuses to write a blob it cannot
// read back, so the pre-flight seam is exercised directly — it is the one path
// that reports a failure with no step ever having started.
func TestWorktreeSetupUnstartableRecipeFailsVisibly(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, nil)

	app.worktreeSetupService().RecordUnstartableThread(
		thread,
		fmt.Errorf("load worktree setup for project %q: corrupt blob", thread.ProjectID),
	)

	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateFailed {
		t.Fatalf("worktree setup state = %q, want failed", got.WorktreeSetupState)
	}
	if phases := recorder.phases(); !equalStrings(phases, []string{
		worktreeSetupPhaseStarted, worktreeSetupPhaseFinished,
	}) {
		t.Fatalf("phases = %v, want started then finished", phases)
	}
	if terminal := recorder.terminal(t); terminal.State != worktreeSetupRunFailed {
		t.Fatalf("terminal state = %q, want failed", terminal.State)
	}
	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if !strings.Contains(snapshot.Error, "corrupt blob") {
		t.Fatalf("snapshot error = %q, want the cause", snapshot.Error)
	}
	if snapshot.RunID == "" {
		t.Fatal("snapshot runId is empty; the retained failure has no identity for the panel to match")
	}
	// The retained failure must not block the repair the user is about to make.
	if _, _, err := app.store.UpdateProjectWorktreeSetup(thread.ProjectID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "true"}},
	}); err != nil {
		t.Fatalf("rewrite recipe: %v", err)
	}
	if err := app.RetryThreadWorktreeSetup(thread.ID); err != nil {
		t.Fatalf("RetryThreadWorktreeSetup after a pre-flight failure: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateNone)
}

// --- call sequences ---

// State coverage is not transition coverage. A second kickoff while a run is
// in flight is refused LOUDLY rather than silently starting a second process
// group in the same directory.
func TestWorktreeSetupRefusesASecondConcurrentRun(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 5"}},
		Timeout: "30s",
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	t.Cleanup(func() { app.cancelThreadWorktreeSetup(thread.ID) })

	err := app.launchThreadWorktreeSetup(thread, false)
	if err == nil {
		t.Fatal("second launch during an active run reported success")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second launch error = %v, want it to name the active run", err)
	}
}

// The full retry sequence: fail, retry, succeed. A retry after a settled run
// is allowed and clears the failure it replaced.
func TestWorktreeSetupRetryAfterFailureSucceeds(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "exit 1"}},
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateFailed)

	// Retry re-reads the recipe, so fixing it in Settings and pressing Retry
	// does what the user means.
	if _, _, err := app.store.UpdateProjectWorktreeSetup(thread.ProjectID, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "true"}},
	}); err != nil {
		t.Fatalf("rewrite recipe: %v", err)
	}
	if err := app.RetryThreadWorktreeSetup(thread.ID); err != nil {
		t.Fatalf("RetryThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateNone)
}

// Three different mistakes, three different refusals. A retry that cannot run
// must say why rather than appearing to start.
func TestWorktreeSetupRetryRefusalsAreSpecific(t *testing.T) {
	t.Run("no worktree", func(t *testing.T) {
		app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
			Run: [][]string{{"/bin/sh", "-c", "true"}},
		})
		thread.WorktreePath = ""
		thread.WorkspacePath = thread.ProjectPath
		if err := app.store.UpdateThread(thread); err != nil {
			t.Fatalf("UpdateThread: %v", err)
		}
		err := app.RetryThreadWorktreeSetup(thread.ID)
		if err == nil || !strings.Contains(err.Error(), "not working in a worktree") {
			t.Fatalf("retry error = %v, want a not-in-a-worktree refusal", err)
		}
	})

	t.Run("no recipe", func(t *testing.T) {
		app, thread, _ := newWorktreeSetupTestApp(t, nil)
		err := app.RetryThreadWorktreeSetup(thread.ID)
		if err == nil || !strings.Contains(err.Error(), "no worktree setup configured") {
			t.Fatalf("retry error = %v, want an unconfigured-project refusal", err)
		}
	})

	t.Run("unknown thread", func(t *testing.T) {
		app, _, _ := newWorktreeSetupTestApp(t, nil)
		if err := app.RetryThreadWorktreeSetup("nope"); err == nil {
			t.Fatal("retry for an unknown thread reported success")
		}
	})
}

// An unconfigured project runs nothing and — the part that matters — SHOWS
// nothing. Most projects have no recipe; a panel on every worktree creation
// would be noise.
func TestWorktreeSetupOnUnconfiguredProjectIsSilent(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, nil)
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("emitted %d events for an unconfigured project, want none", len(events))
	}
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("worktree setup state = %q, want empty", got.WorktreeSetupState)
	}
}

// A recipe carrying only a timeout resolves to zero steps. Config.IsZero calls
// it configured; the resolved step list is what actually decides, because a
// panel listing no steps has nothing to say.
func TestWorktreeSetupWithNoResolvedStepsIsSilent(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{Timeout: "45s"})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("emitted %d events for a recipe with no steps, want none", len(events))
	}
}

// --- cancellation ---

// A cancelled run is neither success nor failure: the durable state clears,
// the record is dropped, and no failure is ever advertised for work the user
// abandoned.
func TestWorktreeSetupCancellationIsNeitherSuccessNorFailure(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 30"}},
		Timeout: "60s",
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateRunning)

	app.cancelThreadWorktreeSetup(thread.ID)

	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("worktree setup state after cancel = %q, want empty", got.WorktreeSetupState)
	}
	if terminal := recorder.terminal(t); terminal.State != worktreeSetupRunCancelled {
		t.Fatalf("terminal state = %q, want cancelled", terminal.State)
	}
	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != worktreeSetupRunIdle {
		t.Fatalf("snapshot state after cancel = %q, want idle", snapshot.State)
	}

	// Idempotent, and safe on a thread that never had a run.
	app.cancelThreadWorktreeSetup(thread.ID)
	app.cancelThreadWorktreeSetup("never-existed")
}

// releaseThreadWorktreeSetup is what every workspace mutator calls. Staying in
// the same worktree must NOT cancel — a switch that lands where it started is
// the no-op the path comparison exists for.
func TestWorktreeSetupReleaseKeepsARunInItsOwnWorktree(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 30"}},
		Timeout: "60s",
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateRunning)
	t.Cleanup(func() { app.cancelThreadWorktreeSetup(thread.ID) })

	app.releaseThreadWorktreeSetup(thread.ID, thread.WorktreePath)
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateRunning {
		t.Fatalf("worktree setup state = %q, want running (the thread never left)", got.WorktreeSetupState)
	}

	app.releaseThreadWorktreeSetup(thread.ID, thread.ProjectPath)
	got, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("worktree setup state after leaving = %q, want empty", got.WorktreeSetupState)
	}
}

// The structural backstop for a future workspace mutator that forgets to call
// the release helper: a failure resolved against a worktree the thread no
// longer occupies is treated as cancelled, so no pill points at a checkout the
// thread has left.
func TestWorktreeSetupFailureForAVacatedWorktreeIsCancelled(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 0.2; exit 1"}},
		Timeout: "30s",
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	// Move the thread back to the project root behind the run's back, exactly
	// as a mutator that skipped releaseThreadWorktreeSetup would.
	moved := thread
	moved.WorkspacePath = thread.ProjectPath
	moved.WorktreePath = ""
	if err := app.store.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	joinSetupRun(t, app, thread.ID)

	if terminal := recorder.terminal(t); terminal.State != worktreeSetupRunCancelled {
		t.Fatalf("terminal state = %q, want cancelled", terminal.State)
	}
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("worktree setup state = %q, want empty", got.WorktreeSetupState)
	}
}

// --- shutdown / crash recovery ---

// Shutdown mid-run kills the process group and DELIBERATELY leaves the row at
// 'running': the worktree's state is genuinely unknown, and the next boot's
// sweep is what turns that into a visible failure. One decision, one place.
func TestWorktreeSetupShutdownLeavesTheSweepToDecide(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "sleep 30"}},
		Timeout: "60s",
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateRunning)

	app.stopThreadWorktreeSetups()
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateRunning {
		t.Fatalf("worktree setup state after shutdown = %q, want running", got.WorktreeSetupState)
	}

	// Idempotent — Wails' lifecycle plus tests can both call Shutdown.
	app.stopThreadWorktreeSetups()

	// Once the runs are stopped, a kickoff is refused rather than started and
	// immediately killed: past that point the WaitGroup may already be being
	// waited on, and one that gains a member during Wait is a race.
	if err := app.launchThreadWorktreeSetup(thread, false); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("launch after stop = %v, want ErrShuttingDown", err)
	}
	if err := app.RetryThreadWorktreeSetup(thread.ID); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("retry after stop = %v, want ErrShuttingDown", err)
	}

	app.sweepCrashedWorktreeSetups()
	got, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.WorktreeSetupState != store.WorktreeSetupStateFailed {
		t.Fatalf("worktree setup state after sweep = %q, want failed", got.WorktreeSetupState)
	}
}

// A durable failure whose process is gone has no run record, but must still
// answer the snapshot RPC — the panel's Retry is the whole point of persisting
// the state across a restart.
func TestWorktreeSetupSnapshotReportsADurableFailureWithoutARecord(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "true"}},
	})
	if err := app.store.SetThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateFailed); err != nil {
		t.Fatalf("SetThreadWorktreeSetupState: %v", err)
	}
	snapshot, err := app.GetThreadWorktreeSetup(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadWorktreeSetup: %v", err)
	}
	if snapshot.State != worktreeSetupRunFailed {
		t.Fatalf("snapshot state = %q, want failed", snapshot.State)
	}
	if snapshot.RunID != "" {
		t.Fatalf("snapshot runId = %q, want empty (the transcript did not survive)", snapshot.RunID)
	}
	if snapshot.Steps == nil || snapshot.StepStatuses == nil {
		t.Fatal("snapshot slices are nil; they must marshal as [] rather than null")
	}
	if snapshot.WorktreePath != thread.WorktreePath {
		t.Fatalf("snapshot worktreePath = %q, want %q", snapshot.WorktreePath, thread.WorktreePath)
	}
}

func TestGetThreadWorktreeSetupRefusesABlankThreadID(t *testing.T) {
	app, _, _ := newWorktreeSetupTestApp(t, nil)
	if _, err := app.GetThreadWorktreeSetup("  "); err == nil {
		t.Fatal("blank thread id reported success")
	}
	if err := app.RetryThreadWorktreeSetup(""); err == nil {
		t.Fatal("blank thread id reported success")
	}
}

// --- env contract ---

// The recipe runs with cwd at the worktree root and both AO_ variables set.
// Every recipe that copies or patches anything depends on this pair.
func TestWorktreeSetupRunsInTheWorktreeWithTheEnvContract(t *testing.T) {
	app, thread, _ := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", `printf '%s\n%s\n%s\n' "$PWD" "$AO_PROJECT_ROOT" "$AO_WORKTREE_PATH" > probe.txt`}},
	})
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateNone)

	data, err := os.ReadFile(filepath.Join(thread.WorktreePath, "probe.txt"))
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("probe = %q, want three lines", string(data))
	}
	for i, want := range []string{thread.WorktreePath, thread.ProjectPath, thread.WorktreePath} {
		// The temp dir can be symlinked (/tmp → /private/tmp on macOS), so
		// compare through the same resolution the runner used.
		resolvedWant, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatalf("resolve %s: %v", want, err)
		}
		resolvedGot, err := filepath.EvalSymlinks(lines[i])
		if err != nil {
			t.Fatalf("resolve %s: %v", lines[i], err)
		}
		if resolvedGot != resolvedWant {
			t.Fatalf("probe line %d = %q, want %q", i, resolvedGot, resolvedWant)
		}
	}
}

// --- copy step ---

// The copy phase is step 0 and reports through the observer like any other
// step, so the panel's indices line up with what actually ran.
func TestWorktreeSetupReportsTheCopyStepFirst(t *testing.T) {
	app, thread, recorder := newWorktreeSetupTestApp(t, &worktreesetup.Config{
		Copy: []string{".env"},
		Run:  [][]string{{"/bin/sh", "-c", "true"}},
	})
	if err := os.WriteFile(filepath.Join(thread.ProjectPath, ".env"), []byte("TOKEN=1\n"), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}
	if err := app.launchThreadWorktreeSetup(thread, false); err != nil {
		t.Fatalf("launchThreadWorktreeSetup: %v", err)
	}
	joinSetupRun(t, app, thread.ID)
	waitForSetupState(t, app, thread.ID, store.WorktreeSetupStateNone)

	events := recorder.snapshot()
	if len(events) == 0 || events[0].Phase != worktreeSetupPhaseStarted {
		t.Fatalf("first frame = %v, want started", events[0])
	}
	steps := events[0].Steps
	if len(steps) != 2 {
		t.Fatalf("resolved steps = %v, want copy + one command", steps)
	}
	if steps[0].Index != 0 || steps[0].Kind != string(worktreesetup.StepCopy) {
		t.Fatalf("step 0 = %+v, want the copy step", steps[0])
	}
	if _, err := os.Stat(filepath.Join(thread.WorktreePath, ".env")); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

// --- helpers ---

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
