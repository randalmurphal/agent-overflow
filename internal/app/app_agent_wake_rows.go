package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// A thread's loops, watches and reminders as the composer's background tray
// lists them (docs/specs/standing-agents.md, "Activity rail"). The rows are
// the wake supervisor's own (`agent_wakes`) and the thread's open `remind`
// requests (`thread_requests`); nothing here keeps a copy. Every write to
// either emits `thread:wakes_changed`, and the tray reads the list again.
//
// Wake now fires a row through the sweep's own fire path, and Cancel takes the
// path thread_cancel takes, so a row the person drives behaves as one the
// clock or the agent drives.

// Kinds a tray row can have: the two agent_wakes kinds and a reminder.
const threadWakeKindReminder = "reminder"

// threadWakeReminderLimit bounds the reminders one listing carries: a thread
// may keep any number, and the tray shows the soonest.
const threadWakeReminderLimit = 64

// threadWakeLabelMax bounds a reminder's label, the first line of its note.
const threadWakeLabelMax = 200

// ThreadWakeRow is one loop, watch or reminder of a thread. It carries what
// the collapsed row shows; GetThreadWakeDetail reads the rest on demand.
type ThreadWakeRow struct {
	// ID is the loop or watch's id, or the reminder's token.
	ID string `json:"id"`
	// Kind is `loop`, `watch` or `reminder`.
	Kind string `json:"kind"`
	// Label names a loop or watch; a reminder's is its note's first line.
	Label string `json:"label"`
	// State is `armed` or `running`: a watch run or a loop fire in flight,
	// or a long-running watch's process alive.
	State           string `json:"state"`
	LongRunning     bool   `json:"longRunning,omitempty"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	// NextAt is the next fire, Unix milliseconds; zero for a long-running
	// watch.
	NextAt      int64  `json:"nextAt,omitempty"`
	LastRunAt   int64  `json:"lastRunAt,omitempty"`
	LastOutcome string `json:"lastOutcome,omitempty"`
	// Undelivered is the outcome of the row's latest wake that did not
	// reach the thread, until a later one does.
	Undelivered string `json:"undelivered,omitempty"`
	Missed      int64  `json:"missed,omitempty"`
}

// ThreadWakes is a thread's live loops and watches by label, then its
// soonest reminders. MoreReminders counts the ones past the listing's bound.
type ThreadWakes struct {
	Rows          []ThreadWakeRow `json:"rows"`
	MoreReminders int             `json:"moreReminders"`
}

// ThreadWakeDetail is what an expanded watch row shows.
type ThreadWakeDetail struct {
	Command string `json:"command"`
	// Cwd is where the watch runs: its own directory, or the thread's
	// workspace when it names none.
	Cwd            string `json:"cwd"`
	TimeoutSeconds int64  `json:"timeoutSeconds,omitempty"`
	// Stderr is the tail of its last stderr.
	Stderr string `json:"stderr"`
}

// emitThreadWakesChanged tells every client that a thread's loops, watches or
// reminders changed.
func (a *App) emitThreadWakesChanged(threadID string) {
	if threadID == "" {
		return
	}
	a.emit(eventchan.ThreadWakesChanged, map[string]string{"threadId": threadID})
}

// ListThreadWakes lists a thread's live loops and watches and its reminders
// that have not fired.
//
//ao:scope threads:read
func (a *App) ListThreadWakes(threadID string) (ThreadWakes, error) {
	wakes, err := a.store.ListAgentWakes(threadID)
	if err != nil {
		return ThreadWakes{}, err
	}
	reminders, total, err := a.store.ListOpenThreadReminders(threadID, threadWakeReminderLimit)
	if err != nil {
		return ThreadWakes{}, err
	}
	out := ThreadWakes{Rows: make([]ThreadWakeRow, 0, len(wakes)+len(reminders)), MoreReminders: total - len(reminders)}
	for _, row := range wakes {
		out.Rows = append(out.Rows, ThreadWakeRow{
			ID: row.ID, Kind: row.Kind, Label: row.Label, State: row.State, LongRunning: row.LongRunning,
			IntervalSeconds: row.IntervalSeconds, NextAt: row.NextAt, LastRunAt: row.LastRunAt,
			LastOutcome: row.LastOutcome, Undelivered: row.Undelivered, Missed: row.Missed,
		})
	}
	for _, row := range reminders {
		out.Rows = append(out.Rows, ThreadWakeRow{
			ID: row.Token, Kind: threadWakeKindReminder, Label: reminderLabel(string(row.Answer)),
			State: store.AgentWakeArmed, NextAt: row.DueAt,
		})
	}
	return out, nil
}

// reminderLabel is the first line of a reminder's note, bounded.
func reminderLabel(note string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(note), "\n")
	line = strings.TrimSpace(line)
	if len(line) > threadWakeLabelMax {
		line = clipUTF8(line, threadWakeLabelMax) + "…"
	}
	return line
}

// GetThreadWakeDetail reads what an expanded watch row shows: its command,
// where it runs, and the tail of its last stderr.
//
//ao:scope threads:read
func (a *App) GetThreadWakeDetail(threadID, wakeID string) (ThreadWakeDetail, error) {
	row, err := a.threadWake(threadID, wakeID)
	if err != nil {
		return ThreadWakeDetail{}, err
	}
	if row.Kind != store.AgentWakeWatch {
		return ThreadWakeDetail{}, fmt.Errorf("the %s %q runs no command", row.Kind, row.Label)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return ThreadWakeDetail{}, fmt.Errorf("read the thread: %w", err)
	}
	return ThreadWakeDetail{
		Command: row.Command, Cwd: agentWatchDir(row, thread.WorkspacePath),
		TimeoutSeconds: row.TimeoutSeconds, Stderr: row.LastStderr,
	}, nil
}

// agentWatchDir is the directory a watch runs in: its own, resolved against
// the thread's workspace when relative, or the workspace.
func agentWatchDir(row store.AgentWake, workspace string) string {
	if row.Cwd == "" {
		return workspace
	}
	if filepath.IsAbs(row.Cwd) {
		return row.Cwd
	}
	return filepath.Join(workspace, row.Cwd)
}

// WakeThreadWakeNow fires a loop, watch or reminder of the thread now, through
// the sweep's own fire path. A loop or watch keeps its schedule. Refused, with
// the reason, for a row that is cancelled or gone, one already running, a
// long-running watch, and a thread that is archived. Starting a turn drives
// the thread as a send does, and a watch runs its script on this computer.
//
//ao:scope threads:operate
func (a *App) WakeThreadWakeNow(ctx context.Context, threadID, kind, id string) error {
	if err := a.requireAutonomyForThread(ctx, threadID, ""); err != nil {
		return err
	}
	if err := a.requireLiveWakeThread(threadID); err != nil {
		return err
	}
	if kind == threadWakeKindReminder {
		return a.wakeThreadReminderNow(threadID, id)
	}
	row, err := a.threadWake(threadID, id)
	if err != nil {
		return err
	}
	if row.Kind != kind {
		return fmt.Errorf("the %s %q is not a %s", row.Kind, row.Label, kind)
	}
	if row.Kind == store.AgentWakeWatch {
		if err := a.requireScope(ctx, transport.ScopeThreadsAutonomy, "running a standing agent's watch"); err != nil {
			return err
		}
	}
	if err := wakeNowRefusal(row); err != nil {
		return err
	}
	now := time.Now()
	if a.fireAgentWake(row, now, agentWakeFire{at: now.UnixMilli(), missed: carriedMissed(row)}) {
		return nil
	}
	// The claim lost to the sweep, a cancel or a run, or could not be
	// written: the row as it stands now says which.
	if current, err := a.threadWake(threadID, id); err != nil {
		return err
	} else if err := wakeNowRefusal(current); err != nil {
		return err
	}
	if a.shuttingDown.Load() {
		return errors.New("the app is shutting down")
	}
	return fmt.Errorf("the %s %q could not be fired; the app log says why", row.Kind, row.Label)
}

// wakeNowRefusal is why a loop or watch cannot be fired now, or nil.
func wakeNowRefusal(row store.AgentWake) error {
	switch {
	case row.State == store.AgentWakeCancelled:
		return fmt.Errorf("the %s %q was cancelled", row.Kind, row.Label)
	case row.LongRunning:
		return fmt.Errorf("the watch %q runs all the time and wakes the thread with each line it prints; it has no fire to bring forward", row.Label)
	case row.State == store.AgentWakeRunning:
		return fmt.Errorf("the %s %q is already running", row.Kind, row.Label)
	}
	return nil
}

// wakeThreadReminderNow settles a reminder now, as the sweep does when it
// comes due, and delivers its wake.
func (a *App) wakeThreadReminderNow(threadID, token string) error {
	row, err := a.threadReminder(threadID, token)
	if err != nil {
		return err
	}
	if row.State != store.ThreadRequestAccepted {
		return errors.New("the reminder has already fired")
	}
	a.fireThreadReminder(row, time.Now())
	current, found, err := a.store.GetThreadRequest(token)
	switch {
	case err != nil:
		return fmt.Errorf("read the reminder: %w", err)
	case !found:
		return errors.New("the reminder was cancelled")
	case current.State == store.ThreadRequestAccepted:
		return errors.New("the reminder could not be fired; the app log says why")
	}
	return nil
}

// CancelThreadWake cancels a loop, watch or reminder of the thread, as
// thread_cancel does. Cancelling one already gone is not an error.
//
//ao:scope threads:operate
func (a *App) CancelThreadWake(threadID, kind, id string) error {
	if kind == threadWakeKindReminder {
		row, err := a.threadReminder(threadID, id)
		if errors.Is(err, errThreadWakeGone) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.State != store.ThreadRequestAccepted {
			return nil
		}
		_, err = a.dropThreadReminder(row)
		return err
	}
	row, err := a.threadWake(threadID, id)
	if errors.Is(err, errThreadWakeGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Kind != kind {
		return fmt.Errorf("the %s %q is not a %s", row.Kind, row.Label, kind)
	}
	_, _, err = a.cancelAgentWake(threadID, row.Label, row.Kind)
	return err
}

// errThreadWakeGone is a row the thread no longer has.
var errThreadWakeGone = errors.New("it is no longer scheduled")

// threadWake reads one loop or watch of a thread.
func (a *App) threadWake(threadID, id string) (store.AgentWake, error) {
	row, found, err := a.store.GetAgentWake(id)
	if err != nil {
		return store.AgentWake{}, err
	}
	if !found || row.ThreadID != threadID {
		return store.AgentWake{}, errThreadWakeGone
	}
	return row, nil
}

// threadReminder reads one reminder of a thread.
func (a *App) threadReminder(threadID, token string) (store.ThreadRequest, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		return store.ThreadRequest{}, fmt.Errorf("read the reminder: %w", err)
	}
	if !found || row.CallerThreadID != threadID || row.Kind != store.ThreadRequestRemind {
		return store.ThreadRequest{}, errThreadWakeGone
	}
	return row, nil
}

// requireLiveWakeThread refuses a thread a fire must not wake: an archived
// one, which a reminder's delivery would unarchive.
func (a *App) requireLiveWakeThread(threadID string) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("read the thread: %w", err)
	}
	if thread.Archived {
		return errors.New("the thread is archived; unarchive it first")
	}
	return nil
}
