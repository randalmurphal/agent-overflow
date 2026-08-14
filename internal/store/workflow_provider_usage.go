package store

import (
	"fmt"
	"math"
	"strings"
)

// WorkflowProviderUsageScopeID is deliberately distinct from timestamps: the
// completion APIs accept both, and swapping two plain int64 values would
// silently corrupt provenance.
type WorkflowProviderUsageScopeID int64

// WorkflowProviderUsageScope is the durable identity of one provider account
// credential generation. It is failure provenance and notification
// correlation only: no send path consults it for admission.
type WorkflowProviderUsageScope struct {
	ID                   WorkflowProviderUsageScopeID
	Provider             string
	AccountID            string
	CredentialGeneration uint64
	FirstSeenAt          int64
	LastSeenAt           int64
}

// WorkflowProviderUsageAttentionClaim reserves one notification generation
// for a provider-account scope and the conversation watching affected runs.
// Token makes delivery settlement compare-and-set safe against a resume that
// re-arms attention while the message is still queued.
type WorkflowProviderUsageAttentionClaim struct {
	ScopeID    WorkflowProviderUsageScopeID
	ThreadID   string
	Generation int64
	Token      string
}

// WorkflowProviderUsageAttentionRecovery identifies one notification claim
// whose process-local delivery was lost. SourceItemID is only a preference:
// the source may have stopped being affected while another run under the same
// durable scope and watching conversation remains parked.
type WorkflowProviderUsageAttentionRecovery struct {
	Claim        WorkflowProviderUsageAttentionClaim
	SourceItemID string
}

// OpenWorkflowProviderUsageScope records a typed usage refusal against the
// exact provider-account generation used for the send and returns its stable
// scope id. Repeated refusals only advance last_seen_at.
func (s *Store) OpenWorkflowProviderUsageScope(provider, accountID string, credentialGeneration uint64, seenAt int64) (WorkflowProviderUsageScopeID, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return 0, fmt.Errorf("store: open workflow provider usage scope: provider is required")
	}
	if credentialGeneration > math.MaxInt64 {
		return 0, fmt.Errorf("store: open workflow provider usage scope: credential generation %d exceeds SQLite INTEGER", credentialGeneration)
	}
	var id WorkflowProviderUsageScopeID
	err := s.db.QueryRow(
		`INSERT INTO workflow_provider_usage_scopes
		 (provider, account_id, credential_generation, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(provider, account_id, credential_generation) DO UPDATE
		 SET last_seen_at = MAX(workflow_provider_usage_scopes.last_seen_at, excluded.last_seen_at)
		 RETURNING id`,
		provider, accountID, int64(credentialGeneration), seenAt, seenAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: open workflow provider usage scope %s/%s/%d: %w", provider, accountID, credentialGeneration, err)
	}
	return id, nil
}

// GetWorkflowProviderUsageScope returns one recorded scope.
func (s *Store) GetWorkflowProviderUsageScope(id WorkflowProviderUsageScopeID) (WorkflowProviderUsageScope, error) {
	var scope WorkflowProviderUsageScope
	var generation int64
	err := s.reader().QueryRow(
		`SELECT id, provider, account_id, credential_generation, first_seen_at, last_seen_at
		 FROM workflow_provider_usage_scopes WHERE id = ?`, id,
	).Scan(&scope.ID, &scope.Provider, &scope.AccountID, &generation, &scope.FirstSeenAt, &scope.LastSeenAt)
	if err != nil {
		return WorkflowProviderUsageScope{}, fmt.Errorf("store: get workflow provider usage scope %d: %w", id, err)
	}
	scope.CredentialGeneration = uint64(generation)
	return scope, nil
}

