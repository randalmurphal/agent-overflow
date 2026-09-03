package supervise

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
)

// StateSchema is the version of the service-state.json shape. A file naming a
// schema this binary does not know is INVALID rather than upgradable: the
// supervisor's whole job is knowing which version to run, and a file it can
// only partly read is a guess.
const StateSchema = 1

// UpdateState is where one update record rests. The set is closed, and the
// selection rule for each is the point of the whole file.
type UpdateState string

const (
	// UpdatePending selects the target as a RETRYABLE trial. The supervisor
	// reaching this state on boot means a previous run was interrupted
	// mid-trial; the snapshot is still on disk and the trial is attempted
	// again, up to TrialAttemptLimit.
	UpdatePending UpdateState = "pending"
	// UpdateCommitted selects the target for ordinary restarts. The trial
	// reported prepared and the supervisor durably said so before telling it.
	UpdateCommitted UpdateState = "committed"
	// UpdateRolledBack selects the previous version. The trial ran and did not
	// reach prepared, so the snapshot was restored over its work.
	UpdateRolledBack UpdateState = "rolled-back"
	// UpdateFailed selects the previous version. The update never got as far
	// as a trial — a target that would not preflight, a snapshot that could
	// not be taken, a spawn that failed — so there was nothing of the
	// target's to undo.
	UpdateFailed UpdateState = "failed"
)

// TrialAttemptLimit bounds how many times a pending record may be retried.
//
// t3code calls pending "retryable" and it is right to: an update interrupted
// by a reboot should finish rather than roll back. But retry with no bound is
// how an unattended host bricks — a trial that reliably kills the supervisor
// (or the machine) is restarted by the service manager into the same trial
// forever, and a watchdog that never gives up is not a watchdog. Two: one
// interruption is bad luck, two is the target.
const TrialAttemptLimit = 2

// State is the supervisor's durable selection. Exactly one update record at a
// time, because an update is a transition between two versions and a queue of
// them is a state machine nobody can reason about at 3am.
type State struct {
	Schema int `json:"schema"`
	// ActiveVersion is the version that was running before the record below.
	// With no record it is simply what runs.
	ActiveVersion string        `json:"activeVersion"`
	Update        *UpdateRecord `json:"update,omitempty"`
}

