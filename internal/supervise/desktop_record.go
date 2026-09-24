package supervise

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
)

// DesktopRecord is the macOS and Linux desktop's in-app update record
// (docs/specs/app-update.md, "macOS and Linux desktop"): State with one
// update, plus where the update installs. It lives at the in-app layout's
// StatePath, which PrepareDataRoot reads as State, and the failure memory
// sits beside it (FailedTrialPath).
//
// A migration (Migration) is the same record for the database of the
// installed version: it stages nothing, and its trial runs the installed
// binary.
type DesktopRecord struct {
	State
	// InstallPath is what the user starts: the .app bundle on macOS, the
	// executable on Linux.
	InstallPath string `json:"installPath"`
	// StagedPath is the target beside InstallPath, on the same filesystem
	// so the publish is a rename. Empty for a migration.
	StagedPath string `json:"stagedPath,omitempty"`
	// TargetDigest is the hex SHA-256 of the staged executable. An install
	// path whose executable has it already holds the target, so a repeated
	// publish does not exchange the two back. Empty for a migration.
	TargetDigest string `json:"targetDigest,omitempty"`
}

// DesktopApplyCommand is the helper mode of a desktop build: it applies an
// update, or migrates the database, with a progress window, then starts the
// install path.
const DesktopApplyCommand = "__update-apply"

// The desktop boot's flags that name a process to wait for: a helper that
// started the app waits no longer than it takes to exit, so the app claims
// the single-instance identity and the backend lock after it.
const (
	DesktopWaitPIDFlag   = "wait-pid"
	DesktopWaitStartFlag = "wait-start"
)

// desktopUpdateLogName is the helper's log beside the record.
const desktopUpdateLogName = "update.log"

// DesktopUpdateLogPath is where the helper's output goes, and the app's
// lines about the update: the log the desktop's pages name.
func DesktopUpdateLogPath(layout Layout) string {
	return filepath.Join(layout.Root(), desktopUpdateLogName)
}

// desktopLogRotateBytes is the size past which the update log is moved to
// <log>.1 when it is opened.
const desktopLogRotateBytes = 1 << 20

// OpenDesktopUpdateLog opens the update log at path for appending, moving a
// log past desktopLogRotateBytes aside first.
func OpenDesktopUpdateLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > desktopLogRotateBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Validate is State's check plus the desktop's fields.
func (r DesktopRecord) Validate() error {
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.Update == nil {
		return errors.New("supervise: the desktop update record holds no update")
	}
	if !filepath.IsAbs(r.InstallPath) {
		return fmt.Errorf("supervise: the desktop update record's install path %q is not absolute", r.InstallPath)
	}
	if r.Migration() {
		if r.StagedPath != "" || r.TargetDigest != "" {
			return errors.New("supervise: the desktop migration record stages a version")
		}
		return nil
	}
	if r.StagedPath == "" || r.TargetDigest == "" {
		return errors.New("supervise: the desktop update record has no staged version")
	}
	if filepath.Dir(r.StagedPath) != filepath.Dir(r.InstallPath) || r.StagedPath == r.InstallPath {
		return fmt.Errorf("supervise: the staged version %q is not beside the install path %q", r.StagedPath, r.InstallPath)
	}
	return nil
}

// LoadDesktopRecord reads the record. found is false with a nil error when
// there is none. A record that exists and does not validate is an error.
func LoadDesktopRecord(layout Layout) (DesktopRecord, bool, error) {
	var record DesktopRecord
	found, err := atomicfile.ReadJSON(layout.StatePath(), &record)
	if err != nil {
		return DesktopRecord{}, false, fmt.Errorf("supervise: read the desktop update record: %w", err)
	}
	if !found {
		return DesktopRecord{}, false, nil
	}
	if err := record.Validate(); err != nil {
		return DesktopRecord{}, true, err
	}
	return record, true, nil
}

// SaveDesktopRecord writes the record durably after validating it.
func SaveDesktopRecord(layout Layout, record DesktopRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(layout.StatePath(), record); err != nil {
		return fmt.Errorf("supervise: write the desktop update record: %w", err)
	}
	return nil
}

