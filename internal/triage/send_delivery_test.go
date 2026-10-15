package triage

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// deliveryLog records what the send-delivered hook reported.
type deliveryLog struct {
	mu   sync.Mutex
	seen []SendDelivery
	// awaiting are the reports of a send taken in before its turn opened.
	awaiting []SendDelivery
}

func (l *deliveryLog) record(_ string, delivery SendDelivery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if delivery.AwaitingTurn {
		l.awaiting = append(l.awaiting, delivery)
		return
	}
	l.seen = append(l.seen, delivery)
}

func (l *deliveryLog) awaitingSnapshot() []SendDelivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]SendDelivery(nil), l.awaiting...)
}

func (l *deliveryLog) snapshot() []SendDelivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]SendDelivery(nil), l.seen...)
}

// deliveryFixture is a thread mid-turn (turn 0 open) with one request's
// message flushed into it, the way the app dispatches a queued send while a
// turn runs: a quiet row at the running turn, its response registered at the
// next one.
type deliveryFixture struct {
	r      *Router
	st     *store.Store
	log    *deliveryLog
	thread string
	uuid   string
	row    store.Item
}

func newDeliveryFixture(t *testing.T, meta string) *deliveryFixture {
	t.Helper()
	r, st, _ := newTestRouter(t)
	const threadID = "t-delivery"
	createTestThread(t, st, threadID)
	f := &deliveryFixture{r: r, st: st, log: &deliveryLog{}, thread: threadID, uuid: "uuid-request-1"}
	r.SetSendDeliveredHook(f.log.record)
	f.turnStart(t)
	now := time.Now().UnixMilli()
	f.row = store.Item{
		ID: "user:0:flush:1", ThreadID: threadID, TurnIndex: 0, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "look at the crash report", Meta: meta, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.PersistAndRegisterPendingQuietFlushSendWithExpectation(
		threadID, "queue:1", f.row, 1, now, PendingSendExpectation{ProviderItemID: f.uuid}); err != nil {
		t.Fatalf("persist the flushed row: %v", err)
	}
	f.lifecycle(t, provider.CommandQueued)
	return f
}

func (f *deliveryFixture) handle(t *testing.T, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = f.thread
	evt.Timestamp = time.Now()
	if err := f.r.Handle(evt); err != nil {
		t.Fatalf("%s: %v", evt.Kind, err)
	}
}

func (f *deliveryFixture) turnStart(t *testing.T) {
	t.Helper()
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
}

func (f *deliveryFixture) turnComplete(t *testing.T) {
	t.Helper()
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
}

func (f *deliveryFixture) lifecycle(t *testing.T, state provider.CommandLifecycleState) {
	t.Helper()
	f.handle(t, commandLifecycleEvent(f.thread, f.uuid, state))
}

func (f *deliveryFixture) echo(t *testing.T) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"provider_item_id": f.uuid})
	if err != nil {
		t.Fatal(err)
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventUserText, ItemID: f.uuid, Content: f.row.Summary, Meta: meta})
}

// assertDelivered checks the hook reported exactly one delivery, into turn,
// and whether that turn opened for the message.
func (f *deliveryFixture) assertDelivered(t *testing.T, turn int, ownTurn bool, sendIDs ...string) {
	t.Helper()
	seen := f.log.snapshot()
	if len(seen) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one", seen)
	}
	got := seen[0]
	if got.TurnIndex != turn || got.OwnTurn != ownTurn || got.UserItemID != f.row.ID || len(got.SendIDs) != len(sendIDs) {
		t.Fatalf("delivery = %+v, want %s into turn %d (own turn %v) with %v", got, f.row.ID, turn, ownTurn, sendIDs)
	}
	for i, id := range sendIDs {
		if got.SendIDs[i] != id {
			t.Fatalf("delivery send ids = %v, want %v", got.SendIDs, sendIDs)
		}
	}
}

const requestSendMeta = `{"sendId":"thread-request:tok-1"}`

// Drained into the running turn: `started` lands while that turn is open,
// and the message is delivered into it. The echo that follows is the same
// delivery, not a second one.
func TestSendDeliveryStartedMidTurnIsTheOpenTurn(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, requestSendMeta)
	f.lifecycle(t, provider.CommandStarted)
	f.assertDelivered(t, 0, false, "thread-request:tok-1")
	f.echo(t)
	f.turnComplete(t)
	f.assertDelivered(t, 0, false, "thread-request:tok-1")
}

