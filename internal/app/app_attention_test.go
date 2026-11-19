package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
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

// tickTurnHarness drives a real router whose deliveries reach the app as in
// production (onSendDelivered), for a thread whose turn 0 is running.
type tickTurnHarness struct {
	t      *testing.T
	app    *App
	thread string
}

func newTickTurnHarness(t *testing.T, name string) *tickTurnHarness {
	t.Helper()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, app.emit)
	app.triage.SetSendDeliveredHook(app.onSendDelivered)
	app.triage.SetQuietTurnClassifier(app.agentTurnIsTick)
	thread := testThread(name)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	h := &tickTurnHarness{t: t, app: app, thread: thread.ID}
	h.handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
	return h
}

func (h *tickTurnHarness) handle(evt provider.ProviderEvent) {
	h.t.Helper()
	evt.ThreadID, evt.Timestamp = h.thread, time.Now()
	if err := h.app.triage.Handle(evt); err != nil {
		h.t.Fatalf("%s: %v", evt.Kind, err)
	}
}

// send writes a message into the running turn: its row, persisted at the
// next turn's index, and its pending send naming its members.
func (h *tickTurnHarness) send(rowID, uuid, meta string) {
	h.t.Helper()
	now := time.Now().UnixMilli()
	if _, err := h.app.store.AppendItem(store.Item{
		ID: rowID, ThreadID: h.thread, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "x", Meta: meta, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		h.t.Fatal(err)
	}
	h.app.triage.RegisterPendingSendWithExpectation(h.thread, rowID, 1, triage.PendingSendExpectation{
		ProviderItemID: uuid, Members: usermessage.MemberSendIDsOfRow(meta),
	})
	h.lifecycle(uuid, provider.CommandQueued)
}

func (h *tickTurnHarness) lifecycle(uuid string, state provider.CommandLifecycleState) {
	h.t.Helper()
	meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: uuid, State: state})
	if err != nil {
		h.t.Fatal(err)
	}
	h.handle(provider.ProviderEvent{Kind: provider.EventCommandLifecycle, ItemID: uuid, Meta: meta})
}

func (h *tickTurnHarness) echo(uuid string) {
	h.t.Helper()
	meta, err := json.Marshal(map[string]any{"provider_item_id": uuid})
	if err != nil {
		h.t.Fatal(err)
	}
	h.handle(provider.ProviderEvent{Kind: provider.EventUserText, ItemID: uuid, Content: "x", Meta: meta})
}

func (h *tickTurnHarness) init(opener provider.TurnOpener) {
	h.t.Helper()
	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: opener})
	if err != nil {
		h.t.Fatal(err)
	}
	h.handle(provider.ProviderEvent{Kind: provider.EventInit, Meta: meta})
}

func (h *tickTurnHarness) end() {
	h.t.Helper()
	h.handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
}

const (
	wakeMeta          = `{"sendId":"agent-wake:w1:1","origin":"agent-wake"}`
	personMeta        = `{"sendId":"send-1"}`
	idlessPersonJoin  = `{"sendId":"agent-wake:w1:1","joinedSendIds":["agent-wake:w1:1"],"origin":"agent-wake","joinedParts":[{"bytes":4},{"sendId":"agent-wake:w1:1","bytes":4,"origin":"agent-wake"}]}`
	personWithWakeIDs = `{"sendId":"agent-wake:w1:1","joinedSendIds":["agent-wake:w1:1","send-1"],"origin":"agent-wake"}`
)

