//go:build !windows

package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// commandRig is a data root plus a scripted trial, for the WSL-side update
// commands.
type commandRig struct {
	*trialRig
	dataDir string
	layout  Layout
	out     *lockedBuffer
}

type lockedBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	failOn string
	failed bool
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed || (b.failOn != "" && bytes.Contains(p, []byte(b.failOn))) {
		b.failed = true
		return 0, syscall.EPIPE
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) events(t *testing.T) []UpdateEvent {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []UpdateEvent
	for _, line := range strings.Split(b.buf.String(), "\n") {
		event, ok, err := ParseUpdateEvent(line)
		if err != nil {
			t.Fatalf("ParseUpdateEvent(%q): %v", line, err)
		}
		if ok {
			events = append(events, event)
		}
	}
	return events
}

func (b *lockedBuffer) details(t *testing.T) string {
	var details []string
	for _, event := range b.events(t) {
		if event.Progress != nil {
			details = append(details, event.Progress.Detail)
		}
	}
	return strings.Join(details, "\n")
}

func newCommandRig(t *testing.T) *commandRig {
	t.Helper()
	tr := newTrialRig(t)
	dataDir := filepath.Dir(tr.db)
	return &commandRig{trialRig: tr, dataDir: dataDir, layout: appLayout(t, dataDir), out: &lockedBuffer{}}
}

// flockAcquire is the lock seam over a real flock on <dataDir>/backend.lock.
func flockAcquire(dataDir string) func(context.Context, time.Duration) (*os.File, func(), error) {
	return func(ctx context.Context, wait time.Duration) (*os.File, func(), error) {
		file, err := os.OpenFile(filepath.Join(dataDir, "backend.lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, nil, err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			file.Close()
			return nil, nil, err
		}
		return file, func() { file.Close() }, nil
	}
}

func (r *commandRig) command(id string) UpdateCommand {
	return UpdateCommand{
		DataDir: r.dataDir, UpdateID: id, Out: r.out,
		AcquireLock: flockAcquire(r.dataDir),
	}
}

func (r *commandRig) trialOptions(behavior string, attempt int) TrialRunOptions {
	cfg := r.config(r.script(helloProgress, behavior))
	return TrialRunOptions{
		Binary: cfg.Binary, Env: cfg.Env, TargetVersion: "2.0.0", Attempt: attempt,
		Rule: cfg.Rule, StopTimeout: cfg.StopTimeout,
	}
}

func (r *commandRig) database(t *testing.T) string {
	t.Helper()
	return readFile(t, r.db)
}

const (
	trialWritesAndPrepares = `printf 'migrated' > "$DB"
progress store.migrate "Applying migration 1 of 1"
printf '{"type":"prepared"}\n' >&4
serve_until_stopped`
	trialWritesAndCrashes = `printf 'half-migrated' > "$DB"
progress store.migrate "Applying migration 1 of 1"
exit 3`
)

func TestSnapshotCommandBacksUpUnderTheLock(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	result := r.command("u1").Snapshot(context.Background(), nil)
	if result.Type != UpdateEventResult || result.Outcome != UpdateOutcomeOK {
		t.Fatalf("result = %+v", result)
	}
	snapshot, found, err := ReadSnapshot(r.layout)
	if err != nil || !found || snapshot.UpdateID != "u1" || len(snapshot.Live) != 3 {
		t.Fatalf("snapshot = %+v %v %v", snapshot, found, err)
	}
	// Reports coalesce behind a slow reader, so only the last is certain.
	if details := r.out.details(t); !strings.HasSuffix(details, "Backing up the database: 1 MB of 1 MB") {
		t.Fatalf("progress %q does not end with the copy", details)
	}
	if !lockable(t, filepath.Join(r.dataDir, "backend.lock")) {
		t.Fatal("the command kept the lock")
	}
}

// TestSnapshotCommandReportsTheSchemaItBacksUp: the snapshot reads the
// database's migration version under the lock, before it copies, and
// reports it whether the copy finished or was refused. A version it cannot
// read is logged and reported as none.
func TestSnapshotCommandReportsTheSchemaItBacksUp(t *testing.T) {
	schemaUnderTheLock := func(r *commandRig, version int, err error) func() (int, error) {
		return func() (int, error) {
			if lockable(t, filepath.Join(r.dataDir, "backend.lock")) {
				t.Error("the schema version was read without the lock")
			}
			if present, _ := SnapshotPresent(r.layout); present {
				t.Error("the schema version was read after the copy")
			}
			return version, err
		}
	}
	t.Run("backed up", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		cmd := r.command("u1")
		cmd.SchemaVersion = schemaUnderTheLock(r, 118, nil)
		if result := cmd.Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK || result.Schema != 118 {
			t.Fatalf("result = %+v, want ok with schema 118", result)
		}
	})
	t.Run("refused for space", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		withFreeBytes(t, func(string) (uint64, error) { return 1, nil })
		cmd := r.command("u1")
		cmd.SchemaVersion = schemaUnderTheLock(r, 118, nil)
		if result := cmd.Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeRefused || result.Schema != 118 {
			t.Fatalf("result = %+v, want refused with schema 118", result)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		var logged []string
		cmd := r.command("u1")
		cmd.SchemaVersion = schemaUnderTheLock(r, 0, errors.New("file is not a database"))
		cmd.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
		if result := cmd.Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK || result.Schema != 0 {
			t.Fatalf("result = %+v, want ok without a schema", result)
		}
		if joined := strings.Join(logged, "\n"); !strings.Contains(joined, "read the database's schema version: file is not a database") {
			t.Fatalf("log = %q", joined)
		}
	})
}

