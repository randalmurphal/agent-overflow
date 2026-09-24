package supervise

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/startupprogress"
)

// The macOS and Linux desktop's in-app update after the old app hands off
// (docs/specs/app-update.md, Sequence (b) and its recovery table). The
// helper and the app at the install path run every step in their own
// process: UpdateCommand's steps under the backend lock the caller holds,
// the trial as this binary's UpdateTrialCommand. UpdateRun owns the order;
// this file owns the record, the publish and the recovery table's desktop
// rows.

// DesktopFiles are the platform's file operations (NativeDesktopFiles).
type DesktopFiles struct {
	// Replace installs staged at install. Afterwards the previous version
	// is at staged (a swap), at previous (two renames), or gone (a file
	// replaced by a rename).
	Replace func(staged, install, previous string) error
	// InUse reports whether a process runs from path. A bundle is retained
	// while one does.
	InUse func(path string) (bool, error)
}

// DesktopUpdate applies and recovers the desktop record of one data root.
type DesktopUpdate struct {
	// DataDir is the data root: the directory that holds the database.
	DataDir string
	// Version is this binary's version.
	Version string
	// Executable is this binary, and InstallPath what the user starts for
	// it (DesktopInstallPath).
	Executable  string
	InstallPath string
	// Lock is the data root's backend lock, which the caller holds for the
	// whole run. The trial inherits it.
	Lock *os.File
	// Trial starts this binary's UpdateTrialCommand: Binary, Args and Env,
	// and the stall rule's defaults. The target and attempt are filled per
	// attempt.
	Trial TrialRunOptions
	// SchemaVersion reads the live database's migration version
	// (UpdateCommand).
	SchemaVersion func() (int, error)
	Files         DesktopFiles
	// Progress receives what the update is doing, for the helper's window.
	Progress func(startupprogress.Progress)
	Now      func() time.Time
	Logf     func(string, ...any)
}

// ErrNoDesktopRecord is a helper started for an update with no record.
var ErrNoDesktopRecord = errors.New("supervise: there is no desktop update record")

// DesktopMigration is one launch's migration: the database of this
// version, at migration version Schema, refused to migrate live.
type DesktopMigration struct {
	Schema int
	// Retry runs the migration even when the failure memory holds this
	// build's failed trial over Schema: the person asked for it.
	Retry bool
}

// Apply runs update id from its durable state (UpdateRun.Apply): the
// snapshot on the first attempt, one trial, then the publish or the
// rollback.
func (d DesktopUpdate) Apply(ctx context.Context, id string) (UpdateEnd, error) {
	layout, err := NewAppUpdateLayout(d.DataDir)
	if err != nil {
		return UpdateEnd{}, err
	}
	record, found, err := LoadDesktopRecord(layout)
	if err != nil {
		return UpdateEnd{}, err
	}
	if !found {
		return UpdateEnd{}, ErrNoDesktopRecord
	}
	if record.Update.ID != id {
		return UpdateEnd{}, fmt.Errorf("supervise: the desktop update record is for update %q, not %q", record.Update.ID, id)
	}
	return d.run(layout, &record).Apply(ctx, record.State)
}

// Migrate migrates the database of this version, which refused to migrate
// it live, through a snapshot and a trial of this binary. The record exists
// only while the migration is pending. Unless the request is a Retry, a
// remembered failed trial of this build over the same schema version stops
// it before anything runs.
func (d DesktopUpdate) Migrate(ctx context.Context, req DesktopMigration) MigrationEnd {
	layout, err := NewAppUpdateLayout(d.DataDir)
	if err != nil {
		d.logf("updater: open the database migration: %v", err)
		return UpdateRun{LogName: desktopLogName}.MigrationNotStarted()
	}
	run := d.run(layout, &DesktopRecord{})
	if !req.Retry {
		if end, remembered := run.RememberedMigration(d.Version, req.Schema); remembered {
			return end
		}
	}
	d.step("update.migrate", "Preparing to upgrade the database")
	id, err := NewUpdateID()
	if err == nil {
		err = d.beginMigration(layout, req.Schema, id)
	}
	if err != nil {
		d.logf("updater: open the database migration: %v", err)
		return run.MigrationNotStarted()
	}
	d.logf("updater: migration %s: this version refused to migrate its database live; migrating it through a trial", id)
	end, err := d.Apply(ctx, id)
	return run.FinishMigration(id, end, err)
}