// A turn is a tick by what it took in, whichever turn its rows show in and
// in whichever order Claude writes `started`, init and echo: queued wakes
// taken in make it their pickup, a turn that took nothing in is the
// provider's own and never one, and a person's message in it makes it
// theirs, an id-less one joined to a wake included. The start's mark
// follows what the turn took in before it opened.
func TestATurnIsATickByWhatItTookIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		meta string
		// pickup takes the message in after turn 0 ends.
		pickup      func(h *tickTurnHarness, uuid string)
		turn        int
		tick        bool
		tickStarted bool
	}{
		{name: "started, init, echo", meta: wakeMeta, turn: 1, tick: true, tickStarted: true, pickup: func(h *tickTurnHarness, uuid string) {
			h.lifecycle(uuid, provider.CommandStarted)
			h.init(provider.TurnOpenerCommand)
			h.echo(uuid)
		}},
		{name: "init, echo, started", meta: wakeMeta, turn: 1, tick: true, pickup: func(h *tickTurnHarness, uuid string) {
			h.init("")
			h.echo(uuid)
			h.lifecycle(uuid, provider.CommandStarted)
		}},
		{name: "a self-woken turn first", meta: wakeMeta, turn: 2, tick: true, tickStarted: true, pickup: func(h *tickTurnHarness, uuid string) {
			h.init("")
			if got := h.app.triage.OpenTurnIndex(h.thread); got != 1 {
				h.t.Fatalf("the self-woken turn is %d, want 1", got)
			}
			if h.app.agentWakeTickStarted(h.thread) {
				h.t.Fatal("the turn Claude woke itself for started as a tick")
			}
			h.end()
			if h.app.agentTurnIsTick(h.thread, 1) {
				h.t.Fatal("the turn Claude woke itself for, which took nothing in, is a tick")
			}
			h.lifecycle(uuid, provider.CommandStarted)
			h.init(provider.TurnOpenerCommand)
			h.echo(uuid)
		}},
		{name: "a person's message", meta: personMeta, turn: 1, pickup: func(h *tickTurnHarness, uuid string) {
			h.lifecycle(uuid, provider.CommandStarted)
			h.init(provider.TurnOpenerCommand)
			h.echo(uuid)
		}},
		{name: "a person's message joined to a wake", meta: personWithWakeIDs, turn: 1, pickup: func(h *tickTurnHarness, uuid string) {
			h.lifecycle(uuid, provider.CommandStarted)
			h.init(provider.TurnOpenerCommand)
			h.echo(uuid)
		}},
		{name: "an id-less person's message joined to a wake", meta: idlessPersonJoin, turn: 1, pickup: func(h *tickTurnHarness, uuid string) {
			h.lifecycle(uuid, provider.CommandStarted)
			h.init(provider.TurnOpenerCommand)
			h.echo(uuid)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTickTurnHarness(t, "tick-"+strings.ReplaceAll(tc.name, " ", "-"))
			h.send("user:1", "uuid-1", tc.meta)
			h.end()
			if h.app.agentTurnIsTick(h.thread, 0) {
				t.Fatal("the turn the message was written during took it in")
			}
			tc.pickup(h, "uuid-1")
			if got := h.app.triage.OpenTurnIndex(h.thread); got != tc.turn {
				t.Fatalf("the turn that took the message in is %d, want %d", got, tc.turn)
			}
			if got := h.app.agentWakeTickStarted(h.thread); got != tc.tickStarted {
				t.Fatalf("the turn started as a tick = %v, want %v", got, tc.tickStarted)
			}
			h.end()
			if got := h.app.agentTurnIsTick(h.thread, tc.turn); got != tc.tick {
				t.Fatalf("turn %d tick = %v, want %v", tc.turn, got, tc.tick)
			}
		})
	}
}

// What a turn took in stays for a later round of it, and goes once a later
// turn ends and with the session.
func TestATurnsIntakeOutlivesItsRoundsAndNotTheNextTurn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	const thread = "intake-rounds"
	app.noteAgentTurnIntake(thread, triage.SendDelivery{UserItemID: "user:3", TurnIndex: 3, Members: []string{"agent-wake:w1:3"}})
	app.noteAgentTurnIntake(thread, triage.SendDelivery{UserItemID: "user:9", AwaitingTurn: true, Members: []string{"send-1"}})
	if !app.agentTurnIsTick(thread, 3) {
		t.Fatal("the pickup turn is not a tick")
	}
	app.endAgentWakeTickTurn(thread, 3)
	if !app.agentTurnIsTick(thread, 3) {
		t.Fatal("a later round of the pickup turn is not a tick")
	}
	if app.agentTurnIsTick(thread, 4) {
		t.Fatal("the turn after the pickup, which took nothing in, is a tick")
	}
	app.endAgentWakeTickTurn(thread, 4)
	if app.agentTurnIsTick(thread, 3) {
		t.Fatal("the pickup outlived the turn after it")
	}
	app.noteAgentTurnIntake(thread, triage.SendDelivery{UserItemID: "user:5", TurnIndex: 5, Members: []string{"agent-wake:w1:5"}})
	app.noteAgentTurnIntake(thread, triage.SendDelivery{UserItemID: "user:6", TurnIndex: 6, Rebind: true, ReboundFrom: 5, Members: []string{"send-2"}})
	if app.agentTurnIsTick(thread, 5) || app.agentTurnIsTick(thread, 6) {
		t.Fatal("a merge's rebind left the emptied turn a tick or made the person's turn one")
	}
	app.forgetAgentWakeTurns(thread)
	app.agentWakes.mu.Lock()
	defer app.agentWakes.mu.Unlock()
	if len(app.agentWakes.intake) != 0 {
		t.Fatalf("intake after the session ended = %v", app.agentWakes.intake)
	}
}
