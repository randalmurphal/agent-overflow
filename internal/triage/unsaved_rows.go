package triage

import "agent-overflow/internal/store"

// Rows the user must see that the store refused.
//
// An error or notification row reaches the user through its item upsert,
// which persistItemWithEmit sends only after the write succeeds. When the
// database refuses the write (disk full, read-only filesystem, I/O error),
// the funnel hands the row to the unsaved row observer instead, and the app
// shows it live (internal/app/app_unsaved_rows.go). When a later write of a
// row with the same id succeeds, the funnel tells the observer, so the row
// stops reading as unsaved. Every producer of those rows writes through the
// funnel, so none has to remember this.

// UnsavedRow is a refused error or notification row, as the user sees it.
type UnsavedRow struct {
	ThreadID string
	// ItemID is the id the row would have had. A refused retry of the
	// same row (a replayed session death, a background bell's next state)
	// carries the same id.
	ItemID   string
	Severity string
	Summary  string
}

// UnsavedRowObserver is told about error and notification row writes.
type UnsavedRowObserver interface {
	// RowRefused receives a row the store refused, with the write error.
	RowRefused(row UnsavedRow, err error)
	// RowSaved receives the id of a row the store just wrote. Called for
	// every saved error or notification row, so it must be cheap when
	// the id was never refused.
	RowSaved(threadID, itemID string)
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
	if r.unsavedRows == nil || item.ThreadID == "" {
		return
	}
	severity := unsavedRowSeverity(item.Kind)
	if severity == "" {
		return
	}
	r.unsavedRows.RowRefused(UnsavedRow{
		ThreadID: item.ThreadID,
		ItemID:   item.ID,
		Severity: severity,
		Summary:  item.Summary,
	}, err)
}

// reportSavedRow tells the observer a saved error or notification row's
// id, so an earlier refusal of it is withdrawn. Nothing for other kinds.
func (r *Router) reportSavedRow(item store.Item) {
	if r.unsavedRows == nil || item.ThreadID == "" || unsavedRowSeverity(item.Kind) == "" {
		return
	}
	r.unsavedRows.RowSaved(item.ThreadID, item.ID)
}
