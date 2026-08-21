package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// provider_thread_cost accessors — the PROVIDER's own cumulative cost estimate
// for one thread (migration v66, provider-thread identity added in v68).
//
// This table answers a different question from `usage_ledger`. The ledger
// records per-turn token DELTAS and AO prices them from its own rate table at
// query time; this records what the provider says the WHOLE THREAD has cost so
// far, in its own words. Codex >= 0.148 is the only producer today
// (`account/usage/read {threadId}` → `threadUsage.estimatedUsageUsdMicros`).
//
// The two never mix. A cumulative figure appended to the ledger would be
// summed on top of the turns it already covers, inflating every dollar total
// in the app — the workflow budget check included. Keeping it in a
// thread-grained table is what makes "prefer the provider's number when it
// exists, otherwise price the tokens ourselves" a CHOICE at the display
// boundary rather than an arithmetic hazard everywhere else.

// ProviderThreadCostSourceEstimate is the only `cost_source` the table admits
// today: a figure the provider itself computed and labels an estimate.
//
// It is deliberately a THIRD value, disjoint from `usage_ledger.cost_source`'s
// 'wire' and 'none'. Those two describe a per-turn row; this describes a
// thread total, and conflating the vocabularies would let a reader believe a
// ledger row could carry it.
const ProviderThreadCostSourceEstimate = "provider-estimate"

// ProviderThreadCost is one thread's provider-reported cumulative cost.
type ProviderThreadCost struct {
	ThreadID string `json:"threadId"`
	// SessionRef is the PROVIDER thread the figure describes — for Codex, the
	// app-server root thread id the usage read answered about. The row is keyed
	// by the AO thread, but a thread can be repointed at a different provider
	// thread (a rollback that forks) or at none at all (a rollback to turn 0),
	// and the lifetime total of the thread it USED to be is not an answer about
	// the thread it is now. Every read compares this against the thread's
	// current session ref, so a row left behind by a failed delete is ignored
	// rather than served.
	SessionRef string `json:"sessionRef"`
	Provider   string `json:"provider"`
	// CostSource is the provenance label, currently always
	// ProviderThreadCostSourceEstimate. Present on the wire so a surface can
	// say WHOSE number it is showing rather than presenting a provider
	// estimate and an AO rate-table estimate as the same kind of thing.
	CostSource string `json:"costSource"`
	// CostUSDMicros is millionths of a US dollar, stored exactly as the
	// provider reported it.
	CostUSDMicros int64 `json:"costUsdMicros"`
	// CreditsMicros is the provider's own credit unit, kept because it is the
	// figure Codex always supplies (the USD one is optional). Nothing renders
	// it yet; it exists so a credits-only account is diagnosable from the row
	// rather than only from a log line.
	CreditsMicros int64 `json:"creditsMicros"`
	UpdatedAt     int64 `json:"updatedAt"`
}

// CostUSD converts the stored micros to dollars.
func (c ProviderThreadCost) CostUSD() float64 { return float64(c.CostUSDMicros) / 1e6 }

