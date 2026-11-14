package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// unechoedSendHarness drives one Claude thread through the wire order the
// 2.1.284 CLI produces for a turn-opening prompt whose replay echo its filter
// withheld (claude-wire.md §Outbound user message).
type unechoedSendHarness struct {
	t      *testing.T
	router *Router
	st     *store.Store
}

func newUnechoedSendHarness(t *testing.T) *unechoedSendHarness {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	return &unechoedSendHarness{t: t, router: router, st: st}
}

func (h *unechoedSendHarness) handle(evt provider.ProviderEvent) {
	h.t.Helper()
	evt.ThreadID = "t1"
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	if err := h.router.Handle(evt); err != nil {
		h.t.Fatalf("handle %s: %v", evt.Kind, err)
	}
}

func (h *unechoedSendHarness) userText(uuid string, extra map[string]any) {
	h.t.Helper()
	fields := map[string]any{"provider_item_id": uuid}
	for k, v := range extra {
		fields[k] = v
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		h.t.Fatal(err)
	}
	h.handle(provider.ProviderEvent{Kind: provider.EventUserText, Meta: meta})
}

func (h *unechoedSendHarness) echo(uuid string) { h.userText(uuid, nil) }

func (h *unechoedSendHarness) transcriptRecord(uuid string) {
	h.userText(uuid, map[string]any{provider.MetaUserTextTranscriptRecordedKey: true})
}

func (h *unechoedSendHarness) lifecycle(uuid string, state provider.CommandLifecycleState) {
	h.t.Helper()
	h.handle(commandLifecycleEvent("t1", uuid, state))
}

func (h *unechoedSendHarness) answer(text string) {
	h.handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: text})
	h.handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
}

func (h *unechoedSendHarness) directSend(turn int, uuid, text string) {
	h.t.Helper()
	now := time.Now().UnixMilli()
	item := store.Item{ID: fmt.Sprintf("user:%d", turn), ThreadID: "t1", TurnIndex: turn, Kind: "user_text", Role: "user", Status: "completed", Summary: text, CreatedAt: now, UpdatedAt: now}
	if _, err := h.st.AppendItem(item); err != nil {
		h.t.Fatalf("append user row: %v", err)
	}
	h.router.RegisterPendingSendWithExpectation("t1", item.ID, turn, PendingSendExpectation{ProviderItemID: uuid})
}

// queueWake dispatches a deferred flush row, the shape a thread_reply wake
// takes when it reaches an idle thread.
func (h *unechoedSendHarness) queueWake(turn int, id, uuid, text string) {
	now := time.Now().UnixMilli()
	item := store.Item{ID: id, ThreadID: "t1", TurnIndex: turn, Kind: "user_text", Role: "user", Status: "completed", Summary: text, CreatedAt: now, UpdatedAt: now}
	h.router.RegisterPendingFlushSendWithExpectation("t1", "queue:"+id, item, 0, PendingSendExpectation{ProviderItemID: uuid})
}

// turn returns turnIndex's rows in timeline order as "id=summary".
func (h *unechoedSendHarness) turn(turnIndex int) []string {
	h.t.Helper()
	items, err := h.st.ListTurnItems("t1", turnIndex)
	if err != nil {
		h.t.Fatalf("list turn %d: %v", turnIndex, err)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID+"="+item.Summary)
	}
	return out
}

func (h *unechoedSendHarness) wantTurn(turnIndex int, want ...string) {
	h.t.Helper()
	got := h.turn(turnIndex)
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		h.t.Fatalf("turn %d rows:\n got  %q\n want %q", turnIndex, got, want)
	}
	row, found, err := h.st.GetTurnByThreadIndex("t1", turnIndex)
	if err != nil || !found || row.CompletedAt == nil {
		h.t.Fatalf("turn %d row found=%v completed=%v err=%v; want a settled turns row", turnIndex, found, found && row.CompletedAt != nil, err)
	}
}

func (h *unechoedSendHarness) wantIdle() {
	h.t.Helper()
	if h.router.HasPendingSendForThread("t1") {
		head, _ := h.router.PeekPendingSendHeadForTest("t1")
		h.t.Fatalf("send %s still pending; the thread reads as running", head.AOItemID)
	}
	if h.router.AnyUnfinishedWork(0) {
		h.t.Fatal("router reports unfinished work after every turn settled")
	}
}

// The incident: a wake reaches an idle thread, opens its own turn, and the
// CLI withholds the echo because the text names a filtered tag. The
// transcript record confirms it, the row lands at the head of its turn, and
// the session is idle afterwards.
func TestTurnOpenerWithoutEchoIsConfirmedByTranscriptRecord(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.directSend(0, "u-0", "hello")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.echo("u-0")
	h.answer("answer zero")

	h.queueWake(1, "user:flush:wake", "u-wake", "Reply: see `<task-notification>`")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.transcriptRecord("u-wake")
	h.answer("answer to the wake")

	h.wantTurn(1, "user:flush:wake=Reply: see `<task-notification>`", "text:1:0=answer to the wake")
	h.wantIdle()
}

