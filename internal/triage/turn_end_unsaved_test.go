package triage

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// A turn's end the store refuses reaches the user as one error entry per
// turn on the unsaved-rows surface, whichever path wrote the end.

// refuseTurnEnds makes the database refuse every write of a turn's end.
func refuseTurnEnds(t *testing.T, raw *sql.DB, reason string) {
	t.Helper()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_turn_end BEFORE UPDATE OF completed_at ON turns
		WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, '` + reason + `'); END`); err != nil {
		t.Fatal(err)
	}
}

// turnEndReports are the observer's reports of unsaved turn ends.
func turnEndReports(recorder *unsavedRowRecorder) []unsavedRowReport {
	var out []unsavedRowReport
	for _, report := range recorder.snapshot() {
		if strings.HasPrefix(report.row.ItemID, "turn-end:") {
			out = append(out, report)
		}
	}
	return out
}

func wantTurnEndReport(t *testing.T, recorder *unsavedRowRecorder, turnID string) {
	t.Helper()
	reports := turnEndReports(recorder)
	want := UnsavedRow{ThreadID: "t1", ItemID: "turn-end:" + turnID, Severity: UnsavedSeverityError, Summary: UnsavedTurnEndSummary}
	if len(reports) != 1 || reports[0].row != want {
		t.Fatalf("turn end reports = %+v, want one %+v", reports, want)
	}
	if reports[0].err == nil || !strings.Contains(reports[0].err.Error(), "database or disk is full") {
		t.Fatalf("turn end report error = %v, want the write failure", reports[0].err)
	}
}

func wantTurnOpen(t *testing.T, router *Router) {
	t.Helper()
	if _, active, err := router.store.GetActiveTurn("t1"); err != nil || !active {
		t.Fatalf("GetActiveTurn = active %v, err %v; want the unsaved turn still open", active, err)
	}
}

// A completion whose end write fails tells the user, and the row stays
// open as the entry says.
func TestTurnCompletionWhoseEndIsRefusedReachesTheUser(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	refuseTurnEnds(t, raw, "database or disk is full")

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	wantTurnEndReport(t, recorder, "t1:1")
	wantTurnOpen(t, router)
}

// A turn still open when the next one starts is settled by that start; a
// failure of that end is told the same way.
func TestPredecessorEndTheStoreRefusesReachesTheUser(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	refuseTurnEnds(t, raw, "database or disk is full")

	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 2})

	wantTurnEndReport(t, recorder, "t1:1")
}

// The end of a turn a merge fold emptied is told the same way.
func TestFoldEmptiedTurnEndTheStoreRefusesReachesTheUser(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	refuseTurnEnds(t, raw, "database or disk is full")

	router.endFoldEmptiedTurn("t1", 1, time.Now().UnixMilli())

	wantTurnEndReport(t, recorder, "t1:1")
}

// A turn whose end fails twice, on two paths, is one entry: both reports
// name the same row, which the app keeps once (app_unsaved_rows.go).
func TestATurnWhoseEndFailsTwiceIsOneEntry(t *testing.T) {
	router, _, raw, recorder, _ := newUnsavedRowRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	refuseTurnEnds(t, raw, "database or disk is full")
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta()})

	router.endFoldEmptiedTurn("t1", 1, time.Now().UnixMilli())

	reports := turnEndReports(recorder)
	if len(reports) != 2 || reports[0].row != reports[1].row {
		t.Fatalf("turn end reports = %+v, want the same row twice", reports)
	}
}

// A refusal to change history a fork shows reaches the thread as its own
// error row; the turn's end adds no second entry for it.
func TestShownHistoryRefusalOfATurnEndIsToldOnce(t *testing.T) {
	router, st, raw, recorder, _ := newUnsavedRowRouter(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	refuseTurnEnds(t, raw, "history another thread shows is immutable")

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	if reports := turnEndReports(recorder); len(reports) != 0 {
		t.Fatalf("turn end reports = %+v, want none beside the refusal's own row", reports)
	}
	var refusals int
	for _, row := range findItemsByKind(t, st, "t1", ItemKindError) {
		if strings.Contains(row.Summary, "would change history a fork of this thread shows") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("refusal error rows = %d, want 1", refusals)
	}
}

// An end with no row leaves nothing open, so nothing is told.
func TestAnEndWithNoRowIsNotTold(t *testing.T) {
	router, _, _, recorder, _ := newUnsavedRowRouter(t)

	if err := router.writeTurnEnd("t1", "t1:9", time.Now().UnixMilli(), "end_turn", "", "", "", "end a missing turn"); err == nil {
		t.Fatal("writeTurnEnd of a missing row returned nil")
	}

	if reports := turnEndReports(recorder); len(reports) != 0 {
		t.Fatalf("turn end reports = %+v, want none for a missing row", reports)
	}
}

// wantRefusedStartSettled checks that the open a replacement refused left
// no open turn: its row reads interrupted, the router holds no open turn,
// the turn announced only the rounds it announced before the open, and the
// thread's row was announced reading the interruption, so a sidebar with no
// pane on the thread reads it as the tools do.
func wantRefusedStartSettled(t *testing.T, router *Router, emissions *emissionLog, turnID string, turnIndex, announced int) {
	t.Helper()
	if _, active, err := router.store.GetActiveTurn("t1"); err != nil || active {
		t.Fatalf("GetActiveTurn = active %v, err %v; want the refused start's row settled", active, err)
	}
	turn, found, err := router.store.GetTurn(turnID)
	if err != nil || !found {
		t.Fatalf("turn %s: found %v, err %v", turnID, found, err)
	}
	if turn.CompletedAt == nil || turn.StopReason != "interrupted" {
		t.Fatalf("turn %s = %+v, want it settled as interrupted", turnID, turn)
	}
	if open := router.OpenTurnIndex("t1"); open != -1 {
		t.Fatalf("OpenTurnIndex = %d, want no open turn", open)
	}
	rounds := 0
	for _, e := range filterEmissions(emissions.snapshot(), eventchan.ProviderTurnStarted.String()) {
		if started, ok := e.data.(TurnStartedEvent); ok && started.TurnIndex == turnIndex {
			rounds++
		}
	}
	if rounds != announced {
		t.Fatalf("turn %d announced %d rounds, want %d: the refused open announces none", turnIndex, rounds, announced)
	}
	rows := threadRowEmissions(t, emissions.snapshot())
	interrupted := 0
	for _, row := range rows {
		if row.HasIncompleteTurn {
			interrupted++
		}
	}
	if interrupted != 1 || len(rows) == 0 || !rows[len(rows)-1].HasIncompleteTurn {
		t.Fatalf("thread row frames = %d, %d reading the interruption; want one, and the last frame reading it", len(rows), interrupted)
	}
}

// awaitTurnAndReplace gives the thread a delivery waiting for its next
// turn, and replaces session s1 with s2 when that turn releases it: after
// the turn is open and before its round is set.
func awaitTurnAndReplace(router *Router) {
	router.SetSendDeliveredHook(func(threadID string, delivery SendDelivery) {
		if !delivery.AwaitingTurn {
			router.MarkThreadActive(threadID, "s2")
		}
	})
	router.mu.Lock()
	router.state("t1").deliveriesAwaitingTurn = []SendDelivery{{UserItemID: "u1", SendIDs: []string{"send-1"}}}
	router.mu.Unlock()
}

// A replacement's commit that refuses a start after it wrote its turn row
// settles that row: no later frame of the session is admitted, and the
// replacement's own end of the replaced turn ran before the commit.
func TestStartRefusedAfterItsRowIsWrittenSettlesTheRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.MarkThreadActive("t1", "s1")
	router.SetTurnRowWrittenGate(func(threadID string) { router.MarkThreadActive(threadID, "s2") })

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, SessionToken: "s1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	wantRefusedStartSettled(t, router, emissions, "t1:1", 1, 0)
}

// The same when the commit lands after the start set its open turn and
// before its round.
func TestStartWhoseRoundIsRefusedSettlesItsRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.MarkThreadActive("t1", "s1")
	awaitTurnAndReplace(router)

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, SessionToken: "s1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	wantRefusedStartSettled(t, router, emissions, "t1:1", 1, 0)
}

// A queued message's echo that opens its turn is refused the same way when
// the commit lands before its round.
func TestQueuedEchoTurnWhoseRoundIsRefusedSettlesItsRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.MarkThreadActive("t1", "s1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, SessionToken: "s1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	awaitTurnAndReplace(router)

	router.openQueuedEchoTurn("t1", 1, time.Now().UnixMilli(), -1, "s1")

	wantRefusedStartSettled(t, router, emissions, "t1:1", 1, 0)
}

// A soft-closed turn's continuation that reopens the row is refused the
// same way when the commit lands before its new round.
func TestContinuationWhoseRoundIsRefusedSettlesItsRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.MarkThreadActive("t1", "s1")
	for _, evt := range []provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0},
		{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"}},
	} {
		evt.SessionToken, evt.Timestamp = "s1", time.Now()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	awaitTurnAndReplace(router)

	continuation := parentContentBlockStart("t1", "text")
	continuation.SessionToken = "s1"
	if err := router.Handle(continuation); err != nil {
		t.Fatalf("continuation: %v", err)
	}

	wantRefusedStartSettled(t, router, emissions, "t1:0", 0, 1)
}
