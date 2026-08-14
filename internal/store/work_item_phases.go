package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// WorkItemPhase is one attempt of a workflow phase.
type WorkItemPhase struct {
	ItemID         string          `json:"itemId"`
	PhaseID        string          `json:"phaseId"`
	Attempt        int             `json:"attempt"`
	ThreadID       string          `json:"threadId,omitempty"`
	InputEnvelope  json.RawMessage `json:"inputEnvelope,omitempty"`
	OutputEnvelope json.RawMessage `json:"outputEnvelope,omitempty"`
	GateTrace      json.RawMessage `json:"gateTrace,omitempty"`
	Intervention   json.RawMessage `json:"intervention,omitempty"`
	NarrativePath  string          `json:"narrativePath,omitempty"`
	// ParkCause is the engine's own statement of why it parked this attempt —
	// the workspace that would not provision, the width the project forbids,
	// the argument that named no input. It is deliberately not the envelope:
	// that belongs to the agent, and an attempt the engine parked before any
	// turn ran has none. Empty for every attempt whose reason names its own
	// cause.
	ParkCause string `json:"parkCause,omitempty"`
	// ProviderUsageScopeID identifies the provider/account credential scope
	// whose typed usage refusal parked this attempt. Zero means this attempt did
	// not park on a recognized usage limit (or predates that provenance).
	ProviderUsageScopeID WorkflowProviderUsageScopeID `json:"providerUsageScopeId,omitempty"`
	Status               string                       `json:"status"`
	StartedAt            int64                        `json:"startedAt"`
	EndedAt              int64                        `json:"endedAt,omitempty"`
}

// WorkItemPhaseContext is the narrow phase-history projection used to rebuild
// variables, attempts, and loop counts. It omits cumulative input envelopes,
// thread IDs, narrative paths, and timestamps.
type WorkItemPhaseContext struct {
	PhaseID        string
	Attempt        int
	Status         string
	OutputEnvelope json.RawMessage
	GateTrace      json.RawMessage
	Intervention   json.RawMessage
}

// WorkItemPhaseTimeline is the narrow phase projection served to workflow
// detail views. Cumulative inputs and backend-only diagnostics never cross the
// SQLite boundary on that read path.
type WorkItemPhaseTimeline struct {
	ItemID         string
	PhaseID        string
	Attempt        int
	ThreadID       string
	OutputEnvelope json.RawMessage
	// ParkCause rides along because the wake composes from this projection: a
	// park the engine diagnosed has no envelope to draw a detail line from, and
	// the cause is the whole of what the woken reader needs.
	ParkCause            string
	ProviderUsageScopeID WorkflowProviderUsageScopeID
	Status               string
	StartedAt            int64
	EndedAt              int64
}

// WorkItemPhaseProvenance is one phase attempt narrowed to what produced it:
// the model settings its thread actually ran with and its gate trace. Envelopes
// and narratives are absent on purpose — this projection feeds an agent's `ao
// run status`, where a context window pays for every byte crossing the wire.
type WorkItemPhaseProvenance struct {
	PhaseID  string
	Attempt  int
	Status   string
	ThreadID string
	// Provider, Model, and Effort are empty for an attempt with no thread row.
	Provider  string
	Model     string
	Effort    string
	GateTrace json.RawMessage
	// ParkCause is why the engine parked this attempt. It is the one envelope-
	// adjacent field this projection carries, and it earns the bytes: without
	// it `ao run status` can name a park reason but never its cause.
	ParkCause string
}

const workItemPhaseColumns = `item_id, phase_id, attempt, thread_id,
input_envelope, output_envelope, gate_trace, intervention, narrative_path,
park_cause, provider_usage_scope_id, status, started_at, ended_at`

