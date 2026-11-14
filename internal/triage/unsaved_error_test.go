package triage

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

type unsavedErrorReport struct {
	threadID string
	summary  string
	err      error
}

type unsavedErrorRecorder struct {
	mu      sync.Mutex
	reports []unsavedErrorReport
}

func (r *unsavedErrorRecorder) observe(threadID, summary string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, unsavedErrorReport{threadID: threadID, summary: summary, err: err})
}

func (r *unsavedErrorRecorder) snapshot() []unsavedErrorReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]unsavedErrorReport(nil), r.reports...)
}

func newUnsavedErrorRouter(t *testing.T) (*Router, *store.Store, *sql.DB, *unsavedErrorRecorder) {
	t.Helper()
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	router := NewRouter(st, func(eventchan.Channel, any) {})
	t.Cleanup(router.DrainWireItemRefresh)
	recorder := &unsavedErrorRecorder{}
	router.SetUnsavedErrorObserver(recorder.observe)
	createTestThread(t, st, "t1")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return router, st, raw, recorder
}

// An error row the store refuses reaches the observer with the summary the
// row would have carried and the reason the write failed; the caller still
// gets the error.
func TestErrorRowTheStoreRefusesReachesTheUnsavedErrorObserver(t *testing.T) {
	router, st, raw, recorder := newUnsavedErrorRouter(t)
	if _, err := raw.Exec(`CREATE TRIGGER fail_error_row BEFORE INSERT ON items WHEN NEW.kind = 'error' BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}

	err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: "reconnect failed"})
	if err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("HandleSynthetic error = %v, want the refused write", err)
	}
	reports := recorder.snapshot()
	if len(reports) != 1 {
		t.Fatalf("observer reports = %+v, want one", reports)
	}
	if reports[0].threadID != "t1" || reports[0].summary != "reconnect failed" {
		t.Fatalf("report = %+v, want t1 / reconnect failed", reports[0])
	}
	if reports[0].err == nil || !strings.Contains(reports[0].err.Error(), "database or disk is full") {
		t.Fatalf("report error = %v, want the write failure", reports[0].err)
	}
	if rows := findItemsByKind(t, st, "t1", ItemKindError); len(rows) != 0 {
		t.Fatalf("error rows = %+v, want none", rows)
	}
}

func TestSavedErrorRowDoesNotReachTheUnsavedErrorObserver(t *testing.T) {
	router, st, _, recorder := newUnsavedErrorRouter(t)

	if err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: "reconnect failed"}); err != nil {
		t.Fatal(err)
	}
	if reports := recorder.snapshot(); len(reports) != 0 {
		t.Fatalf("observer reports = %+v, want none for a saved row", reports)
	}
	if rows := findItemsByKind(t, st, "t1", ItemKindError); len(rows) != 1 {
		t.Fatalf("error rows = %+v, want one", rows)
	}
}

// A fatal wire error on a full disk fails at the flip of the turn's
// streaming rows before its own row is written. The error row is still
// attempted, so the user hears about the error the provider reported.
func TestFatalWireErrorWhoseFlipFailsStillReportsTheError(t *testing.T) {
	router, _, raw, recorder := newUnsavedErrorRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "half an answer"})
	if _, err := raw.Exec(`CREATE TRIGGER fail_item_update BEFORE UPDATE ON items BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER fail_item_insert BEFORE INSERT ON items BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}

	fatal, _ := json.Marshal(map[string]any{"fatal": true})
	err := router.Handle(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: "provider crashed", Meta: fatal})
	if err == nil {
		t.Fatal("Handle returned nil for a fatal error whose writes all failed")
	}
	reports := recorder.snapshot()
	if len(reports) != 1 || reports[0].summary != "provider crashed" {
		t.Fatalf("observer reports = %+v, want the fatal error", reports)
	}
}