// Held for a turn of its own, in the CLI's own order: the running turn's
// result, then `started` with no turn open, then the init of the turn the
// CLI opened for the message. The delivery is that turn, never the one the
// message was written during.
func TestSendDeliveryStartedBeforeInitIsTheTurnItOpens(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, requestSendMeta)
	f.turnComplete(t)
	f.lifecycle(t, provider.CommandStarted)
	if seen := f.log.snapshot(); len(seen) != 0 {
		t.Fatalf("delivered before the turn it runs in opened: %+v", seen)
	}
	// Taken in, which a session ending now must not report as unread.
	if awaiting := f.log.awaitingSnapshot(); len(awaiting) != 1 || !slices.Equal(awaiting[0].SendIDs, []string{"thread-request:tok-1"}) {
		t.Fatalf("reports of the send taken in before its turn = %+v, want one", awaiting)
	}
	f.turnStart(t)
	f.assertDelivered(t, 1, true, "thread-request:tok-1")
	f.echo(t)
	f.turnComplete(t)
	f.assertDelivered(t, 1, true, "thread-request:tok-1")
	if awaiting := f.log.awaitingSnapshot(); len(awaiting) != 1 {
		t.Fatalf("the send was reported taken in %d times, want once", len(awaiting))
	}
}

// Held, with the init first and the echo before `started`, the order the
// harness mock writes: the confirmation is the first signal, inside the turn
// the init opened.
func TestSendDeliveryEchoAfterInitIsTheTurnItOpened(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, requestSendMeta)
	f.turnComplete(t)
	f.turnStart(t)
	f.echo(t)
	f.assertDelivered(t, 1, true, "thread-request:tok-1")
	f.lifecycle(t, provider.CommandStarted)
	f.assertDelivered(t, 1, true, "thread-request:tok-1")
}

// A provider with no lifecycle frames (claude-tui, an older CLI, Codex)
// delivers at its confirmation, into the turn open then.
func TestSendDeliveryWithoutLifecycleIsTheConfirmationsTurn(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, requestSendMeta)
	f.echo(t)
	f.assertDelivered(t, 0, false, "thread-request:tok-1")
}

// One message joined from several queued sends is one delivery that names
// every member: each request it carries is answered by the same turn.
func TestSendDeliveryOfAJoinedMessageNamesEveryMember(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, `{"sendId":"thread-request:tok-1","joinedSendIds":["thread-request:tok-1","composer-send-2","thread-request:tok-3"]}`)
	f.turnComplete(t)
	f.lifecycle(t, provider.CommandStarted)
	f.turnStart(t)
	f.assertDelivered(t, 1, true, "thread-request:tok-1", "composer-send-2", "thread-request:tok-3")
}

// A delivery waiting for a turn start belongs to the process that acked it.
// A replacement session's first turn is not that turn, and nothing reports.
func TestSendDeliveryWaitingForATurnDiesWithItsSession(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, requestSendMeta)
	f.turnComplete(t)
	f.lifecycle(t, provider.CommandStarted)
	f.r.MarkThreadActive(f.thread, "replacement")
	f.handle(t, fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart}, "replacement"))
	if seen := f.log.snapshot(); len(seen) != 0 {
		t.Fatalf("a dead session's delivery bound the replacement's turn: %+v", seen)
	}
}