func TestSnapshotCommandRefusesWithoutChangingAnything(t *testing.T) {
	t.Run("lock held", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		cmd := r.command("u1")
		cmd.AcquireLock = func(context.Context, time.Duration) (*os.File, func(), error) {
			return nil, nil, errors.New("another Agent Overflow backend already holds backend.lock")
		}
		result := cmd.Snapshot(context.Background(), nil)
		if result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, "still in use") {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("no space", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		withFreeBytes(t, func(string) (uint64, error) { return 1, nil })
		result := r.command("u1").Snapshot(context.Background(), nil)
		if result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, "Free at least") {
			t.Fatalf("result = %+v", result)
		}
		if present, _ := SnapshotPresent(r.layout); present {
			t.Fatal("a refused snapshot left a manifest")
		}
	})
	t.Run("host drive short", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		host := uint64(1)
		result := r.command("u1").Snapshot(context.Background(), &host)
		if result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, hostDiskDescription) {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("serve update pending", func(t *testing.T) {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		serve, err := NewLayout(r.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveState(serve, pendingState(t, "serve-u")); err != nil {
			t.Fatal(err)
		}
		result := r.command("u1").Snapshot(context.Background(), nil)
		if result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, "serve-u") {
			t.Fatalf("result = %+v", result)
		}
	})
}

// The space command runs while the version being replaced still holds the
// data root, so it must answer without the lock and change nothing.
func TestSpaceCommandAnswersWithoutTheLock(t *testing.T) {
	r := newCommandRig(t)
	cmd := r.command("u1")
	cmd.AcquireLock = func(context.Context, time.Duration) (*os.File, func(), error) {
		t.Fatal("the space command took the lock")
		return nil, nil, nil
	}
	if result := cmd.Space(nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("no database = %+v, want the snapshot step to refuse it", result)
	}
	writeFile(t, r.db, strings.Repeat("x", 4000))
	withFreeBytes(t, func(string) (uint64, error) { return SnapshotSpaceNeeded(4000) - 1, nil })
	result := cmd.Space(nil)
	if result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, "Free at least 1 MB") {
		t.Fatalf("short disk = %+v", result)
	}
	withFreeBytes(t, func(string) (uint64, error) { return SnapshotSpaceNeeded(4000), nil })
	host := uint64(1)
	if result := cmd.Space(&host); result.Outcome != UpdateOutcomeRefused || !strings.Contains(result.Reason, hostDiskDescription) {
		t.Fatalf("short host drive = %+v", result)
	}
	if result := cmd.Space(nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("enough room = %+v", result)
	}
	if present, _ := SnapshotPresent(r.layout); present {
		t.Fatal("the space command wrote a snapshot")
	}
}

