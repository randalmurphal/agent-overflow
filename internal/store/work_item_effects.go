package store

import (
	"encoding/json"
	"fmt"
)

// WorkItemEffect records one externally visible phase tool effect.
type WorkItemEffect struct {
	ItemID      string          `json:"itemId"`
	PhaseID     string          `json:"phaseId"`
	Tool        string          `json:"tool"`
	PayloadHash string          `json:"payloadHash"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   int64           `json:"createdAt"`
}

// RecordWorkItemEffect records an effect once. Replaying the same effect key
// is an idempotent no-op for the engine's surface-and-skip protocol.
func (s *Store) RecordWorkItemEffect(effect WorkItemEffect) error {
	_, err := s.db.Exec(
		`INSERT INTO work_item_effects
		 (item_id, phase_id, tool, payload_hash, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(item_id, phase_id, tool, payload_hash) DO NOTHING`,
		effect.ItemID, effect.PhaseID, effect.Tool, effect.PayloadHash,
		jsonText(effect.Payload), effect.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: record work item effect %s/%s/%s/%s: %w",
			effect.ItemID, effect.PhaseID, effect.Tool, effect.PayloadHash, err)
	}
	return nil
}

func (s *Store) GetWorkItemEffect(itemID, phaseID, tool, payloadHash string) (WorkItemEffect, error) {
	var effect WorkItemEffect
	var payload string
	err := s.db.QueryRow(
		`SELECT item_id, phase_id, tool, payload_hash, payload, created_at
		 FROM work_item_effects
		 WHERE item_id = ? AND phase_id = ? AND tool = ? AND payload_hash = ?`,
		itemID, phaseID, tool, payloadHash,
	).Scan(
		&effect.ItemID, &effect.PhaseID, &effect.Tool, &effect.PayloadHash,
		&payload, &effect.CreatedAt,
	)
	if err != nil {
		return WorkItemEffect{}, fmt.Errorf("store: get work item effect %s/%s/%s/%s: %w",
			itemID, phaseID, tool, payloadHash, err)
	}
	effect.Payload = json.RawMessage(payload)
	return effect, nil
}