// A message the CLI drains mid-loop, after a task-notification cascade
// re-opened a wire round on a settled turn: `started` arrives with no logical
// turn open and no init follows, so the echo is what opens the message's
// turn (openQueuedEchoTurn). The delivery is that turn, and it is the
// message's own, never the next unrelated turn to start.
func TestSendDeliveryStartedBeforeAMidLoopEchoIsTheTurnTheEchoOpens(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "t-cascade"
	createTestThread(t, st, threadID)
	log := &deliveryLog{}
	r.SetSendDeliveredHook(log.record)
	handle := func(evt provider.ProviderEvent) {
		t.Helper()
		evt.ThreadID = threadID
		evt.Timestamp = time.Now()
		if err := r.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	const uuid = "uuid-cascade"
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "turn 0 response"})
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	handle(provider.ProviderEvent{Kind: provider.EventInit})
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "cascade wake-up work"})
	r.RegisterPendingFlushSendWithExpectation(threadID, "queue:1", store.Item{
		ID: "user:1:flush:1", ThreadID: threadID, TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "look at the crash report", Meta: requestSendMeta,
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: uuid})
	handle(commandLifecycleEvent(threadID, uuid, provider.CommandQueued))
	handle(commandLifecycleEvent(threadID, uuid, provider.CommandStarted))
	if seen := log.snapshot(); len(seen) != 0 {
		t.Fatalf("delivered before the turn it runs in opened: %+v", seen)
	}
	meta, err := json.Marshal(map[string]any{"provider_item_id": uuid})
	if err != nil {
		t.Fatal(err)
	}
	handle(provider.ProviderEvent{Kind: provider.EventUserText, ItemID: uuid, Content: "look at the crash report", Meta: meta})
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "the stall is in the watchdog"})
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	seen := log.snapshot()
	if len(seen) != 1 || seen[0].TurnIndex != 1 || !seen[0].OwnTurn || seen[0].UserItemID != "user:1:flush:1" {
		t.Fatalf("deliveries = %+v, want one into turn 1, its own", seen)
	}
}

// deferredSendsFixture is a thread whose turn 0 has ended, with deferred
// sends dispatched while no turn ran: each reserved its own turn, 1, 2, ...
type deferredSendsFixture struct {
	r      *Router
	log    *deliveryLog
	thread string
	rows   []store.Item
}

func newDeferredSendsFixture(t *testing.T, metas ...string) *deferredSendsFixture {
	t.Helper()
	r, st, _ := newTestRouter(t)
	const threadID = "t-deferred"
	createTestThread(t, st, threadID)
	f := &deferredSendsFixture{r: r, log: &deliveryLog{}, thread: threadID}
	r.SetSendDeliveredHook(f.log.record)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "turn 0 response"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	for i, meta := range metas {
		turn := i + 1
		row := store.Item{
			ID: fmt.Sprintf("user:%d:flush:%d", turn, turn), ThreadID: threadID, TurnIndex: turn, Kind: "user_text",
			Role: "user", Status: "completed", Summary: fmt.Sprintf("message %d", turn), Meta: meta,
		}
		r.RegisterPendingFlushSendWithExpectation(threadID, fmt.Sprintf("queue:%d", turn), row, time.Now().UnixMilli(),
			PendingSendExpectation{ProviderItemID: f.uuid(turn)})
		f.rows = append(f.rows, row)
	}
	return f
}

func (f *deferredSendsFixture) uuid(turn int) string { return fmt.Sprintf("uuid-deferred-%d", turn) }

func (f *deferredSendsFixture) handle(t *testing.T, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = f.thread
	evt.Timestamp = time.Now()
	if err := f.r.Handle(evt); err != nil {
		t.Fatalf("%s: %v", evt.Kind, err)
	}
}

func (f *deferredSendsFixture) started(t *testing.T, turn int) {
	t.Helper()
	f.handle(t, commandLifecycleEvent(f.thread, f.uuid(turn), provider.CommandQueued))
	f.handle(t, commandLifecycleEvent(f.thread, f.uuid(turn), provider.CommandStarted))
}

func (f *deferredSendsFixture) echo(t *testing.T, turn int) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"provider_item_id": f.uuid(turn)})
	if err != nil {
		t.Fatal(err)
	}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventUserText, ItemID: f.uuid(turn), Content: f.rows[turn-1].Summary, Meta: meta})
}

// deliveredInto reports the turn each send was delivered into, and whether it
// was its own, keyed by row id.
func (f *deferredSendsFixture) deliveredInto() map[string]SendDelivery {
	out := make(map[string]SendDelivery)
	for _, delivery := range f.log.snapshot() {
		out[delivery.UserItemID] = delivery
	}
	return out
}

