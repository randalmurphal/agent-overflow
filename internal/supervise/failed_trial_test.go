package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
)

// stepSteps is UpdateSteps whose trial reports only the command's own step
// and names where it failed on its result, as a trial whose last report was
// coalesced away before delivery does.
type stepSteps struct{ saved []State }

func (s *stepSteps) Save(state State) error { s.saved = append(s.saved, state); return nil }
func (s *stepSteps) RemoveRecord() error    { return nil }
func (s *stepSteps) Snapshot(context.Context, func(startupprogress.Progress)) (UpdateEvent, error) {
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK, Schema: 7}, nil
}
func (s *stepSteps) Trial(_ context.Context, _ string, _ int, progress func(startupprogress.Progress)) (UpdateEvent, error) {
	progress(startupprogress.Progress{Phase: "update.trial", Detail: "Starting v2.0.0"})
	progress(startupprogress.Progress{Phase: phaseRestore, Detail: "Restoring the database"})
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeRolledBack,
		Reason: "the new version exited before it finished starting: exit status 3", Step: "Applying migration 1 of 1"}, nil
}
func (s *stepSteps) Restore(context.Context, string, func(startupprogress.Progress)) (UpdateEvent, error) {
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}, nil
}
func (s *stepSteps) Discard(context.Context, func(startupprogress.Progress)) (UpdateEvent, error) {
	return UpdateEvent{Type: UpdateEventResult, Outcome: UpdateOutcomeOK}, nil
}
func (s *stepSteps) RemoveStaged(context.Context) error { return nil }
func (s *stepSteps) Publish(context.Context) error      { return nil }

// TestTheFailureMemoryNamesTheStepTheTrialResultCarries: the step a failed
// trial names on its result is the one remembered, whatever progress
// reached the run before the restore's.
func TestTheFailureMemoryNamesTheStepTheTrialResultCarries(t *testing.T) {
	memory := filepath.Join(t.TempDir(), "service-state.failed-trial.json")
	base, err := Adopt("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Begin("0123456789abcdef", "2.0.0", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run := UpdateRun{Steps: &stepSteps{}, MemoryPath: memory, LogName: "update log", Logf: func(string, ...any) {}}
	end, err := run.Apply(context.Background(), state)
	if err != nil || end.State != UpdateRolledBack {
		t.Fatalf("Apply = %+v, %v", end, err)
	}
	failed, found, err := LoadFailedTrial(memory)
	if err != nil || !found || failed.Phase != "Applying migration 1 of 1" || failed.Schema != 7 {
		t.Fatalf("remembered %+v (found %t, %v), want the result's step", failed, found, err)
	}
}

// TestFailedTrialRoundTripsAndMatchesOneBuildOverOneSchema: the memory is
// written durably, read back as written, matches only its own build over
// its own schema version, and is gone once forgotten.
func TestFailedTrialRoundTripsAndMatchesOneBuildOverOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "app-update-prod.ubuntu.failed-trial.json")
	if _, found, err := LoadFailedTrial(path); found || err != nil {
		t.Fatalf("LoadFailedTrial(none) = found %v, %v", found, err)
	}
	failed := FailedTrial{Build: "2.0.0", Schema: 118, Reason: "migration v119 failed", Phase: "Applying migration 1 of 3", AtMs: 5}
	if err := SaveFailedTrial(path, failed); err != nil {
		t.Fatal(err)
	}
	got, found, err := LoadFailedTrial(path)
	if err != nil || !found || got != failed {
		t.Fatalf("LoadFailedTrial = %+v, %v, %v; want %+v", got, found, err, failed)
	}
	for _, c := range []struct {
		build  string
		schema int
		want   bool
	}{
		{"2.0.0", 118, true},
		{"2.0.1", 118, false},
		{"2.0.0", 119, false},
	} {
		if got := failed.Matches(c.build, c.schema); got != c.want {
			t.Errorf("Matches(%s, %d) = %v, want %v", c.build, c.schema, got, c.want)
		}
	}
	if err := ForgetFailedTrial(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the forgotten memory survived: %v", err)
	}
	if err := ForgetFailedTrial(path); err != nil {
		t.Fatalf("forgetting no memory = %v", err)
	}
}

// TestFailedTrialRefusesAMemoryThatNamesNoTrial: a memory without a build or
// a schema version could match nothing, so it is neither written nor read.
func TestFailedTrialRefusesAMemoryThatNamesNoTrial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed-trial.json")
	for _, bad := range []FailedTrial{
		{Build: "", Schema: 118},
		{Build: "2.0.0", Schema: 0},
		{Build: "../2.0.0", Schema: 118},
	} {
		if err := SaveFailedTrial(path, bad); err == nil {
			t.Errorf("SaveFailedTrial(%+v) wrote it", bad)
		}
	}
	writeFile(t, path, `{"build":"2.0.0","schema":0,"reason":"r","atMs":1}`)
	if _, found, err := LoadFailedTrial(path); !found || err == nil || !strings.Contains(err.Error(), "schema version 0") {
		t.Fatalf("LoadFailedTrial(schema 0) = found %v, %v", found, err)
	}
	writeFile(t, path, `{`)
	if _, _, err := LoadFailedTrial(path); err == nil {
		t.Fatal("LoadFailedTrial read a torn file")
	}
}

// TestUpdateRecordKeepsItsSchemaThroughItsTransitions: the schema version an
// update started from survives each attempt and the settlement, and a
// negative one is refused.
func TestUpdateRecordKeepsItsSchemaThroughItsTransitions(t *testing.T) {
	base, err := Adopt("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	state, err := base.Begin("u1", "2.0.0", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	update := *state.Update
	update.FromSchema = 118
	state.Update = &update
	if state, err = state.Retry(); err != nil {
		t.Fatal(err)
	}
	if state, err = state.Settle(UpdateRolledBack, "r", time.UnixMilli(2)); err != nil {
		t.Fatal(err)
	}
	if state, _, err = state.MarkReported(); err != nil {
		t.Fatal(err)
	}
	if state.Update.FromSchema != 118 {
		t.Fatalf("FromSchema = %d after the transitions, want 118", state.Update.FromSchema)
	}
	update = *state.Update
	update.FromSchema = -1
	state.Update = &update
	if err := state.Validate(); err == nil {
		t.Fatal("a negative schema version validated")
	}
}