// ResumeMigration continues a pending migration record from its durable
// state.
func (d DesktopUpdate) ResumeMigration(ctx context.Context, record DesktopRecord) MigrationEnd {
	layout, err := NewAppUpdateLayout(d.DataDir)
	if err != nil {
		d.logf("updater: migration %s: %v", record.Update.ID, err)
		return UpdateRun{LogName: desktopLogName}.MigrationNotStarted()
	}
	d.step("update.migrate", "Resuming the database upgrade")
	d.logf("updater: migration %s: resuming after %d attempts", record.Update.ID, record.Update.Attempts)
	end, err := d.Apply(ctx, record.Update.ID)
	return d.run(layout, &record).FinishMigration(record.Update.ID, end, err)
}

// beginMigration records durably that this version's database, at
// migration version schema, is migrated through a trial. A pending record
// refuses it; a settled one is replaced.
func (d DesktopUpdate) beginMigration(layout Layout, schema int, id string) error {
	existing, found, err := LoadDesktopRecord(layout)
	if err != nil {
		return err
	}
	if found && !existing.Update.Settled() {
		return fmt.Errorf("the update to %s is still in progress", existing.Update.To)
	}
	base, err := Adopt(d.Version)
	if err != nil {
		return err
	}
	state, err := base.BeginMigration(id, d.now())
	if err != nil {
		return err
	}
	update := *state.Update
	update.FromSchema = schema
	state.Update = &update
	return SaveDesktopRecord(layout, DesktopRecord{State: state, InstallPath: d.InstallPath})
}

// DesktopAction is what the app at the install path does after Reconcile.
type DesktopAction int

const (
	// DesktopLaunch is an ordinary launch.
	DesktopLaunch DesktopAction = iota
	// DesktopHandOff starts Helper to continue the record's update or
	// migration, and exits.
	DesktopHandOff
	// DesktopBlocked starts nothing and shows Title and Detail.
	DesktopBlocked
)

// DesktopDecision is Reconcile's answer.
type DesktopDecision struct {
	Action DesktopAction
	Record DesktopRecord
	// Helper is the executable that continues the record: the staged
	// target of an update, this binary for a migration.
	Helper string
	// Title and Detail are the blocked page's copy.
	Title, Detail string
	// UpdatingTo is set on the first launch of a committed target: the
	// update this launch finishes, which the startup report names.
	UpdatingTo string
	// FailedTo and FailedReason are an update from this version that
	// settled rolled back or failed and that this launch reports.
	FailedTo, FailedReason string
}

