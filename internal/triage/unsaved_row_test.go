package triage

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

type unsavedRowReport struct {
	row UnsavedRow
	err error
}

type unsavedRowRecorder struct {
	mu      sync.Mutex
	reports []unsavedRowReport
	saved   []UnsavedRow
}

func (r *unsavedRowRecorder) RowRefused(row UnsavedRow, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, unsavedRowReport{row: row, err: err})
}

func (r *unsavedRowRecorder) RowSaved(row UnsavedRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, row)
}

func (r *unsavedRowRecorder) savedRows() []UnsavedRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]UnsavedRow(nil), r.saved...)
}

func (r *unsavedRowRecorder) snapshot() []unsavedRowReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]unsavedRowReport(nil), r.reports...)
}

func newUnsavedRowRouter(t *testing.T) (*Router, *store.Store, *sql.DB, *unsavedRowRecorder, *emissionLog) {
	t.Helper()
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	emissions := &emissionLog{}
	router := NewRouter(st, func(name eventchan.Channel, data any) { emissions.add(emitted{name.String(), data}) })
	t.Cleanup(router.DrainWireItemRefresh)
	recorder := &unsavedRowRecorder{}
	router.SetUnsavedRowObserver(recorder)
	createTestThread(t, st, "t1")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw database: %v", err)
		}
	})
	return router, st, raw, recorder, emissions
}