func TestTrialRunCommandPreparesAndKeepsTheTrialsDatabase(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 1))
	if result.Outcome != UpdateOutcomePrepared {
		t.Fatalf("result = %+v", result)
	}
	if got := r.database(t); got != "migrated" {
		t.Fatalf("database = %q, want the trial's", got)
	}
	if present, _ := SnapshotPresent(r.layout); !present {
		t.Fatal("the snapshot was dropped before the caller committed")
	}
	if details := r.out.details(t); !strings.HasSuffix(details, "Stopping the trial") {
		t.Fatalf("progress %q does not end with the stop", details)
	}
	if !strings.Contains(r.read("log"), "stopped") {
		t.Fatal("the trial was not stopped")
	}
}

func TestTrialRunCommandRestoresTheSnapshotWhenTheTrialFails(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndCrashes, 1))
	// The result names the trial's last step, which the restore's report
	// may have replaced before the trial's was delivered.
	if result.Outcome != UpdateOutcomeRolledBack || !strings.Contains(result.Reason, "exit status 3") ||
		result.Step != "Applying migration 1 of 1" {
		t.Fatalf("result = %+v", result)
	}
	if got := r.database(t); got != "live" {
		t.Fatalf("database = %q, want the snapshot restored", got)
	}
	if !absent(t, r.layout.MarkerPath()) {
		t.Fatal("the restore left its marker")
	}
	if !strings.Contains(r.out.details(t), "Restoring the database") {
		t.Fatal("the restore reported no progress")
	}
	// The restore's reports say what the command did after the failure,
	// so a remembered failure does not name them as where it stopped.
	for _, event := range r.out.events(t) {
		if p := event.Progress; p != nil && strings.HasPrefix(p.Detail, "Restoring the database") && !RecoveryPhase(p.Phase) {
			t.Fatalf("the restore reports phase %q, which RecoveryPhase does not know", p.Phase)
		}
	}
}

func TestTrialRunCommandStopsOnADatabaseChangedSinceTheSnapshot(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	// A backend started by hand between the two commands wrote to it.
	writeFile(t, r.db, "written in the gap")
	for _, attempt := range []int{1, 2} {
		result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, attempt))
		if result.Outcome != UpdateOutcomeChanged || !strings.Contains(result.Reason, "changed after it was backed up") {
			t.Fatalf("attempt %d = %+v", attempt, result)
		}
	}
	if got := r.database(t); got != "written in the gap" {
		t.Fatalf("database = %q, want the gap's work kept", got)
	}
	if r.read("activate") != "" {
		t.Fatal("a trial ran on a database the snapshot does not match")
	}
}

// interruptAttempt runs attempt 1 until its output closes mid-migration,
// the way a launcher that dies takes its WSL command's pipe with it. The
// trial leaves "half-migrated".
func interruptAttempt(t *testing.T, r *commandRig) {
	t.Helper()
	r.out.failOn = "store.migrate"
	opts := r.trialOptions(`printf 'half-migrated' > "$DB"
progress store.migrate "Applying migration 1 of 9"
serve_until_stopped`, 1)
	opts.Rule = StallRule{Window: time.Minute, Ceiling: time.Minute}
	if result := r.command("u1").TrialRun(context.Background(), opts); result.Outcome != UpdateOutcomeFailed {
		t.Fatalf("interrupted attempt = %+v", result)
	}
	r.out = &lockedBuffer{}
}

func activations(r *commandRig) int {
	return strings.Count(r.read("activate"), "\n")
}

