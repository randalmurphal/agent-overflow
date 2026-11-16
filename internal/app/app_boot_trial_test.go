package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

	// The thread deletes land before the workflow release, so only the end
	// of the steps says every one has run.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.waitForBootSteps(ctx); err != nil {
		t.Fatalf("boot steps after the commit: %v", err)
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