func refuseRows(t *testing.T, raw *sql.DB, kind, reason string) {
	t.Helper()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_` + kind + ` BEFORE INSERT ON items WHEN NEW.kind = '` + kind + `' BEGIN SELECT RAISE(ABORT, '` + reason + `'); END`); err != nil {
		t.Fatal(err)
	}
}

// An error row the store refuses reaches the observer with the summary the
// row would have carried and the reason the write failed; the caller still
// gets the error.
func TestErrorRowTheStoreRefusesReachesTheUnsavedRowObserver(t *testing.T) {
	router, st, raw, recorder, _ := newUnsavedRowRouter(t)
	refuseRows(t, raw, "error", "database or disk is full")

	err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: "reconnect failed"})
	if err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("HandleSynthetic error = %v, want the refused write", err)
	}
	reports := recorder.snapshot()
	if len(reports) != 1 {
		t.Fatalf("observer reports = %+v, want one", reports)
	}
	row := reports[0].row
	if row.ThreadID != "t1" || row.Summary != "reconnect failed" || row.Severity != UnsavedSeverityError || row.ItemID == "" {
		t.Fatalf("row = %+v, want t1 / reconnect failed / error with an id", row)
	}
	if reports[0].err == nil || !strings.Contains(reports[0].err.Error(), "database or disk is full") {
		t.Fatalf("report error = %v, want the write failure", reports[0].err)
	}
	if rows := findItemsByKind(t, st, "t1", ItemKindError); len(rows) != 0 {
		t.Fatalf("error rows = %+v, want none", rows)
	}
}

func TestSavedRowsDoNotReachTheUnsavedRowObserver(t *testing.T) {
	router, st, _, recorder, _ := newUnsavedRowRouter(t)

	if err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: "reconnect failed"}); err != nil {
		t.Fatal(err)
	}
	if err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventNotification, ThreadID: "t1", Content: "worktree kept"}); err != nil {
		t.Fatal(err)
	}
	if reports := recorder.snapshot(); len(reports) != 0 {
		t.Fatalf("observer reports = %+v, want none for saved rows", reports)
	}
	if rows := findItemsByKind(t, st, "t1", ItemKindError); len(rows) != 1 {
		t.Fatalf("error rows = %+v, want one", rows)
	}
}

// A notice the store refuses reaches the observer as a warning.
func TestNotificationRowTheStoreRefusesReachesTheObserverAsAWarning(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
	refuseRows(t, raw, "notification", "attempt to write a readonly database")
	meta, _ := json.Marshal(map[string]string{"kind": "warning"})

	err := router.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventNotification, ThreadID: "t1", Content: "Kept worktree feature: 2 uncommitted files.", Meta: meta})
	if err == nil {
		t.Fatal("HandleSynthetic returned nil for a refused notice")
	}
	reports := recorder.snapshot()
	if len(reports) != 1 {
		t.Fatalf("observer reports = %+v, want one", reports)
	}
	if row := reports[0].row; row.Severity != UnsavedSeverityWarning || row.Summary != "Kept worktree feature: 2 uncommitted files." {
		t.Fatalf("row = %+v, want the notice as a warning", row)
	}
}

// A fatal wire error on a full disk fails at the flip of the turn's
// streaming rows before its own row is written. The error row is still
// attempted, so the user hears about the error the provider reported.
func TestFatalWireErrorWhoseFlipFailsStillReportsTheError(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
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
	var crashed []UnsavedRow
	for _, report := range recorder.snapshot() {
		if report.row.Summary == "provider crashed" {
			crashed = append(crashed, report.row)
		}
	}
	if len(crashed) != 1 || crashed[0].Severity != UnsavedSeverityError {
		t.Fatalf("observer reports = %+v, want the fatal error once", recorder.snapshot())
	}
}

// A session death whose notice the store refuses is still announced on
// provider:session_died, and the notice reaches the observer. A replay of
// the same death retries the same row id.
func TestSessionDeathWhoseNoticeIsRefusedIsStillAnnounced(t *testing.T) {
	router, _, raw, recorder, emissions := newUnsavedRowRouter(t)
	refuseRows(t, raw, "notification", "database or disk is full")
	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{ExitCode: 1})
	death := provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: "t1", Content: "error", Meta: exitMeta, Timestamp: time.Now()}

	if err := router.Handle(death); err != nil {
		t.Fatalf("session death: %v", err)
	}

	died := filterEmissions(emissions.snapshot(), eventchan.ProviderSessionDied.String())
	if len(died) != 1 {
		t.Fatalf("provider:session_died = %d, want 1 although its notice was not saved", len(died))
	}
	reports := recorder.snapshot()
	if len(reports) != 1 || reports[0].row.Severity != UnsavedSeverityWarning ||
		reports[0].row.Summary != "Provider session exited with code 1" || !strings.HasPrefix(reports[0].row.ItemID, "session_died:") {
		t.Fatalf("observer reports = %+v, want the session_died notice as a warning", reports)
	}

	if err := router.Handle(death); err != nil {
		t.Fatalf("replayed session death: %v", err)
	}
	reports = recorder.snapshot()
	if len(reports) != 2 || reports[1].row.ItemID != reports[0].row.ItemID {
		t.Fatalf("replay reports = %+v, want the same row id again", reports)
	}
	// The replay is the same death: announced once, so a Reconnect banner
	// the user dismissed does not come back.
	if died := filterEmissions(emissions.snapshot(), eventchan.ProviderSessionDied.String()); len(died) != 1 {
		t.Fatalf("provider:session_died after the replay = %d, want 1", len(died))
	}

	// A replacement session's death is its own, even at the same turn.
	router.MarkThreadActive("t1")
	if err := router.Handle(death); err != nil {
		t.Fatalf("next session's death: %v", err)
	}
	if died := filterEmissions(emissions.snapshot(), eventchan.ProviderSessionDied.String()); len(died) != 2 {
		t.Fatalf("provider:session_died after a new session died = %d, want 2", len(died))
	}
}

// A refused row that a later write saves is reported saved by id, so it
// stops reading as unsaved. Rows of other kinds report nothing.
func TestARefusedRowThatLaterSavesIsReportedSaved(t *testing.T) {
	router, st, raw, recorder, _ := newUnsavedRowRouter(t)
	refuseRows(t, raw, "notification", "database or disk is full")
	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{ExitCode: 1})
	death := provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: "t1", Content: "error", Meta: exitMeta, Timestamp: time.Now()}
	if err := router.Handle(death); err != nil {
		t.Fatalf("session death: %v", err)
	}
	refused := recorder.snapshot()
	if len(refused) != 1 {
		t.Fatalf("refused = %+v, want the death notice", refused)
	}
	if _, err := raw.Exec(`DROP TRIGGER refuse_notification`); err != nil {
		t.Fatal(err)
	}

	if err := router.Handle(death); err != nil {
		t.Fatalf("replayed session death: %v", err)
	}

	if got := recorder.savedRows(); len(got) != 1 || got[0] != refused[0].row {
		t.Fatalf("saved = %+v, want the refused notice %+v", got, refused[0].row)
	}
	if rows := findItemsByKind(t, st, "t1", itemKindNotification); len(rows) != 1 {
		t.Fatalf("notification rows = %+v, want the saved death", rows)
	}

	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "an answer"})
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})
	if got := recorder.savedRows(); len(got) != 1 {
		t.Fatalf("saved = %v, want nothing for rows of other kinds", got)
	}
}

// A death whose notice cannot even be looked up is still announced, and
// its notice reaches the observer: an unknown answer reads as a first
// report rather than dropping the death.
func TestSessionDeathWhoseNoticeLookupFailsIsStillAnnounced(t *testing.T) {
	router, _, raw, recorder, emissions := newUnsavedRowRouter(t)
	if _, err := raw.Exec(`ALTER TABLE items RENAME TO items_unreadable`); err != nil {
		t.Fatal(err)
	}
	exitMeta, _ := json.Marshal(provider.ProcessExitInfo{ExitCode: 2})

	_ = router.Handle(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: "t1", Content: "error", Meta: exitMeta, Timestamp: time.Now()})

	if died := filterEmissions(emissions.snapshot(), eventchan.ProviderSessionDied.String()); len(died) != 1 {
		t.Fatalf("provider:session_died = %d, want 1", len(died))
	}
	var notices []UnsavedRow
	for _, report := range recorder.snapshot() {
		if strings.HasPrefix(report.row.ItemID, "session_died:") {
			notices = append(notices, report.row)
		}
	}
	if len(notices) != 1 || notices[0].Summary != "Provider session exited with code 2" {
		t.Fatalf("observer reports = %+v, want the session_died notice once", recorder.snapshot())
	}
}