// Every attempt compares the live database with what the update last left
// it: a retry after an interrupted trial runs on that trial's database, and
// anything else that wrote since is another backend's work.
func TestTrialRunCommandChecksEachRetryAgainstWhatTheLastAttemptLeft(t *testing.T) {
	snapshotted := func(t *testing.T) *commandRig {
		r := newCommandRig(t)
		writeFile(t, r.db, "live")
		if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
			t.Fatalf("snapshot = %+v", result)
		}
		return r
	}
	t.Run("unchanged since an interrupted attempt", func(t *testing.T) {
		r := snapshotted(t)
		interruptAttempt(t, r)
		result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 2))
		if result.Outcome != UpdateOutcomePrepared {
			t.Fatalf("retry = %+v", result)
		}
	})
	t.Run("written since an interrupted attempt", func(t *testing.T) {
		r := snapshotted(t)
		interruptAttempt(t, r)
		before := activations(r)
		writeFile(t, r.db, "another backend's work")
		result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 2))
		if result.Outcome != UpdateOutcomeChanged || !strings.Contains(result.Reason, "interrupted trial left it") {
			t.Fatalf("retry = %+v", result)
		}
		if got := r.database(t); got != "another backend's work" {
			t.Fatalf("database = %q, want the other backend's work kept", got)
		}
		if activations(r) != before {
			t.Fatal("a trial ran on a database another backend changed")
		}
		if present, _ := SnapshotPresent(r.layout); !present {
			t.Fatal("the snapshot is gone")
		}
	})
	// A launcher that died before it recorded the commit retries a trial
	// that prepared.
	t.Run("written since a prepared attempt", func(t *testing.T) {
		r := snapshotted(t)
		if result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 1)); result.Outcome != UpdateOutcomePrepared {
			t.Fatalf("attempt 1 = %+v", result)
		}
		r.out = &lockedBuffer{}
		writeFile(t, r.db, "written after the trial prepared")
		result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 2))
		if result.Outcome != UpdateOutcomeChanged {
			t.Fatalf("retry = %+v", result)
		}
	})
	t.Run("written since a rolled-back attempt", func(t *testing.T) {
		r := snapshotted(t)
		if result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndCrashes, 1)); result.Outcome != UpdateOutcomeRolledBack {
			t.Fatalf("attempt 1 = %+v", result)
		}
		r.out = &lockedBuffer{}
		writeFile(t, r.db, "written after the restore")
		result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 2))
		if result.Outcome != UpdateOutcomeChanged {
			t.Fatalf("retry = %+v", result)
		}
	})
	// The command died with its trial: nothing recorded the end, and the
	// trial's own writes cannot be told from anyone else's. The retry
	// restores the snapshot, which ends the dead attempt, and runs on it.
	t.Run("after an attempt that never recorded its end", func(t *testing.T) {
		r := snapshotted(t)
		interruptAttempt(t, r)
		if err := RecordAttempt(r.layout, r.dataDir, 1, false); err != nil {
			t.Fatal(err)
		}
		writeFile(t, r.db, "the dead trial's last write")
		var logged []string
		cmd := r.command("u1")
		cmd.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
		result := cmd.TrialRun(context.Background(), r.trialOptions(trialRecordsItsStart+trialWritesAndPrepares, 2))
		if result.Outcome != UpdateOutcomePrepared {
			t.Fatalf("retry = %+v", result)
		}
		if got := r.read("seen"); got != "live\n" {
			t.Fatalf("the retry's trial started on %q, want the snapshot", got)
		}
		if !strings.Contains(strings.Join(logged, "\n"), "restoring the database backup before attempt 2") {
			t.Fatalf("log %q does not say the retry restored", logged)
		}
		if !strings.Contains(r.out.details(t), "Restoring the database") {
			t.Fatal("the restore reported no progress")
		}
		snapshot, _, err := ReadSnapshot(r.layout)
		if err != nil || snapshot.Left == nil || snapshot.Left.Attempt != 2 || !snapshot.Left.Ended {
			t.Fatalf("manifest = %+v, %v; want attempt 2 ended", snapshot.Left, err)
		}
	})
	// A trial that failed and whose restore did not finish leaves the
	// restore marked. The next command finishes it, which ends the
	// attempt, so the retry is checked against the restored database.
	t.Run("after a restore that did not finish", func(t *testing.T) {
		for _, foreign := range []bool{false, true} {
			r := snapshotted(t)
			failRestore(t, r, func() {
				result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndCrashes, 1))
				if result.Outcome != UpdateOutcomeFailed || !strings.Contains(result.Reason, "could not be restored") {
					t.Fatalf("attempt 1 = %+v", result)
				}
			})
			if absent(t, r.layout.MarkerPath()) {
				t.Fatal("the failed restore left no marker")
			}
			if foreign {
				// Another backend finishes the marked restore and writes.
				if err := PrepareDataRoot(r.dataDir, PrepareOptions{}); err != nil {
					t.Fatal(err)
				}
				writeFile(t, r.db, "another backend's work")
			}
			r.out = &lockedBuffer{}
			result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialRecordsItsStart+trialWritesAndPrepares, 2))
			if foreign {
				if result.Outcome != UpdateOutcomeChanged {
					t.Fatalf("retry after another backend's write = %+v", result)
				}
				continue
			}
			if result.Outcome != UpdateOutcomePrepared {
				t.Fatalf("retry = %+v", result)
			}
			if got := r.read("seen"); got != "live\n" {
				t.Fatalf("the retry's trial started on %q, want the restored snapshot", got)
			}
		}
	})
}

