package supervise

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/startupprogress"
)

// FailedTrial remembers a migration or update trial that settled without
// committing (docs/specs/app-update.md, failure memory). A launch that would
// run the same build's trial over a database at the same schema version
// shows it and runs the trial again only when asked; a different build or
// schema version runs normally, and a trial that commits removes it. It is
// kept beside the durable record, one per record.
type FailedTrial struct {
	// Build is the version whose trial failed: the target of an update, or
	// the installed version for a migration.
	Build string `json:"build"`
	// Schema is the database's migration version the trial started from,
	// the one store.MigrationsPendingError reports as Database.
	Schema int `json:"schema"`
	// Reason is the settled reason.
	Reason string `json:"reason"`
	// Phase is the last progress the failing step reported, empty when it
	// reported none.
	Phase string `json:"phase,omitempty"`
	// AtMs is when the failure settled.
	AtMs int64 `json:"atMs"`
}

// Matches reports whether a trial of build over a database at schema would
// repeat this failure.
func (f FailedTrial) Matches(build string, schema int) bool {
	return f.Build == build && f.Schema == schema
}

// Validate refuses a memory that cannot name the trial it remembers.
func (f FailedTrial) Validate() error {
	if err := ValidVersion(f.Build); err != nil {
		return fmt.Errorf("supervise: failed trial build: %w", err)
	}
	if f.Schema <= 0 {
		return fmt.Errorf("supervise: failed trial of %s names schema version %d", f.Build, f.Schema)
	}
	return nil
}

// LoadFailedTrial reads the memory at path. found is false with a nil error
// when there is none.
func LoadFailedTrial(path string) (FailedTrial, bool, error) {
	var failed FailedTrial
	found, err := atomicfile.ReadJSON(path, &failed)
	if err != nil {
		return FailedTrial{}, false, fmt.Errorf("supervise: read the failed trial: %w", err)
	}
	if !found {
		return FailedTrial{}, false, nil
	}
	if err := failed.Validate(); err != nil {
		return FailedTrial{}, true, err
	}
	return failed, true, nil
}

// SaveFailedTrial writes the memory durably, replacing any earlier one.
func SaveFailedTrial(path string, failed FailedTrial) error {
	if err := failed.Validate(); err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(path, failed); err != nil {
		return fmt.Errorf("supervise: write the failed trial: %w", err)
	}
	return nil
}

// ForgetFailedTrial removes the memory. None is not an error.
func ForgetFailedTrial(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("supervise: remove the failed trial: %w", err)
	}
	return nil
}

// FailedTrialPath is the failure memory beside the record at recordPath.
func FailedTrialPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, ".json") + ".failed-trial.json"
}

// trialTrace is what a failed trial is remembered by: the schema version the
// database started from and the last progress the failing step reported.
type trialTrace struct {
	schema int
	phase  string
}

// observe keeps the report's detail unless it is the step's recovery after
// the failure.
func (t *trialTrace) observe(p startupprogress.Progress) {
	if p.Detail != "" && !RecoveryPhase(p.Phase) {
		t.phase = p.Detail
	}
}

// rememberFailedTrial records that build's trial settled without committing,
// replacing any earlier memory. Without a schema version the failure cannot
// be matched to a later launch, so it is not remembered.
func (r UpdateRun) rememberFailedTrial(build string, trace trialTrace, reason string) {
	if trace.schema <= 0 {
		r.logf("updater: the failed trial of %s is not remembered: the database's schema version is unknown", build)
		return
	}
	failed := FailedTrial{
		Build: build, Schema: trace.schema, Reason: reason, Phase: trace.phase, AtMs: r.now().UnixMilli(),
	}
	if err := SaveFailedTrial(r.MemoryPath, failed); err != nil {
		r.logf("updater: remember the failed trial of %s: %v", build, err)
	}
}

// forgetFailedTrial removes the memory. A memory left behind names a build
// or schema version the next migration gate no longer matches, and the gate
// removes it then.
func (r UpdateRun) forgetFailedTrial() {
	if err := ForgetFailedTrial(r.MemoryPath); err != nil {
		r.logf("updater: %v", err)
	}
}

// rememberedFailure is the failure memory when it names build's trial over
// schema. A memory of another build or schema version no longer applies and
// is removed. One that cannot be read is logged and treated as none: the
// migration runs, and its outcome replaces or removes the file.
func (r UpdateRun) rememberedFailure(build string, schema int) (FailedTrial, bool) {
	failed, found, err := LoadFailedTrial(r.MemoryPath)
	switch {
	case err != nil:
		r.logf("updater: %v; running the database upgrade", err)
		return FailedTrial{}, false
	case !found:
		return FailedTrial{}, false
	case failed.Matches(build, schema):
		return failed, true
	}
	r.logf("updater: forgetting the failed trial of %s over schema v%d: this launch runs %s over schema v%d",
		failed.Build, failed.Schema, build, schema)
	r.forgetFailedTrial()
	return FailedTrial{}, false
}
