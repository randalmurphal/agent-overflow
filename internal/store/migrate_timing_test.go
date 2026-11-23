package store

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// durationsLogged counts the lines in output that match pattern, whose
// first group is a duration, failing on one that does not parse.
func durationsLogged(t *testing.T, output, pattern string) int {
	t.Helper()
	re := regexp.MustCompile(pattern)
	matches := re.FindAllStringSubmatch(output, -1)
	for _, m := range matches {
		if _, err := time.ParseDuration(m[1]); err != nil {
			t.Errorf("%q: %v", m[0], err)
		}
	}
	return len(matches)
}

// An update's trial runs its migrations, and the log says how long each
// one took, and separately how long a rebuild's foreign key check took. A
// migration that fails says so with how long it ran. Serial: it captures
// the standard logger.
func TestMigrationsLogTheirDurations(t *testing.T) {
	db := migratedDB(t)
	logged := captureLog(t)
	latest := migrations[len(migrations)-1].Version

	plain := Migration{Version: latest + 1, Name: "timed_plain", SQL: `CREATE TABLE timed_plain (id INTEGER PRIMARY KEY);`}
	rebuild := Migration{Version: latest + 2, Name: "timed_rebuild", Rebuild: true,
		SQL: `CREATE TABLE timed_rebuild (id INTEGER PRIMARY KEY); CREATE INDEX timed_rebuild_id ON timed_rebuild(id);`}
	broken := Migration{Version: latest + 3, Name: "timed_broken", SQL: `CREATE TABLE timed_plain (id INTEGER PRIMARY KEY);`}
	if err := applyMigrationContext(context.Background(), db, plain); err != nil {
		t.Fatal(err)
	}
	if err := applyRebuildMigrationSteps(context.Background(), db, rebuild, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrationContext(context.Background(), db, broken); err == nil {
		t.Fatal("the broken migration applied")
	}

	output := logged()
	for _, want := range []struct {
		pattern string
		count   int
	}{
		{`store: migration v\d+ applied duration=(\S+)`, 1},
		{`store: rebuild migration v\d+ applied duration=(\S+)`, 1},
		{`store: rebuild migration v\d+ foreign key check duration=(\S+)`, 1},
		{`store: migration v\d+ failed duration=(\S+): `, 1},
	} {
		if n := durationsLogged(t, output, want.pattern); n != want.count {
			t.Errorf("%d lines match %s, want %d\nlog:\n%s", n, want.pattern, want.count, output)
		}
	}
}

// Opening and closing a store checkpoints its WAL; a trial's shutdown
// waits on the close-time one, so both say how long they took.
func TestStoreLogsItsCheckpointDurations(t *testing.T) {
	logged := captureLog(t)
	s, err := New(newTestStorePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	output := logged()
	for _, moment := range []string{"boot", "close"} {
		pattern := `store: ` + moment + ` WAL checkpoint duration=(\S+) frames=\d+`
		if n := durationsLogged(t, output, pattern); n != 1 {
			t.Errorf("%d lines match %s, want 1\nlog:\n%s", n, pattern, strings.TrimSpace(output))
		}
	}
}
