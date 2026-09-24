package supervise

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/mod/semver"
)

// The running desktop app's half of an in-app update (docs/specs/app-update.md,
// Sequence (b) steps 1 and 2): decide whether the downloaded target takes
// the trial, stage it beside the install path, record the update and start
// the target's helper. The caller quits once HandOff returns nil.

// desktopTrialSince is the first release that answers PreflightSubcommand.
// An older target would treat that argv as an ordinary launch and open its
// window, so it is never asked; it takes the framework's swap.
const desktopTrialSince = "v0.0.15"

// DesktopHandoff hands a downloaded update to its helper.
type DesktopHandoff struct {
	// DataDir is the data root: the directory that holds the database.
	DataDir string
	// DataDirFlag is the --data-dir this app was started with, "" for the
	// default. The helper, and the trial it starts, get the same.
	DataDirFlag string
	// Version is this app's version; Executable is this app's binary.
	Version    string
	Executable string
	// GOOS names the platform's install layout (DesktopInstallPath).
	GOOS string
	// RelaunchArgs are the arguments the install path is started with
	// after the update (DesktopRelaunchArgs of this app's argv).
	RelaunchArgs []string
	// Preflight asks a binary what it is (PreflightBinary).
	Preflight func(ctx context.Context, binary string) (Preflight, error)
	// Start starts the helper detached, its output going to logPath
	// (StartDesktopHelper with the environment the caller gives helpers).
	Start func(executable string, args []string, logPath string) error
	Now   func() time.Time
	Logf  func(string, ...any)
}

// Check reports whether the downloaded release at downloaded, of version,
// takes the trial, or why it cannot be installed now. The same version,
// a release that predates the flow and a build without the helper take
// the framework's swap. A target that takes the trial must fit its
// database snapshot.
func (h DesktopHandoff) Check(ctx context.Context, downloaded, version string) (bool, error) {
	if version == h.Version {
		return false, nil
	}
	if v := "v" + version; !semver.IsValid(v) || semver.Compare(v, desktopTrialSince) < 0 {
		return false, nil
	}
	answer, err := h.preflight(ctx, DesktopExecutable(downloaded), version)
	if err != nil || !answer.AppUpdateTrial {
		return false, err
	}
	if err := h.checkSpace(); err != nil {
		return false, err
	}
	return true, nil
}

// HandOff stages the downloaded target beside the install path, records the
// update pending and starts the target's helper, which waits for this app
// to exit. Nothing is recorded unless the staged target answers as version
// and the snapshot fits; a helper that cannot start settles the update
// failed, and its error is the caller's to show.
func (h DesktopHandoff) HandOff(ctx context.Context, downloaded, version string) error {
	install, err := DesktopInstallPath(h.Executable, h.GOOS)
	if err != nil {
		return err
	}
	layout, err := NewAppUpdateLayout(h.DataDir)
	if err != nil {
		return err
	}
	existing, found, err := LoadDesktopRecord(layout)
	if err != nil {
		return err
	}
	if found && !existing.Update.Settled() {
		return fmt.Errorf("the update to %s is still in progress", existing.Update.To)
	}
	id, err := NewUpdateID()
	if err != nil {
		return err
	}
	staged := DesktopStagedPath(install, id)
	// Staging beside the install path also proves the folder is writable,
	// which the publish needs.
	if err := movePath(downloaded, staged); err != nil {
		return fmt.Errorf("stage the update beside %s: %w", install, err)
	}
	discard := func() {
		if err := os.RemoveAll(staged); err != nil {
			h.logf("updater: remove the staged update %s: %v", staged, err)
		}
	}
	executable := DesktopExecutable(staged)
	record, err := h.record(ctx, layout, install, staged, executable, version, id)
	if err != nil {
		discard()
		return err
	}
	self, err := CurrentProcessRef()
	if err == nil {
		args := DesktopHelperArgs([]string{"--id", id}, self, h.DataDirFlag, h.RelaunchArgs)
		err = h.Start(executable, args, DesktopUpdateLogPath(layout))
	}
	if err != nil {
		// Settled and reported before the error is shown, so no later launch
		// resumes an update whose helper never ran or reports it twice.
		if settleErr := SettleDesktopUpdate(h.DataDir, id, UpdateFailed,
			"the update could not be started: "+err.Error(), true, h.now()); settleErr != nil {
			h.logf("updater: settle update %s: %v", id, settleErr)
		}
		discard()
		return fmt.Errorf("start the update: %w", err)
	}
	h.logf("updater: update %s to %s handed to %s", id, record.Update.To, executable)
	return nil
}