// ClaimWorkflowProviderUsageAttention reserves the current attention
// generation. False means this watcher already has either a delivered message
// or a live queued claim for the same generation.
func (s *Store) ClaimWorkflowProviderUsageAttention(scopeID WorkflowProviderUsageScopeID, threadID, sourceItemID, token string, now int64) (WorkflowProviderUsageAttentionClaim, bool, error) {
	threadID = strings.TrimSpace(threadID)
	token = strings.TrimSpace(token)
	if scopeID <= 0 || threadID == "" || strings.TrimSpace(sourceItemID) == "" || token == "" {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: scope, thread, source item, and token are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var scopeExists int
	if err := tx.QueryRow(`SELECT 1 FROM workflow_provider_usage_scopes WHERE id = ?`, scopeID).Scan(&scopeExists); err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: load scope %d: %w", scopeID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO workflow_provider_usage_attention
		 (scope_id, thread_id, generation, delivered_generation, queued_generation,
		  queued_token, source_item_id, updated_at)
		 VALUES (?, ?, 1, 0, 0, '', '', ?)
		 ON CONFLICT(scope_id, thread_id) DO NOTHING`, scopeID, threadID, now,
	); err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: create watcher: %w", err)
	}
	var generation, delivered, queued int64
	if err := tx.QueryRow(
		`SELECT generation, delivered_generation, queued_generation
		 FROM workflow_provider_usage_attention WHERE scope_id = ? AND thread_id = ?`,
		scopeID, threadID,
	).Scan(&generation, &delivered, &queued); err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: read watcher: %w", err)
	}
	claim := WorkflowProviderUsageAttentionClaim{ScopeID: scopeID, ThreadID: threadID, Generation: generation, Token: token}
	if delivered == generation || queued == generation {
		if err := tx.Commit(); err != nil {
			return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: commit suppression: %w", err)
		}
		return claim, false, nil
	}
	result, err := tx.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET queued_generation = generation, queued_token = ?, source_item_id = ?, updated_at = ?
		 WHERE scope_id = ? AND thread_id = ? AND generation = ?
		   AND delivered_generation <> generation AND queued_generation = 0`,
		token, sourceItemID, now, scopeID, threadID, generation,
	)
	if err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: reserve: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkflowProviderUsageAttentionClaim{}, false, fmt.Errorf("store: claim workflow provider usage attention: commit: %w", err)
	}
	return claim, affected == 1, nil
}

// PromoteWorkflowProviderUsageAttention marks a queued claim delivered only
// if no action re-armed its watcher in the meantime.
func (s *Store) PromoteWorkflowProviderUsageAttention(claim WorkflowProviderUsageAttentionClaim, now int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET delivered_generation = generation, queued_generation = 0,
		     queued_token = '', source_item_id = '', updated_at = ?
		 WHERE scope_id = ? AND thread_id = ? AND generation = ?
		   AND queued_generation = ? AND queued_token = ?`,
		now, claim.ScopeID, claim.ThreadID, claim.Generation, claim.Generation, claim.Token,
	)
	if err != nil {
		return false, fmt.Errorf("store: promote workflow provider usage attention: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: promote workflow provider usage attention: rows affected: %w", err)
	}
	return affected == 1, nil
}

// ReleaseWorkflowProviderUsageAttention drops a claim whose message did not
// reach a durable delivery point. The generation remains unnotified, so the
// next affected park may claim it again.
func (s *Store) ReleaseWorkflowProviderUsageAttention(claim WorkflowProviderUsageAttentionClaim, now int64) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET queued_generation = 0, queued_token = '', source_item_id = '', updated_at = ?
		 WHERE scope_id = ? AND thread_id = ? AND generation = ?
		   AND queued_generation = ? AND queued_token = ?`,
		now, claim.ScopeID, claim.ThreadID, claim.Generation, claim.Generation, claim.Token,
	)
	if err != nil {
		return false, fmt.Errorf("store: release workflow provider usage attention: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: release workflow provider usage attention: rows affected: %w", err)
	}
	return affected == 1, nil
}

// RearmWorkflowProviderUsageAttention advances every provider scope watched by
// a conversation. It never changes provider admission; it only makes a later
// park worth announcing after somebody started or resumed work.
func (s *Store) RearmWorkflowProviderUsageAttention(threadID string, now int64) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET generation = generation + 1, queued_generation = 0,
		     queued_token = '', source_item_id = '', updated_at = ?
		 WHERE thread_id = ?`, now, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: rearm workflow provider usage attention for thread %s: %w", threadID, err)
	}
	return nil
}

// ReleaseQueuedWorkflowProviderUsageAttentionForThread clears claims whose
// in-memory delivery queue is being destroyed with a provider session.
func (s *Store) ReleaseQueuedWorkflowProviderUsageAttentionForThread(threadID string, now int64) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET queued_generation = 0, queued_token = '', source_item_id = '', updated_at = ?
		 WHERE thread_id = ? AND queued_generation > 0`, now, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: release queued workflow provider usage attention for thread %s: %w", threadID, err)
	}
	return nil
}

