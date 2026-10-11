package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The wake supervisor's rows (docs/specs/standing-agents.md, "Wake
// supervisor"): the loops and watches a thread registered, and the threads
// the user promoted to standing agents. The app owns the clock and the
// processes; these accessors make each state change one conditional write, so
// a change that finds the row in another state is a lost race, never a second
// run.

// Wake kinds.
const (
	AgentWakeLoop  = "loop"
	AgentWakeWatch = "watch"
)

// Wake states. `running` is the supervisor's own execution: a watch script in
// flight, a long-running watch's process alive, or a loop between its fire and
// its queued message. A row still `running` at boot was cut by quit.
const (
	AgentWakeArmed     = "armed"
	AgentWakeRunning   = "running"
	AgentWakeCancelled = "cancelled"
)

// MaxAgentWakesPerThread bounds the loops and watches one thread keeps armed
// at once. Each long-running watch is a process, so the bound is a process
// bound as well as a row bound.
const MaxAgentWakesPerThread = 16

// ErrAgentWakeLabelTaken refuses a registration whose label names a live row
// of the other kind: `thread_cancel` addresses both kinds by label, so one
// label names one thing.
var ErrAgentWakeLabelTaken = errors.New("store: that label already names a live loop or watch of the other kind")

// ErrAgentWakeLimit refuses a registration past MaxAgentWakesPerThread.
var ErrAgentWakeLimit = errors.New("store: too many live loops and watches on this thread")

// ErrAgentWakeStillQueued refuses a second wake for a row whose previous one
// has not reached the provider yet.
var ErrAgentWakeStillQueued = errors.New("store: this loop or watch's previous wake is still queued")

// AgentWake is one row of agent_wakes. Zero means NULL for the three nullable
// integers.
type AgentWake struct {
	ID              string `json:"id"`
	ThreadID        string `json:"threadId"`
	Kind            string `json:"kind"`
	Label           string `json:"label"`
	Note            string `json:"note,omitempty"`
	Command         string `json:"command,omitempty"`
	Cwd             string `json:"cwd,omitempty"`
	IntervalSeconds int64  `json:"intervalSeconds,omitempty"`
	LongRunning     bool   `json:"longRunning,omitempty"`
	NextAt          int64  `json:"nextAt,omitempty"`
	LastRunAt       int64  `json:"lastRunAt,omitempty"`
	LastOutcome     string `json:"lastOutcome,omitempty"`
	Missed          int64  `json:"missed,omitempty"`
	State           string `json:"state"`
	CreatedAt       int64  `json:"createdAt"`
}

// StandingAgent is one row of standing_agents.
type StandingAgent struct {
	ThreadID     string `json:"threadId"`
	Home         string `json:"home"`
	NotifyOnTick bool   `json:"notifyOnTick"`
	PromotedAt   int64  `json:"promotedAt"`
}

const agentWakeColumns = `id, thread_id, kind, label, note, command, cwd, interval_seconds,
	long_running, next_at, last_run_at, last_outcome, missed, state, created_at`

func scanAgentWake(scanner interface{ Scan(...any) error }) (AgentWake, error) {
	var row AgentWake
	var interval, nextAt, lastRunAt sql.NullInt64
	var longRunning int
	if err := scanner.Scan(&row.ID, &row.ThreadID, &row.Kind, &row.Label, &row.Note, &row.Command,
		&row.Cwd, &interval, &longRunning, &nextAt, &lastRunAt, &row.LastOutcome, &row.Missed,
		&row.State, &row.CreatedAt); err != nil {
		return AgentWake{}, err
	}
	row.IntervalSeconds, row.NextAt, row.LastRunAt = interval.Int64, nextAt.Int64, lastRunAt.Int64
	row.LongRunning = longRunning == 1
	return row, nil
}