// Reconcile applies the recovery table for the app at the install path. It
// runs under the backend lock, before anything opens the database, and
// while this process holds the single-instance identity, so no helper is
// running.
func (d DesktopUpdate) Reconcile(ctx context.Context) (DesktopDecision, error) {
	layout, err := NewAppUpdateLayout(d.DataDir)
	if err != nil {
		return DesktopDecision{}, err
	}
	record, found, err := LoadDesktopRecord(layout)
	if err != nil {
		return DesktopDecision{}, err
	}
	if !found {
		return DesktopDecision{Action: DesktopLaunch}, nil
	}
	run := d.run(layout, &record)
	if record.Migration() {
		resume, err := run.RecoverMigration(ctx, record.State)
		if err != nil {
			return DesktopDecision{}, err
		}
		if resume {
			return DesktopDecision{Action: DesktopHandOff, Record: record, Helper: d.Executable}, nil
		}
		return DesktopDecision{Action: DesktopLaunch}, nil
	}
	update := record.Update
	staged := DesktopExecutable(record.StagedPath)
	// Only the version the update started from hands the record on: a
	// version installed by hand since must not be replaced by the staged
	// one.
	previous := d.Version == update.From
	if update.Settled() && update.Reported {
		// What an earlier launch kept because it was in use.
		d.retire(record)
		return DesktopDecision{Action: DesktopLaunch, Record: record}, nil
	}
	switch update.State {
	case UpdateCommitted:
		if previous {
			if !regularFile(staged) {
				version := startupprogress.DisplayVersion(update.To)
				return DesktopDecision{Action: DesktopBlocked, Record: record,
					Title:  "The update to " + version + " could not be installed.",
					Detail: "Its new version is missing, so nothing was started. Install " + version + " from the releases page.",
				}, nil
			}
			return DesktopDecision{Action: DesktopHandOff, Record: record, Helper: staged}, nil
		}
		run.Discard(ctx, record.State)
		decision, err := d.markReported(run, record)
		if err == nil && d.Version == update.To {
			decision.UpdatingTo = update.To
		}
		return decision, err
	case UpdateRolledBack, UpdateFailed:
		run.Discard(ctx, record.State)
		return d.markReported(run, record)
	}

	stopped := "the update was interrupted and its new version is missing"
	if !previous {
		stopped = "the update was interrupted and " + startupprogress.DisplayVersion(d.Version) + " was started instead"
	}
	end, resume, err := run.RecoverPending(ctx, record.State, previous && regularFile(staged), stopped)
	if err != nil {
		return DesktopDecision{}, err
	}
	if resume {
		return DesktopDecision{Action: DesktopHandOff, Record: record, Helper: staged}, nil
	}
	if !end.Settled() {
		return DesktopDecision{Action: DesktopBlocked, Record: record,
			Title:  "The update did not finish, and the database backup could not be restored.",
			Detail: "Nothing was started, so the data is left as it is. Start Agent Overflow again to retry the restore. Details are in the " + desktopLogName + ".",
		}, nil
	}
	record, _, err = LoadDesktopRecord(layout)
	if err != nil {
		return DesktopDecision{}, err
	}
	return d.markReported(d.run(layout, &record), record)
}

// markReported records that this launch acted on the settled record,
// removes what the update left beside the install path, and names an
// unsuccessful update from this version for its notice.
func (d DesktopUpdate) markReported(run UpdateRun, record DesktopRecord) (DesktopDecision, error) {
	d.retire(record)
	next, err := run.MarkReported(record.State)
	if err != nil {
		return DesktopDecision{}, err
	}
	record.State = next
	decision := DesktopDecision{Action: DesktopLaunch, Record: record}
	if update := record.Update; update.From == d.Version && (update.State == UpdateRolledBack || update.State == UpdateFailed) {
		decision.FailedTo, decision.FailedReason = update.To, update.Reason
	}
	return decision, nil
}

// desktopLogName is the log the desktop's pages point to.
const desktopLogName = "update log"

func (d DesktopUpdate) run(layout Layout, record *DesktopRecord) UpdateRun {
	return UpdateRun{
		Steps:      &desktopSteps{d: d, layout: layout, record: record},
		MemoryPath: FailedTrialPath(layout.StatePath()),
		LogName:    desktopLogName,
		Progress:   d.Progress,
		Now:        d.Now,
		Logf:       d.logf,
	}
}

// desktopSteps is UpdateSteps for the desktop record: every step runs in
// this process, under the lock the caller holds.
type desktopSteps struct {
	d      DesktopUpdate
	layout Layout
	record *DesktopRecord
}

func (s *desktopSteps) Save(state State) error {
	next := *s.record
	next.State = state
	if err := SaveDesktopRecord(s.layout, next); err != nil {
		return err
	}
	*s.record = next
	return nil
}

