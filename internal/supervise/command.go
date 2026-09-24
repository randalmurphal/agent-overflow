package supervise

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"agent-overflow/internal/startupprogress"
)

// The in-app update's steps on a host whose supervisor cannot hold a child
// channel open across them: the Windows launcher runs each step as a command
// of the backend binary through wsl.exe (docs/specs/app-update.md). A command
// reports on stdout with UpdateEventPrefix and ends with one result event and
// its exit code, as the headless bootstrap handshake does.

// The update commands. Double-underscored like the other internal re-execs,
// because nobody types them.
const (
	// UpdateSpaceCommand answers whether the snapshot would fit, without the
	// lock, while the running version still serves.
	UpdateSpaceCommand = "__update-space"
	// UpdateSnapshotCommand waits for the data root's lock, finishes a marked
	// restore, checks free space and snapshots the database.
	UpdateSnapshotCommand = "__update-snapshot"
	// UpdateTrialRunCommand holds the lock, runs UpdateTrialCommand as a
	// supervised child under the stall rule, and restores the snapshot when
	// the trial fails.
	UpdateTrialRunCommand = "__update-trial-run"
	// UpdateRestoreCommand puts the snapshot back, or finishes a marked
	// restore.
	UpdateRestoreCommand = "__update-restore"
	// UpdateDiscardCommand removes the snapshot.
	UpdateDiscardCommand = "__update-discard"
	// UpdateTrialCommand is the trial itself: a backend boot that reports
	// progress and prepared on the supervise channel and never serves a
	// client.
	UpdateTrialCommand = "__update-trial"
)

// UpdateEventPrefix marks a command's report lines on stdout. Everything
// else on stdout is ignored, so a log line cannot be read as a report.
const UpdateEventPrefix = "__AO_UPDATE__:"

// UpdateEventType is one of the three report kinds.
type UpdateEventType string

const (
	// UpdateEventStarted is the first report: the command's pid inside its
	// host, which is what stops it when its own stall rule cannot.
	UpdateEventStarted UpdateEventType = "started"
	// UpdateEventProgress carries a progress report, heartbeats included.
	// Its UpdatedAt and AliveAt are what the stall rule reads.
	UpdateEventProgress UpdateEventType = "progress"
	// UpdateEventResult is the last report.
	UpdateEventResult UpdateEventType = "result"
)

// UpdateOutcome is a command's result.
type UpdateOutcome string

const (
	// UpdateOutcomeOK is a snapshot, restore or discard that finished.
	UpdateOutcomeOK UpdateOutcome = "ok"
	// UpdateOutcomePrepared is a trial that reached prepared and was stopped.
	// The database is the trial's; the caller commits.
	UpdateOutcomePrepared UpdateOutcome = "prepared"
	// UpdateOutcomeRolledBack is a trial that failed, with the snapshot
	// restored. Reason says why the trial failed.
	UpdateOutcomeRolledBack UpdateOutcome = "rolled-back"
	// UpdateOutcomeRefused is a command that changed nothing: the lock
	// stayed held or the disk is too full.
	UpdateOutcomeRefused UpdateOutcome = "refused"
	// UpdateOutcomeChanged is a trial that did not start because the live
	// database is not what the update left it: another backend used it. The
	// snapshot must not be restored, because that would discard the other
	// backend's work.
	UpdateOutcomeChanged UpdateOutcome = "changed"
	// UpdateOutcomeNoSnapshot is a trial or restore that found no snapshot of
	// this update. Nothing was changed.
	UpdateOutcomeNoSnapshot UpdateOutcome = "no-snapshot"
	// UpdateOutcomeFailed is a command that failed part-way. The database is
	// in an unknown state until a restore finishes.
	UpdateOutcomeFailed UpdateOutcome = "failed"
)

