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
	Status         string          `json:"status"`
	StartedAt      int64           `json:"startedAt"`
	EndedAt        int64           `json:"endedAt,omitempty"`
}

const workItemPhaseColumns = `item_id, phase_id, attempt, thread_id,
input_envelope, output_envelope, gate_trace, intervention, narrative_path,
status, started_at, ended_at`

func scanWorkItemPhase(scanner interface{ Scan(...any) error }) (WorkItemPhase, error) {
	var phase WorkItemPhase
	var input, output, gateTrace, intervention string
	if err := scanner.Scan(
		&phase.ItemID, &phase.PhaseID, &phase.Attempt, &phase.ThreadID,
		&input, &output, &gateTrace, &intervention, &phase.NarrativePath,
		&phase.Status, &phase.StartedAt, &phase.EndedAt,
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
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		phase.ItemID, phase.PhaseID, phase.Attempt, phase.ThreadID,
		jsonText(phase.InputEnvelope), jsonText(phase.OutputEnvelope),
		jsonText(phase.GateTrace), jsonText(phase.Intervention), phase.NarrativePath,
		phase.Status, phase.StartedAt, phase.EndedAt,
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

func (s *Store) CompleteWorkItemPhase(itemID, phaseID string, attempt int, outputEnvelope, gateTrace json.RawMessage, status string, endedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE work_item_phases
		 SET output_envelope = ?, gate_trace = ?, status = ?, ended_at = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?`,
		jsonText(outputEnvelope), jsonText(gateTrace), status, endedAt,
		itemID, phaseID, attempt,
	)
	if err != nil {
		return fmt.Errorf("store: complete work item phase %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: complete work item phase %s/%s/%d", itemID, phaseID, attempt))
}

func (s *Store) ListWorkItemPhases(itemID string) ([]WorkItemPhase, error) {
	rows, err := s.db.Query(
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

// GetLatestWorkItemPhase returns only the newest attempt for attention-state
// summaries. It avoids materializing a run's full envelope history on the
// workflow engine's synchronous event path.
func (s *Store) GetLatestWorkItemPhase(itemID string) (WorkItemPhase, bool, error) {
	phase, err := scanWorkItemPhase(s.db.QueryRow(
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

// GetCurrentWorkItemPhase returns the highest attempt number for a phase.
func (s *Store) GetCurrentWorkItemPhase(itemID, phaseID string) (WorkItemPhase, error) {
	phase, err := scanWorkItemPhase(s.db.QueryRow(
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
