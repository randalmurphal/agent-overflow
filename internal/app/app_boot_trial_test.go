package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadmode"
)

// lastRun is what a boot finds the last run left outside its database, and
// what the boot publishes there.
type lastRun struct {
	terminalThread, preparingFork string
	terminalFile, forkFile        string
	promptFile, reviewDiff        string
	cliCommand, providerAccounts  string
}

// removed is every file the boot's steps delete.
func (r lastRun) removed() []string {
	return []string{r.terminalFile, r.forkFile, r.promptFile, r.reviewDiff}
}

// written is every file the boot's steps publish.
func (r lastRun) written() []string { return []string{r.cliCommand, r.providerAccounts} }

// seedLastRun gives a boot app's data directory a terminal thread and a fork
// the last run never finished, each with an attachment file, and leftover
// prompt files and review diff snapshots.
func seedLastRun(t *testing.T, a *App) lastRun {
	t.Helper()
	dataDir := filepath.Join(a.dataDirOverride, "agent-overflow")
	dbPath := filepath.Join(dataDir, databaseFileName)
	migratedDatabaseAt(t, dbPath)
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	workspace := t.TempDir()
	if _, err := st.CreateProject(store.Project{ID: defaultTestProjectID, Path: workspace, Name: "Project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := lastRun{terminalThread: "thread-terminal", preparingFork: "thread-preparing-fork"}
	for _, row := range []store.Thread{
		{ID: run.terminalThread, Mode: threadmode.ModeTerminal},
		{ID: run.preparingFork, Mode: threadmode.ModeChat, ForkPreparing: true},
	} {
		row.ProjectID, row.Title, row.Provider = defaultTestProjectID, row.ID, string(provider.Claude)
		row.WorkspacePath, row.Model, row.CreatedAt, row.UpdatedAt = workspace, "claude-opus-4-7", now, now
		if err := st.CreateThread(row); err != nil {
			t.Fatal(err)
		}
	}
	attachments, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(dataDir, "attachments")}, st)
	if err != nil {
		t.Fatal(err)
	}
	attachmentFile := func(threadID string) string {
		row, err := attachments.Upload(threadID, "note.txt", "text/plain", 5, strings.NewReader("hello"), now)
		if err != nil {
			t.Fatalf("upload to %s: %v", threadID, err)
		}
		_, path, err := attachments.PathForThread(threadID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	run.terminalFile, run.forkFile = attachmentFile(run.terminalThread), attachmentFile(run.preparingFork)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	leftover := func(dir string) string {
		path := filepath.Join(dataDir, dir, "leftover")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run.promptFile, run.reviewDiff = leftover(claudePromptDir), leftover(reviewDiffSnapshotDir)
	run.cliCommand = filepath.Join(dataDir, "bin", cliCommandName)
	run.providerAccounts = filepath.Join(dataDir, "provider-accounts.json")
	return run
}

// A rolled-back trial gets its database back and nothing else, so every
// boot step that deletes a file, publishes one or starts provider work waits
// for the commit. Before the commit everything the last run left is still
// there, nothing was published and the workflow rebuild's starts are held;
// after it, each step has run.
func TestAParkedTrialTouchesNothingOutsideTheDatabaseBeforeItsCommit(t *testing.T) {
	a := newBootTestApp(t)
	run := seedLastRun(t, a)
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	ParkUnattendedWork(a)
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Long enough for anything the boot left running to act.
	time.Sleep(200 * time.Millisecond)
	for _, path := range run.removed() {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s before the commit: %v, want it kept", path, err)
		}
	}
	for _, id := range []string{run.terminalThread, run.preparingFork} {
		if _, err := a.store.GetThread(id); err != nil {
			t.Errorf("thread %s before the commit: %v, want it kept", id, err)
		}
	}
	for _, path := range run.written() {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s before the commit: %v, want it unwritten", path, err)
		}
	}
	if state, err := a.WorkflowGetEngineState(); err != nil || !state.Paused {
		t.Errorf("workflow engine before the commit = %+v, %v; want its starts held", state, err)
	}
	if t.Failed() {
		t.FailNow()
	}

	ActivateUnattendedWork(a, ServiceUpdateOutcome{})

	// Clients are admitted once every step before them has run.
	admitCtx, cancelAdmit := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelAdmit()
	if err := WaitForActivation(a, admitCtx); err != nil {
		t.Fatalf("WaitForActivation after the commit: %v", err)
	}
	for _, id := range []string{run.terminalThread, run.preparingFork} {
		if _, err := a.store.GetThread(id); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("thread %s after the commit: %v, want it removed", id, err)
		}
	}
	for _, path := range run.removed() {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the commit: %v, want it removed", path, err)
		}
	}
	for _, path := range run.written() {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s after the commit: %v, want it written", path, err)
		}
	}
	if state, err := a.WorkflowGetEngineState(); err != nil || state.Paused {
		t.Errorf("workflow engine after the commit = %+v, %v; want its starts released", state, err)
	}
	if failures := progress.failures(); len(failures) != 0 {
		t.Fatalf("boot failures = %v", failures)
	}
}

