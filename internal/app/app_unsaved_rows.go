package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

// Thread rows AO could not save.
//
// Errors and notices reach history as rows, and triage announces a row only
// after its write succeeds. When the database refuses an error or
// notification row (disk full, read-only filesystem, I/O error), triage
// hands it here (triage/unsaved_rows.go) and it is pushed on
// `thread:unsaved_rows` instead, so the user still sees it, marked as not
// saved, with its severity and the reason. It is not history: nothing here
// is written to the store or replayed from it, it dies with the process,
// and the list is the thread's last few rows. GetThreadUnsavedRows answers
// it for a client that opens the thread later. An entry leaves when a later
// write of its row succeeds, when DismissUnsavedThreadRows names it, or with
// the thread's deletion.

// maxUnsavedThreadRows bounds the rows kept per thread. Older ones are
// dropped and counted in Omitted.
const maxUnsavedThreadRows = 5

// UnsavedThreadRow is one thread error or notice AO could not save to
// history.
type UnsavedThreadRow struct {
	// ID is the id the row would have had. A refused retry of the same
	// row replaces the entry rather than adding one.
	ID string `json:"id"`
	// Severity is "error" for an error row and "warning" for a notice.
	Severity string `json:"severity"`
	// Message is the text the row would have shown.
	Message string `json:"message"`
	// SaveError is why the row could not be written.
	SaveError  string `json:"saveError"`
	OccurredAt int64  `json:"occurredAt"`
}

// UnsavedThreadRowsEvent is the `thread:unsaved_rows` payload and the
// GetThreadLiveState mirror: the thread's whole current list, newest last.
// An empty list clears it.
type UnsavedThreadRowsEvent struct {
	ThreadID string             `json:"threadId"`
	Rows     []UnsavedThreadRow `json:"rows"`
	// Omitted counts older unsaved rows dropped to keep the list bounded.
	Omitted int `json:"omitted,omitempty"`
}

// appUnsavedRowState is the process-local set of threads with unsaved rows.
type appUnsavedRowState struct {
	// mu serializes every change end to end, emission included, so two
	// changes cannot publish their lists in the opposite order from the
	// state they leave behind.
	mu      sync.Mutex
	seq     uint64
	threads map[string]*UnsavedThreadRowsEvent
}

// snapshot returns a copy of the thread's list, nil when it has none.
func (s *appUnsavedRowState) snapshot(threadID string) *UnsavedThreadRowsEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.threads[threadID]
	if !ok {
		return nil
	}
	return cloneUnsavedRows(entry)
}

func cloneUnsavedRows(entry *UnsavedThreadRowsEvent) *UnsavedThreadRowsEvent {
	out := *entry
	out.Rows = append([]UnsavedThreadRow(nil), entry.Rows...)
	return &out
}

// errEventRouterNotWired is the save failure of a row produced before the
// triage router exists: there was no write to try.
var errEventRouterNotWired = errors.New("the event router is not wired yet")

// unsavedRowSink is the App's triage.UnsavedRowObserver. A separate
// unexported type, so its methods are not candidate RPCs.
type unsavedRowSink struct{ app *App }

func (s unsavedRowSink) RowRefused(row triage.UnsavedRow, err error) {
	s.app.deliverUnsavedThreadRow(row, err)
}

func (s unsavedRowSink) RowSaved(threadID, itemID string) {
	s.app.withdrawSavedThreadRow(threadID, itemID)
}

// deliverUnsavedThreadRow keeps a row the store refused and pushes the
// thread's list to every client. A row with the id of one already kept
// replaces it at the end of the list.
func (a *App) deliverUnsavedThreadRow(row triage.UnsavedRow, saveErr error) {
	if row.ThreadID == "" {
		return
	}
	reason := "unknown error"
	if saveErr != nil {
		reason = triage.ClampErrorSummary(saveErr.Error())
	}
	s := &a.unsavedRows
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := row.ItemID
	if id == "" {
		id = fmt.Sprintf("unsaved-%d", s.seq)
	}
	entry, ok := s.threads[row.ThreadID]
	if !ok {
		if s.threads == nil {
			s.threads = make(map[string]*UnsavedThreadRowsEvent, 1)
		}
		entry = &UnsavedThreadRowsEvent{ThreadID: row.ThreadID}
		s.threads[row.ThreadID] = entry
	}
	kept := entry.Rows[:0:0]
	for _, existing := range entry.Rows {
		if existing.ID != id {
			kept = append(kept, existing)
		}
	}
	kept = append(kept, UnsavedThreadRow{
		ID:         id,
		Severity:   row.Severity,
		Message:    triage.ClampErrorSummary(row.Summary),
		SaveError:  reason,
		OccurredAt: time.Now().UnixMilli(),
	})
	if over := len(kept) - maxUnsavedThreadRows; over > 0 {
		kept = kept[over:]
		entry.Omitted += over
	}
	entry.Rows = kept
	a.emit(eventchan.ThreadUnsavedRows, *cloneUnsavedRows(entry))
}