func collectAgentWakes(rows *sql.Rows, action string) ([]AgentWake, error) {
	defer rows.Close()
	var out []AgentWake
	for rows.Next() {
		row, err := scanAgentWake(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

func nullableMillis(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

// RegisterAgentWake arms one loop or watch, replacing the row the thread
// already keeps under the same label. A label is one row for the life of the
// thread: a cancelled row is revived rather than joined by a second one, so
// the rows stay bounded by the labels a thread ever used.
//
// An interval run in flight keeps `running`: the run that holds it re-arms it
// when it finishes, and arming it here would let the sweep start a second run
// beside the first. The new schedule and command apply from the next run. A
// long-running watch's process is the caller's to stop and restart, so its
// row is armed like any other.
func (s *Store) RegisterAgentWake(w AgentWake) (AgentWake, error) {
	action := fmt.Sprintf("store: register %s %q on %s", w.Kind, w.Label, w.ThreadID)
	if w.ID == "" || w.ThreadID == "" || strings.TrimSpace(w.Label) == "" {
		return AgentWake{}, fmt.Errorf("%s: id, thread and label are required", action)
	}
	if w.Kind != AgentWakeLoop && w.Kind != AgentWakeWatch {
		return AgentWake{}, fmt.Errorf("%s: unknown kind", action)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return AgentWake{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer tx.Rollback()

	existing, found, err := agentWakeByLabelTx(tx, w.ThreadID, w.Label)
	if err != nil {
		return AgentWake{}, fmt.Errorf("%s: %w", action, err)
	}
	if found && existing.State != AgentWakeCancelled && existing.Kind != w.Kind {
		return AgentWake{}, ErrAgentWakeLabelTaken
	}
	if !found || existing.State == AgentWakeCancelled {
		var live int
		if err := tx.QueryRow(`SELECT count(*) FROM agent_wakes WHERE thread_id = ? AND state <> 'cancelled'`,
			w.ThreadID).Scan(&live); err != nil {
			return AgentWake{}, fmt.Errorf("%s: count live rows: %w", action, err)
		}
		if live >= MaxAgentWakesPerThread {
			return AgentWake{}, ErrAgentWakeLimit
		}
	}
	longRunning := 0
	if w.LongRunning {
		longRunning = 1
	}
	if found {
		state := AgentWakeArmed
		if existing.State == AgentWakeRunning && !existing.LongRunning {
			state = AgentWakeRunning
		}
		if _, err := tx.Exec(`UPDATE agent_wakes
			SET kind = ?, note = ?, command = ?, cwd = ?, interval_seconds = ?, long_running = ?,
			    next_at = ?, missed = 0, state = ?
			WHERE id = ?`,
			w.Kind, w.Note, w.Command, w.Cwd, nullableMillis(w.IntervalSeconds), longRunning,
			nullableMillis(w.NextAt), state, existing.ID); err != nil {
			return AgentWake{}, fmt.Errorf("%s: update: %w", action, err)
		}
		w.ID = existing.ID
	} else {
		if w.CreatedAt == 0 {
			w.CreatedAt = nowMillis()
		}
		// A label is revived rather than duplicated, but a thread can keep
		// choosing new ones. The newest cancelled rows stay for the record and
		// the rest go, so the rows of one thread stay bounded.
		if _, err := tx.Exec(`DELETE FROM agent_wakes WHERE thread_id = ? AND state = 'cancelled'
			AND id NOT IN (SELECT id FROM agent_wakes WHERE thread_id = ? AND state = 'cancelled'
			               ORDER BY created_at DESC LIMIT `+strconv.Itoa(MaxAgentWakesPerThread)+`)`,
			w.ThreadID, w.ThreadID); err != nil {
			return AgentWake{}, fmt.Errorf("%s: prune cancelled rows: %w", action, err)
		}
		if _, err := tx.Exec(`INSERT INTO agent_wakes
			(id, thread_id, kind, label, note, command, cwd, interval_seconds, long_running, next_at, state, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'armed', ?)`,
			w.ID, w.ThreadID, w.Kind, w.Label, w.Note, w.Command, w.Cwd, nullableMillis(w.IntervalSeconds),
			longRunning, nullableMillis(w.NextAt), w.CreatedAt); err != nil {
			return AgentWake{}, fmt.Errorf("%s: insert: %w", action, err)
		}
	}
	row, err := scanAgentWake(tx.QueryRow(`SELECT `+agentWakeColumns+` FROM agent_wakes WHERE id = ?`, w.ID))
	if err != nil {
		return AgentWake{}, fmt.Errorf("%s: read back: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return AgentWake{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	return row, nil
}

func agentWakeByLabelTx(tx *sql.Tx, threadID, label string) (AgentWake, bool, error) {
	row, err := scanAgentWake(tx.QueryRow(`SELECT `+agentWakeColumns+` FROM agent_wakes
		WHERE thread_id = ? AND label = ? ORDER BY created_at LIMIT 1`, threadID, label))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentWake{}, false, nil
	}
	if err != nil {
		return AgentWake{}, false, err
	}
	return row, true, nil
}

// GetAgentWake reads one row by id.
func (s *Store) GetAgentWake(id string) (AgentWake, bool, error) {
	row, err := scanAgentWake(s.reader().QueryRow(`SELECT `+agentWakeColumns+` FROM agent_wakes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentWake{}, false, nil
	}
	if err != nil {
		return AgentWake{}, false, fmt.Errorf("store: get agent wake %s: %w", id, err)
	}
	return row, true, nil
}

// ListAgentWakes lists one thread's live loops and watches by label.
func (s *Store) ListAgentWakes(threadID string) ([]AgentWake, error) {
	rows, err := s.reader().Query(`SELECT `+agentWakeColumns+` FROM agent_wakes
		WHERE thread_id = ? AND state IN ('armed','running') ORDER BY label`, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: list agent wakes for %s: %w", threadID, err)
	}
	return collectAgentWakes(rows, "store: list agent wakes for "+threadID)
}

// ListLiveAgentWakes lists every armed or running row: what the supervisor
// rebuilds at boot.
func (s *Store) ListLiveAgentWakes() ([]AgentWake, error) {
	rows, err := s.reader().Query(`SELECT ` + agentWakeColumns + ` FROM agent_wakes
		WHERE state IN ('armed','running') ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list live agent wakes: %w", err)
	}
	return collectAgentWakes(rows, "store: list live agent wakes")
}

// DueAgentWakes lists the interval rows whose fire has come. A long-running
// watch has no fire time; its process is the schedule.
func (s *Store) DueAgentWakes(now int64, limit int) ([]AgentWake, error) {
	if limit < 1 {
		limit = 32
	}
	rows, err := s.reader().Query(`SELECT `+agentWakeColumns+` FROM agent_wakes
		WHERE state = 'armed' AND next_at <= ? AND long_running = 0
		ORDER BY next_at LIMIT `+strconv.Itoa(limit), now)
	if err != nil {
		return nil, fmt.Errorf("store: list due agent wakes: %w", err)
	}
	return collectAgentWakes(rows, "store: list due agent wakes")
}

// NextAgentWakeDue is the earliest armed interval fire, for the sweep's sleep.
func (s *Store) NextAgentWakeDue() (ScheduledMoment, error) {
	var next sql.NullInt64
	if err := s.reader().QueryRow(`SELECT MIN(next_at) FROM agent_wakes
		WHERE state = 'armed' AND long_running = 0`).Scan(&next); err != nil {
		return ScheduledMoment{}, fmt.Errorf("store: read next agent wake: %w", err)
	}
	return ScheduledMoment{At: next.Int64, Set: next.Valid}, nil
}

// ClaimAgentWake starts one interval run: armed to running, with the next fire
// and the fires this one stands in for written in the same update. Claimed
// false is a lost race: cancelled, re-registered or claimed elsewhere.
func (s *Store) ClaimAgentWake(id string, now, nextAt, missed int64) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: claim agent wake %s", id),
		`UPDATE agent_wakes SET state = 'running', last_run_at = ?, next_at = ?, missed = ?
		  WHERE id = ? AND state = 'armed'`, now, nullableMillis(nextAt), missed, id)
}

// FinishAgentWake ends one interval run: running back to armed, with what it
// came to and the fires it now stands for. A row re-registered as a
// long-running watch while the run was in flight belongs to its process, and
// the finish is a lost race.
func (s *Store) FinishAgentWake(id, outcome string, missed int64) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: finish agent wake %s", id),
		`UPDATE agent_wakes SET state = 'armed', last_outcome = ?, missed = ?
		  WHERE id = ? AND state = 'running' AND long_running = 0`,
		outcome, missed, id)
}

// ReleaseUndeliveredAgentWake ends an interval run whose output a shutdown
// kept from the queue: armed, `not delivered`, and due again at the fire it
// ran for, so the next boot treats it as not run and replays it.
func (s *Store) ReleaseUndeliveredAgentWake(id, outcome string, fireAt int64) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: release undelivered agent wake %s", id),
		`UPDATE agent_wakes SET state = 'armed', last_outcome = ?, next_at = ?
		  WHERE id = ? AND state = 'running' AND long_running = 0`,
		outcome, fireAt, id)
}

// ReleaseQueuedAgentWake answers a wake the app quit with still queued: the
// row records the outcome, and an interval row is due again no later than the
// fire the wake was for, so the boot pass replays it.
func (s *Store) ReleaseQueuedAgentWake(id, outcome string, fireAt int64) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: release queued agent wake %s", id),
		`UPDATE agent_wakes SET last_outcome = ?,
		        next_at = CASE WHEN long_running = 0 AND next_at > ? THEN ? ELSE next_at END
		  WHERE id = ? AND state <> 'cancelled'`,
		outcome, fireAt, fireAt, id)
}

