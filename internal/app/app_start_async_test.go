package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// recordingBootProgress records reported phases. A phase named in block
// parks its BeginBootPhase until release is closed, which holds Start at a
// known point.
type recordingBootProgress struct {
	block   string
	reached chan struct{}
	release chan struct{}

	mu      sync.Mutex
	begun   []string
	ended   []string
	details []string
	watched []string
	open    []string
	// failed holds "phase: error" for each BootPhaseFailed.
	failed []string
}

func newRecordingBootProgress(block string) *recordingBootProgress {
	return &recordingBootProgress{block: block, reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *recordingBootProgress) BeginBootPhase(phase, _ string) func() {
	p.mu.Lock()
	p.begun = append(p.begun, phase)
	p.open = append(p.open, phase)
	p.mu.Unlock()
	if phase == p.block {
		close(p.reached)
		<-p.release
	}
	return func() {
		p.mu.Lock()
		p.ended = append(p.ended, phase)
		if i := slices.Index(p.open, phase); i >= 0 {
			p.open = p.open[:i]
		}
		p.mu.Unlock()
	}
}

func (p *recordingBootProgress) BootPhaseFailed(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	phase := "(no phase)"
	if len(p.open) > 0 {
		phase = p.open[len(p.open)-1]
	}
	p.failed = append(p.failed, phase+": "+err.Error())
}

func (p *recordingBootProgress) failures() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.failed...)
}

func (p *recordingBootProgress) BootPhaseDetail(detail string, _, _ int) {
	p.mu.Lock()
	p.details = append(p.details, detail)
	p.mu.Unlock()
}

func (p *recordingBootProgress) WatchBootFiles(paths ...string) {
	p.mu.Lock()
	p.watched = append(p.watched, paths...)
	p.mu.Unlock()
}

func (p *recordingBootProgress) snapshot() (begun, ended, details []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.begun...), append([]string(nil), p.ended...), append([]string(nil), p.details...)
}

// newBootTestApp builds an App whose Start can only reach temporary
// directories: its data root, provider home and keychain are all under
// t.TempDir(), and the browser engine is the fake.
func newBootTestApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	a := NewApp()
	a.dataDirOverride = t.TempDir()
	ConfigureIsolation(a, IsolationConfig{
		CredentialHome:    home,
		UseFileKeychain:   true,
		MockBrowserEngine: true,
	})
	return a
}

