package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The auto_vacuum conversion is the last step of v119's deferred phase.
// These tests pin that it waits for the host's quiet moment only when there
// is something to convert, retries a snapshot a commit invalidated, records
// a wait it could not make, and records nothing on a quit.

func TestConversionStepFinishesAtOnceOnAnIncrementalDatabase(t *testing.T) {
	s := newTestStore(t)
	waits := 0
	run := &deferredRun{host: DeferredHost{AwaitFileSwap: func(context.Context) error {
		waits++
		return nil
	}}}
	if err := convertToIncrementalVacuumStep(context.Background(), s, run); err != nil {
		t.Fatal(err)
	}
	if waits != 0 || run.failures != 0 {
		t.Fatalf("waits = %d, failures = %d on an incremental database", waits, run.failures)
	}
}

func TestV119PhaseConvertsALegacyDatabase(t *testing.T) {
	s := newLegacyStore(t)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	raced := false
	s.convertHooks.afterSnapshot = func() {
		if raced {
			return
		}
		raced = true
		// A commit during the snapshot: the attempt is discarded.
		if err := s.SetUIState("client:test", map[string]string{"k": "raced"}); err != nil {
			t.Errorf("racing write: %v", err)
		}
	}
	defer func() { s.convertHooks.afterSnapshot = nil }()
	waits := 0
	host := DeferredHost{AwaitFileSwap: func(ctx context.Context) error {
		waits++
		return ctx.Err()
	}}

	if err := s.RunDeferredMigrations(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if waits != 2 {
		t.Fatalf("the step waited %d times, want one wait per attempt (2)", waits)
	}
	if mode, err := s.AutoVacuumMode(); err != nil || mode != AutoVacuumIncremental {
		t.Fatalf("auto_vacuum = %v (%v), want incremental", mode, err)
	}
	if deferredPending(t, s) || deferredWatermarkOf(t, s) != 119 {
		t.Fatalf("watermark = %d after the conversion", deferredWatermarkOf(t, s))
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a converted database recorded a failure: %+v", failure)
	}
	if state, err := s.GetUIState("client:test"); err != nil || state["k"] != "raced" {
		t.Fatalf("the racing write = %q (%v)", state["k"], err)
	}
}

// The swap installs a VACUUM INTO copy, which keeps the header's
// user_version, so the deferred watermark and the failure record survive a
// conversion.
func TestConversionKeepsTheDeferredState(t *testing.T) {
	s := newLegacyStore(t)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	mustExec(t, s.db, `INSERT INTO deferred_migration_failures(version,failures,first_error,failed_at) VALUES(119,1,'kept',1)`)
	result, err := s.ConvertToIncrementalVacuum(context.Background())
	if err != nil || result.Outcome != ConvertConverted {
		t.Fatalf("convert = %+v, %v", result, err)
	}
	if deferredWatermarkOf(t, s) != 118 {
		t.Fatalf("watermark = %d after the swap, want 118", deferredWatermarkOf(t, s))
	}
	if failure := deferredFailureOf(t, s); failure == nil || failure.FirstError != "kept" {
		t.Fatalf("failure record after the swap = %+v", failure)
	}
}

func TestV119PhaseRecordsAConversionThatCannotWait(t *testing.T) {
	s := newLegacyStore(t)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	host := DeferredHost{AwaitFileSwap: func(context.Context) error { return errors.New("no commit watcher") }}
	if err := s.RunDeferredMigrations(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	if !deferredPending(t, s) {
		t.Fatal("a conversion that could not wait moved the watermark")
	}
	failure := deferredFailureOf(t, s)
	if failure == nil || failure.Failures != 1 || !strings.Contains(failure.FirstError, "auto_vacuum_conversion") ||
		!strings.Contains(failure.FirstError, "no commit watcher") {
		t.Fatalf("recorded failure = %+v", failure)
	}
	if mode, err := s.AutoVacuumMode(); err != nil || mode != AutoVacuumNone {
		t.Fatalf("auto_vacuum = %v (%v), want the file untouched", mode, err)
	}
}

func TestV119PhaseQuitDuringTheConversionWaitRecordsNothing(t *testing.T) {
	s := newLegacyStore(t)
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	ctx, cancel := context.WithCancel(context.Background())
	host := DeferredHost{AwaitFileSwap: func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}}
	if err := s.RunDeferredMigrations(ctx, host); err != nil {
		t.Fatal(err)
	}
	if !deferredPending(t, s) {
		t.Fatal("a quit moved the watermark")
	}
	if failure := deferredFailureOf(t, s); failure != nil {
		t.Fatalf("a quit recorded a failure: %+v", failure)
	}
}