// The echo of a send its transcript record already confirmed is a duplicate:
// no second row, no injected-context row.
func TestEchoAfterTranscriptRecordIsDeduplicated(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.queueWake(0, "user:flush:a", "u-a", "first")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.transcriptRecord("u-a")
	h.echo("u-a")
	h.answer("answer a")

	h.wantTurn(0, "user:flush:a=first", "text:0:0=answer a")
	h.wantIdle()
}

// A record naming no pending send (another session's prompt, a send this
// router already confirmed) is never content.
func TestUnmatchedTranscriptRecordWritesNothing(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.directSend(0, "u-0", "hello")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.transcriptRecord("u-foreign")
	h.echo("u-0")
	h.answer("answer zero")

	h.wantTurn(0, "user:0=hello", "text:0:0=answer zero")
	h.wantIdle()
}

// A send nothing ever confirms opens one turn and no more. Later sends get
// turns of their own instead of reopening its index, which reset that turn's
// counters and overwrote its rows in the incident.
func TestUnconfirmedSendOpensOnlyItsOwnTurn(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.queueWake(0, "user:flush:wake", "u-wake", "lost")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.answer("answer to the wake")

	h.directSend(1, "u-1", "g")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.echo("u-1")
	h.answer("answer to g")

	h.directSend(2, "u-2", "next")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.echo("u-2")
	h.answer("answer to next")

	h.wantTurn(0, "text:0:0=answer to the wake")
	h.wantTurn(1, "user:1=g", "text:1:0=answer to g")
	h.wantTurn(2, "user:2=next", "text:2:0=answer to next")
}

// With neither a record nor an echo, the command_lifecycle bracket proves
// consumption: `started` then a terminal state. The row lands at the head
// of the turn it opened, and the thread is idle.
func TestLifecycleBracketConfirmsSendWithNoOtherConfirmation(t *testing.T) {
	for _, terminal := range []provider.CommandLifecycleState{provider.CommandCompleted, provider.CommandCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
			h := newUnechoedSendHarness(t)
			h.queueWake(0, "user:flush:wake", "u-wake", "wake")
			h.lifecycle("u-wake", provider.CommandQueued)
			h.lifecycle("u-wake", provider.CommandStarted)
			h.handle(provider.ProviderEvent{Kind: provider.EventInit})
			h.answer("answer to the wake")
			h.lifecycle("u-wake", terminal)

			h.wantTurn(0, "user:flush:wake=wake", "text:0:0=answer to the wake")
			h.wantIdle()
		})
	}
}

// A `cancelled` without `started` is a message the CLI never ran while the
// session lives on. It is never confirmed: it leaves the registry for the
// undelivered list, the app is told once, and nothing lands in the timeline.
func TestCancelledUnrunSendIsHandedToTheApp(t *testing.T) {
	h := newUnechoedSendHarness(t)
	signals := 0
	h.router.SetSendUndeliveredHook(func(threadID string) { signals++ })
	h.queueWake(0, "user:flush:wake", "u-wake", "wake")
	h.lifecycle("u-wake", provider.CommandQueued)
	h.lifecycle("u-wake", provider.CommandCancelled)

	if signals != 1 {
		t.Fatalf("app signalled %d times, want 1", signals)
	}
	h.wantIdle()
	returned := h.router.DrainUndeliveredSends("t1")
	if len(returned) != 1 || returned[0].UserItemID != "user:flush:wake" || returned[0].DeferredItem == nil || returned[0].Message != "wake" {
		t.Fatalf("returned = %+v, want the wake with its retained row", returned)
	}
	if again := h.router.DrainUndeliveredSends("t1"); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty", again)
	}
	if rows := h.turn(0); len(rows) != 0 {
		t.Fatalf("turn 0 rows = %q, want none", rows)
	}
}

// A direct send retains no copy of its message, so its row is read when it
// is retired: the restore needs it to delete the row and return the text.
func TestCancelledUnrunDirectSendCarriesItsRow(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.directSend(0, "u-0", "typed")
	h.lifecycle("u-0", provider.CommandQueued)
	h.lifecycle("u-0", provider.CommandCancelled)

	returned := h.router.DrainUndeliveredSends("t1")
	if len(returned) != 1 || returned[0].QuietItem == nil || returned[0].QuietItem.ID != "user:0" || returned[0].QuietItem.Summary != "typed" {
		t.Fatalf("returned = %+v, want the direct send with its row", returned)
	}
	if returned[0].EnqueuedAt == 0 {
		t.Fatal("direct send has no enqueue time; a batched restore would order it last")
	}
	h.wantIdle()
}