// ReclaimQueuedWorkflowProviderUsageAttention transfers queued claims whose
// process-local delivery was lost to this process without ever clearing their
// durable reservation. A second crash can therefore reclaim the same claim
// again rather than landing between reset and redelivery. The app reselects a
// currently affected source because the original may have resolved meanwhile.
func (s *Store) ReclaimQueuedWorkflowProviderUsageAttention(tokenPrefix string, now int64) ([]WorkflowProviderUsageAttentionRecovery, error) {
	tokenPrefix = strings.TrimSpace(tokenPrefix)
	if tokenPrefix == "" {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: token prefix is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET queued_token = ? || ':' || scope_id || ':' || thread_id, updated_at = ?
		 WHERE queued_generation > 0`, tokenPrefix, now,
	); err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: transfer claims: %w", err)
	}
	rows, err := tx.Query(
		`SELECT scope_id, thread_id, generation, queued_token, source_item_id
		 FROM workflow_provider_usage_attention
		 WHERE queued_generation > 0 AND source_item_id <> ''
		 ORDER BY scope_id, thread_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: list claims: %w", err)
	}
	var recoveries []WorkflowProviderUsageAttentionRecovery
	for rows.Next() {
		var recovery WorkflowProviderUsageAttentionRecovery
		if err := rows.Scan(
			&recovery.Claim.ScopeID, &recovery.Claim.ThreadID, &recovery.Claim.Generation,
			&recovery.Claim.Token, &recovery.SourceItemID,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: scan claim: %w", err)
		}
		recoveries = append(recoveries, recovery)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: close claims: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: iterate claims: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: reclaim queued workflow provider usage attention: commit: %w", err)
	}
	return recoveries, nil
}

// ReassignWorkflowProviderUsageAttentionSource moves an owned queued claim to
// the currently affected run selected for recovery. The compare-and-set keeps
// a concurrent action or later recovery from being overwritten.
func (s *Store) ReassignWorkflowProviderUsageAttentionSource(claim WorkflowProviderUsageAttentionClaim, sourceItemID string, now int64) (bool, error) {
	sourceItemID = strings.TrimSpace(sourceItemID)
	if claim.ScopeID <= 0 || claim.ThreadID == "" || claim.Generation <= 0 || claim.Token == "" || sourceItemID == "" {
		return false, fmt.Errorf("store: reassign workflow provider usage attention source: complete claim and source item are required")
	}
	result, err := s.db.Exec(
		`UPDATE workflow_provider_usage_attention
		 SET source_item_id = ?, updated_at = ?
		 WHERE scope_id = ? AND thread_id = ? AND generation = ?
		   AND queued_generation = ? AND queued_token = ?`,
		sourceItemID, now, claim.ScopeID, claim.ThreadID, claim.Generation, claim.Generation, claim.Token,
	)
	if err != nil {
		return false, fmt.Errorf("store: reassign workflow provider usage attention source: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: reassign workflow provider usage attention source: rows affected: %w", err)
	}
	return affected == 1, nil
}

// ListWorkflowProviderUsageAffectedItemIDs returns the narrow set of parked
// runs that may currently be wholly attributable to scopeID. A unit-failed run
// is intentionally only a candidate here: the app verifies that every failed
// unit has the same scope before coalescing its attention.
func (s *Store) ListWorkflowProviderUsageAffectedItemIDs(scopeID WorkflowProviderUsageScopeID) ([]string, error) {
	if scopeID <= 0 {
		return nil, fmt.Errorf("store: list workflow provider usage affected items: scope must be positive")
	}
	rows, err := s.reader().Query(
		`SELECT w.id
		 FROM work_items AS w
		 JOIN work_item_phases AS phase ON phase.rowid = (
		     SELECT latest.rowid FROM work_item_phases AS latest
		      WHERE latest.item_id = w.id
		      ORDER BY latest.started_at DESC, latest.rowid DESC
		      LIMIT 1
		 )
		 WHERE w.state = 'needs-human'
		   AND (
		       (w.reason = 'provider-usage-limited' AND phase.provider_usage_scope_id = ?)
		       OR
		       (w.reason = 'unit-failed' AND EXISTS (
		           SELECT 1 FROM work_item_units AS unit
		            WHERE unit.item_id = w.id
		              AND unit.phase_id = phase.phase_id
		              AND unit.attempt = phase.attempt
		              AND unit.status = 'failed'
		              AND unit.provider_usage_scope_id = ?
		       ))
		   )
		 ORDER BY w.created_at, w.id`, scopeID, scopeID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list workflow provider usage affected items for scope %d: %w", scopeID, err)
	}
	defer rows.Close()
	var itemIDs []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			return nil, fmt.Errorf("store: list workflow provider usage affected items for scope %d: scan: %w", scopeID, err)
		}
		itemIDs = append(itemIDs, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list workflow provider usage affected items for scope %d: iterate: %w", scopeID, err)
	}
	return itemIDs, nil
}
