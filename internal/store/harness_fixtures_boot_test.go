package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootSweepFailureFixtureEnv names the database file
// TestBootSweepFailureHarnessFixture writes for
// e2e/tests/boot-sweep-failure.spec.ts.
const bootSweepFailureFixtureEnv = "AO_TEST_BOOT_SWEEP_FAILURE_FIXTURE"

const (
	fixtureCrashedThread = "fixture-crashed"
	// fixtureBootSweepError is what the fixture's trigger raises, and so
	// the error the harness boot reports for its crashed-turn sweep.
	fixtureBootSweepError = "fixture: turns are read-only"
)

// TestBootSweepFailureHarnessFixture writes a current database whose
// crashed-turn boot sweep fails: a thread with a turn left in flight, and a
// trigger that refuses every write to a turn. It runs only when
// bootSweepFailureFixtureEnv names an absolute path whose directory exists
// and which does not exist yet.
func TestBootSweepFailureHarnessFixture(t *testing.T) {
	writeBootSweepFailureFixture(t, harnessFixturePath(t, bootSweepFailureFixtureEnv))
}

// TestBootSweepFailureFixtureFailsTheSweep: the crash sweep over the
// fixture fails with the trigger's error and leaves the turn in flight, so
// the fixture the harness boots exercises the failure.
func TestBootSweepFailureFixtureFailsTheSweep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-overflow.db")
	writeBootSweepFailureFixture(t, path)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if _, err := s.RecoverCrashedTurns(interruptedForTest, time.Now().UnixMilli()); err == nil || !strings.Contains(err.Error(), fixtureBootSweepError) {
		t.Fatalf("RecoverCrashedTurns() = %v, want the fixture's refusal", err)
	}
	if _, open, err := s.GetActiveTurn(fixtureCrashedThread); err != nil || !open {
		t.Fatalf("the fixture's turn is not in flight after the failed sweep (open %v, %v)", open, err)
	}
}

func writeBootSweepFailureFixture(t *testing.T, path string) {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(Project{
		ID: defaultTestProjectID, Path: filepath.Dir(path), Name: "Boot sweep failure fixture",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(makeThread(fixtureCrashedThread, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTurn(Turn{TurnID: fixtureCrashedThread + ":0", ThreadID: fixtureCrashedThread, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `CREATE TRIGGER fixture_turns_read_only BEFORE UPDATE ON turns
BEGIN SELECT RAISE(ABORT, '`+fixtureBootSweepError+`'); END`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