func (s *desktopSteps) RemoveRecord() error {
	if err := os.Remove(s.layout.StatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *desktopSteps) command(progress func(startupprogress.Progress)) UpdateCommand {
	lock := s.d.Lock
	return UpdateCommand{
		DataDir:       s.d.DataDir,
		UpdateID:      s.record.Update.ID,
		Progress:      progress,
		OwnsAppLayout: true,
		AcquireLock: func(context.Context, time.Duration) (*os.File, func(), error) {
			if lock == nil {
				return nil, nil, errors.New("this process does not hold the backend lock")
			}
			return lock, func() {}, nil
		},
		SchemaVersion: s.d.SchemaVersion,
		Now:           s.d.Now,
		Log:           s.d.logf,
	}
}

// sink is a step's progress sink: never nil, so the step reports to its
// own relay rather than to a writer.
func sink(progress func(startupprogress.Progress)) func(startupprogress.Progress) {
	if progress == nil {
		return func(startupprogress.Progress) {}
	}
	return progress
}

func (s *desktopSteps) Snapshot(ctx context.Context, progress func(startupprogress.Progress)) (UpdateEvent, error) {
	return s.command(sink(progress)).Snapshot(ctx, nil), nil
}

func (s *desktopSteps) Trial(ctx context.Context, to string, attempt int, progress func(startupprogress.Progress)) (UpdateEvent, error) {
	opts := s.d.Trial
	opts.TargetVersion, opts.Attempt = to, attempt
	return s.command(sink(progress)).TrialRun(ctx, opts), nil
}

func (s *desktopSteps) Restore(ctx context.Context, reason string, progress func(startupprogress.Progress)) (UpdateEvent, error) {
	return s.command(sink(progress)).Restore(ctx, reason), nil
}

func (s *desktopSteps) Discard(ctx context.Context, progress func(startupprogress.Progress)) (UpdateEvent, error) {
	return s.command(sink(progress)).Discard(ctx), nil
}

// RemoveStaged deletes the staged target after a rollback. A helper runs
// from it, so it is left for the next launch, which runs from the install
// path.
func (s *desktopSteps) RemoveStaged(context.Context) error {
	staged := s.record.StagedPath
	if pathWithin(s.d.Executable, staged) {
		s.d.logf("updater: update %s: %s runs this helper; the next launch removes it", s.record.Update.ID, staged)
		return nil
	}
	return s.d.removeUnused(staged)
}

// Publish installs the committed target at the install path and removes
// the previous version once nothing runs from it. The snapshot is
// discarded first. A repeat finds the install path holding the target's
// executable and replaces nothing.
func (s *desktopSteps) Publish(ctx context.Context) error {
	record := *s.record
	run := UpdateRun{Steps: s, Progress: s.d.Progress, Logf: s.d.logf}
	run.Discard(ctx, record.State)
	published, err := holdsDigest(DesktopExecutable(record.InstallPath), record.TargetDigest)
	if err != nil {
		return fmt.Errorf("check the installed version: %w", err)
	}
	if !published {
		if err := s.d.Files.Replace(record.StagedPath, record.InstallPath, desktopPreviousPath(record)); err != nil {
			return fmt.Errorf("install the new version: %w", err)
		}
	}
	s.d.retire(record)
	return nil
}

// retire removes what an update left beside the install path: the
// staged path, which holds the previous version after a swap, and the
// two-rename publish's set-aside. A path something runs from is kept for a
// later launch.
func (d DesktopUpdate) retire(record DesktopRecord) {
	if record.Migration() {
		return
	}
	for _, path := range []string{record.StagedPath, desktopPreviousPath(record)} {
		if err := d.removeUnused(path); err != nil {
			d.logf("updater: update %s: %v", record.Update.ID, err)
		}
	}
}

// removeUnused deletes path unless a process runs from it.
func (d DesktopUpdate) removeUnused(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	inUse, err := d.Files.InUse(path)
	if err != nil {
		return fmt.Errorf("keep %s: cannot tell whether it is in use: %w", path, err)
	}
	if inUse {
		return fmt.Errorf("keep %s: a process runs from it; a later launch removes it", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// holdsDigest reports whether the file at path has the hex SHA-256 want. A
// missing file does not.
func holdsDigest(path, want string) (bool, error) {
	got, err := fileDigest(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hex.EncodeToString(got) == want, nil
}

// pathWithin reports whether path is root or inside it.
func pathWithin(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (d DesktopUpdate) step(phase, detail string) {
	if d.Progress == nil {
		return
	}
	now := d.now().UnixMilli()
	d.Progress(startupprogress.Progress{Phase: phase, Detail: detail, StartedAt: now, UpdatedAt: now})
}

func (d DesktopUpdate) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d DesktopUpdate) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
