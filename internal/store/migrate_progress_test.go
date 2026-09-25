package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"
)

// fileDBThrough writes a database file migrated through target and closes
// it, so the next open finds every later migration pending.
func fileDBThrough(t *testing.T, target int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pending.db")
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version > target {
			break
		}
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// recordedVersion reads the newest applied migration through a
// connection of its own, which sees only committed work.
func recordedVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(path, readerConnPragmas))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer db.Close()
	version, err := currentMigrationVersion(db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	return version
}

// pendingTail is the chain from its last Rebuild migration but the final
// migration on, which the fixtures below leave pending: the rebuild is a
// migration whose parts are reported, and the tail holds at least one
// migration after it, which a cancel after the first commit leaves
// pending, whatever the chain grows to. target is the version the fixture
// migrates through.
func pendingTail(t *testing.T) (target int, pending []Migration) {
	t.Helper()
	last := -1
	for i, m := range migrations[:len(migrations)-1] {
		if m.Rebuild {
			last = i
		}
	}
	if last < 1 {
		t.Fatal("the chain has no rebuild migration after its first and before its last")
	}
	return migrations[last-1].Version, migrations[last:]
}

// TestNewWithOptionsReportsEachPendingMigrationBeforeItRuns pins the hook
// the boot turns into "Applying migration k of n": one call per pending
// migration, in chain order, counted from 1, and each before its
// migration commits. A rebuild also reports each index build and then its
// foreign key check, under its own step and before it commits.
func TestNewWithOptionsReportsEachPendingMigrationBeforeItRuns(t *testing.T) {
	target, pending := pendingTail(t)
	path := fileDBThrough(t, target)

	var steps, parts []MigrationStep
	var recordedAtHook []int
	st, err := NewWithOptions(path, Options{OnMigration: func(step MigrationStep) {
		if step.Activity != "" {
			parts = append(parts, step)
			if got := recordedVersion(t, path); got >= step.Version {
				t.Errorf("v%d %q reported after the migration committed", step.Version, step.Activity)
			}
			return
		}
		steps = append(steps, step)
		recordedAtHook = append(recordedAtHook, recordedVersion(t, path))
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	indexName := regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+)\s+ON`)
	rebuilds := 0
	for i, m := range pending {
		var want, got []string
		if m.Rebuild {
			rebuilds++
			for _, match := range indexName.FindAllStringSubmatch(m.SQL, -1) {
				want = append(want, "building index "+match[1])
			}
			want = append(want, "checking foreign keys")
		}
		for _, part := range parts {
			if part.Version != m.Version {
				continue
			}
			if part.Name != m.Name || part.Index != i+1 || part.Pending != len(pending) {
				t.Errorf("v%d part %+v does not carry its migration's step", m.Version, part)
			}
			got = append(got, part.Activity)
		}
		if !slices.Equal(got, want) {
			t.Errorf("v%d (%s) reported %q, want %q", m.Version, m.Name, got, want)
		}
	}
	if rebuilds == 0 {
		t.Fatal("the pending tail holds no rebuild migration")
	}

	if len(steps) != len(pending) {
		t.Fatalf("reported %d steps, want %d: %+v", len(steps), len(pending), steps)
	}
	previous := target
	for i, step := range steps {
		want := MigrationStep{Version: pending[i].Version, Name: pending[i].Name, Index: i + 1, Pending: len(pending)}
		if step != want {
			t.Errorf("step %d = %+v, want %+v", i, step, want)
		}
		if recordedAtHook[i] != previous {
			t.Errorf("at the hook for v%d the recorded version was %d, want %d (the hook runs before its migration)", step.Version, recordedAtHook[i], previous)
		}
		previous = step.Version
	}

	var reopened []MigrationStep
	again, err := NewWithOptions(filepath.Join(t.TempDir(), "fresh.db"), Options{OnMigration: func(step MigrationStep) {
		if step.Activity == "" {
			reopened = append(reopened, step)
		}
	}})
	if err != nil {
		t.Fatalf("fresh open: %v", err)
	}
	_ = again.Close()
	if len(reopened) != len(migrations) || reopened[0].Index != 1 || reopened[len(reopened)-1].Pending != len(migrations) {
		t.Fatalf("fresh database reported %d steps, want the whole chain counted from 1", len(reopened))
	}
}

// TestNewWithOptionsCancelledMidChainRollsBack: cancelling the boot while
// migrations run fails the open with the context's error and leaves every
// migration that had not committed pending for the next open.
func TestNewWithOptionsCancelledMidChainRollsBack(t *testing.T) {
	target, pending := pendingTail(t)
	path := fileDBThrough(t, target)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := NewWithOptions(path, Options{Context: ctx, OnMigration: func(step MigrationStep) {
		if step.Index == 2 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("open cancelled mid-chain = %v, want context.Canceled", err)
	}
	if got := recordedVersion(t, path); got != pending[0].Version {
		t.Fatalf("recorded version after cancel = %d, want %d (only the first pending migration committed)", got, pending[0].Version)
	}

	var resumed []MigrationStep
	st, err := NewWithOptions(path, Options{OnMigration: func(step MigrationStep) {
		if step.Activity == "" {
			resumed = append(resumed, step)
		}
	}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if len(resumed) != len(pending)-1 || resumed[0].Version != pending[1].Version || resumed[0].Index != 1 {
		t.Fatalf("reopen ran %+v, want the %d migrations the cancel left pending", resumed, len(pending)-1)
	}
	if got := recordedVersion(t, path); got != pending[len(pending)-1].Version {
		t.Fatalf("recorded version after reopen = %d, want %d", got, pending[len(pending)-1].Version)
	}
}

// slowMigrationSQL writes one row after counting to a billion, which runs
// for minutes: long enough that only an interrupt ends it in a test.
const slowMigrationSQL = `CREATE TABLE slow_probe AS
	WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000000000)
	SELECT count(*) AS n FROM c`

// TestMigrationInterruptedMidStatementRollsBack: cancelling the boot while
// a migration statement runs interrupts that statement, rolls its
// transaction back and records nothing. A rebuild also hands its pinned
// connection back with foreign keys enforced.
func TestMigrationInterruptedMidStatementRollsBack(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "ordinary"
		if rebuild {
			name = "rebuild"
		}
		t.Run(name, func(t *testing.T) {
			db := migrateThrough(t, migrations[len(migrations)-1].Version)
			slow := Migration{Version: 1_000_000, Name: "slow_probe", SQL: slowMigrationSQL, Rebuild: rebuild}
			apply := applyMigrationContext
			if rebuild {
				apply = applyRebuildMigrationContext
			}
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(200*time.Millisecond, cancel)
			defer timer.Stop()
			started := time.Now()
			err := apply(ctx, db, slow)
			if err == nil {
				t.Fatal("the slow migration finished; it was not interrupted")
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("interrupt took %s", elapsed)
			}
			var tables int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'slow_probe'`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if tables != 0 {
				t.Fatal("the interrupted migration's table survived")
			}
			var recorded int
			if err := db.QueryRow(`SELECT count(*) FROM migration_versions WHERE version = ?`, slow.Version).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if recorded != 0 {
				t.Fatal("the interrupted migration was recorded as applied")
			}
			var enabled int
			if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if enabled != 1 {
				t.Fatal("an interrupted migration left foreign_keys off")
			}
		})
	}
}