// asyncStartGoroutines counts goroutines still running the desktop Start.
func asyncStartGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "(*App).startAsync.func")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestAsyncStartCanceledMidMigrationStopsWithoutReporting: closing the
// desktop window while Start is inside the migration chain cancels it.
// stopAsyncStart waits for Start to return, the canceled Start reports
// nothing to the startDone hook, the migration it was about to apply is
// not recorded, no Start goroutine remains, and Shutdown runs on the
// partial App.
func TestAsyncStartCanceledMidMigrationStopsWithoutReporting(t *testing.T) {
	a := newBootTestApp(t)
	progress := newRecordingBootProgress("store.migrate")
	SetBootProgress(a, progress)
	var doneCalls []error
	var doneMu sync.Mutex
	SetStartDone(a, func(err error) {
		doneMu.Lock()
		doneCalls = append(doneCalls, err)
		doneMu.Unlock()
	})

	a.startAsync(context.Background())
	select {
	case <-progress.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("Start never reached the migration chain")
	}
	// The database and its WAL are watched before the first migration
	// runs, so a long statement that writes reads as progress.
	dbPath := filepath.Join(a.dataDirOverride, "agent-overflow", databaseFileName)
	progress.mu.Lock()
	watched := append([]string(nil), progress.watched...)
	progress.mu.Unlock()
	if strings.Join(watched, ",") != dbPath+","+dbPath+"-wal" {
		t.Fatalf("watched %v when the migrations began, want the database and its WAL", watched)
	}

	stopped := make(chan struct{})
	go func() {
		a.stopAsyncStart()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stopAsyncStart returned while Start was still inside a migration")
	case <-time.After(50 * time.Millisecond):
	}
	close(progress.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopAsyncStart did not return after Start was canceled")
	}

	doneMu.Lock()
	calls := len(doneCalls)
	doneMu.Unlock()
	if calls != 0 {
		t.Fatalf("startDone called %d times for a canceled Start: %v", calls, doneCalls)
	}
	begun, ended, details := progress.snapshot()
	if strings.Join(begun, ",") != "app.init_stores,store.migrate" {
		t.Fatalf("phases begun = %v, want Start to stop inside the migration chain", begun)
	}
	if strings.Join(ended, ",") != "store.migrate,app.init_stores" {
		t.Fatalf("phases ended = %v, want every begun phase ended", ended)
	}
	if len(details) != 1 || !strings.HasPrefix(details[0], "Applying migration 1 of ") {
		t.Fatalf("details = %v, want the first migration reported", details)
	}
	if n := asyncStartGoroutines(); n != 0 {
		t.Fatalf("%d Start goroutines remain after stopAsyncStart", n)
	}

	// Nothing was applied: reopening runs the whole chain from the start.
	var first store.MigrationStep
	st, err := store.NewWithOptions(dbPath, store.Options{OnMigration: func(step store.MigrationStep) {
		if step.Index == 1 {
			first = step
		}
	}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if first.Index != 1 || first.Pending == 0 || !strings.HasSuffix(details[0], first.Name) {
		t.Fatalf("first migration on reopen = %+v, want the one the canceled Start began (%q)", first, details[0])
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after a canceled Start: %v", err)
	}
	a.stopAsyncStart()
}

// TestBootNamesTheRunningPartOfARebuild: a fresh boot's migration phase
// begins once, and a rebuild's details name each index build and the
// foreign key check under the migration's own step.
func TestBootNamesTheRunningPartOfARebuild(t *testing.T) {
	a := newBootTestApp(t)
	progress := newRecordingBootProgress("app.init_identity")
	SetBootProgress(a, progress)
	a.startAsync(context.Background())
	select {
	case <-progress.reached:
	case <-time.After(30 * time.Second):
		t.Fatal("Start never finished opening the database")
	}
	begun, _, details := progress.snapshot()
	close(progress.release)
	a.stopAsyncStart()
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if strings.Join(begun, ",") != "app.init_stores,store.migrate,app.init_identity" {
		t.Fatalf("phases begun = %v, want the migration phase once", begun)
	}
	var local []string
	for _, detail := range details {
		if _, part, ok := strings.Cut(detail, " local_sessions"); ok && strings.HasPrefix(detail, "Applying migration ") {
			local = append(local, part)
		}
	}
	want := []string{"", ": building index idx_sessions_device", ": building index idx_sessions_user",
		": building index idx_sessions_local", ": building index idx_sessions_live", ": checking foreign keys"}
	if strings.Join(local, "|") != strings.Join(want, "|") {
		t.Fatalf("local_sessions details = %q, want %q", local, want)
	}
}

// TestReportMigrationBeginsThePhaseOnce: when the first pending migration
// is a rebuild, its index builds and foreign key check are details of its
// step and do not begin the phase again.
func TestReportMigrationBeginsThePhaseOnce(t *testing.T) {
	a := NewApp()
	progress := newRecordingBootProgress("")
	SetBootProgress(a, progress)
	end := func() {}
	report := a.reportMigration(&end)
	report(store.MigrationStep{Version: 110, Name: "local_sessions", Index: 1, Pending: 2})
	report(store.MigrationStep{Version: 110, Name: "local_sessions", Index: 1, Pending: 2, Activity: "building index idx_sessions_device"})
	report(store.MigrationStep{Version: 110, Name: "local_sessions", Index: 1, Pending: 2, Activity: "checking foreign keys"})
	report(store.MigrationStep{Version: 111, Name: "scoped_timeline_indexes", Index: 2, Pending: 2})
	end()

	begun, ended, details := progress.snapshot()
	if strings.Join(begun, ",") != "store.migrate" || strings.Join(ended, ",") != "store.migrate" {
		t.Fatalf("phases begun %v and ended %v, want store.migrate once", begun, ended)
	}
	want := []string{
		"Applying migration 1 of 2 local_sessions",
		"Applying migration 1 of 2 local_sessions: building index idx_sessions_device",
		"Applying migration 1 of 2 local_sessions: checking foreign keys",
		"Applying migration 2 of 2 scoped_timeline_indexes",
	}
	if strings.Join(details, "|") != strings.Join(want, "|") {
		t.Fatalf("details = %q, want %q", details, want)
	}
}

// TestAsyncStartReportsFailureToStartDone: a Start that fails on its own
// hands the error to the hook, which is how the desktop boot marks the
// bootstrap failed and exits with the cause.
func TestAsyncStartReportsFailureToStartDone(t *testing.T) {
	a := newBootTestApp(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.dataDirOverride = blocker
	done := make(chan error, 1)
	SetStartDone(a, func(err error) { done <- err })

	a.startAsync(context.Background())
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "data directory") {
			t.Fatalf("startDone(%v), want the data directory failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startDone was not called for a failed Start")
	}
	a.stopAsyncStart()
	if n := asyncStartGoroutines(); n != 0 {
		t.Fatalf("%d Start goroutines remain", n)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after a failed Start: %v", err)
	}
}

// TestStopAsyncStartWithoutStartIsANoop: ServiceShutdown runs even when
// ServiceStartup never did.
func TestStopAsyncStartWithoutStartIsANoop(t *testing.T) {
	a := NewApp()
	a.stopAsyncStart()
}