// UpdateRecord is one update, from a version to a version, resting somewhere.
type UpdateRecord struct {
	ID    string      `json:"id"`
	State UpdateState `json:"state"`
	From  string      `json:"from"`
	To    string      `json:"to"`
	// Attempts counts trial starts, so an interrupted supervisor cannot retry
	// forever. Meaningful only while the record is pending, and incremented at
	// the ONE place a trial is actually started (Supervisor.Run) rather than
	// where the update is opened — a count kept anywhere else counts intentions.
	Attempts    int    `json:"attempts,omitempty"`
	StartedAtMs int64  `json:"startedAtMs,omitempty"`
	SettledAtMs int64  `json:"settledAtMs,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Reported is whether a backend has already carried this settled
	// record's outcome to its clients. A settled record rests in the file
	// until the NEXT update collapses it, which can be months, so without
	// this flag every boot in between would re-announce the same
	// "committed" or "rolled back" to every admin client that attached
	// and there would be no way to tell a fresh outcome from an old one.
	//
	// It is set at the moment the outcome reaches a process that publishes
	// it: with the commit itself, because the commit frame carries the
	// outcome to the child that trialled it, and otherwise when a NON-TRIAL
	// child says hello, because that child was handed the outcome in its
	// activate frame. Absent in the file means false, which is what every state
	// written before this field existed decodes to and is the correct
	// reading of one: nobody had reported it.
	Reported bool `json:"reported,omitempty"`
}

// Settled reports whether the record has reached a terminal state.
func (r *UpdateRecord) Settled() bool {
	return r != nil && r.State != UpdatePending
}

// Selection is what a validated state says to run.
type Selection struct {
	// Version is the staged version to spawn.
	Version string
	// Trial is true when that spawn is a trial that must report prepared.
	Trial bool
	// UpdateID is the in-flight update's id while Trial is true, and the id of
	// the update whose outcome this boot carries otherwise (empty when this
	// boot follows no update at all).
	UpdateID string
	// Outcome is the settled state of the most recent update record, empty
	// when there is none. It is what the backend publishes for a client that
	// asked for the update and is now reconnecting.
	Outcome UpdateState
	// Reason is that outcome's recorded reason, for the same reader.
	Reason string
	// Target is the version the update was aiming AT, which is not always
	// the version running: a rollback's whole point is that the version
	// answering is the one that came back. A client told only "rolled
	// back" plus the running version would read the old version's number
	// as the one that failed.
	Target string
}

// Select answers which version to run, and how.
//
// This is the entire selection semantics, in one function, so no caller can
// hold a second opinion:
//
//	no record          → the active version, ordinarily
//	pending A → B      → B, as a retryable trial
//	committed A → B    → B, ordinarily
//	rolled-back A → B  → A, ordinarily
//	failed A → B       → A, ordinarily
//
// Which version to run is the only half that survives being told. The
// OUTCOME half is carried once: a settled record that has already been
// reported selects the same version with nothing to announce, so a
// machine that rebooted nightly since an update does not tell every
// client about it again on every one of those boots.
//
// An invalid state has no answer and gets an error. The supervisor exits
// non-zero on one rather than guessing, because every guess it could make is
// "run a version the operator did not choose".
func (s State) Select() (Selection, error) {
	if err := s.Validate(); err != nil {
		return Selection{}, err
	}
	if s.Update == nil {
		return Selection{Version: s.ActiveVersion}, nil
	}
	record := *s.Update
	switch record.State {
	case UpdatePending:
		return Selection{
			Version: record.To, Trial: true,
			UpdateID: record.ID, Target: record.To,
		}, nil
	case UpdateCommitted:
		return record.announce(Selection{Version: record.To}), nil
	case UpdateRolledBack, UpdateFailed:
		return record.announce(Selection{Version: record.From}), nil
	default:
		return Selection{}, fmt.Errorf("supervise: update %q rests in unknown state %q", record.ID, record.State)
	}
}

// announce fills the outcome half of a settled record's selection, or
// leaves it empty when the record has already been reported. The version
// to run is the caller's, because that is the one thing the two settled
// branches disagree about.
func (r UpdateRecord) announce(selection Selection) Selection {
	if r.Reported {
		return selection
	}
	selection.UpdateID = r.ID
	selection.Outcome = r.State
	selection.Reason = r.Reason
	selection.Target = r.To
	return selection
}

// Validate is the fail-closed check. Everything it refuses is a state whose
// selection would be a guess.
func (s State) Validate() error {
	if s.Schema != StateSchema {
		return fmt.Errorf("supervise: service state schema %d, this binary knows %d", s.Schema, StateSchema)
	}
	if err := ValidVersion(s.ActiveVersion); err != nil {
		return fmt.Errorf("supervise: active version: %w", err)
	}
	if s.Update == nil {
		return nil
	}
	record := *s.Update
	if strings.TrimSpace(record.ID) == "" {
		return errors.New("supervise: the update record has no id")
	}
	switch record.State {
	case UpdatePending, UpdateCommitted, UpdateRolledBack, UpdateFailed:
	default:
		return fmt.Errorf("supervise: update %q rests in unknown state %q", record.ID, record.State)
	}
	if err := ValidVersion(record.From); err != nil {
		return fmt.Errorf("supervise: update %q from: %w", record.ID, err)
	}
	if err := ValidVersion(record.To); err != nil {
		return fmt.Errorf("supervise: update %q to: %w", record.ID, err)
	}
	// The record's From is what ActiveVersion means: the version that was
	// running when the update started. A file where the two disagree has been
	// edited by something that did not understand it, and both readings are
	// plausible — which is exactly when guessing is worst.
	if record.From != s.ActiveVersion {
		return fmt.Errorf("supervise: update %q says it started from %q but the active version is %q",
			record.ID, record.From, s.ActiveVersion)
	}
	if record.Attempts < 0 {
		return fmt.Errorf("supervise: update %q has %d attempts", record.ID, record.Attempts)
	}
	return nil
}

// LoadState reads the durable state. found is false with a nil error when
// there is none, which is the fresh-install case and not a fault.
//
// A file that exists and cannot be read, parsed, or validated IS an error.
// That is the fail-closed half: a supervisor that treated an unreadable state
// file as "no state" would silently adopt its own binary over an update
// somebody committed.
func LoadState(layout Layout) (State, bool, error) {
	var state State
	found, err := atomicfile.ReadJSON(layout.StatePath(), &state)
	if err != nil {
		return State{}, false, fmt.Errorf("supervise: read service state: %w", err)
	}
	if !found {
		return State{}, false, nil
	}
	if err := state.Validate(); err != nil {
		return State{}, true, err
	}
	return state, true, nil
}

// SaveState writes the durable state: same-directory temp, fsync, rename,
// directory fsync. Validated first, so this package cannot be the thing that
// writes a state it would later refuse to read.
func SaveState(layout Layout, state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if err := atomicfile.WriteJSON(layout.StatePath(), state); err != nil {
		return fmt.Errorf("supervise: write service state: %w", err)
	}
	return nil
}

// Adopt returns the state for a version that is simply what runs now: no
// update record, nothing in flight. It is both the fresh-install case and what
// `service update` writes, because a local update performed with the unit
// stopped is a selection rather than a trial — the operator is standing there.
func Adopt(version string) (State, error) {
	state := State{Schema: StateSchema, ActiveVersion: version}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

// Begin opens an update from the currently selected version to target.
//
// The previous record is COLLAPSED here rather than kept: the version it
// selected becomes the new ActiveVersion, so the file always holds one
// transition and "previous" always means one thing. That collapse is why a
// committed update needs no separate promotion step.
func (s State) Begin(id, target string, now time.Time) (State, error) {
	selection, err := s.Select()
	if err != nil {
		return State{}, err
	}
	if selection.Trial {
		return State{}, fmt.Errorf("supervise: update %q is already in flight", selection.UpdateID)
	}
	if strings.TrimSpace(id) == "" {
		return State{}, errors.New("supervise: an update id is required")
	}
	if err := ValidVersion(target); err != nil {
		return State{}, err
	}
	if target == selection.Version {
		return State{}, fmt.Errorf("supervise: version %q is already running", target)
	}
	next := State{
		Schema:        StateSchema,
		ActiveVersion: selection.Version,
		Update: &UpdateRecord{
			ID: id, State: UpdatePending,
			From: selection.Version, To: target,
			// Zero trials so far. Run counts one immediately before it spawns.
			Attempts: 0, StartedAtMs: now.UnixMilli(),
		},
	}
	if err := next.Validate(); err != nil {
		return State{}, err
	}
	return next, nil
}

// Retry counts one more trial start against a pending record. Called by
// Supervisor.Run immediately before every trial spawn, first one included, so
// Attempts is what actually happened rather than what was intended — an
// interrupted supervisor that never got as far as spawning must not burn one.
func (s State) Retry() (State, error) {
	if s.Update == nil || s.Update.State != UpdatePending {
		return State{}, errors.New("supervise: no pending update to retry")
	}
	record := *s.Update
	record.Attempts++
	next := s
	next.Update = &record
	if err := next.Validate(); err != nil {
		return State{}, err
	}
	return next, nil
}

// MarkReported records that this settled record's outcome has been
// delivered. changed is false when there was nothing to mark, which is
// both a state with no record and one already marked, so a caller can
// skip a durable write it does not need.
func (s State) MarkReported() (State, bool, error) {
	if s.Update == nil || !s.Update.Settled() || s.Update.Reported {
		return s, false, nil
	}
	record := *s.Update
	record.Reported = true
	next := s
	next.Update = &record
	if err := next.Validate(); err != nil {
		return State{}, false, err
	}
	return next, true, nil
}

// Settle moves the pending record to a terminal state.
func (s State) Settle(state UpdateState, reason string, now time.Time) (State, error) {
	if s.Update == nil {
		return State{}, errors.New("supervise: no update to settle")
	}
	if state == UpdatePending {
		return State{}, errors.New("supervise: pending is not a settlement")
	}
	record := *s.Update
	record.State = state
	record.Reason = reason
	record.SettledAtMs = now.UnixMilli()
	next := s
	next.Update = &record
	if err := next.Validate(); err != nil {
		return State{}, err
	}
	return next, nil
}
