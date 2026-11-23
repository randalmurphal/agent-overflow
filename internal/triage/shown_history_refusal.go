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
// last turn, when err is a refusal. told reports that the user was told of
// the refusal: its row was saved or already there, or its own refused write
// reached the unsaved-rows observer (persistItem). It is false for any
// other error, which is the caller's, and when the last turn could not be
// read, so no row was attempted.
func (r *Router) reportShownHistoryRefusal(threadID string, err error) (told bool, reportErr error) {
	if !store.IsShownHistoryRefusal(err) {
		return false, nil
	}
	turnIndex, lookupErr := r.store.LastTurnIndex(threadID)
	if lookupErr != nil {
		return false, fmt.Errorf("report the refused provider report on %s: %w", threadID, lookupErr)
	}
	if persistErr := r.persistProviderErrorItem(threadID, turnIndex, ShownHistoryRefusalSummary(err), nil, "", "", time.Now().UnixMilli()); persistErr != nil {
		return true, fmt.Errorf("report the refused provider report on %s: %w", threadID, persistErr)
	}
	return true, nil
}

// providerWriteFailed records the failed write of a provider report its
// caller goes on without: the log keeps the error, and a refusal reaches
// the thread (reportShownHistoryRefusal). It reports whether the user was
// told of err.
func (r *Router) providerWriteFailed(threadID, what string, err error) (told bool) {
	log.Printf("triage: %s: %v", what, err)
	told, reportErr := r.reportShownHistoryRefusal(threadID, err)
	if reportErr != nil {
		log.Printf("triage: %s: %v", what, reportErr)
	}
	return told
}