// A boot step that stops an ordinary boot still does, but only after the
// steps behind it have run, so a committed trial that reports the same
// failure does not leave its workflow starts held. A fork the store refuses
// to delete is such a failure.
func TestAFailingBootStepFailsTheBootAfterTheOtherSteps(t *testing.T) {
	a := newBootTestApp(t)
	run := seedLastRun(t, a)
	raw, err := sql.Open("sqlite", filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER keep_the_fork BEFORE DELETE ON threads WHEN OLD.id = '` + run.preparingFork +
		`' BEGIN SELECT RAISE(ABORT, 'injected fork delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	err = a.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "clean interrupted fork "+run.preparingFork) {
		t.Fatalf("Start = %v, want the fork cleanup's failure", err)
	}
	if _, err := a.store.GetThread(run.terminalThread); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("terminal thread after the failed boot: %v, want the step after the failure to have run", err)
	}
	if state, err := a.WorkflowGetEngineState(); err != nil || state.Paused {
		t.Errorf("workflow engine after the failed boot = %+v, %v; want its starts released", state, err)
	}
}

// The goroutines the unattended work leaves running with no narrower owner
// are ones Shutdown joins. The worktree watch's first read and the issue
// trackers' cleanup wait for the first catalog reads, which no client
// makes here, and the binary watcher, the domain certificate loop and the
// tailnet loop run until shutdown.
func TestShutdownJoinsTheLoopsTheUnattendedWorkLeavesRunning(t *testing.T) {
	a := newBootTestApp(t)
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := a.unattendedLoops.members(); got != 5 {
		t.Errorf("unattended loops after the boot = %d, want 5", got)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := a.unattendedLoops.members(); got != 0 {
		t.Errorf("unattended loops after Shutdown = %d, want 0", got)
	}
}

// The startup provider probes spawn provider processes; Shutdown waits for
// every one the boot started instead of returning beside it.
func TestShutdownWaitsForTheStartupProviderProbes(t *testing.T) {
	marks := t.TempDir()
	binary := mockexec.Write(t, filepath.Join(t.TempDir(), "provider"), fmt.Sprintf(
		"#!/bin/sh\ntouch %q/start-$$\nif [ \"$1\" = --version ]; then sleep 1; echo '0.0.0 (boot test mock)'; fi\ntouch %q/done-$$\nexit 0\n",
		marks, marks))
	a := newBootTestAppWithProvider(t, binary)
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	count := func(prefix string) int {
		matches, err := filepath.Glob(filepath.Join(marks, prefix+"-*"))
		if err != nil {
			t.Fatal(err)
		}
		return len(matches)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntil(t, 30*time.Second, func() bool { return count("start") > count("done") })
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A process Shutdown cancelled was killed before it could mark itself
	// done; one that outlived Shutdown is still running.
	starts, err := filepath.Glob(filepath.Join(marks, "start-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range starts {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(start), "start-"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(marks, fmt.Sprintf("done-%d", pid))); err == nil {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil && process.Signal(syscall.Signal(0)) == nil {
			t.Errorf("provider process %d the boot started outlived Shutdown", pid)
		}
	}
}

// deferredBootPhases are the phases a trial's deferred steps open: three
// before clients and the first of the unattended work.
var deferredBootPhases = []string{
	"app.sweep_review_diffs", "app.clean_preparing_forks", "app.end_terminal_threads",
	"app.finish_thread_request_sweep",
}

// A quit while a committed trial's boot step runs waits for that step, so it
// never meets a closed store or engine, and begins no other: no later step
// before clients and none of the unattended work.
func TestShutdownDuringACommittedTrialsBootStepWaitsForItAndStartsNoOther(t *testing.T) {
	a := newBootTestApp(t)
	run := seedLastRun(t, a)
	progress := newRecordingBootProgress("app.clean_preparing_forks")
	// Registered before release, so it runs after it on an early failure.
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(progress.release) }) }
	t.Cleanup(release)
	SetBootProgress(a, progress)
	ParkUnattendedWork(a)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ActivateUnattendedWork(a, ServiceUpdateOutcome{})
	select {
	case <-progress.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the committed trial never reached the fork cleanup")
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- a.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned (%v) while a boot step was still running", err)
	case <-time.After(bootStepsHeldFor):
	}
	release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return after the boot step finished")
	}

	begun, _, _ := progress.snapshot()
	for _, phase := range deferredBootPhases[2:] {
		if slices.Contains(begun, phase) {
			t.Errorf("%s began after Shutdown stopped the boot (begun %v)", phase, begun)
		}
	}
	if failures := progress.failures(); len(failures) != 0 {
		t.Errorf("boot failures = %v, want none", failures)
	}
	// The step that was running finished; the one after it never began.
	if _, err := os.Stat(run.forkFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fork attachment after the stopped boot: %v, want the running step to have removed it", err)
	}
	if _, err := os.Stat(run.terminalFile); err != nil {
		t.Errorf("terminal attachment after the stopped boot: %v, want it kept", err)
	}

	second := make(chan error, 1)
	go func() { second <- a.Shutdown(context.Background()) }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second Shutdown did not return")
	}
}

// A trial stopped before its commit runs none of its deferred steps, and its
// Shutdown does not wait for a commit that will never come. A commit that
// arrives after the stop starts nothing either.
func TestShutdownWhileATrialIsParkedReturnsWithoutRunningItsBootSteps(t *testing.T) {
	a := newBootTestApp(t)
	run := seedLastRun(t, a)
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	ParkUnattendedWork(a)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- a.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown of a parked trial did not return")
	}
	ActivateUnattendedWork(a, ServiceUpdateOutcome{})
	time.Sleep(bootStepsHeldFor)

	begun, _, _ := progress.snapshot()
	for _, phase := range deferredBootPhases {
		if slices.Contains(begun, phase) {
			t.Errorf("%s began on a trial stopped before its commit", phase)
		}
	}
	for _, path := range run.removed() {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s after the stopped trial: %v, want it kept", path, err)
		}
	}
	for _, path := range run.written() {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the stopped trial: %v, want it unwritten", path, err)
		}
	}
}