// withdrawSavedThreadRow drops the entry of a row whose write has now
// succeeded, so it no longer reads as unsaved, and pushes the list. Called
// for every saved error or notification row: a thread with nothing kept
// costs one map lookup and pushes nothing.
func (a *App) withdrawSavedThreadRow(threadID, itemID string) {
	s := &a.unsavedRows
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.threads[threadID]
	if !ok {
		return
	}
	if !removeUnsavedRows(entry, map[string]bool{itemID: true}) {
		return
	}
	s.publishLocked(a, entry)
}

// removeUnsavedRows drops the rows whose ids are in ids and reports
// whether any was there.
func removeUnsavedRows(entry *UnsavedThreadRowsEvent, ids map[string]bool) bool {
	kept := entry.Rows[:0:0]
	for _, row := range entry.Rows {
		if !ids[row.ID] {
			kept = append(kept, row)
		}
	}
	removed := len(kept) != len(entry.Rows)
	entry.Rows = kept
	return removed
}

// publishLocked pushes the thread's list, forgetting it once empty.
// Caller holds s.mu.
func (s *appUnsavedRowState) publishLocked(a *App, entry *UnsavedThreadRowsEvent) {
	if len(entry.Rows) == 0 {
		s.forgetLocked(entry.ThreadID)
		a.emit(eventchan.ThreadUnsavedRows, UnsavedThreadRowsEvent{ThreadID: entry.ThreadID, Rows: []UnsavedThreadRow{}})
		return
	}
	a.emit(eventchan.ThreadUnsavedRows, *cloneUnsavedRows(entry))
}

// GetThreadUnsavedRows returns the thread's errors and notices AO could
// not save to history, newest last: the snapshot leg of
// `thread:unsaved_rows`, at the same scope. A thread with none answers an
// empty list.
//
//ao:scope threads:read
func (a *App) GetThreadUnsavedRows(threadID string) (UnsavedThreadRowsEvent, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return UnsavedThreadRowsEvent{}, fmt.Errorf("get unsaved rows: empty thread id")
	}
	if retained := a.unsavedRows.snapshot(threadID); retained != nil {
		return *retained, nil
	}
	return UnsavedThreadRowsEvent{ThreadID: threadID, Rows: []UnsavedThreadRow{}}, nil
}

// DismissUnsavedThreadRows removes the entries named by rowIDs, the ones
// the caller displayed, and pushes what remains to every client. A row
// that arrived after the caller's screen last updated is not named, so it
// stays. The dismiss also clears the omitted count: the note about older
// rows was displayed with them. Ids no longer kept are ignored.
//
//ao:scope threads:operate
func (a *App) DismissUnsavedThreadRows(threadID string, rowIDs []string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("dismiss unsaved rows: empty thread id")
	}
	ids := make(map[string]bool, len(rowIDs))
	for _, id := range rowIDs {
		ids[id] = true
	}
	s := &a.unsavedRows
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.threads[threadID]
	if !ok {
		return nil
	}
	removed := removeUnsavedRows(entry, ids)
	clearedOmitted := entry.Omitted != 0
	entry.Omitted = 0
	if removed || clearedOmitted {
		s.publishLocked(a, entry)
	}
	return nil
}

// forgetUnsavedThreadRows drops a deleted thread's list. Nothing is pushed:
// the thread's deletion removes it from every client.
func (a *App) forgetUnsavedThreadRows(threadID string) {
	s := &a.unsavedRows
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(threadID)
}

func (s *appUnsavedRowState) forgetLocked(threadID string) {
	delete(s.threads, threadID)
	if len(s.threads) == 0 {
		s.threads = nil
	}
}
