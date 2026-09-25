package triage

import (
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
