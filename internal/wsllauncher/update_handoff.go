package wsllauncher

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/supervise"
)

// The old launcher's half of an in-app update (docs/specs/app-update.md,
// Windows sequence steps 2 and 3): ask the new launcher to stage and check
// its payload, record the update, and hand off.

// PreflightAnswer is what `--update-preflight` writes. A launcher that
// predates the trial flow writes nothing, which is how the old launcher
// knows to use the plain swap.
type PreflightAnswer struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Version is the staged payload's own answer to __service-preflight.
	Version string `json:"version,omitempty"`
	// Fingerprint is the new launcher's embedded payload digest.
	Fingerprint string `json:"fingerprint,omitempty"`
	// StagedPayload is where the new payload was installed.
	StagedPayload string `json:"stagedPayload,omitempty"`
}

// WritePreflightAnswer writes the answer durably.
func WritePreflightAnswer(path string, answer PreflightAnswer) error {
	return atomicfile.WriteJSON(path, answer)
}

// ReadPreflightAnswer reads an answer. found is false when the new launcher
// wrote none.
func ReadPreflightAnswer(path string) (PreflightAnswer, bool, error) {
	var answer PreflightAnswer
	found, err := atomicfile.ReadJSON(path, &answer)
	if err != nil || !found {
		return PreflightAnswer{}, found, err
	}
	if answer.OK && (answer.Version == "" || answer.Fingerprint == "" || answer.StagedPayload == "") {
		return PreflightAnswer{}, true, errors.New("wsllauncher: the preflight answer is incomplete")
	}
	return answer, true, nil
}

// NewUpdateID returns a fresh update id. It names files on both sides, so it
// is lowercase hex.
func NewUpdateID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// ValidUpdateID reports whether id is one NewUpdateID could have made. Ids
// arrive on the command line and name files, so nothing else is accepted.
func ValidUpdateID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// StagedPayloadPath is where update id stages the payload: beside the stable
// payload, so commit is a rename on one filesystem.
func StagedPayloadPath(stable, id string) string {
	return path.Join(path.Dir(stable), path.Base(stable)+".update-"+id)
}

// StagedLauncherPath is where update id keeps the new launcher.
func StagedLauncherPath(configDir, id string) string {
	return filepath.Join(configDir, supervise.LauncherRecordDir, "agent-overflow-update-"+id+".exe")
}

// PreflightAnswerPath is where the new launcher answers update id.
func PreflightAnswerPath(configDir, id string) string {
	return filepath.Join(configDir, supervise.LauncherRecordDir, "preflight-"+id+".json")
}

// BeginLauncherUpdate records a pending update durably. A pending record
// refuses a second update; a settled one is replaced, because the version
// running now is the one this update starts from whatever the old record
// selected.
func BeginLauncherUpdate(recordPath string, record supervise.LauncherRecord, from, to, id string, now time.Time) (supervise.LauncherRecord, error) {
	existing, found, err := supervise.LoadLauncherRecord(recordPath)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	if found && !existing.Update.Settled() {
		return supervise.LauncherRecord{}, fmt.Errorf("the update to %s is still in progress", existing.Update.To)
	}
	base, err := supervise.Adopt(from)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	state, err := base.Begin(id, to, now)
	if err != nil {
		return supervise.LauncherRecord{}, err
	}
	record.State = state
	if err := supervise.SaveLauncherRecord(recordPath, record); err != nil {
		return supervise.LauncherRecord{}, err
	}
	return record, nil
}

// SettleLauncherUpdate ends a pending update that never reached the new
// launcher, such as a hand-off that could not start it.
func SettleLauncherUpdate(recordPath, id string, state supervise.UpdateState, reason string, now time.Time) error {
	record, found, err := supervise.LoadLauncherRecord(recordPath)
	if err != nil {
		return err
	}
	if !found || record.Update.ID != id || record.Update.Settled() {
		return nil
	}
	next, err := record.State.Settle(state, reason, now)
	if err != nil {
		return err
	}
	record.State = next
	return supervise.SaveLauncherRecord(recordPath, record)
}

// PublishLauncherFile replaces install with a copy of staged. replace is
// MoveFileEx with REPLACE_EXISTING and WRITE_THROUGH on Windows. A running
// launcher at install cannot be replaced but can be renamed, so when the
// replace fails the old file is moved aside to install+".old-"+id first. The
// copy is fsynced before it is renamed into place.
func PublishLauncherFile(staged, install, id string, replace func(from, to string) error) error {
	next := install + ".new"
	if err := copyFileSynced(staged, next); err != nil {
		return err
	}
	err := replace(next, install)
	if err == nil {
		return nil
	}
	aside := install + ".old-" + id
	if asideErr := replace(install, aside); asideErr != nil {
		_ = os.Remove(next)
		return fmt.Errorf("replace %s: %w (moving it aside failed too: %v)", install, err, asideErr)
	}
	if err := replace(next, install); err != nil {
		if backErr := replace(aside, install); backErr != nil {
			return fmt.Errorf("replace %s: %w (and restoring the previous launcher from %s failed: %v)", install, err, aside, backErr)
		}
		_ = os.Remove(next)
		return fmt.Errorf("replace %s: %w", install, err)
	}
	return nil
}

// RemoveLauncherFiles deletes the staged launcher and what a publish set
// aside or left half-written. A file still running cannot be deleted on
// Windows; that failure is returned and the next launch tries again.
func RemoveLauncherFiles(record supervise.LauncherRecord) error {
	paths := []string{record.StagedLauncher, record.InstallPath + ".new"}
	aside, err := filepath.Glob(record.InstallPath + ".old-*")
	if err != nil {
		return err
	}
	paths = append(paths, aside...)
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func copyFileSynced(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Sync()
}
