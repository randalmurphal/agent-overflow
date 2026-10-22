package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

const attentionReason = "Pick the release date before Friday"

// turnStartedAt is when the turns these tests end started: a fixed time
// in 2096, after any test thread was created and read however long the
// run takes, so a flag raised at or after it was raised during the turn.
const turnStartedAt int64 = 4_000_000_000_000

func quietTurnCompleted(a *App, quiet bool, failed bool) func() {
	return func() {
		event := triage.TurnCompletedEvent{ThreadID: mappingThreadID, TurnID: "turn-1", CountsAsActivity: true, StopReason: "end_turn", Quiet: quiet, StartedAt: turnStartedAt}
		if failed {
			event.ErrorMessage = "boom"
		}
		a.emit(eventchan.ProviderTurnCompleted, event)
	}
}

// flagThread raises a flag during the turn quietTurnCompleted ends.
func flagThread(t *testing.T, a *App, kind string) {
	t.Helper()
	if _, _, err := a.store.SetThreadAttention(mappingThreadID, kind, attentionReason, turnStartedAt+500); err != nil {
		t.Fatalf("SetThreadAttention: %v", err)
	}
}

// A quiet tick raises nothing. A turn that ends with its agent's flag up is
// said as the flag's kind, never with the agent's reason; a quiet tick says
// only a flag it raised itself; a failure still says Failed; a read flag is
// a plain completion.
func TestARestWithAnAttentionFlagNotifiesByItsKind(t *testing.T) {
	t.Parallel()
	t.Run("a quiet tick", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		if sends := settled(t, app, recorder, quietTurnCompleted(app, true, false)); len(sends) != 0 {
			t.Fatalf("a quiet tick notified: %#v", sends)
		}
	})
	for _, tc := range []struct {
		name  string
		kind  string
		quiet bool
		body  string
	}{
		{"a quiet tick that flagged a decision", store.AttentionDecision, true, "Needs your decision"},
		{"a turn that flagged a review", store.AttentionReview, false, "Ready for your review"},
		{"a turn that flagged a problem", store.AttentionProblem, false, "Hit a problem that needs you"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, recorder := newNotificationMappingApp(t)
			flagThread(t, app, tc.kind)
			sends := settled(t, app, recorder, quietTurnCompleted(app, tc.quiet, false))
			if len(sends) != 1 || sends[0].Kind != notify.KindThreadAttention || sends[0].Body != tc.body || sends[0].Title != mappingTitle {
				t.Fatalf("sends = %#v, want one %s notification saying %q", sends, notify.KindThreadAttention, tc.body)
			}
			if strings.Contains(sends[0].Body, attentionReason) || strings.Contains(sends[0].Title, attentionReason) {
				t.Fatalf("the agent's reason reached the notification: %#v", sends[0])
			}
		})
	}
	t.Run("a quiet tick after the turn that raised the flag", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		if _, _, err := app.store.SetThreadAttention(mappingThreadID, store.AttentionReview, attentionReason, turnStartedAt-1); err != nil {
			t.Fatal(err)
		}
		if sends := settled(t, app, recorder, quietTurnCompleted(app, true, false)); len(sends) != 0 {
			t.Fatalf("a quiet tick said again a flag an earlier turn raised: %#v", sends)
		}
		// A turn the user started still says it.
		sends := settled(t, app, recorder, quietTurnCompleted(app, false, false))
		if len(sends) != 1 || sends[0].Kind != notify.KindThreadAttention {
			t.Fatalf("sends = %#v, want the person's turn to say the flag", sends)
		}
	})
	t.Run("a failure with a flag up", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		flagThread(t, app, store.AttentionDecision)
		sends := settled(t, app, recorder, quietTurnCompleted(app, false, true))
		if len(sends) != 1 || sends[0].Kind != notify.KindError {
			t.Fatalf("sends = %#v, want the failure", sends)
		}
	})
	t.Run("a flag the user read", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		if _, _, err := app.store.SetThreadAttention(mappingThreadID, store.AttentionReview, attentionReason, 1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := app.store.MarkThreadReadNow(context.Background(), mappingThreadID); err != nil {
			t.Fatal(err)
		}
		sends := settled(t, app, recorder, quietTurnCompleted(app, false, false))
		if len(sends) != 1 || sends[0].Kind != notify.KindTurnComplete {
			t.Fatalf("sends = %#v, want a plain completion", sends)
		}
		if sends := settled(t, app, recorder, quietTurnCompleted(app, true, false)); len(sends) != 1 {
			t.Fatalf("a quiet tick after the flag was read notified: %#v", sends)
		}
	})
}