// trialRecordsItsStart prepends a note of the database the trial found.
const trialRecordsItsStart = `cat "$DB" >> "$OBS/seen"
printf '\n' >> "$OBS/seen"
`

// failRestore runs fn while the data directory refuses the restore's
// removal of the live files, the way a full or failing disk would.
func failRestore(t *testing.T, r *commandRig, fn func()) {
	t.Helper()
	if err := os.Chmod(r.dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(r.dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}()
	fn()
}

func TestTrialRunCommandRequiresThisUpdatesSnapshot(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	result := r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 1))
	if result.Outcome != UpdateOutcomeNoSnapshot {
		t.Fatalf("no snapshot = %+v", result)
	}
	if result := r.command("other").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	result = r.command("u1").TrialRun(context.Background(), r.trialOptions(trialWritesAndPrepares, 1))
	if result.Outcome != UpdateOutcomeNoSnapshot || !strings.Contains(result.Reason, `"other"`) {
		t.Fatalf("another update's snapshot = %+v", result)
	}
	if got := r.database(t); got != "live" {
		t.Fatalf("database = %q", got)
	}
}

// A supervisor that went away takes the command's output with it. The trial
// stops and nothing is restored: the update's recovery belongs to whoever
// resumes it, and a restore racing that recovery could undo a commit.
func TestTrialRunCommandStopsWithoutRestoringWhenItsOutputCloses(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	r.out.failOn = "store.migrate"
	opts := r.trialOptions(`printf 'half-migrated' > "$DB"
progress store.migrate "Applying migration 1 of 9"
note ready
serve_until_stopped`, 1)
	opts.Rule = StallRule{Window: time.Minute, Ceiling: time.Minute}
	result := r.command("u1").TrialRun(context.Background(), opts)
	if result.Outcome != UpdateOutcomeFailed || !strings.Contains(result.Reason, "interrupted") {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(r.read("log"), "stopped") {
		t.Fatal("the trial was left running")
	}
	if got := r.database(t); got != "half-migrated" {
		t.Fatalf("database = %q, want it left for recovery", got)
	}
	if present, _ := SnapshotPresent(r.layout); !present {
		t.Fatal("the snapshot recovery needs is gone")
	}
}

func TestRestoreCommandPutsTheSnapshotBackOrFinishesAMarkedOne(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	writeFile(t, r.db, "trial")
	if result := r.command("u1").Restore(context.Background(), "the launcher stopped the trial"); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("restore = %+v", result)
	}
	if got := r.database(t); got != "live" {
		t.Fatalf("database = %q", got)
	}

	// Interrupted mid-restore: the marker is down and the database is gone.
	writeFile(t, r.layout.MarkerPath(), `{"updateId":"u1","dataDir":`+quote(r.dataDir)+`,"reason":"x","writtenAtMs":1}`)
	if err := os.Remove(r.db); err != nil {
		t.Fatal(err)
	}
	if result := r.command("u1").Restore(context.Background(), "again"); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("resumed restore = %+v", result)
	}
	if got := r.database(t); got != "live" || !absent(t, r.layout.MarkerPath()) {
		t.Fatalf("database = %q after the resumed restore", got)
	}
}

func TestDiscardCommandKeepsASnapshotARestoreStillNeeds(t *testing.T) {
	r := newCommandRig(t)
	writeFile(t, r.db, "live")
	if result := r.command("u1").Snapshot(context.Background(), nil); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("snapshot = %+v", result)
	}
	writeFile(t, r.layout.MarkerPath(), `{"updateId":"u1","dataDir":`+quote(r.dataDir)+`,"reason":"x","writtenAtMs":1}`)
	if result := r.command("u1").Discard(context.Background()); result.Outcome != UpdateOutcomeRefused {
		t.Fatalf("discard under a marker = %+v", result)
	}
	if present, _ := SnapshotPresent(r.layout); !present {
		t.Fatal("the snapshot a restore needs was discarded")
	}
	if err := os.Remove(r.layout.MarkerPath()); err != nil {
		t.Fatal(err)
	}
	if result := r.command("u1").Discard(context.Background()); result.Outcome != UpdateOutcomeOK {
		t.Fatalf("discard = %+v", result)
	}
	if !absent(t, r.layout.SnapshotDir()) {
		t.Fatal("the snapshot survived its discard")
	}
}
