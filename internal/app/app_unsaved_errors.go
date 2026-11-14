package app

import (
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

// Thread errors AO could not save.
//
// A thread error reaches history as an error row, and triage announces the
// row only after the write succeeds. When the database refuses the write
// (disk full, read-only filesystem, I/O error), the error is kept here and
// pushed on `thread:unsaved_error` instead, so the user still sees it,
// marked as not saved and with the reason. It is not history: nothing here
// is written to the store or replayed from it, it dies with the process,
// and the list is the thread's last few errors. GetThreadLiveState mirrors
// it for a client that opens the thread later; DismissSessionBanner with
// kind `unsaved_error` clears it for every client; deleting the thread
// forgets it.

// unsavedErrorBannerKind is the DismissSessionBanner kind that clears a
// thread's unsaved errors.
const unsavedErrorBannerKind = "unsaved_error"

// maxUnsavedThreadErrors bounds the errors kept per thread. Older ones are
// dropped and counted in Omitted.
const maxUnsavedThreadErrors = 5

// UnsavedThreadError is one thread error AO could not save to history.
type UnsavedThreadError struct {
	ID string `json:"id"`
	// Message is the error the row would have shown.
	Message string `json:"message"`
	// SaveError is why the row could not be written.
	SaveError  string `json:"saveError"`
	OccurredAt int64  `json:"occurredAt"`
}

// UnsavedThreadErrorsEvent is the `thread:unsaved_error` payload and the
// GetThreadLiveState mirror: the thread's whole current list, newest last.
// An empty list clears it.
type UnsavedThreadErrorsEvent struct {
	ThreadID string               `json:"threadId"`
	Errors   []UnsavedThreadError `json:"errors"`
	// Omitted counts older unsaved errors dropped to keep the list bounded.
	Omitted int `json:"omitted,omitempty"`
}

// appUnsavedErrorState is the process-local set of threads with unsaved
// errors.
type appUnsavedErrorState struct {
	// mu serializes every change end to end, emission included, so two
	// changes cannot publish their lists in the opposite order from the
	// state they leave behind.
	mu      sync.Mutex
	seq     uint64
	threads map[string]*UnsavedThreadErrorsEvent
}

// snapshot returns a copy of the thread's list, nil when it has none.
func (s *appUnsavedErrorState) snapshot(threadID string) *UnsavedThreadErrorsEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.threads[threadID]
	if !ok {
		return nil
	}
	return cloneUnsavedErrors(entry)
}

func cloneUnsavedErrors(entry *UnsavedThreadErrorsEvent) *UnsavedThreadErrorsEvent {
	out := *entry
	out.Errors = append([]UnsavedThreadError(nil), entry.Errors...)
	return &out
}

// deliverUnsavedThreadError keeps an error whose row could not be saved and
// pushes the thread's list to every client. Triage calls it for every error
// row the store refuses (Router.SetUnsavedErrorObserver).
func (a *App) deliverUnsavedThreadError(threadID, message string, saveErr error) {
	if threadID == "" {
		return
	}
	reason := "unknown error"
	if saveErr != nil {
		reason = triage.ClampErrorSummary(saveErr.Error())
	}
	s := &a.unsavedErrors
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	entry, ok := s.threads[threadID]
	if !ok {
		if s.threads == nil {
			s.threads = make(map[string]*UnsavedThreadErrorsEvent, 1)
		}
		entry = &UnsavedThreadErrorsEvent{ThreadID: threadID}
		s.threads[threadID] = entry
	}
	entry.Errors = append(entry.Errors, UnsavedThreadError{
		ID:         fmt.Sprintf("unsaved-%d", s.seq),
		Message:    triage.ClampErrorSummary(message),
		SaveError:  reason,
		OccurredAt: time.Now().UnixMilli(),
	})
	if over := len(entry.Errors) - maxUnsavedThreadErrors; over > 0 {
		entry.Errors = append(entry.Errors[:0:0], entry.Errors[over:]...)
		entry.Omitted += over
	}
	a.emit(eventchan.ThreadUnsavedError, *cloneUnsavedErrors(entry))
}

// dismissUnsavedThreadErrors clears the thread's list and pushes the empty
// list to every client. A no-op when the thread has none.
func (a *App) dismissUnsavedThreadErrors(threadID string) {
	s := &a.unsavedErrors
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[threadID]; !ok {
		return
	}
	s.forgetLocked(threadID)
	a.emit(eventchan.ThreadUnsavedError, UnsavedThreadErrorsEvent{ThreadID: threadID, Errors: []UnsavedThreadError{}})
}

// forgetUnsavedThreadErrors drops a deleted thread's list. Nothing is
// pushed: the thread's deletion removes it from every client.
func (a *App) forgetUnsavedThreadErrors(threadID string) {
	s := &a.unsavedErrors
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(threadID)
}

func (s *appUnsavedErrorState) forgetLocked(threadID string) {
	delete(s.threads, threadID)
	if len(s.threads) == 0 {
		s.threads = nil
	}
}