// completeTurnAt records a top-level turn of the mapping thread that ended
// at completedAt, which is when MapTurnRest said whatever flag was up.
func completeTurnAt(t *testing.T, a *App, turnID string, index int, completedAt int64, errorMessage string) {
	t.Helper()
	if err := a.store.InsertTurn(store.Turn{TurnID: turnID, ThreadID: mappingThreadID, TurnIndex: index, StartedAt: completedAt - 10}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	stop := "end_turn"
	if errorMessage != "" {
		stop = "error"
	}
	if err := a.store.UpdateTurnCompleted(turnID, completedAt, stop, "", "", errorMessage); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
}

func lowerFlag(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.threadTools().FlagAttention(context.Background(), threadtools.Caller{ThreadID: mappingThreadID}, threadtools.AttentionCall{Kind: threadtools.AttentionNone}); err != nil {
		t.Fatalf("lower: %v", err)
	}
}

// thread_attention raises the caller's flag and moves the thread up the
// sidebar in one write.
func TestFlagAttentionRaisesTheCallersFlag(t *testing.T) {
	t.Parallel()
	app, _ := newNotificationMappingApp(t)
	before, err := app.store.GetThread(mappingThreadID)
	if err != nil {
		t.Fatal(err)
	}
	frames := captureOrderedEmissions(app, string(eventchan.ThreadUpdated))

	ack, err := app.threadTools().FlagAttention(context.Background(), threadtools.Caller{ThreadID: mappingThreadID}, threadtools.AttentionCall{Kind: threadtools.AttentionReview, Reason: attentionReason})
	if err != nil || ack.Kind != threadtools.AttentionReview || ack.Reason != attentionReason {
		t.Fatalf("raise = %+v, %v", ack, err)
	}
	row, err := app.store.GetThread(mappingThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.AttentionPending() || row.Attention.Kind != store.AttentionReview {
		t.Fatalf("attention = %+v, want a pending review", row.Attention)
	}
	if row.UpdatedAt <= before.UpdatedAt {
		t.Errorf("updated_at = %d, want it past %d: a flag moves the thread up", row.UpdatedAt, before.UpdatedAt)
	}
	var announced bool
	for _, frame := range frames() {
		if event, ok := frame.data.(triage.ThreadUpdateEvent); ok && event.Thread != nil && event.Thread.Attention != nil {
			announced = true
		}
	}
	if !announced {
		t.Error("no thread:updated frame carried the flag")
	}
	if _, err := app.threadTools().FlagAttention(threadtools.WithForwarded(context.Background()), threadtools.Caller{ThreadID: mappingThreadID}, threadtools.AttentionCall{Kind: threadtools.AttentionReview, Reason: "r"}); err == nil {
		t.Error("a forwarded flag was accepted")
	}
}

// Lowering a flag takes back the notification that said it, and only that:
// a flag no turn has ended with yet was never said, and a failure after the
// flag replaced it with its own.
func TestLoweringAFlagTakesBackOnlyTheNotificationThatSaidIt(t *testing.T) {
	t.Parallel()
	retracted := func(sends []notify.Send) bool {
		for _, send := range sends {
			if send.Retract && send.ID == "thread:"+mappingThreadID {
				return true
			}
		}
		return false
	}
	t.Run("a flag a turn said", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		flagThread(t, app, store.AttentionReview)
		completeTurnAt(t, app, "turn-said", 1, turnStartedAt+1_000, "")
		lowerFlag(t, app)
		sends := settled(t, app, recorder)
		if len(sends) != 1 || !retracted(sends) || sends[0].Kind != notify.KindThreadAttention {
			t.Fatalf("sends = %#v, want the flag's notification retracted", sends)
		}
		if row, _ := app.store.GetThread(mappingThreadID); row.Attention != nil {
			t.Fatalf("attention = %+v after lowering", row.Attention)
		}
	})
	t.Run("a flag raised and lowered in one turn", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		// An earlier turn the user has not read ended Completed.
		completeTurnAt(t, app, "turn-earlier", 1, turnStartedAt-1_000, "")
		flagThread(t, app, store.AttentionReview)
		lowerFlag(t, app)
		if sends := settled(t, app, recorder); retracted(sends) {
			t.Fatalf("sends = %#v, want the earlier turn's notification left", sends)
		}
	})
	t.Run("a flag a failure replaced", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		flagThread(t, app, store.AttentionDecision)
		completeTurnAt(t, app, "turn-said", 1, turnStartedAt+1_000, "")
		completeTurnAt(t, app, "turn-failed", 2, turnStartedAt+2_000, "boom")
		if _, err := app.store.AppendItem(store.Item{
			ID: "err-1", ThreadID: mappingThreadID, TurnIndex: 2, Kind: "error", Role: "system",
			Status: "completed", Summary: "boom", CreatedAt: turnStartedAt + 2_000, UpdatedAt: turnStartedAt + 2_000,
		}); err != nil {
			t.Fatal(err)
		}
		lowerFlag(t, app)
		if sends := settled(t, app, recorder); retracted(sends) {
			t.Fatalf("sends = %#v, want the failure's notification left", sends)
		}
	})
	t.Run("a flag the user read", func(t *testing.T) {
		t.Parallel()
		app, recorder := newNotificationMappingApp(t)
		flagThread(t, app, store.AttentionProblem)
		completeTurnAt(t, app, "turn-said", 1, turnStartedAt+1_000, "")
		if _, _, err := app.store.MarkThreadReadNow(context.Background(), mappingThreadID); err != nil {
			t.Fatal(err)
		}
		lowerFlag(t, app)
		if sends := settled(t, app, recorder); retracted(sends) {
			t.Fatalf("sends = %#v, want nothing taken back", sends)
		}
	})
}