// record checks the staged target and writes the update pending.
func (h DesktopHandoff) record(ctx context.Context, layout Layout, install, staged, executable, version, id string) (DesktopRecord, error) {
	if !isBundle(staged) {
		// The download is written without the executable bit; the publish
		// keeps the mode the install path has.
		info, err := os.Stat(h.Executable)
		if err != nil {
			return DesktopRecord{}, err
		}
		if err := os.Chmod(executable, info.Mode().Perm()); err != nil {
			return DesktopRecord{}, err
		}
	}
	answer, err := h.preflight(ctx, executable, version)
	if err != nil {
		return DesktopRecord{}, err
	}
	if !answer.AppUpdateTrial {
		return DesktopRecord{}, fmt.Errorf("the downloaded %s cannot apply an update", version)
	}
	if err := h.checkSpace(); err != nil {
		return DesktopRecord{}, err
	}
	digest, err := fileDigest(executable)
	if err != nil {
		return DesktopRecord{}, err
	}
	base, err := Adopt(h.Version)
	if err != nil {
		return DesktopRecord{}, err
	}
	state, err := base.Begin(id, version, h.now())
	if err != nil {
		return DesktopRecord{}, err
	}
	record := DesktopRecord{State: state, InstallPath: install, StagedPath: staged, TargetDigest: hex.EncodeToString(digest)}
	if err := SaveDesktopRecord(layout, record); err != nil {
		return DesktopRecord{}, fmt.Errorf("record the update: %w", err)
	}
	return record, nil
}

// RestartingTo is the version this app handed its update to: the target
// of a pending update from this version, "" when there is none.
func (h DesktopHandoff) RestartingTo() string {
	layout, err := NewAppUpdateLayout(h.DataDir)
	if err != nil {
		return ""
	}
	record, found, err := LoadDesktopRecord(layout)
	if err != nil {
		h.logf("updater: read the update record: %v", err)
		return ""
	}
	if !found || record.Migration() || record.Update.State != UpdatePending || record.Update.From != h.Version {
		return ""
	}
	return record.Update.To
}

// preflight asks the executable what it is. The download is written without
// the executable bit, which the owner gets.
func (h DesktopHandoff) preflight(ctx context.Context, executable, version string) (Preflight, error) {
	info, err := os.Stat(executable)
	if err != nil {
		return Preflight{}, fmt.Errorf("the downloaded %s has no executable: %w", version, err)
	}
	if perm := info.Mode().Perm(); perm&0o100 == 0 {
		if err := os.Chmod(executable, perm|0o700); err != nil {
			return Preflight{}, err
		}
	}
	answer, err := h.Preflight(ctx, executable)
	if err != nil {
		return Preflight{}, fmt.Errorf("the downloaded %s did not start: %w", version, err)
	}
	if answer.Version != version {
		return Preflight{}, fmt.Errorf("the downloaded %s reports version %s", version, answer.Version)
	}
	return answer, nil
}

// checkSpace is UpdateCommand.Space: the snapshot the trial takes must fit.
func (h DesktopHandoff) checkSpace() error {
	result := UpdateCommand{DataDir: h.DataDir}.Space(nil)
	if result.Outcome != UpdateOutcomeOK {
		return errors.New(result.Reason)
	}
	return nil
}

func (h DesktopHandoff) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h DesktopHandoff) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// moveRename is movePath's rename.
var moveRename = os.Rename

// movePath renames src to dst, copying across filesystems: the download
// lands in the temp directory, which is often another one.
func movePath(src, dst string) error {
	err := moveRename(src, dst)
	if err == nil || !crossDevice(err) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		if removeErr := os.RemoveAll(dst); removeErr != nil {
			return fmt.Errorf("%w (and the partial copy could not be removed: %v)", err, removeErr)
		}
		return err
	}
	return os.RemoveAll(src)
}

// copyTree copies a file or a directory with its modes and symlinks, and
// syncs what it wrote.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.Mkdir(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyRegular(path, target, info.Mode().Perm())
		}
		return fmt.Errorf("%s is neither a file, a directory nor a link", path)
	})
}

func copyRegular(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = out.ReadFrom(in)
	if syncErr := out.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}
