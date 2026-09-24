package supervise

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
)

// LauncherRecord is the Windows launcher's in-app update record
// (docs/specs/app-update.md). The launcher supervises that update, so the
// record lives on the Windows side, where it can be read before WSL starts and
// fsync and rename are native. It is State with one update, plus what a later
// launch needs to resume or recover that update. The launcher passes the
// backend what the record settled (wsllauncher.ReconcileDecision.BackendArgs);
// the backend never reads it.
//
// A migration (Migration) is the same record for the database of the version
// already installed: its update runs from that version to itself, stages
// nothing, and its trial runs the stable payload.
type LauncherRecord struct {
	State
	// Distro is the WSL distribution the payload runs in.
	Distro string `json:"distro"`
	// StablePayload is the Linux path the launcher runs. It holds the
	// previous version's backend until commit.
	StablePayload string `json:"stablePayload"`
	// StagedPayload is the target's backend, beside StablePayload on the same
	// filesystem so commit is a rename. Empty for a migration.
	StagedPayload string `json:"stagedPayload,omitempty"`
	// StagedLauncher is the target launcher's Windows path. Empty for a
	// migration.
	StagedLauncher string `json:"stagedLauncher,omitempty"`
	// InstallPath is the launcher path the user starts. Empty for a
	// migration.
	InstallPath string `json:"installPath,omitempty"`
	// TargetFingerprint is the target launcher's embedded payload digest. A
	// launcher at InstallPath whose own digest matches is the target. Empty
	// for a migration.
	TargetFingerprint string `json:"targetFingerprint,omitempty"`
	// Applier is the launcher running this update's --update-apply,
	// recorded by the launcher that started it before that one exits. While
	// it runs, another launch joins the update instead of acting on it.
	Applier *ProcessRef `json:"applier,omitempty"`
}

// LauncherRecordDir is the directory under the launcher's config directory
// that holds the record and the staged launcher.
const LauncherRecordDir = "runtime"

// LauncherRecordPath is the record for one launcher build and one data root,
// under the launcher's config directory (wsldistro.WSLConfigDir). mode is the
// launcher's runtime mode (appidentity.LauncherMode: dev, prod or an isolated
// profile) and distro the WSL distribution that holds the backend's data.
// Dev and production launchers, and launches of different distributions,
// never read each other's record.
func LauncherRecordPath(configDir, mode, distro string) string {
	return filepath.Join(configDir, LauncherRecordDir, "app-update-"+recordNamePart(mode)+"."+recordNamePart(distro)+".json")
}

// recordNamePart is s as part of a file name: lower case, because Windows
// file names and WSL distribution names ignore case, with every byte outside
// [a-z0-9._-] percent-encoded, so distinct names never share a file and no
// name leaves the directory. A mode holds no dot, so the first dot ends it.
func recordNamePart(s string) string {
	var b strings.Builder
	for _, c := range []byte(strings.ToLower(s)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Migration reports whether the record migrates the database of the
// version already installed (State.BeginMigration) instead of updating it.
func (r LauncherRecord) Migration() bool {
	return r.Update != nil && r.Update.From == r.Update.To
}

// TrialPayload is the payload whose commands snapshot the database and run
// the trial: the staged target for an update, the stable payload for a
// migration.
func (r LauncherRecord) TrialPayload() string {
	if r.Migration() {
		return r.StablePayload
	}
	return r.StagedPayload
}

// Validate is State's check plus the launcher's fields. A launcher record
// always holds an update: without one there is nothing for it to select.
// A migration stages nothing.
func (r LauncherRecord) Validate() error {
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.Update == nil {
		return errors.New("supervise: the launcher update record holds no update")
	}
	staged := []struct{ name, value string }{
		{"stagedPayload", r.StagedPayload},
		{"stagedLauncher", r.StagedLauncher},
		{"installPath", r.InstallPath},
		{"targetFingerprint", r.TargetFingerprint},
	}
	required := []struct{ name, value string }{
		{"distro", r.Distro},
		{"stablePayload", r.StablePayload},
	}
	if r.Migration() {
		for _, field := range staged {
			if field.value != "" {
				return fmt.Errorf("supervise: the launcher migration record has a %s", field.name)
			}
		}
	} else {
		required = append(required, staged...)
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("supervise: the launcher update record has no %s", field.name)
		}
	}
	if !r.Migration() && r.StagedPayload == r.StablePayload {
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

// UnsuccessfulUpdate is the update from version from that the record settled
// as rolled back or failed, and its recorded reason. ok is false when the
// record holds no such update: another starting version, still pending,
// committed, or a migration, whose outcome the launcher shows itself.
func (r LauncherRecord) UnsuccessfulUpdate(from string) (to, reason string, ok bool) {
	update := r.Update
	if update == nil || update.From != from || r.Migration() {
		return "", "", false
	}
	switch update.State {
	case UpdateRolledBack, UpdateFailed:
		return update.To, update.Reason, true
	}
	return "", "", false
}
