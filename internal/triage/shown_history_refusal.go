package triage

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/store"
)

// A write of a provider report to a row, payload or turn a pointer fork
// shows gives the forks their own copy first
// (docs/architecture/sqlite-store.md#ownership). The store still refuses
// such a write when that copy needs a fork level past the depth cap, or
// when a guard refuses it (store.IsShownHistoryRefusal). The report is
// then not recorded, and its thread gets an error row that says so.

// ShownHistoryRefusalSummary is the thread error row of a provider report
// the store refused.
func ShownHistoryRefusalSummary(err error) string {
	if errors.Is(err, store.ErrForkChainTooDeep) {
		return "A provider report was not recorded: a fork of this thread reads through too many levels of forks to keep the history it shows."
	}
	return "A provider report was not recorded: it would change history a fork of this thread shows."
}

// reportShownHistoryRefusal gives threadID the error row of err, at its
// last turn, when err is a refusal. Any other error is the caller's.
func (r *Router) reportShownHistoryRefusal(threadID string, err error) error {
	if !store.IsShownHistoryRefusal(err) {
		return nil
	}
	turnIndex, lookupErr := r.store.LastTurnIndex(threadID)
	if lookupErr != nil {
		return fmt.Errorf("report the refused provider report on %s: %w", threadID, lookupErr)
	}
	if reportErr := r.persistProviderErrorItem(threadID, turnIndex, ShownHistoryRefusalSummary(err), nil, "", "", time.Now().UnixMilli()); reportErr != nil {
		return fmt.Errorf("report the refused provider report on %s: %w", threadID, reportErr)
	}
	return nil
}

// providerWriteFailed records the failed write of a provider report its
// caller goes on without: the log keeps the error, and a refusal reaches
// the thread (reportShownHistoryRefusal).
func (r *Router) providerWriteFailed(threadID, what string, err error) {
	log.Printf("triage: %s: %v", what, err)
	if reportErr := r.reportShownHistoryRefusal(threadID, err); reportErr != nil {
		log.Printf("triage: %s: %v", what, reportErr)
	}
}

// TurnSettleFailureSummary is the thread error row of a turn whose end was
// not recorded.
func TurnSettleFailureSummary(err error) string {
	return "This turn's end could not be recorded, so it shows as running until the app restarts: " + err.Error()
}

// turnSettleFailed reports a failed write of a turn's end: the turn row
// stays open, which every reader takes for a running turn until the next
// boot's sweep (store/turn_newest.go), so the thread gets an error row at
// that turn whatever the failure, the refusal's own row for a refusal.
// A turn with no row (a completion with no turn start, settleTurnRow) is
// left open nowhere, so only the log keeps it. The row's dedup
// (persistProviderErrorItem) keeps a repeated report of one turn to one
// row.
func (r *Router) turnSettleFailed(threadID string, turnIndex int, turnID string, err error, now int64) {
	what := "settle turn " + turnID
	log.Printf("triage: %s: %v", what, err)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	summary := TurnSettleFailureSummary(err)
	if store.IsShownHistoryRefusal(err) {
		summary = ShownHistoryRefusalSummary(err)
	}
	if reportErr := r.persistProviderErrorItem(threadID, turnIndex, summary, nil, "", "", now); reportErr != nil {
		log.Printf("triage: %s: report: %v", what, reportErr)
	}
}