// SettleDesktopUpdate settles the pending update id in dataDir's record,
// marked reported when reported is set: a failure the caller has already
// shown. The caller holds the data root's backend lock, as every writer of
// the record does: a process without it could overwrite a helper that took
// the lock and is about to count its trial. The old app settles an update
// whose helper could not start this way. Only an update that has not
// started a trial is settled here: its database was not touched. One that
// started a trial is left to the recovery table, which restores the
// database or resumes it.
func SettleDesktopUpdate(dataDir, id string, state UpdateState, reason string, reported bool, now time.Time) error {
	layout, err := NewAppUpdateLayout(dataDir)
	if err != nil {
		return err
	}
	record, found, err := LoadDesktopRecord(layout)
	if err != nil {
		return err
	}
	if !found || record.Update.ID != id || record.Update.Settled() {
		return fmt.Errorf("supervise: update %s is not pending", id)
	}
	if record.Update.Attempts > 0 {
		return fmt.Errorf("supervise: update %s started a trial; the next launch recovers it", id)
	}
	next, err := record.Settle(state, reason, now)
	if err == nil && reported {
		next, _, err = next.MarkReported()
	}
	if err != nil {
		return err
	}
	record.State = next
	return SaveDesktopRecord(layout, record)
}

// DesktopInstallPath is what the user starts, for this executable: the
// enclosing .app bundle on macOS, the executable itself otherwise. A bundle
// runs Contents/MacOS/agent-overflow; any other name is refused, because
// the staged bundle's executable is found by that name.
func DesktopInstallPath(executable, goos string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("supervise: the executable path %q is not absolute", executable)
	}
	macos := filepath.Dir(executable)
	contents := filepath.Dir(macos)
	bundle := filepath.Dir(contents)
	if goos != "darwin" || filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(bundle, ".app") {
		return executable, nil
	}
	if filepath.Base(executable) != BinaryName {
		return "", fmt.Errorf("supervise: the bundle %s runs %s, not %s", bundle, filepath.Base(executable), BinaryName)
	}
	return bundle, nil
}

// DesktopExecutable is the executable an install or staged path runs.
func DesktopExecutable(path string) string {
	if isBundle(path) {
		return filepath.Join(path, "Contents", "MacOS", BinaryName)
	}
	return path
}

// DesktopStagedPath is where update id stages the target of installPath:
// beside it, hidden, and a bundle again when it is one.
func DesktopStagedPath(installPath, id string) string {
	return desktopSibling(installPath, ".agent-overflow-update-"+id)
}

// desktopPreviousPath is where the two-rename publish sets the previous
// version aside.
func desktopPreviousPath(record DesktopRecord) string {
	return desktopSibling(record.InstallPath, ".agent-overflow-previous-"+record.Update.ID)
}

func desktopSibling(installPath, name string) string {
	if isBundle(installPath) {
		name += ".app"
	}
	return filepath.Join(filepath.Dir(installPath), name)
}

func isBundle(path string) bool { return strings.HasSuffix(path, ".app") }

// DesktopHelperArgs is the argv of the helper: DesktopApplyCommand with
// mode (--id <id>, or --migrate <schema>), the process it waits for, the
// data root flag the app was started with, and the app's own arguments,
// which it passes to the install path it starts.
func DesktopHelperArgs(mode []string, wait ProcessRef, dataDirFlag string, relaunch []string) []string {
	args := append([]string{DesktopApplyCommand}, mode...)
	args = append(args, "--"+DesktopWaitPIDFlag, strconv.Itoa(wait.PID), "--"+DesktopWaitStartFlag, wait.Start)
	if dataDirFlag != "" {
		args = append(args, "--data-dir", dataDirFlag)
	}
	return append(append(args, "--"), relaunch...)
}

// DesktopAppArgs is the argv of the app a helper starts: it waits for
// the helper, then boots with its original arguments.
func DesktopAppArgs(wait ProcessRef, relaunch []string) []string {
	return append([]string{"--" + DesktopWaitPIDFlag, strconv.Itoa(wait.PID), "--" + DesktopWaitStartFlag, wait.Start}, relaunch...)
}

// DesktopLaunchCommand starts the app at installPath with args: through
// LaunchServices for a macOS bundle, as the framework's swap relaunches it,
// and directly otherwise.
func DesktopLaunchCommand(installPath string, args []string, goos string) *exec.Cmd {
	if goos == "darwin" && isBundle(installPath) {
		return exec.Command("/usr/bin/open", append([]string{"-n", installPath, "--args"}, args...)...)
	}
	return exec.Command(installPath, args...)
}

// DesktopRelaunchArgs is an app's argv without the wait flags a helper
// gave it, so a relaunch carries the arguments the person started it with.
func DesktopRelaunchArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, hasValue := desktopWaitFlag(args[i])
		switch {
		case name == "":
			out = append(out, args[i])
		case !hasValue:
			i++
		}
	}
	return out
}

// desktopWaitFlag names a wait flag in any form the flag package accepts:
// one or two dashes, the value inline after "=" or in the next argument.
func desktopWaitFlag(arg string) (name string, inline bool) {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if trimmed == arg {
		return "", false
	}
	name, _, inline = strings.Cut(trimmed, "=")
	if name != DesktopWaitPIDFlag && name != DesktopWaitStartFlag {
		return "", false
	}
	return name, inline
}