// RecoverAgentWake records what a quit did to a row it left `running` and
// arms it, so the boot pass can claim it again.
func (s *Store) RecoverAgentWake(id, outcome string) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: recover agent wake %s", id),
		`UPDATE agent_wakes SET state = 'armed', last_outcome = ? WHERE id = ? AND state = 'running'`,
		outcome, id)
}

// StartAgentWakeProcess marks a long-running watch's process alive. A row
// cancelled meanwhile stays cancelled, and the caller stops the process.
func (s *Store) StartAgentWakeProcess(id string, now int64) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: start agent wake process %s", id),
		`UPDATE agent_wakes SET state = 'running', last_run_at = ?
		  WHERE id = ? AND state IN ('armed','running') AND long_running = 1`, now, id)
}

// NoteAgentWakeOutcome records what a live row last came to without changing
// its state: a long-running watch's delivered batch or its process exit.
func (s *Store) NoteAgentWakeOutcome(id, outcome string) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: note agent wake outcome %s", id),
		`UPDATE agent_wakes SET last_outcome = ? WHERE id = ? AND state <> 'cancelled'`, outcome, id)
}

// CancelAgentWake cancels one thread's live row by label and returns it as it
// was. An empty kind matches either kind. Not found is no live row of that
// kind under that label.
func (s *Store) CancelAgentWake(threadID, label, kind, outcome string) (AgentWake, bool, error) {
	action := fmt.Sprintf("store: cancel %q on %s", label, threadID)
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return AgentWake{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer tx.Rollback()
	row, found, err := agentWakeByLabelTx(tx, threadID, label)
	if err != nil {
		return AgentWake{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if !found || row.State == AgentWakeCancelled || kind != "" && row.Kind != kind {
		return row, false, nil
	}
	if _, err := tx.Exec(`UPDATE agent_wakes SET state = 'cancelled', next_at = NULL, last_outcome = ?
		WHERE id = ?`, outcome, row.ID); err != nil {
		return AgentWake{}, false, fmt.Errorf("%s: update: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return AgentWake{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return row, true, nil
}

// CancelAgentWakesForThread cancels every live row of one thread, or every
// live row of one kind when kind is set, and returns the ids it cancelled so
// the caller can stop their processes.
func (s *Store) CancelAgentWakesForThread(threadID, kind, outcome string) ([]string, error) {
	action := fmt.Sprintf("store: cancel agent wakes for %s", threadID)
	rows, err := s.db.Query(`UPDATE agent_wakes SET state = 'cancelled', next_at = NULL, last_outcome = ?
		WHERE thread_id = ? AND state <> 'cancelled' AND (? = '' OR kind = ?) RETURNING id`,
		outcome, threadID, kind, kind)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return ids, nil
}

func (s *Store) changeAgentWake(action, query string, args ...any) (bool, error) {
	result, err := s.db.Exec(query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: rows affected: %w", action, err)
	}
	return changed == 1, nil
}

// QueueAgentWake writes one wake's durable queue row, refusing with
// ErrAgentWakeStillQueued while an earlier wake of the same row (any send id
// under sendIDPrefix) is still waiting for the provider. The check and the
// insert are one transaction, so two fires cannot both pass it.
func (s *Store) QueueAgentWake(item FlushQueueItem, sendIDPrefix string) error {
	action := fmt.Sprintf("store: queue agent wake %s", item.SendID)
	if sendIDPrefix == "" || !strings.HasPrefix(item.SendID, sendIDPrefix) {
		return fmt.Errorf("%s: send id must carry the wake's prefix", action)
	}
	tx, release, err := s.beginDurableTx(context.Background())
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer release()
	defer tx.Rollback()
	var pending bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM flush_queue_items
		WHERE thread_id = ? AND substr(send_id, 1, ?) = ?)`,
		item.ThreadID, len(sendIDPrefix), sendIDPrefix).Scan(&pending); err != nil {
		return fmt.Errorf("%s: probe queued wake: %w", action, err)
	}
	if pending {
		return ErrAgentWakeStillQueued
	}
	if err := insertFlushQueueItem(tx, item); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// PromoteStandingAgent records a promotion. Promoted false means the thread
// already was a standing agent; the row is returned either way.
func (s *Store) PromoteStandingAgent(threadID, home string, now int64) (StandingAgent, bool, error) {
	action := fmt.Sprintf("store: promote %s", threadID)
	result, err := s.db.Exec(`INSERT INTO standing_agents (thread_id, home, promoted_at) VALUES (?, ?, ?)
		ON CONFLICT(thread_id) DO NOTHING`, threadID, home, now)
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: rows affected: %w", action, err)
	}
	row, found, err := s.GetStandingAgent(threadID)
	if err != nil {
		return StandingAgent{}, false, err
	}
	if !found {
		return StandingAgent{}, false, fmt.Errorf("%s: the row is gone after insert", action)
	}
	return row, inserted == 1, nil
}

// DemoteStandingAgent removes the row and reports whether there was one.
func (s *Store) DemoteStandingAgent(threadID string) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: demote %s", threadID),
		`DELETE FROM standing_agents WHERE thread_id = ?`, threadID)
}

// GetStandingAgent reads one thread's agent row.
func (s *Store) GetStandingAgent(threadID string) (StandingAgent, bool, error) {
	var row StandingAgent
	var notify int
	err := s.reader().QueryRow(`SELECT thread_id, home, notify_on_tick, promoted_at FROM standing_agents
		WHERE thread_id = ?`, threadID).Scan(&row.ThreadID, &row.Home, &notify, &row.PromotedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StandingAgent{}, false, nil
	}
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("store: get standing agent %s: %w", threadID, err)
	}
	row.NotifyOnTick = notify == 1
	return row, true, nil
}

// ThreadHasLiveWorkflowRun reports whether a workflow run that has not ended
// is bound to the thread: as the thread it reports back to, or as the thread
// one of its phases or fan-out units runs in.
func (s *Store) ThreadHasLiveWorkflowRun(threadID string) (bool, error) {
	var live bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM work_items
		WHERE state NOT IN ('done','failed','cancelled')
		  AND (origin_thread_id = ?1
		       OR id IN (SELECT item_id FROM work_item_phases WHERE thread_id = ?1)
		       OR id IN (SELECT item_id FROM work_item_units WHERE thread_id = ?1)))`, threadID).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("store: read live workflow runs of %s: %w", threadID, err)
	}
	return live, nil
}