// PutProviderThreadCost records (or replaces) a thread's provider-reported
// cost. Last write wins by construction: every read of the provider restates
// the same cumulative total, so there is no history here to preserve and no
// ordering to enforce — a later read is simply a better answer.
//
// SessionRef is REQUIRED. A row that cannot name the provider thread it
// describes can never be checked against the thread's current one, which is
// the whole safety property; storing it anyway would reintroduce the
// unattributed row v68 exists to remove. An empty one is a caller bug, not a
// race, so it is an error rather than a silent skip.
//
// A thread row that no longer exists is NOT an error, and not an FK failure
// either: the estimate arrives asynchronously after a turn settles, so a
// thread deleted in that window is an ordinary race. The existence check is
// in-statement (the same shape PutEditFileSnapshot uses) so the race writes
// nothing and reports nothing, rather than surfacing a constraint violation a
// caller would have to pattern-match to ignore.
func (s *Store) PutProviderThreadCost(cost ProviderThreadCost) error {
	threadID := strings.TrimSpace(cost.ThreadID)
	if threadID == "" {
		return fmt.Errorf("store: put provider thread cost: empty thread id")
	}
	sessionRef := strings.TrimSpace(cost.SessionRef)
	if sessionRef == "" {
		return fmt.Errorf("store: put provider thread cost %s: empty session ref", threadID)
	}
	if cost.CostSource == "" {
		cost.CostSource = ProviderThreadCostSourceEstimate
	}
	_, err := s.db.Exec(
		`INSERT INTO provider_thread_cost
		 (thread_id, session_ref, provider, cost_source, cost_usd_micros, credits_micros, updated_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?
		 WHERE EXISTS(SELECT 1 FROM threads WHERE id = ?)
		 ON CONFLICT(thread_id) DO UPDATE SET
		     session_ref     = excluded.session_ref,
		     provider        = excluded.provider,
		     cost_source     = excluded.cost_source,
		     cost_usd_micros = excluded.cost_usd_micros,
		     credits_micros  = excluded.credits_micros,
		     updated_at      = excluded.updated_at`,
		threadID, sessionRef, cost.Provider, cost.CostSource,
		cost.CostUSDMicros, cost.CreditsMicros, cost.UpdatedAt,
		threadID,
	)
	if err != nil {
		return fmt.Errorf("store: put provider thread cost %s: %w", threadID, err)
	}
	return nil
}

// GetProviderThreadCost reads one thread's provider-reported cost, and only
// when the stored row still describes the provider thread the AO thread points
// at. The bool is false when no provider has priced this thread — the ordinary
// case for every Claude thread, every Codex thread on a pre-0.148 binary, and
// every Codex thread whose account has no billing route — and equally false
// when a row exists but names a DIFFERENT provider thread.
//
// The identity comparison is the invalidation. A rollback that forks into a new
// provider thread, or one that clears the session ref entirely, moves
// `threads.session_ref` away from what the row was read under; from that moment
// the row is unreadable, whether or not the delete that follows it ever lands.
// That makes a failed delete a wasted row instead of a wrong number, which is a
// property of the schema rather than of any process staying alive.
//
// The join also covers the deleted-thread case for free: no thread row, no
// match, no answer to give.
func (s *Store) GetProviderThreadCost(threadID string) (ProviderThreadCost, bool, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ProviderThreadCost{}, false, nil
	}
	var cost ProviderThreadCost
	err := s.reader().QueryRow(
		`SELECT c.thread_id, c.session_ref, c.provider, c.cost_source,
		        c.cost_usd_micros, c.credits_micros, c.updated_at
		 FROM provider_thread_cost c
		 JOIN threads t ON t.id = c.thread_id
		 WHERE c.thread_id = ? AND c.session_ref = COALESCE(t.session_ref, '')`, threadID,
	).Scan(
		&cost.ThreadID, &cost.SessionRef, &cost.Provider, &cost.CostSource,
		&cost.CostUSDMicros, &cost.CreditsMicros, &cost.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderThreadCost{}, false, nil
	}
	if err != nil {
		return ProviderThreadCost{}, false, fmt.Errorf("store: get provider thread cost %s: %w", threadID, err)
	}
	return cost, true, nil
}

// DeleteProviderThreadCost drops one thread's provider-reported cost.
//
// This is housekeeping, not the invalidation. The row is keyed by the AO thread
// id but DESCRIBES the provider thread it was read from, and since v68 it says
// so: once a rollback repoints the thread at a different provider thread (or at
// none), GetProviderThreadCost's identity comparison already refuses to serve
// it. Deleting it merely reclaims a row nothing can ever match again, so a
// failure here costs storage rather than correctness.
//
// A missing row is not an error: most threads never had one.
func (s *Store) DeleteProviderThreadCost(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM provider_thread_cost WHERE thread_id = ?`, threadID); err != nil {
		return fmt.Errorf("store: delete provider thread cost %s: %w", threadID, err)
	}
	return nil
}