// UpdateEvent is one report line.
type UpdateEvent struct {
	Type     UpdateEventType           `json:"type"`
	PID      int                       `json:"pid,omitempty"`
	Progress *startupprogress.Progress `json:"progress,omitempty"`
	Outcome  UpdateOutcome             `json:"outcome,omitempty"`
	Reason   string                    `json:"reason,omitempty"`
	// Schema is the database's migration version, reported by a snapshot
	// that could read it before it copied (UpdateCommand.SchemaVersion).
	Schema int `json:"schema,omitempty"`
	// Step is the last step a trial that failed reported
	// (TrialFailedError.Step), on the trial's result. Its progress report
	// may have been coalesced away by the restore's before it was
	// delivered, so the failure memory takes the step from here.
	Step string `json:"step,omitempty"`
}

// The phases a command reports while it undoes or stops work after a
// failure. They say what the command did next, not where it failed.
const (
	phaseRestore   = "update.restore"
	phaseTrialStop = "update.trial.stop"
)

// RecoveryPhase reports whether a progress phase is a command's recovery
// after a failure rather than the step that failed.
func RecoveryPhase(phase string) bool {
	return phase == phaseRestore || phase == phaseTrialStop
}

// WriteUpdateEvent writes one report line.
func WriteUpdateEvent(w io.Writer, event UpdateEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line := make([]byte, 0, len(UpdateEventPrefix)+len(data)+1)
	line = append(line, UpdateEventPrefix...)
	line = append(line, data...)
	line = append(line, '\n')
	_, err = w.Write(line)
	return err
}

// ParseUpdateEvent reads one stdout line. ok is false for a line that is not
// a report. A line with the prefix that does not decode is an error.
func ParseUpdateEvent(line string) (event UpdateEvent, ok bool, err error) {
	line = strings.TrimRight(line, "\r\n")
	rest, found := strings.CutPrefix(line, UpdateEventPrefix)
	if !found {
		return UpdateEvent{}, false, nil
	}
	if err := json.Unmarshal([]byte(rest), &event); err != nil {
		return UpdateEvent{}, true, fmt.Errorf("supervise: decode update report: %w", err)
	}
	switch event.Type {
	case UpdateEventStarted, UpdateEventProgress, UpdateEventResult:
	default:
		return UpdateEvent{}, true, fmt.Errorf("supervise: update report of unknown type %q", event.Type)
	}
	if event.Type == UpdateEventProgress && event.Progress == nil {
		return UpdateEvent{}, true, errors.New("supervise: progress report carries no progress")
	}
	return event, true, nil
}

// ProgressRelay delivers progress reports on its own goroutine and keeps only
// the latest undelivered one, so a slow or stalled reader never blocks the
// process that reports. Coalescing may drop a report's detail, never the fact
// that progress happened: a reporter's UpdatedAt and AliveAt only advance, so
// the latest report carries every advance it replaced.
type ProgressRelay struct {
	deliver func(startupprogress.Progress) error

	mu      sync.Mutex
	pending *startupprogress.Progress
	closed  bool
	err     error

	wake   chan struct{}
	failed chan struct{}
	done   chan struct{}
}

// NewProgressRelay starts the delivery goroutine. Close stops it.
func NewProgressRelay(deliver func(p startupprogress.Progress) error) *ProgressRelay {
	r := &ProgressRelay{
		deliver: deliver,
		wake:    make(chan struct{}, 1),
		failed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go r.run()
	return r
}

// Report queues p. It never blocks on delivery.
func (r *ProgressRelay) Report(p startupprogress.Progress) {
	r.mu.Lock()
	if r.closed || r.err != nil {
		r.mu.Unlock()
		return
	}
	r.pending = &p
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Failed is closed once a delivery fails. Later reports are dropped.
func (r *ProgressRelay) Failed() <-chan struct{} { return r.failed }

// Close delivers the last queued report, stops the goroutine and returns the
// first delivery error.
func (r *ProgressRelay) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *ProgressRelay) run() {
	defer close(r.done)
	for {
		r.mu.Lock()
		next := r.pending
		r.pending = nil
		closed := r.closed
		r.mu.Unlock()
		if next != nil {
			if err := r.deliver(*next); err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				close(r.failed)
				return
			}
			continue
		}
		if closed {
			return
		}
		<-r.wake
	}
}