// `discarded` means the session is ending. The app is not told separately:
// the session-end drain takes the unrun sends with the rest of the queue,
// so one restore keeps queue order. A send whose turn a system/init already
// claimed leaves no turn open once the session's death settles it.
func TestDiscardedUnrunSendsJoinTheSessionEndDrain(t *testing.T) {
	h := newUnechoedSendHarness(t)
	signals := 0
	h.router.SetSendUndeliveredHook(func(threadID string) { signals++ })
	h.directSend(0, "u-0", "first")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.queueWake(1, "user:flush:wake", "u-wake", "wake")
	for _, uuid := range []string{"u-0", "u-wake"} {
		h.lifecycle(uuid, provider.CommandQueued)
	}
	for _, uuid := range []string{"u-0", "u-wake"} {
		h.lifecycle(uuid, provider.CommandDiscarded)
	}
	h.handle(provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})

	if signals != 0 {
		t.Fatalf("app signalled %d times for discarded sends, want 0", signals)
	}
	drained := h.router.DrainUnconfirmedFlushItems("t1")
	ids := make([]string, 0, len(drained))
	for _, item := range drained {
		ids = append(ids, item.UserItemID)
	}
	if strings.Join(ids, ",") != "user:0,user:flush:wake" {
		t.Fatalf("session-end drain = %q, want both discarded sends", ids)
	}
	h.wantIdle()
}

// Stop drops queued messages; an unrun send waiting to be returned goes with
// them.
func TestCleanupDropsUnrunSends(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.queueWake(0, "user:flush:wake", "u-wake", "wake")
	h.lifecycle("u-wake", provider.CommandQueued)
	h.lifecycle("u-wake", provider.CommandCancelled)
	h.router.CleanupThread("t1")
	if returned := h.router.DrainUndeliveredSends("t1"); len(returned) != 0 {
		t.Fatalf("returned after cleanup = %+v, want none", returned)
	}
}

// A send whose row was never written (the send failed after registering) is
// still consumed by its transcript record, and the echo that follows is a
// duplicate rather than new provider content.
func TestRecordedSendWithNoRowDedupesItsEcho(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.router.RegisterPendingSendWithExpectation("t1", "user:0", 0, PendingSendExpectation{ProviderItemID: "u-0"})
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.transcriptRecord("u-0")
	h.echo("u-0")
	h.answer("answer")

	items, err := h.st.ListItems("t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind != "assistant_text" {
			t.Fatalf("unexpected row %s kind=%s summary=%q", item.ID, item.Kind, item.Summary)
		}
	}
	h.wantIdle()
}

// `started` is recorded on the send itself, so a full correlation map
// cannot turn a delivered message into an unrun one.
func TestStartedIsKeptWhenTheLifecycleMapIsFull(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.router.SetSendUndeliveredHook(func(threadID string) {})
	for i := 0; i < maxCommandLifecycleEntriesPerThread; i++ {
		h.lifecycle(fmt.Sprintf("other-%d", i), provider.CommandQueued)
	}
	h.queueWake(0, "user:flush:wake", "u-wake", "wake")
	h.lifecycle("u-wake", provider.CommandQueued)
	h.lifecycle("u-wake", provider.CommandStarted)
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.answer("answer to the wake")
	h.lifecycle("u-wake", provider.CommandCancelled)

	if returned := h.router.DrainUndeliveredSends("t1"); len(returned) != 0 {
		t.Fatalf("a started send was returned to the composer: %+v", returned)
	}
	h.wantTurn(0, "user:flush:wake=wake", "text:0:0=answer to the wake")
}

// The ordinary order is untouched: when the record or echo confirmed the
// send, the bracket's terminal frame finds nothing to do.
func TestLifecycleBracketAfterConfirmationIsInert(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.queueWake(0, "user:flush:a", "u-a", "first")
	h.lifecycle("u-a", provider.CommandQueued)
	h.lifecycle("u-a", provider.CommandStarted)
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.transcriptRecord("u-a")
	h.echo("u-a")
	h.answer("answer a")
	h.lifecycle("u-a", provider.CommandCompleted)

	h.wantTurn(0, "user:flush:a=first", "text:0:0=answer a")
	h.wantIdle()
}

// A direct send the CLI drains into a turn it started itself (a background
// completion woke it) gets no system/init of its own. The wake is a turn of
// its own, the send takes the turn after it, and its confirmation opens
// that turn and settles the wake's, so each turn's output stays in it.
func TestDirectSendDrainedIntoProviderTurnOpensItsTurn(t *testing.T) {
	h := newUnechoedSendHarness(t)
	h.directSend(0, "u-0", "start the background work")
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.echo("u-0")
	h.answer("started it")

	// The CLI wakes on a background completion: a turn this app did not
	// ask for opens turn 1.
	h.handle(provider.ProviderEvent{Kind: provider.EventInit})
	h.handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "background done"})
	h.handle(provider.ProviderEvent{Kind: provider.EventContentBlockStop})
	h.directSend(2, "u-2", "one more thing")
	h.echo("u-2")
	h.answer("handled the addition")

	h.wantTurn(0, "user:0=start the background work", "text:0:0=started it")
	h.wantTurn(1, "text:1:0=background done")
	h.wantTurn(2, "user:2=one more thing", "text:2:0=handled the addition")
	h.wantIdle()
}
