package supervise

import (
	"errors"
	"fmt"
	"os"

	"agent-overflow/internal/atomicfile"
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