// The attention kinds are one set in the store, the tool and the
// notification: each tool kind is one the store accepts, and each has a
// phrase of its own at rest.
func TestAttentionKindsAgreeAcrossTheStoreToolAndNotification(t *testing.T) {
	t.Parallel()
	tool := []string{threadtools.AttentionDecision, threadtools.AttentionReview, threadtools.AttentionProblem}
	if !slices.Equal(tool, store.AttentionKinds()) {
		t.Errorf("tool kinds %v, store kinds %v: want one set", tool, store.AttentionKinds())
	}
	phrases := map[string]string{}
	for _, kind := range store.AttentionKinds() {
		rest, ok := notify.MapTurnRest(notify.TurnRest{Thread: notify.ThreadRef{ID: "t", Attention: kind}, TopLevel: true})
		if !ok || rest.Send.Kind != notify.KindThreadAttention || rest.Send.Body == notify.KindPhrase(notify.KindThreadAttention) {
			t.Errorf("kind %q at rest = %#v, want a phrase of its own", kind, rest.Send)
		}
		if other, seen := phrases[rest.Send.Body]; seen {
			t.Errorf("kinds %q and %q both say %q", other, kind, rest.Send.Body)
		}
		phrases[rest.Send.Body] = kind
	}
	if store.ValidAttentionKind(threadtools.AttentionNone) {
		t.Error("the store accepts none as a kind")
	}
	if threadtools.MaxAttentionReasonRunes != store.MaxAttentionReasonRunes {
		t.Errorf("reason bound: tool %d, store %d", threadtools.MaxAttentionReasonRunes, store.MaxAttentionReasonRunes)
	}
}

// A Claude turn that took up queued wakes holds no rows of its own; it is
// a tick for every round of it, and the pickup is forgotten once a later
// turn ends.
func TestAPickupTurnIsATickForEachOfItsRounds(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("pickup-tick")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.agentWakes.mu.Lock()
	app.agentWakes.pickups = map[string]agentWakePickup{thread.ID: {turn: 3, tick: true}}
	app.agentWakes.mu.Unlock()

	if !app.agentTurnIsTick(thread.ID, 3) {
		t.Fatal("the pickup turn is not a tick")
	}
	app.endAgentWakeTickTurn(thread.ID, 3)
	if !app.agentTurnIsTick(thread.ID, 3) {
		t.Fatal("a later round of the pickup turn is not a tick")
	}
	if app.agentTurnIsTick(thread.ID, 4) {
		t.Fatal("the turn after the pickup is a tick")
	}
	app.endAgentWakeTickTurn(thread.ID, 4)
	if app.agentTurnIsTick(thread.ID, 3) {
		t.Fatal("the pickup outlived the turn after it")
	}
}

// A turn's own rows decide: wakes only is a tick, and a person's message,
// or one joined to a wake, is not.
func TestATurnOfWakesIsATick(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("rows-tick")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i, row := range []struct {
		turn int
		meta string
	}{
		{1, `{"sendId":"agent-wake:w1:1","origin":"agent-wake"}`},
		{1, `{"sendId":"agent-wake:w2:1","origin":"agent-wake"}`},
		{2, `{"sendId":"agent-wake:w1:2","joinedSendIds":["agent-wake:w1:2","send-1"],"origin":"agent-wake"}`},
		{3, `{"sendId":"send-2"}`},
	} {
		if _, err := app.store.AppendItem(store.Item{
			ID: "user:" + string(rune('a'+i)), ThreadID: thread.ID, TurnIndex: row.turn, Kind: "user_text", Role: "user",
			Status: "completed", Summary: "x", Meta: row.meta, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for turn, want := range map[int]bool{1: true, 2: false, 3: false, 4: false} {
		if got := app.agentTurnIsTick(thread.ID, turn); got != want {
			t.Errorf("turn %d tick = %v, want %v", turn, got, want)
		}
	}
}

// A turn whose rows cannot be read is not a tick, whatever the marks say: a
// person's turn going silent is the worse mistake.
func TestATurnWhoseRowsCannotBeReadIsNotATick(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("unreadable-tick")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	app.agentWakes.mu.Lock()
	app.agentWakes.pickups = map[string]agentWakePickup{thread.ID: {turn: 2, tick: true}}
	app.agentWakes.mu.Unlock()
	if !app.agentTurnIsTick(thread.ID, 2) {
		t.Fatal("the pickup turn is not a tick while its rows read")
	}
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	if app.agentTurnIsTick(thread.ID, 2) {
		t.Fatal("a turn whose rows could not be read is a tick")
	}
}
