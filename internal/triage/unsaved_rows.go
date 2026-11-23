package triage

import (
	"database/sql"
	"errors"

	"agent-overflow/internal/store"
)

// Rows the user must see that the store refused.
//
// An error or notification row reaches the user through its item upsert,
// which persistItemWithEmit sends only after the write succeeds. When the
// database refuses the write (disk full, read-only filesystem, I/O error),
// the funnel hands the row to the unsaved row observer instead, and the app
// shows it live (internal/app/app_unsaved_rows.go). The funnel also tells
// the observer about every such row it saves, so a refused row that a later
// write saves stops reading as unsaved. Every producer of those rows writes
// through the funnel, so none has to remember this.
//
// A turn's end the store refused is told to the observer the same way, as
// an error entry of its own (reportUnsavedTurnEnd): the end is no row of
// the timeline, but the user must know that the thread's newest turn row
// still reads as running.

// UnsavedRow is an error or notification row, as the user sees it.
type UnsavedRow struct {
	ThreadID string
	// ItemID is the row's id. It does not identify the row by itself: the
	// per-turn counters behind ErrorItemID and NotificationItemID restart
	// at CleanupThread, and a provider id is reused for a row's next
	// state. A retry of the same row (a replayed session death) carries
	// the same id, severity and summary.
	ItemID   string
	Severity string
	Summary  string
}

// UnsavedRowObserver is told about error and notification row writes.
type UnsavedRowObserver interface {
	// RowRefused receives a row the store refused, with the write error.
	RowRefused(row UnsavedRow, err error)
	// RowSaved receives a row the store just wrote. Called for every
	// saved error or notification row, so it must be cheap when the row
	// was never refused.
	RowSaved(row UnsavedRow)
}

// The severities of an unsaved row: an error row, or any notification row.
const (
	UnsavedSeverityError   = "error"
	UnsavedSeverityWarning = "warning"
)

// unsavedRowSeverity answers the severity of a refused row, or "" for a
// kind the user is not told about this way.
func unsavedRowSeverity(kind string) string {
	switch kind {
	case ItemKindError, itemKindAPIError:
		return UnsavedSeverityError
	case itemKindNotification:
		return UnsavedSeverityWarning
	default:
		return ""
	}
}

// reportUnsavedRow hands a refused error or notification row to the
// observer. Other kinds are history the user is not told about one row at
// a time; their callers report the failure as before.
func (r *Router) reportUnsavedRow(item store.Item, err error) {
	if row, ok := r.observedRow(item); ok {
		r.unsavedRows.RowRefused(row, err)
	}
}

// reportSavedRow tells the observer about a saved error or notification
// row, so an earlier refusal of it is withdrawn. Nothing for other kinds.
func (r *Router) reportSavedRow(item store.Item) {
	if row, ok := r.observedRow(item); ok {
		r.unsavedRows.RowSaved(row)
	}
}

// UnsavedTurnEndSummary is the entry of a turn whose end the store
// refused. The row stays open, so every reader of the thread's newest turn
// row (the thread tools, send placement, revert) reads it as running while
// it is the newest, and the next boot's sweep settles it as interrupted.
const UnsavedTurnEndSummary = "The provider finished this turn, but its end could not be saved. " +
	"While it is the thread's latest turn, agents and tools see the thread as still running, " +
	"and after the app restarts the turn reads as interrupted."

// unsavedTurnEndRow is the observer's entry for turnID's unsaved end: one
// per turn, whichever path wrote the end and however often it failed, so a
// second failure replaces the entry rather than adding one. Like every entry
// it leaves when the user dismisses it, when the thread is deleted, or with
// the process. A later end of the same turn that saves does not withdraw
// it: successful ends are not told to the observer.
func unsavedTurnEndRow(threadID, turnID string) UnsavedRow {
	return UnsavedRow{ThreadID: threadID, ItemID: "turn-end:" + turnID, Severity: UnsavedSeverityError, Summary: UnsavedTurnEndSummary}
}

// reportUnsavedTurnEnd tells the observer that turnID's end was not saved.
// A shown-history refusal already reached the thread as its own error row
// (reportShownHistoryRefusal), and a missing row leaves nothing open, so
// neither is told again.
func (r *Router) reportUnsavedTurnEnd(threadID, turnID string, err error) {
	if r.unsavedRows == nil || threadID == "" || store.IsShownHistoryRefusal(err) || errors.Is(err, sql.ErrNoRows) {
		return
	}
	r.unsavedRows.RowRefused(unsavedTurnEndRow(threadID, turnID), err)
}

// observedRow is the item as the observer sees it; false for a kind the
// observer is not told about, or with no observer.
func (r *Router) observedRow(item store.Item) (UnsavedRow, bool) {
	severity := unsavedRowSeverity(item.Kind)
	if r.unsavedRows == nil || item.ThreadID == "" || severity == "" {
		return UnsavedRow{}, false
	}
	return UnsavedRow{ThreadID: item.ThreadID, ItemID: item.ID, Severity: severity, Summary: item.Summary}, true
}