func scanWorkItemPhase(scanner interface{ Scan(...any) error }) (WorkItemPhase, error) {
	var phase WorkItemPhase
	var input, output, gateTrace, intervention string
	if err := scanner.Scan(
		&phase.ItemID, &phase.PhaseID, &phase.Attempt, &phase.ThreadID,
		&input, &output, &gateTrace, &intervention, &phase.NarrativePath,
		&phase.ParkCause, &phase.ProviderUsageScopeID, &phase.Status, &phase.StartedAt, &phase.EndedAt,
	); err != nil {
		return WorkItemPhase{}, err
	}
	phase.InputEnvelope = json.RawMessage(input)
	phase.OutputEnvelope = json.RawMessage(output)
	phase.GateTrace = json.RawMessage(gateTrace)
	phase.Intervention = json.RawMessage(intervention)
	return phase, nil
}

func (s *Store) CreateWorkItemPhase(phase WorkItemPhase) error {
	_, err := s.db.Exec(
		`INSERT INTO work_item_phases (`+workItemPhaseColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		phase.ItemID, phase.PhaseID, phase.Attempt, phase.ThreadID,
		jsonText(phase.InputEnvelope), jsonText(phase.OutputEnvelope),
		jsonText(phase.GateTrace), jsonText(phase.Intervention), phase.NarrativePath,
		phase.ParkCause, phase.ProviderUsageScopeID, phase.Status, phase.StartedAt, phase.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create work item phase %s/%s/%d: %w", phase.ItemID, phase.PhaseID, phase.Attempt, err)
	}
	return nil
}

// AttachWorkItemPhaseRun records the AO thread and system-owned narrative path
// after the runner creates them for a persisted phase attempt.
func (s *Store) AttachWorkItemPhaseRun(itemID, phaseID string, attempt int, threadID, narrativePath string) error {
	result, err := s.db.Exec(
		`UPDATE work_item_phases SET thread_id = ?, narrative_path = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?`,
		threadID, narrativePath, itemID, phaseID, attempt,
	)
	if err != nil {
		return fmt.Errorf("store: attach work item phase run %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: attach work item phase run %s/%s/%d", itemID, phaseID, attempt))
}

