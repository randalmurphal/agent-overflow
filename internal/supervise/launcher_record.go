package supervise

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/atomicfile"
)

// LauncherRecord is the Windows launcher's in-app update record
// (docs/specs/app-update.md). The launcher supervises that update, so the
// record lives on the Windows side, where it can be read before WSL starts and
// fsync and rename are native. It is State with one update, plus what a later
// launch needs to resume or recover that update. The WSL backend reads it
// through /mnt/c for the reason a rollback shows.
type LauncherRecord struct {
	State
	// Distro is the WSL distribution the payload runs in.
	Distro string `json:"distro"`
	// StablePayload is the Linux path the launcher runs. It holds the
	// previous version's backend until commit.
	StablePayload string `json:"stablePayload"`
	// StagedPayload is the target's backend, beside StablePayload on the same
	// filesystem so commit is a rename.
	StagedPayload string `json:"stagedPayload"`
	// StagedLauncher is the target launcher's Windows path.
	StagedLauncher string `json:"stagedLauncher"`
	// InstallPath is the launcher path the user starts.
	InstallPath string `json:"installPath"`
	// TargetFingerprint is the target launcher's embedded payload digest. A
	// launcher at InstallPath whose own digest matches is the target.
	TargetFingerprint string `json:"targetFingerprint"`
}

// LauncherRecordDir is the directory under the launcher's config directory
// that holds the record and the staged launcher.
const LauncherRecordDir = "runtime"

// LauncherRecordPath is one runtime profile's record under the launcher's
// config directory (wsldistro.WSLConfigDir).
func LauncherRecordPath(configDir, mode string) string {
	return filepath.Join(configDir, LauncherRecordDir, appidentity.StateFileName("app-update.json", mode))
}

// Validate is State's check plus the launcher's fields. A launcher record
// always holds an update: without one there is nothing for it to select.
func (r LauncherRecord) Validate() error {
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.Update == nil {
		return errors.New("supervise: the launcher update record holds no update")
	}
	for _, field := range []struct{ name, value string }{
		{"distro", r.Distro},
		{"stablePayload", r.StablePayload},
		{"stagedPayload", r.StagedPayload},
		{"stagedLauncher", r.StagedLauncher},
		{"installPath", r.InstallPath},
		{"targetFingerprint", r.TargetFingerprint},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("supervise: the launcher update record has no %s", field.name)
		}
	}
	if r.StagedPayload == r.StablePayload {
		return errors.New("supervise: the launcher update record stages the payload over the stable path")
	}
	return nil
}

// LoadLauncherRecord reads the record. found is false with a nil error when
// there is none. A record that exists and does not validate is an error, as
// for LoadState: the launcher must not guess which version owns the data.
func LoadLauncherRecord(path string) (LauncherRecord, bool, error) {
	var record LauncherRecord
	found, err := atomicfile.ReadJSON(path, &record)
	if err != nil {
		return LauncherRecord{}, false, fmt.Errorf("supervise: read the launcher update record: %w", err)
	}
	if !found {
		return LauncherRecord{}, false, nil
	}
	if err := record.Validate(); err != nil {
		return LauncherRecord{}, true, err
	}
	return record, true, nil
}

// SaveLauncherRecord writes the record durably after validating it.
func SaveLauncherRecord(path string, record LauncherRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(path, record); err != nil {
		return fmt.Errorf("supervise: write the launcher update record: %w", err)
	}
	return nil
}

// UnsuccessfulUpdateReason is the recorded reason an update to target did not
// commit, or "" when the record says nothing about that update: another
// target, still pending, or committed.
func (r LauncherRecord) UnsuccessfulUpdateReason(target string) string {
	update := r.Update
	if update == nil || update.To != target {
		return ""
	}
	switch update.State {
	case UpdateRolledBack, UpdateFailed:
		return update.Reason
	}
	return ""
}