// Two deferred sends, each with its own reserved turn. The CLI runs the first
// and drains the second mid-loop into its round: `started` for the second
// lands while the first's turn is open, and its echo then opens its own turn.
// The second is delivered into its own turn, never the first's.
func TestSendDeliveryOfASecondDeferredSendIsItsOwnTurn(t *testing.T) {
	t.Parallel()
	f := newDeferredSendsFixture(t, `{"sendId":"composer-send-1"}`, `{"sendId":"thread-request:tok-b"}`)
	f.started(t, 1)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventInit})
	f.echo(t, 1)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "answer to the first"})
	f.started(t, 2)
	if got, ok := f.deliveredInto()[f.rows[1].ID]; ok {
		t.Fatalf("the second send was delivered into the first's turn: %+v", got)
	}
	f.echo(t, 2)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "answer to the second"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	got := f.deliveredInto()
	if first := got[f.rows[0].ID]; first.TurnIndex != 1 || !first.OwnTurn {
		t.Fatalf("first delivery = %+v, want turn 1, its own", first)
	}
	if second := got[f.rows[1].ID]; second.TurnIndex != 2 || !second.OwnTurn {
		t.Fatalf("second delivery = %+v, want turn 2, its own; all: %+v", second, f.log.snapshot())
	}
}

// Three deferred sends drained into one round, with both later `started`
// frames ahead of their echoes: each waits for its own turn, and one turn's
// start releases only the sends whose turn it is.
func TestSendDeliveryOfThreeDeferredSendsWaitsForEachOwnTurn(t *testing.T) {
	t.Parallel()
	f := newDeferredSendsFixture(t, `{"sendId":"composer-send-1"}`, `{"sendId":"thread-request:tok-b"}`, `{"sendId":"thread-request:tok-c"}`)
	f.started(t, 1)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventInit})
	f.echo(t, 1)
	f.started(t, 2)
	f.started(t, 3)
	f.echo(t, 2)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "answer to the second"})
	if got, ok := f.deliveredInto()[f.rows[2].ID]; ok {
		t.Fatalf("the third send was delivered before its own turn opened: %+v", got)
	}
	f.echo(t, 3)
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "answer to the third"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	got := f.deliveredInto()
	for i, row := range f.rows {
		if d := got[row.ID]; d.TurnIndex != i+1 || !d.OwnTurn {
			t.Fatalf("delivery of %s = %+v, want turn %d, its own; all: %+v", row.ID, d, i+1, f.log.snapshot())
		}
	}
}

// A request deferred behind a person's message sent straight to the idle
// thread: the person's message opens turn 1, the request reserved turn 2.
// Drained mid-loop into the person's round, the request is still delivered
// into its own turn, which its echo opens.
func TestSendDeliveryOfADeferredSendBehindAPersonsMessageIsItsOwnTurn(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "t-behind"
	createTestThread(t, st, threadID)
	log := &deliveryLog{}
	r.SetSendDeliveredHook(log.record)
	handle := func(evt provider.ProviderEvent) {
		t.Helper()
		evt.ThreadID = threadID
		evt.Timestamp = time.Now()
		if err := r.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	echo := func(uuid, text string) {
		t.Helper()
		meta, err := json.Marshal(map[string]any{"provider_item_id": uuid})
		if err != nil {
			t.Fatal(err)
		}
		handle(provider.ProviderEvent{Kind: provider.EventUserText, ItemID: uuid, Content: text, Meta: meta})
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	now := time.Now().UnixMilli()
	person := store.Item{
		ID: "user:1", ThreadID: threadID, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "rename the flag", Meta: `{"sendId":"composer-send-1"}`, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := st.UpsertItem(person, nil); err != nil {
		t.Fatalf("persist the person's row: %v", err)
	}
	r.RegisterPendingSendWithExpectation(threadID, person.ID, 1, PendingSendExpectation{ProviderItemID: "uuid-person"})
	request := store.Item{
		ID: "user:2:flush:2", ThreadID: threadID, TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "check the changelog", Meta: requestSendMeta,
	}
	r.RegisterPendingFlushSendWithExpectation(threadID, "queue:2", request, now, PendingSendExpectation{ProviderItemID: "uuid-request"})

	handle(provider.ProviderEvent{Kind: provider.EventInit})
	echo("uuid-person", person.Summary)
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "renamed it"})
	handle(commandLifecycleEvent(threadID, "uuid-request", provider.CommandQueued))
	handle(commandLifecycleEvent(threadID, "uuid-request", provider.CommandStarted))
	echo("uuid-request", request.Summary)
	handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "the changelog lists it"})
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
	for _, d := range log.snapshot() {
		if d.UserItemID == request.ID {
			if d.TurnIndex != 2 || !d.OwnTurn {
				t.Fatalf("request delivery = %+v, want turn 2, its own", d)
			}
			return
		}
	}
	t.Fatalf("the request was never delivered: %+v", log.snapshot())
}