// CompleteWorkItemPhase settles one attempt. `parkCause` is the engine's own
// account of an engine-diagnosed park and is empty for every other settlement —
// it is written unconditionally rather than only when non-empty, so a reopened
// attempt that settles a second time cannot inherit the first park's cause.
func (s *Store) CompleteWorkItemPhase(itemID, phaseID string, attempt int, outputEnvelope, gateTrace json.RawMessage, status, parkCause string, providerUsageScopeID WorkflowProviderUsageScopeID, endedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE work_item_phases
		 SET output_envelope = ?, gate_trace = ?, park_cause = ?, provider_usage_scope_id = ?, status = ?, ended_at = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?`,
		jsonText(outputEnvelope), jsonText(gateTrace), parkCause, providerUsageScopeID, status, endedAt,
		itemID, phaseID, attempt,
	)
	if err != nil {
		return fmt.Errorf("store: complete work item phase %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: complete work item phase %s/%s/%d", itemID, phaseID, attempt))
}

// ReopenWorkItemPhase returns a settled attempt to running. It backs fan-out
// unit recovery, where repairing one unit continues the attempt its siblings
// already produced results for instead of superseding it. The output envelope
// and gate trace are cleared because the reopened attempt has not produced them
// yet — leaving the parked ones would make an unfinished attempt look decided.
// The park cause goes with them for the same reason: the attempt is running
// again, so the last park is no longer why it rests. started_at is untouched: it
// is the same attempt, and attempt ordering is how phase history is read back.
func (s *Store) ReopenWorkItemPhase(itemID, phaseID string, attempt int) error {
	result, err := s.db.Exec(
		`UPDATE work_item_phases
		 SET status = 'running', output_envelope = '', gate_trace = '',
		     park_cause = '', provider_usage_scope_id = 0, ended_at = 0
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?`,
		itemID, phaseID, attempt,
	)
	if err != nil {
		return fmt.Errorf("store: reopen work item phase %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: reopen work item phase %s/%s/%d", itemID, phaseID, attempt))
}

func (s *Store) ListWorkItemPhases(itemID string) ([]WorkItemPhase, error) {
	rows, err := s.reader().Query(
		`SELECT `+workItemPhaseColumns+` FROM work_item_phases
		 WHERE item_id = ? ORDER BY started_at ASC, phase_id ASC, attempt ASC`, itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item phases %s: %w", itemID, err)
	}
	defer rows.Close()
	phases := make([]WorkItemPhase, 0)
	for rows.Next() {
		phase, err := scanWorkItemPhase(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list work item phases %s: scan: %w", itemID, err)
		}
		phases = append(phases, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item phases %s: iterate: %w", itemID, err)
	}
	return phases, nil
}

func (s *Store) ListWorkItemPhaseTimeline(itemID string) ([]WorkItemPhaseTimeline, error) {
	rows, err := s.reader().Query(
		`SELECT item_id, phase_id, attempt, thread_id, output_envelope,
		 park_cause, provider_usage_scope_id, status, started_at, ended_at
		 FROM work_item_phases
		 WHERE item_id = ? ORDER BY started_at ASC, phase_id ASC, attempt ASC`, itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item phase timeline %s: %w", itemID, err)
	}
	defer rows.Close()
	timeline := make([]WorkItemPhaseTimeline, 0)
	for rows.Next() {
		var phase WorkItemPhaseTimeline
		var output string
		if err := rows.Scan(
			&phase.ItemID, &phase.PhaseID, &phase.Attempt, &phase.ThreadID,
			&output, &phase.ParkCause, &phase.ProviderUsageScopeID, &phase.Status, &phase.StartedAt, &phase.EndedAt,
		); err != nil {
			return nil, fmt.Errorf("store: list work item phase timeline %s: scan: %w", itemID, err)
		}
		phase.OutputEnvelope = json.RawMessage(output)
		timeline = append(timeline, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item phase timeline %s: iterate: %w", itemID, err)
	}
	return timeline, nil
}

// ListWorkItemPhaseProvenance answers "what ran this attempt, and how did its
// gate decide". The model settings are joined from the attempt's thread row
// because that row is where they were RESOLVED — the definition authors a
// model and an effort tier, and coercion against the model's catalog happens
// once, at thread creation — so reading them anywhere else would report what
// was asked for rather than what ran. A LEFT JOIN because an attempt need not
// have a thread: a tool-driver phase runs a command, and an attempt that has
// not started yet has none.
func (s *Store) ListWorkItemPhaseProvenance(itemID string) ([]WorkItemPhaseProvenance, error) {
	rows, err := s.reader().Query(
		`SELECT p.phase_id, p.attempt, p.status, p.thread_id, p.gate_trace, p.park_cause,
		 COALESCE(t.provider, ''), COALESCE(t.model, ''), COALESCE(t.reasoning_effort, '')
		 FROM work_item_phases AS p
		 LEFT JOIN threads AS t ON t.id = p.thread_id
		 WHERE p.item_id = ?
		 ORDER BY p.started_at ASC, p.phase_id ASC, p.attempt ASC`, itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item phase provenance %s: %w", itemID, err)
	}
	defer rows.Close()
	provenance := make([]WorkItemPhaseProvenance, 0)
	for rows.Next() {
		var phase WorkItemPhaseProvenance
		var gateTrace string
		if err := rows.Scan(
			&phase.PhaseID, &phase.Attempt, &phase.Status, &phase.ThreadID, &gateTrace,
			&phase.ParkCause, &phase.Provider, &phase.Model, &phase.Effort,
		); err != nil {
			return nil, fmt.Errorf("store: list work item phase provenance %s: scan: %w", itemID, err)
		}
		phase.GateTrace = json.RawMessage(gateTrace)
		provenance = append(provenance, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item phase provenance %s: iterate: %w", itemID, err)
	}
	return provenance, nil
}

func (s *Store) ListWorkItemPhaseContexts(itemID string) ([]WorkItemPhaseContext, error) {
	rows, err := s.reader().Query(
		`SELECT phase_id, attempt, status, output_envelope, gate_trace, intervention
		 FROM work_item_phases
		 WHERE item_id = ? ORDER BY started_at ASC, phase_id ASC, attempt ASC`, itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item phase contexts %s: %w", itemID, err)
	}
	defer rows.Close()
	contexts := make([]WorkItemPhaseContext, 0)
	for rows.Next() {
		var context WorkItemPhaseContext
		var output, gateTrace, intervention string
		if err := rows.Scan(
			&context.PhaseID, &context.Attempt, &context.Status,
			&output, &gateTrace, &intervention,
		); err != nil {
			return nil, fmt.Errorf("store: list work item phase contexts %s: scan: %w", itemID, err)
		}
		context.OutputEnvelope = json.RawMessage(output)
		context.GateTrace = json.RawMessage(gateTrace)
		context.Intervention = json.RawMessage(intervention)
		contexts = append(contexts, context)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work item phase contexts %s: iterate: %w", itemID, err)
	}
	return contexts, nil
}

// GetLatestWorkItemPhase returns only the newest attempt for attention-state
// summaries. It avoids materializing a run's full envelope history on the
// workflow engine's synchronous event path.
func (s *Store) GetLatestWorkItemPhase(itemID string) (WorkItemPhase, bool, error) {
	phase, err := scanWorkItemPhase(s.reader().QueryRow(
		`SELECT `+workItemPhaseColumns+` FROM work_item_phases
		 WHERE item_id = ? ORDER BY started_at DESC, phase_id DESC, attempt DESC LIMIT 1`, itemID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemPhase{}, false, nil
	}
	if err != nil {
		return WorkItemPhase{}, false, fmt.Errorf("store: get latest work item phase %s: %w", itemID, err)
	}
	return phase, true, nil
}

// GetWorkItemPhaseByThread resolves the phase attempt an AO thread is running.
// A phase thread is reused across attempts of the same phase, so the newest row
// is the answer; what the caller needs from it — which (item, phase) the thread
// belongs to — is identical on every attempt.
func (s *Store) GetWorkItemPhaseByThread(threadID string) (WorkItemPhase, bool, error) {
	if threadID == "" {
		return WorkItemPhase{}, false, nil
	}
	phase, err := scanWorkItemPhase(s.reader().QueryRow(
		`SELECT `+workItemPhaseColumns+` FROM work_item_phases
		 WHERE thread_id = ? ORDER BY started_at DESC, attempt DESC LIMIT 1`,
		threadID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemPhase{}, false, nil
	}
	if err != nil {
		return WorkItemPhase{}, false, fmt.Errorf("store: get work item phase for thread %s: %w", threadID, err)
	}
	return phase, true, nil
}

// GetCurrentWorkItemPhase returns the highest attempt number for a phase.
func (s *Store) GetCurrentWorkItemPhase(itemID, phaseID string) (WorkItemPhase, error) {
	phase, err := scanWorkItemPhase(s.reader().QueryRow(
		`SELECT `+workItemPhaseColumns+` FROM work_item_phases
		 WHERE item_id = ? AND phase_id = ? ORDER BY attempt DESC LIMIT 1`,
		itemID, phaseID,
	))
	if err != nil {
		return WorkItemPhase{}, fmt.Errorf("store: get current work item phase %s/%s: %w", itemID, phaseID, err)
	}
	return phase, nil
}

func (s *Store) UpdateWorkItemPhaseIntervention(itemID, phaseID string, attempt int, intervention json.RawMessage) error {
	result, err := s.db.Exec(
		`UPDATE work_item_phases SET intervention = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?`,
		jsonText(intervention), itemID, phaseID, attempt,
	)
	if err != nil {
		return fmt.Errorf("store: update work item phase intervention %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item phase intervention %s/%s/%d", itemID, phaseID, attempt))
}
