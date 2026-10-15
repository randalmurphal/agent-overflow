package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// A request sent into a busy thread is written to the provider during that
// thread's turn, but the provider decides which turn reads it: Claude can
// drain it into the running turn or hold it for a turn of its own. These
// tests drive a real dispatch into a session that records its stdin, then
// the provider's frames through the session's own event handler, so the
// turn observer, the delivery hook and the settlement all run as they do in
// production.

// heldRequestTarget is a Claude thread mid-turn (turn 0 open) with a live
// session that records what it is sent and answers nothing.
type heldRequestTarget struct {
	f      *requestFixture
	thread store.Thread
	handle func(provider.ProviderEvent)
	// stdinLog is every line the session wrote to the fake CLI.
	stdinLog string
	// sessionEvents receives the session's own events, its disconnect at
	// Close among them, once a test hands them to the handler
	// (handSessionEventsToHandler).
	sessionEvents *atomic.Pointer[func(provider.ProviderEvent)]
}

// handSessionEventsToHandler routes the session's own events to the handler,
// as production does, so a close delivers the session's disconnect in the
// order a real close does.
func (h *heldRequestTarget) handSessionEventsToHandler() {
	handle := h.handle
	h.sessionEvents.Store(&handle)
}

// forwardTo returns a session event callback that hands each event to what
// events holds, and drops it while that is empty.
func forwardTo(events *atomic.Pointer[func(provider.ProviderEvent)]) func(provider.ProviderEvent) {
	return func(evt provider.ProviderEvent) {
		if handle := events.Load(); handle != nil {
			(*handle)(evt)
		}
	}
}

func newHeldRequestTarget(t *testing.T, f *requestFixture) *heldRequestTarget {
	t.Helper()
	// A wake to a caller with no live session starts one, so the caller
	// gets a mock provider as the other wake tests give it.
	f.mockClaude(t, "read the wake")
	workspace := initGitRepo(t)
	thread, err := createTestThread(t, f.app, string(provider.Claude), workspace, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sessionEvents := &atomic.Pointer[func(provider.ProviderEvent)]{}
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: workspace},
		forwardTo(sessionEvents))
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude), Token: "held-tok", Claude: sess,
		Liveness: newSessionLiveness(time.Now()),
	})
	// The fixture's router is built bare; these are the two hooks this flow
	// crosses, wired as configureTriageQueueCallbacks wires them. The
	// dispatcher is left out so each test chooses when the queue flushes.
	f.app.triage.SetSendDeliveredHook(f.app.bindDeliveredRequests)
	f.app.triage.SetSendUndeliveredHook(func(threadID string) { go f.app.restoreUndeliveredSends(threadID) })
	h := &heldRequestTarget{f: f, thread: thread, handle: startedSessionEventHandler(f.app, thread.ID, "held-tok", string(provider.Claude)), stdinLog: stdinLog, sessionEvents: sessionEvents}
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	return h
}

// callerMidTurn keeps the caller's own turn open, so each wake stays in its
// queue to be counted.
func callerMidTurn(t *testing.T, f *requestFixture) {
	t.Helper()
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
}

// frame hands one provider frame to the session's handler and waits until it
// and every settlement it started have run.
func (h *heldRequestTarget) frame(t *testing.T, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = h.thread.ID
	evt.Timestamp = time.Now()
	h.handle(evt)
	waitProviderEvents(t, h.f.app, h.thread.ID)
	h.f.app.threadRequestsWG.Wait()
}

func (h *heldRequestTarget) text(t *testing.T, itemID, text string) {
	t.Helper()
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTextDelta, ItemID: itemID, Content: text})
}

func (h *heldRequestTarget) turnComplete(t *testing.T) {
	t.Helper()
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
}

func (h *heldRequestTarget) lifecycle(t *testing.T, uuid string, state provider.CommandLifecycleState) {
	t.Helper()
	meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: uuid, State: state})
	if err != nil {
		t.Fatal(err)
	}
	h.frame(t, provider.ProviderEvent{Kind: provider.EventCommandLifecycle, ItemID: uuid, Meta: meta})
}

func (h *heldRequestTarget) echo(t *testing.T, uuid, text string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"provider_item_id": uuid})
	if err != nil {
		t.Fatal(err)
	}
	h.frame(t, provider.ProviderEvent{Kind: provider.EventUserText, ItemID: uuid, Content: text, Meta: meta})
}

// send queues one request into the busy target and flushes it to the
// session, returning its token and the wire uuid the dispatch minted.
func (h *heldRequestTarget) send(t *testing.T, messages ...string) ([]string, string) {
	t.Helper()
	tokens := make([]string, 0, len(messages))
	for _, message := range messages {
		ack, err := h.f.adapter().Send(t.Context(), h.f.callerIdentity(), threadtools.SendCall{
			ThreadID: h.thread.ID, Message: message, WaitSeconds: 0, Notify: true,
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		tokens = append(tokens, ack.Token)
	}
	queued := h.f.app.triage.QueuedFlushItems(h.thread.ID)
	if len(queued) != len(messages) {
		t.Fatalf("queued = %+v, want the requests waiting on the turn", queued)
	}
	h.f.app.dispatchFlush(h.thread.ID, queued)
	head, ok := h.f.app.triage.PeekPendingSendHeadForTest(h.thread.ID)
	if !ok || head.ExpectedProviderItemID == "" {
		t.Fatalf("no pending send carries the dispatched uuid: %+v", head)
	}
	for _, token := range tokens {
		if receipt := h.f.receipt(t, token); receipt.State != store.ThreadReceiptRunning || receipt.TurnID != "" {
			t.Fatalf("receipt after the flush = %+v, want running with no turn", receipt)
		}
	}
	return tokens, head.ExpectedProviderItemID
}

// wakes returns the wake rows queued for the caller about token, first and
// late.
func (f *requestFixture) wakes(t *testing.T, token string) []store.FlushQueueItem {
	t.Helper()
	var out []store.FlushQueueItem
	for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
		if row.SendID == threadWakeSendID(token) || row.SendID == threadWakeLateSendID(token) {
			out = append(out, row)
		}
	}
	return out
}

// The bug this file exists for. The CLI held the flushed request and ran it
// as its own next turn: the turn it was written during ended first, with
// text written before the request was read. That end settles nothing; the
// held turn's reply is the one answer, and the caller gets one wake.
func TestAHeldRequestIsAnsweredByTheTurnThatReadIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "where does the stall come from?")
	token := tokens[0]

	h.text(t, "a-turn0", "Still on the first task.")
	h.turnComplete(t)
	if row := f.request(t, token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request = %q after the turn it was written during, want still running", row.State)
	}
	if receipt := f.receipt(t, token); receipt.TurnID != "" {
		t.Fatalf("receipt bound to %q before its delivery", receipt.TurnID)
	}
	if wakes := f.wakes(t, token); len(wakes) != 0 {
		t.Fatalf("the caller was woken before the request was read: %+v", wakes)
	}

	// The CLI's order for a held message: `started`, then the init of the
	// turn it opens for it, then the echo.
	h.lifecycle(t, uuid, provider.CommandQueued)
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	h.echo(t, uuid, "where does the stall come from?")
	if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) || !receipt.OwnTurn {
		t.Fatalf("receipt bound to %q (own turn %v), want the turn the CLI opened for it", receipt.TurnID, receipt.OwnTurn)
	}

	ack, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, h.thread.ID), threadtools.ReplyCall{
		Token: token, Text: "The stall is in the watchdog.",
	})
	if err != nil || !ack.Accepted || ack.Late {
		t.Fatalf("Reply = %+v, %v; want the request's answer", ack, err)
	}
	h.text(t, "a-turn1", "Answered the caller.")
	h.turnComplete(t)

	if row := f.request(t, token); row.State != store.ThreadRequestReplied || !strings.Contains(string(row.Answer), "watchdog") {
		t.Fatalf("request = %q %q, want the reply", row.State, row.Answer)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(f.wakes(t, token)) > 0 })
	wakes := f.wakes(t, token)
	if len(wakes) != 1 || wakes[0].SendID != threadWakeSendID(token) {
		t.Fatalf("wakes = %+v, want exactly one", wakes)
	}
	if !strings.Contains(wakes[0].Message, "watchdog") || strings.Contains(wakes[0].Message, "Still on the first task") {
		t.Fatalf("wake = %q, want the reply and none of the earlier turn's text", wakes[0].Message)
	}
}

// Held and then answered without thread_reply: the held turn's own last
// message is the fallback answer, never the earlier turn's, and a reply
// after that still reaches the caller as a late reply.
func TestAHeldRequestFinishesOnItsOwnTurnAndTakesALateReply(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "what did the profiler say?")
	token := tokens[0]

	h.text(t, "a-turn0", "Still on the first task.")
	h.turnComplete(t)
	// The order the harness mock writes: init, echo, then `started`.
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	h.echo(t, uuid, "what did the profiler say?")
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.text(t, "a-turn1", "Looking into the profile now.")
	h.turnComplete(t)

	row := f.request(t, token)
	if row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "Looking into the profile") {
		t.Fatalf("request = %q %q, want it finished with the held turn's text", row.State, row.Answer)
	}
	if strings.Contains(string(row.Answer), "first task") {
		t.Fatalf("the answer is the earlier turn's text: %q", row.Answer)
	}
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, token).DeliveredAt != 0 })

	late, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, h.thread.ID), threadtools.ReplyCall{
		Token: token, Text: "the profiler blames the JSON decode",
	})
	if err != nil || !late.Accepted || !late.Late {
		t.Fatalf("late Reply = %+v, %v", late, err)
	}
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, token).LateDeliveredAt != 0 })
	wakes := f.wakes(t, token)
	if len(wakes) != 2 || wakes[0].SendID != threadWakeSendID(token) || wakes[1].SendID != threadWakeLateSendID(token) {
		t.Fatalf("wakes = %+v, want the answer and then the late reply", wakes)
	}
	if !strings.Contains(wakes[1].Message, "JSON decode") {
		t.Fatalf("late wake = %q", wakes[1].Message)
	}
}

// Drained into the running turn: `started` lands while that turn is open,
// the binding is that turn, and its end settles the request.
func TestARequestDrainedMidTurnSettlesWithThatTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]

	h.lifecycle(t, uuid, provider.CommandStarted)
	h.echo(t, uuid, "check the changelog")
	if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 0) || receipt.OwnTurn {
		t.Fatalf("receipt bound to %q (own turn %v), want the running turn it was drained into, not its own", receipt.TurnID, receipt.OwnTurn)
	}
	h.text(t, "a-turn0", "The changelog lists the fix.")
	h.turnComplete(t)
	row := f.request(t, token)
	if row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "changelog lists") {
		t.Fatalf("request = %q %q, want it finished with the turn it was drained into", row.State, row.Answer)
	}
}

// Two requests flushed together are one message: one delivery binds both to
// the held turn, a reply settles one, and that turn's end settles the
// other.
func TestRequestsFlushedTogetherShareTheHeldTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog", "and the migration notes")
	first, second := tokens[0], tokens[1]
	if a, b := f.receipt(t, first), f.receipt(t, second); a.MessageItemID != b.MessageItemID {
		t.Fatalf("message rows = %q / %q, want the one joined message", a.MessageItemID, b.MessageItemID)
	}

	h.text(t, "a-turn0", "Still on the first task.")
	h.turnComplete(t)
	for _, token := range tokens {
		if row := f.request(t, token); row.State != store.ThreadRequestRunning {
			t.Fatalf("request %s = %q after the earlier turn, want running", token, row.State)
		}
	}
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	for _, token := range tokens {
		if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) {
			t.Fatalf("receipt %s bound to %q, want the held turn", token, receipt.TurnID)
		}
	}
	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, h.thread.ID), threadtools.ReplyCall{
		Token: first, Text: "the changelog lists the fix",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	h.text(t, "a-turn1", "Both read.")
	h.turnComplete(t)
	if row := f.request(t, first); row.State != store.ThreadRequestReplied {
		t.Fatalf("first = %q, want replied", row.State)
	}
	if row := f.request(t, second); row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "Both read") {
		t.Fatalf("second = %q %q, want it finished by the held turn", row.State, row.Answer)
	}
}

// The session dies after the CLI started the held message but before the
// turn it opens for it: nothing will ever deliver it, and the request
// settles interrupted rather than running forever. The CLI had read it, so
// the answer says it was not answered rather than not read.
func TestARequestHeldWhenTheSessionDiesSettlesInterrupted(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]

	h.turnComplete(t)
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})

	row := f.awaitRequestState(t, token, store.ThreadRequestInterrupted)
	if !strings.Contains(string(row.Answer), "session ended before it answered") {
		t.Fatalf("answer = %q, want it read and not answered", row.Answer)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(f.wakes(t, token)) == 1 })
}

// The CLI retires the held message unrun while the session lives on: the
// request settles interrupted, and its text, an agent's and not the
// person's, stays out of the composer.
func TestARequestRetiredUnrunSettlesWhenItsMessageGoesBack(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]

	h.lifecycle(t, uuid, provider.CommandQueued)
	h.lifecycle(t, uuid, provider.CommandCancelled)
	row := f.awaitRequestState(t, token, store.ThreadRequestInterrupted)
	if string(row.Answer) != threadtools.InterruptNotTakenIn {
		t.Fatalf("answer = %q", row.Answer)
	}
	if draft, _, err := f.app.store.GetThreadDraft(h.thread.ID); err != nil || strings.Contains(draft.Content, "check the changelog") {
		t.Fatalf("draft = %q, %v; want the request's text kept out of it", draft.Content, err)
	}
}

// A cancel of a request whose message no turn has read yet stops nothing:
// the turn running meanwhile is not the request's, and the report says it
// keeps running.
func TestCancellingAnUndeliveredRequestLeavesTheRunningTurnAlone(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	tokens, _ := h.send(t, "check the changelog")

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: tokens[0]})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectLeftUnread {
		t.Fatalf("cancel effect = %q, want the request cancelled unread, nothing stopped", report.Effect)
	}
	if row := f.request(t, tokens[0]); row.State != store.ThreadRequestCancelled {
		t.Fatalf("request = %q, want cancelled", row.State)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the running turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

// A turn end that finds a dispatched receipt with no turn settles nothing,
// returns no error, and keeps watching for the turn its delivery names.
func TestTheObserverKeepsTheGateOfAnUndeliveredReceipt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	token := newThreadRequestToken()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.caller.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: target.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	f.adapter().markRequestDispatched(token, target.ID, target.ID+":u0")

	if _, err := f.app.settleReceiptOnTurnEnd(target.ID, token); err != nil {
		t.Fatalf("settleReceiptOnTurnEnd: %v", err)
	}
	if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("receipt = %q, want still running", receipt.State)
	}
	tokens, ok := f.app.runningReceiptTokens(target.ID)
	if !ok || len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("gate after the observer = %v (%v), want the receipt still watched", tokens, ok)
	}
}

// Codex has no hold: `turn/steer` puts the message into the active turn's
// pending input, and Codex reads pending input before that turn may end
// (codex-wire.md §`turn/steer` and client message identity). Its delivery
// is the steered message's own echo, inside that turn, and the turn's end
// settles the request. A steer that finds no active turn falls back to a
// fresh `turn/start`, whose turn is the message's own.
func TestASteeredCodexRequestSettlesWithTheTurnItJoined(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	c := newCodexRequestTarget(t, f)
	token, head := c.steer(t)
	if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptRunning || receipt.TurnID != "" {
		t.Fatalf("receipt after the steer = %+v, want running with no turn", receipt)
	}
	c.echo(t, head)
	if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(c.thread.ID, 0) || receipt.OwnTurn {
		t.Fatalf("receipt bound to %q (own turn %v), want the turn the steer joined, not its own", receipt.TurnID, receipt.OwnTurn)
	}
	c.frame(t, provider.ProviderEvent{Kind: provider.EventTextDelta, ItemID: "codex-a0", Content: "The changelog lists the fix."})
	c.frame(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnID: "codex-turn-0", TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
	row := f.request(t, token)
	if row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "changelog lists") {
		t.Fatalf("request = %q %q, want it finished by the turn it joined", row.State, row.Answer)
	}
}

// codexRequestTarget is a Codex thread mid-turn (codex-turn-0 open) with a
// live session that accepts turn/steer, for a request steered into the
// running turn.
type codexRequestTarget struct {
	f      *requestFixture
	thread store.Thread
	handle func(provider.ProviderEvent)
	// sessionEvents is heldRequestTarget.sessionEvents for this session.
	sessionEvents *atomic.Pointer[func(provider.ProviderEvent)]
}

func (c *codexRequestTarget) handSessionEventsToHandler() {
	handle := c.handle
	c.sessionEvents.Store(&handle)
}

func newCodexRequestTarget(t *testing.T, f *requestFixture) *codexRequestTarget {
	t.Helper()
	f.mockClaude(t, "read the wake")
	callerMidTurn(t, f)
	f.app.triage.SetSendDeliveredHook(f.app.bindDeliveredRequests)
	thread, err := createTestThread(t, f.app, string(provider.Codex), t.TempDir(), "gpt-5.5", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	sessionEvents := &atomic.Pointer[func(provider.ProviderEvent)]{}
	sess := installSteerTestSessionWithEvents(t, f.app, thread, "ok", forwardTo(sessionEvents))
	f.app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "codex-tok", Codex: sess})
	c := &codexRequestTarget{
		f: f, thread: thread, sessionEvents: sessionEvents,
		handle: startedSessionEventHandler(f.app, thread.ID, "codex-tok", string(provider.Codex)),
	}
	c.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: "codex-turn-0"})
	return c
}

// frame hands one provider frame to the session's handler and waits until it
// and every settlement it started have run.
func (c *codexRequestTarget) frame(t *testing.T, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = c.thread.ID
	evt.Timestamp = time.Now()
	c.handle(evt)
	waitProviderEvents(t, c.f.app, c.thread.ID)
	c.f.app.threadRequestsWG.Wait()
}

// steer sends one request into the running turn and dispatches it as a
// steer, returning its token and the pending send its echo confirms.
func (c *codexRequestTarget) steer(t *testing.T) (string, triage.PendingSendSnapshot) {
	t.Helper()
	ack, err := c.f.adapter().Send(t.Context(), c.f.callerIdentity(), threadtools.SendCall{
		ThreadID: c.thread.ID, Message: "check the changelog", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued := c.f.app.triage.QueuedFlushItems(c.thread.ID)
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want the request waiting on the turn", queued)
	}
	c.f.app.dispatchFlush(c.thread.ID, queued)
	head, ok := c.f.app.triage.PeekPendingSendHeadForTest(c.thread.ID)
	if !ok {
		t.Fatal("the steer left no pending send")
	}
	return ack.Token, head
}

// echo is Codex's echo of the steered message, inside the turn it joined.
func (c *codexRequestTarget) echo(t *testing.T, head triage.PendingSendSnapshot) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"provider_item_id": "codex-user-1", "client_id": head.AOItemID})
	if err != nil {
		t.Fatal(err)
	}
	c.frame(t, provider.ProviderEvent{Kind: provider.EventUserText, ItemID: "codex-user-1", Content: "check the changelog", Meta: meta})
}

// A restore that takes back a joined message before a turn read it: on a
// session death the request in it goes back to its queue; on a failed Codex
// resend, with the session alive, it settles with its text gone nowhere; and
// on either, a message that does not split leaves the request's text in the
// person's composer and settles it saying so. A delivered request in the same
// message is left to its turn.
func TestARequestARestoreTakesBackIsRequeuedOrSettled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path    string
		parts   bool
		requeue bool
		want    string
	}{
		{path: "session death", parts: true, requeue: true},
		{path: "failed resend", parts: true, want: threadtools.InterruptNotTakenIn},
		{path: "session death", want: threadtools.InterruptNotTakenInToComposer},
		{path: "failed resend", want: threadtools.InterruptNotTakenInToComposer},
	} {
		name := tc.path
		if !tc.parts {
			name += ", no parts"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			target, err := createTestThread(t, f.app, string(provider.Codex), t.TempDir(), "gpt-5.5", threadmode.ModeChat)
			if err != nil {
				t.Fatalf("create target: %v", err)
			}
			undelivered, delivered := newThreadRequestToken(), newThreadRequestToken()
			for _, token := range []string{undelivered, delivered} {
				if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
					Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
					TargetThreadID: target.ID, State: store.ThreadRequestAccepted,
				}); err != nil {
					t.Fatalf("InsertThreadRequest: %v", err)
				}
				if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
					Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.caller.ID,
					Kind: store.ThreadRequestSend, TargetThreadID: target.ID,
				}); err != nil {
					t.Fatalf("AcceptThreadRequestReceipt: %v", err)
				}
				f.adapter().markRequestDispatched(token, target.ID, "user:1:flush:1")
			}
			f.app.bindRequestDelivery(delivered, target.ID, "user:1:flush:1", 1, true)

			ids := []string{threadRequestSendID(undelivered), threadRequestSendID(delivered)}
			const first, second = "check the changelog", "and the release notes"
			fields := usermessage.Meta{SendID: ids[0], JoinedSendIDs: ids}
			if tc.parts {
				fields.JoinedParts = []usermessage.JoinedPart{
					{SendID: ids[0], Bytes: len(first), Origin: usermessage.OriginAgentThread},
					{SendID: ids[1], Bytes: len(second), Origin: usermessage.OriginAgentThread},
				}
			}
			meta, err := usermessage.MarshalMeta(fields)
			if err != nil {
				t.Fatal(err)
			}
			text := first + usermessage.JoinSeparator + second
			if tc.path == "session death" {
				row := store.Item{ThreadID: target.ID, ID: "user:1:flush:1", Summary: text, Meta: meta}
				f.app.restoreFlushItemsToDraftLocked(target.ID, []triage.UnconfirmedFlushItem{
					{UserItemID: row.ID, Message: row.Summary, EnqueuedAt: 1000, QuietItem: &row},
				}, queueRestoredReasonSessionDied)
			} else {
				f.app.restoreEagerPersistedFlushesToDraft(target.ID, []triage.EagerPersistedFlush{
					{UserItemID: "user:1:flush:1", Content: text, Meta: meta, EnqueuedAt: 1000},
				})
			}
			f.app.threadRequestsWG.Wait()

			if tc.requeue {
				assertRequestOwedAgain(t, f, target.ID, undelivered)
				if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 1 || rows[0].Message != first {
					t.Fatalf("queue = %+v, want the request's own text alone", rows)
				}
			} else {
				row := f.request(t, undelivered)
				if row.State != store.ThreadRequestInterrupted || string(row.Answer) != tc.want {
					t.Fatalf("undelivered request = %q %q, want interrupted with %q", row.State, row.Answer, tc.want)
				}
			}
			if receipt := f.receipt(t, delivered); receipt.State != store.ThreadReceiptRunning {
				t.Fatalf("delivered receipt = %q, want it left to its turn", receipt.State)
			}
		})
	}
}

// A Stop drops the session's queue. A request the CLI was handed and had not
// taken in goes back to its queue, owed again, with its timeline row gone,
// and so does one still queued behind it; neither sender hears anything.
func TestARequestHeldWhenTheThreadIsStoppedGoesBackToItsQueue(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]
	h.lifecycle(t, uuid, provider.CommandQueued)
	queuedAck, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: h.thread.ID, Message: "then the release notes", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := f.app.StopSession(h.thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	for _, token := range []string{token, queuedAck.Token} {
		assertRequestOwedAgain(t, f, h.thread.ID, token)
	}
	assertNothingPending(t, f.app, h.thread.ID)
}

// assertNothingPending checks that a thread with no session reads as not
// working, its owed messages queued for the next start notwithstanding.
func assertNothingPending(t *testing.T, app *App, threadID string) {
	t.Helper()
	if answer := app.publishSendsPending(threadID); answer.Pending {
		t.Fatalf("sends pending = %+v with no session", answer)
	}
	rows, err := app.ListThreadLiveActivity()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ThreadID == threadID && row.SendsPending {
			t.Fatalf("live activity = %+v, want nothing pending", row)
		}
	}
}

// The idle reaper tears a session down holding the thread's mutation lock,
// which queueing a message takes. The requests it took back are queued once
// the reaper lets go, not under it.
func TestAReapedSessionQueuesItsRequestsAgainOnceItsLockIsReleased(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	h.lifecycle(t, uuid, provider.CommandQueued)

	unlock := f.app.threadApplication().LockMutations(h.thread.ID)
	sess, _ := f.app.sessionManager().take(h.thread.ID)
	done := make(chan error, 1)
	go func() { done <- f.app.teardownAndCloseSessionHoldingMutations(h.thread.ID, sess) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("teardown: %v", err)
		}
	case <-time.After(10 * time.Second):
		unlock()
		t.Fatal("the teardown waited on the mutation lock its caller holds")
	}
	unlock()
	f.app.threadRequestsWG.Wait()
	assertRequestOwedAgain(t, f, h.thread.ID, tokens[0])
}

// A session that starts while the reaper still holds the lock (a wake, the
// person) flushes the queue before the reaped requests are back on it. Live
// and idle, it has no turn boundary coming, so the requeue sends them.
func TestAReapedRequestGoesToASessionThatStartedBeforeItWasQueuedAgain(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	h.lifecycle(t, uuid, provider.CommandQueued)
	// The fixture's router has no dispatcher; a flush needs it.
	f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)

	unlock := f.app.threadApplication().LockMutations(h.thread.ID)
	sess, _ := f.app.sessionManager().take(h.thread.ID)
	if err := f.app.teardownAndCloseSessionHoldingMutations(h.thread.ID, sess); err != nil {
		unlock()
		t.Fatalf("teardown: %v", err)
	}
	stdin := filepath.Join(t.TempDir(), "next-session-stdin.jsonl")
	next, err := claude.NewSession(context.Background(), h.thread.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdin), WorkDir: h.thread.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	f.app.sessionManager().put(h.thread.ID, session{Provider: string(provider.Claude), Token: "next-session", Claude: next, Liveness: newSessionLiveness(time.Now())})
	unlock()
	f.app.threadRequestsWG.Wait()

	waitUntil(t, 10*time.Second, func() bool {
		sent, _ := os.ReadFile(stdin)
		return strings.Contains(string(sent), "check the changelog")
	})
	if queued := f.app.triage.QueuedFlushItems(h.thread.ID); len(queued) != 0 {
		t.Fatalf("%d still queued after the flush", len(queued))
	}
	if wakes := f.wakes(t, tokens[0]); len(wakes) != 0 {
		t.Fatalf("the sender was woken: %+v", wakes)
	}
}

// assertRequestOwedAgain checks that a request is back where it started:
// accepted on both sides, its message queued and not in the timeline, its
// sender told nothing and the person's composer without it.
func assertRequestOwedAgain(t *testing.T, f *requestFixture, threadID, token string) {
	t.Helper()
	if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptAccepted || receipt.MessageItemID != "" {
		t.Fatalf("receipt = %+v, want it accepted with no message", receipt)
	}
	if row := f.request(t, token); row.State != store.ThreadRequestAccepted {
		t.Fatalf("request = %q, want accepted", row.State)
	}
	record, found, err := f.app.findRecordedSend(threadID, threadRequestSendID(token))
	if err != nil || !found || record.dispatched {
		t.Fatalf("request message = %+v found=%v err=%v, want it queued and not in the timeline", record, found, err)
	}
	if wakes := f.wakes(t, token); len(wakes) != 0 {
		t.Fatalf("the sender was woken: %+v", wakes)
	}
	if draft, _, err := f.app.store.GetThreadDraft(threadID); err != nil || draft.Content != "" {
		t.Fatalf("draft = %q, %v; want the request kept out of it", draft.Content, err)
	}
}

// A session that dies before the CLI took in a request it was handed gives
// the request back to its queue, owed again, its sender told nothing, and
// the thread's next session start delivers it.
func TestARequestTheSessionDiedHoldingIsDeliveredAtTheNextStart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]
	h.lifecycle(t, uuid, provider.CommandQueued)

	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	// The death restores twice, at its error and at its disconnect; the
	// second finds the request queued again and queues it once more. Each
	// holds the thread action lock from its drain until the request is
	// queued again, so the request is read under it.
	waitUntil(t, 10*time.Second, func() bool {
		unlock := f.app.threadLocks().Lock(h.thread.ID)
		defer unlock()
		record, found, err := f.app.findRecordedSend(h.thread.ID, threadRequestSendID(token))
		if err != nil || !found || record.dispatched || f.receipt(t, token).State != store.ThreadReceiptAccepted {
			return false
		}
		assertRequestOwedAgain(t, f, h.thread.ID, token)
		return true
	})
	assertNothingPending(t, f.app, h.thread.ID)

	// The fixture's router has no dispatcher; the next start's flush needs it.
	f.app.triage.SetFlushDispatcher(f.app.enqueueFlushDispatch)
	stdin := filepath.Join(t.TempDir(), "next-session-stdin.jsonl")
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": writeClaudeStdinRecorderBinary(t, stdin)}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := f.app.StartSession(h.thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = f.app.StopSession(h.thread.ID) })
	waitUntil(t, 10*time.Second, func() bool {
		sent, _ := os.ReadFile(stdin)
		return strings.Contains(string(sent), "check the changelog")
	})
	// In flight until its echo, which this CLI never sends.
	if answer := f.app.publishSendsPending(h.thread.ID); !answer.Pending {
		t.Fatalf("sends pending = %+v with the request in flight", answer)
	}
	if wakes := f.wakes(t, token); len(wakes) != 0 {
		t.Fatalf("the sender was woken: %+v", wakes)
	}
}

// The session-end settlement can run before the death's restore reaches the
// thread. It leaves a request whose message the session still holds to that
// restore, which gives it back.
func TestTheSessionEndLeavesAHeldRequestToItsRestore(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]
	h.lifecycle(t, uuid, provider.CommandQueued)

	f.app.settleReceiptsForEndedSession(h.thread.ID)
	f.app.threadRequestsWG.Wait()
	if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("receipt = %q before the restore, want it left running", receipt.State)
	}
	f.app.restoreUnconfirmedQueueOnSessionDeath(h.thread.ID)
	assertRequestOwedAgain(t, f, h.thread.ID, token)
	f.app.settleReceiptsForEndedSession(h.thread.ID)
	f.app.threadRequestsWG.Wait()
	assertRequestOwedAgain(t, f, h.thread.ID, token)
}

// A session that dies during the turn that read a request settles it with
// what the turn had written.
func TestARequestReadWhenItsSessionDiesSettlesWithWhatItWrote(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h, token := heldRequestMidTurn(t, f)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	row := f.awaitRequestState(t, token, store.ThreadRequestInterrupted)
	if !strings.Contains(string(row.Answer), "session ended during the turn that read the request") ||
		!strings.Contains(string(row.Answer), "Halfway through the changelog.") {
		t.Fatalf("answer = %q, want the session's end and what the turn had written", row.Answer)
	}
}

// An archived thread takes nothing back on its queue: a request its archive
// or its death took back before the agent took it in settles as never read.
func TestARequestAnArchivedThreadCannotTakeBackSettlesUnread(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"archive", "death"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			callerMidTurn(t, f)
			h := newHeldRequestTarget(t, f)
			tokens, uuid := h.send(t, "check the changelog")
			token := tokens[0]
			h.lifecycle(t, uuid, provider.CommandQueued)
			if end == "archive" {
				if err := f.app.ArchiveThread(h.thread.ID); err != nil {
					t.Fatalf("ArchiveThread: %v", err)
				}
				h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
			} else {
				if _, _, err := f.app.store.ArchiveThread(h.thread.ID); err != nil {
					t.Fatalf("store.ArchiveThread: %v", err)
				}
				h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "error"})
				h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
			}
			row := f.awaitRequestState(t, token, store.ThreadRequestInterrupted)
			if string(row.Answer) != threadtools.InterruptSessionEndedUnread {
				t.Fatalf("answer = %q, want the session's end before it read the request", row.Answer)
			}
			if record, found, err := f.app.findRecordedSend(h.thread.ID, threadRequestSendID(token)); err != nil || (found && !record.dispatched) {
				t.Fatalf("request message = %+v found=%v err=%v, want it not queued", record, found, err)
			}
		})
	}
}

// A cancel that reads the receipt as `accepted` while the dispatch is
// already writing the message into a running turn finds a dispatched row
// whose turn is that running turn. The message did not start it, so the
// cancel leaves it running.
func TestCancellingARequestCaughtInItsFlushLeavesTheRunningTurnAlone(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	token := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		TargetThreadID: h.thread.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.caller.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: h.thread.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	// Queued and flushed with no dispatch mark: the receipt stays
	// `accepted` while the message is with the provider.
	if _, err := f.app.sendMessageWithOptions(t.Context(), h.thread.ID, "check the changelog", sendMessageOptions{
		SendID: threadRequestSendID(token), ReconcileBySendID: true, QueueIfActive: true, PreserveDraft: true,
	}); err != nil {
		t.Fatalf("queue the request message: %v", err)
	}
	f.app.dispatchFlush(h.thread.ID, f.app.triage.QueuedFlushItems(h.thread.ID))
	if record, found, err := f.app.findRecordedSend(h.thread.ID, threadRequestSendID(token)); err != nil || !found || !record.dispatched {
		t.Fatalf("fixture: the message was not dispatched: %+v found=%v err=%v", record, found, err)
	}

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectLeftUnread {
		t.Fatalf("cancel effect = %q, want the request cancelled unread, nothing stopped", report.Effect)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the running turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

// The person interrupts the turn the request was written during while the
// CLI holds the request. That interrupt ends someone else's turn, so the
// request keeps running, and the turn the CLI then opens for it answers it.
func TestInterruptingTheTurnAHeldRequestWasWrittenDuringLeavesItRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]

	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted"}})
	if row := f.request(t, token); row.State != store.ThreadRequestRunning {
		t.Fatalf("request = %q after the interrupted turn it did not run in, want running", row.State)
	}
	if wakes := f.wakes(t, token); len(wakes) != 0 {
		t.Fatalf("the caller was woken by another turn's interrupt: %+v", wakes)
	}
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	h.echo(t, uuid, "check the changelog")
	h.text(t, "a-turn1", "The changelog lists the fix.")
	h.turnComplete(t)
	if row := f.request(t, token); row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "changelog lists") {
		t.Fatalf("request = %q %q, want it finished by the turn that read it", row.State, row.Answer)
	}
}

// A replacement start (a session-config restart) retires the old process
// without a death. A request that process discarded unrun is the session's
// end before the agent took it in: it goes back to its queue for the
// replacement to send.
func TestARequestDiscardedByAReplacedSessionGoesBackToItsQueue(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]
	h.lifecycle(t, uuid, provider.CommandQueued)
	h.lifecycle(t, uuid, provider.CommandDiscarded)

	h.handSessionEventsToHandler()
	unlock := f.app.threadLocks().Lock(h.thread.ID)
	err := f.app.stopExistingSessionLocked(h.thread.ID)
	unlock()
	if err != nil {
		t.Fatalf("stopExistingSessionLocked: %v", err)
	}
	f.app.threadRequestsWG.Wait()
	assertRequestOwedAgain(t, f, h.thread.ID, token)
}

// A request queued during turn 0 and flushed after a task-notification
// cascade re-opened a wire round on that settled turn. The CLI drains it
// mid-loop: `started` with no logical turn open, then the echo, which opens
// turn 1 for it (no init follows). Turn 1 is the request's, and its end
// answers it.
func TestARequestDrainedAfterACascadeSettlesWithTheTurnItsEchoOpened(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: h.thread.ID, Message: "where does the stall come from?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	token := ack.Token

	h.text(t, "a-turn0", "Still on the first task.")
	h.turnComplete(t)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventInit})
	h.text(t, "a-cascade", "A background task finished.")
	f.app.dispatchFlush(h.thread.ID, f.app.triage.QueuedFlushItems(h.thread.ID))
	head, ok := f.app.triage.PeekPendingSendHeadForTest(h.thread.ID)
	if !ok || head.ExpectedProviderItemID == "" || head.TurnIndex != 1 {
		t.Fatalf("fixture: the flush was not deferred at turn 1: %+v ok=%v", head, ok)
	}
	uuid := head.ExpectedProviderItemID

	h.lifecycle(t, uuid, provider.CommandQueued)
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.echo(t, uuid, "where does the stall come from?")
	if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) || !receipt.OwnTurn {
		t.Fatalf("receipt bound to %q (own turn %v), want turn 1, which the echo opened for it", receipt.TurnID, receipt.OwnTurn)
	}
	h.text(t, "a-turn1", "The stall is in the watchdog.")
	h.turnComplete(t)
	row := f.request(t, token)
	if row.State != store.ThreadRequestFinished || !strings.Contains(string(row.Answer), "stall is in the watchdog") {
		t.Fatalf("request = %q %q, want it finished with turn 1's text", row.State, row.Answer)
	}
}

// A request the CLI drained into the person's running turn joined that turn;
// it did not start it. Cancelling the request settles it and leaves the
// person's turn running.
func TestCancellingARequestDrainedMidTurnLeavesThePersonsTurnRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	token := tokens[0]
	h.lifecycle(t, uuid, provider.CommandQueued)
	h.lifecycle(t, uuid, provider.CommandStarted)
	if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 0) || receipt.OwnTurn {
		t.Fatalf("fixture: receipt = %+v, want it bound to the running turn, not its own", receipt)
	}

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectLeftRunning {
		t.Fatalf("cancel effect = %q, want the person's turn left running", report.Effect)
	}
	if row := f.request(t, token); row.State != store.ThreadRequestCancelled {
		t.Fatalf("request = %q, want cancelled", row.State)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the person's turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

// twoDeferredRequests queues two requests while the target's turn 0 runs,
// lets that turn end, then flushes them one at a time with no turn running:
// each is deferred at a turn of its own, 1 and 2. It returns the tokens and
// the wire uuids, first then second.
func twoDeferredRequests(t *testing.T, f *requestFixture, h *heldRequestTarget) ([2]string, [2]string) {
	t.Helper()
	var tokens [2]string
	for i, message := range []string{"rename the flag", "where does the stall come from?"} {
		ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
			ThreadID: h.thread.ID, Message: message, WaitSeconds: 0, Notify: true,
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		tokens[i] = ack.Token
	}
	h.text(t, "a-turn0", "Still on the first task.")
	h.turnComplete(t)
	queued := f.app.triage.QueuedFlushItems(h.thread.ID)
	if len(queued) != 2 {
		t.Fatalf("queued = %+v, want both requests", queued)
	}
	f.app.dispatchFlush(h.thread.ID, queued[:1])
	f.app.dispatchFlush(h.thread.ID, queued[1:])
	pending := f.app.triage.PendingSendsForTest(h.thread.ID)
	if len(pending) != 2 || !pending[0].HasDeferred || !pending[1].HasDeferred ||
		pending[0].TurnIndex != 1 || pending[1].TurnIndex != 2 {
		t.Fatalf("fixture: pending = %+v, want two deferred sends at turns 1 and 2", pending)
	}
	return tokens, [2]string{pending[0].ExpectedProviderItemID, pending[1].ExpectedProviderItemID}
}

// The CLI runs the first deferred request and drains the second mid-loop into
// its round: the second's `started` lands while turn 1 is open, and its echo
// opens turn 2. Each request is answered by its own turn.
func TestASecondDeferredRequestSettlesWithItsOwnTurn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuids := twoDeferredRequests(t, f, h)

	h.lifecycle(t, uuids[0], provider.CommandQueued)
	h.lifecycle(t, uuids[0], provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	h.echo(t, uuids[0], "rename the flag")
	h.text(t, "a-turn1", "Renamed the flag.")
	h.lifecycle(t, uuids[1], provider.CommandQueued)
	h.lifecycle(t, uuids[1], provider.CommandStarted)
	h.echo(t, uuids[1], "where does the stall come from?")
	if receipt := f.receipt(t, tokens[1]); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 2) || !receipt.OwnTurn {
		t.Fatalf("second receipt bound to %q (own turn %v), want turn 2, its own", receipt.TurnID, receipt.OwnTurn)
	}
	h.text(t, "a-turn2", "The stall is in the watchdog.")
	h.turnComplete(t)
	first, second := f.request(t, tokens[0]), f.request(t, tokens[1])
	if first.State != store.ThreadRequestFinished || !strings.Contains(string(first.Answer), "Renamed the flag") {
		t.Fatalf("first request = %q %q, want it finished with turn 1's text", first.State, first.Answer)
	}
	if second.State != store.ThreadRequestFinished || !strings.Contains(string(second.Answer), "stall is in the watchdog") ||
		strings.Contains(string(second.Answer), "Renamed the flag") {
		t.Fatalf("second request = %q %q, want it finished with turn 2's text", second.State, second.Answer)
	}
}

// Cancelling the second deferred request after its `started`, while the
// first's turn runs, leaves that turn alone: it is not the second's.
func TestCancellingASecondDeferredRequestLeavesTheFirstsTurnRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	tokens, uuids := twoDeferredRequests(t, f, h)

	h.lifecycle(t, uuids[0], provider.CommandQueued)
	h.lifecycle(t, uuids[0], provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventTurnStart})
	h.echo(t, uuids[0], "rename the flag")
	h.lifecycle(t, uuids[1], provider.CommandQueued)
	h.lifecycle(t, uuids[1], provider.CommandStarted)

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: tokens[1]})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectLeftUnread {
		t.Fatalf("cancel effect = %q, want the request cancelled unread, nothing stopped", report.Effect)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 1 {
		t.Fatalf("the first request's turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

// A paired caller is told nothing_to_stop for a cancel that left a turn
// alone: the peer call carries no version, and a build that predates the two
// effects would render an unknown one as an interruption. The turn the
// request was drained into keeps running.
func TestAPeerCancelThatLeavesATurnRunningReportsNothingStopped(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	const peerDevice = "device-peer"
	token := newThreadRequestToken()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: peerDevice, SourceThreadID: "peer-caller",
		Kind: store.ThreadRequestSend, TargetThreadID: h.thread.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	f.app.bindRequestDelivery(token, h.thread.ID, "user:0:flush:1", 0, false)

	origin := threadRequestOrigin{caller: threadtools.Caller{ThreadID: "peer-caller", ComputerID: "peer"}, foreign: true, ownerDevice: peerDevice}
	reply, err := f.app.cancelPeerThreadRequest(t.Context(), origin, token, ThreadPeerCall{Tool: "thread_cancel", Token: token})
	if err != nil {
		t.Fatalf("cancelPeerThreadRequest: %v", err)
	}
	if reply.Effect != threadtools.EffectNothing {
		t.Fatalf("peer effect = %q, want %q", reply.Effect, threadtools.EffectNothing)
	}
	if reply.Request == nil || reply.Request.State != store.ThreadReceiptCancelled {
		t.Fatalf("peer request = %+v, want it cancelled", reply.Request)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the running turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

func TestPeerCancelEffectHidesTheEffectsThatLeaveATurnAlone(t *testing.T) {
	t.Parallel()
	for effect, want := range map[string]string{
		threadtools.EffectLeftRunning:   threadtools.EffectNothing,
		threadtools.EffectLeftUnread:    threadtools.EffectNothing,
		threadtools.EffectInterrupted:   threadtools.EffectInterrupted,
		threadtools.EffectQueuedRemoved: threadtools.EffectQueuedRemoved,
		threadtools.EffectNothing:       threadtools.EffectNothing,
	} {
		if got := peerCancelEffect(effect); got != want {
			t.Errorf("peerCancelEffect(%q) = %q, want %q", effect, got, want)
		}
	}
}

// interruptsSent counts the interrupt control requests the session wrote.
func (h *heldRequestTarget) interruptsSent(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(h.stdinLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(data), `"subtype":"interrupt"`)
}

// A paired caller cancels a request whose answer it has not collected yet:
// the receipt has settled, though the turn that answered it, the request's
// own, still runs. A settled receipt has nothing left to stop, and that turn
// is not interrupted.
func TestAPeerCancelOfASettledRequestLeavesItsTurnRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	const peerDevice = "device-peer"
	token := newThreadRequestToken()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: peerDevice, SourceThreadID: "peer-caller",
		Kind: store.ThreadRequestSend, TargetThreadID: h.thread.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	f.app.bindRequestDelivery(token, h.thread.ID, h.thread.ID+":u0", 0, true)
	if _, err := f.app.store.SettleThreadRequestReceipt(token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
		State: store.ThreadReceiptReplied, Answer: []byte("The stall is in the watchdog."), AnswerKind: store.ThreadAnswerReply,
	}); err != nil {
		t.Fatalf("settle replied: %v", err)
	}

	origin := threadRequestOrigin{caller: threadtools.Caller{ThreadID: "peer-caller", ComputerID: "peer"}, foreign: true, ownerDevice: peerDevice}
	reply, err := f.app.cancelPeerThreadRequest(t.Context(), origin, token, ThreadPeerCall{Tool: "thread_cancel", Token: token})
	if err != nil {
		t.Fatalf("cancelPeerThreadRequest: %v", err)
	}
	if reply.Effect != threadtools.EffectNothing {
		t.Fatalf("peer effect = %q, want %q", reply.Effect, threadtools.EffectNothing)
	}
	if reply.Request == nil || reply.Request.State != store.ThreadReceiptReplied {
		t.Fatalf("peer request = %+v, want it still replied", reply.Request)
	}
	if n := h.interruptsSent(t); n != 0 {
		t.Fatalf("the request's turn was interrupted %d times after it had answered", n)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the running turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}

// A request the CLI ran as a turn of its own, mid-way through that turn.
func heldRequestMidTurn(t *testing.T, f *requestFixture) (*heldRequestTarget, string) {
	t.Helper()
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuid := h.send(t, "check the changelog")
	h.turnComplete(t)
	h.lifecycle(t, uuid, provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventInit})
	h.echo(t, uuid, "check the changelog")
	if receipt := f.receipt(t, tokens[0]); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) {
		t.Fatalf("fixture: receipt bound to %q, want turn 1", receipt.TurnID)
	}
	h.text(t, "a-turn1", "Halfway through the changelog.")
	return h, tokens[0]
}

// assertEndedMidTurn checks a request whose session ended during the turn
// that read it: interrupted, with what that turn had written, its turn
// ended, and its caller woken once.
func assertEndedMidTurn(t *testing.T, f *requestFixture, threadID, token string, turnIndex int, written string) {
	t.Helper()
	row := f.awaitRequestState(t, token, store.ThreadRequestInterrupted)
	if !strings.Contains(string(row.Answer), "session ended during the turn that read the request") ||
		!strings.Contains(string(row.Answer), written) {
		t.Fatalf("answer = %q, want the session's end and what the turn had written", row.Answer)
	}
	turn, found, err := f.app.store.GetTurnByThreadIndex(threadID, turnIndex)
	if err != nil || !found || turn.CompletedAt == nil || turn.StopReason != "interrupted" {
		t.Fatalf("turn %d = %+v found=%v err=%v, want it ended interrupted", turnIndex, turn, found, err)
	}
	waitUntil(t, 10*time.Second, func() bool { return len(f.wakes(t, token)) == 1 })
}

// Stopping the thread mid-way through the turn that read a request ends that
// turn, and the request with it: nothing will continue the turn.
func TestARequestReadWhenTheThreadIsStoppedSettlesWithWhatItWrote(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h, token := heldRequestMidTurn(t, f)
	if err := f.app.StopSession(h.thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	h.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
	assertEndedMidTurn(t, f, h.thread.ID, token, 1, "Halfway through the changelog.")
}

// A replacement start closes the session mid-way through the turn that read a
// request. Its disconnect settles the request with what the turn had written,
// and the turn ends with the session rather than staying active for the
// replacement.
func TestARequestReadWhenItsSessionIsReplacedSettlesWithWhatItWrote(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h, token := heldRequestMidTurn(t, f)
	// The close's own disconnect is handled before the turn ends.
	h.handSessionEventsToHandler()
	unlock := f.app.threadLocks().Lock(h.thread.ID)
	err := f.app.stopExistingSessionLocked(h.thread.ID)
	unlock()
	if err != nil {
		t.Fatalf("stopExistingSessionLocked: %v", err)
	}
	f.app.threadRequestsWG.Wait()
	assertEndedMidTurn(t, f, h.thread.ID, token, 1, "Halfway through the changelog.")
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || found {
		t.Fatalf("active turn after the replacement = %+v found=%v err=%v, want none: the next message would wait on it", turn, found, err)
	}
}

// The same for Codex, whose request joined the running turn: a stop or a
// replacement mid-way through it settles the request with what it wrote.
func TestACodexRequestReadWhenItsSessionEndsSettlesWithWhatItWrote(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"stop", "replacement"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			c := newCodexRequestTarget(t, f)
			token, head := c.steer(t)
			c.echo(t, head)
			c.frame(t, provider.ProviderEvent{Kind: provider.EventTextDelta, ItemID: "codex-a0", Content: "Halfway through the changelog."})
			if end == "stop" {
				if err := f.app.StopSession(c.thread.ID); err != nil {
					t.Fatalf("StopSession: %v", err)
				}
				c.frame(t, provider.ProviderEvent{Kind: provider.EventSessionStatus, Content: "disconnected"})
			} else {
				c.handSessionEventsToHandler()
				unlock := f.app.threadLocks().Lock(c.thread.ID)
				err := f.app.stopExistingSessionLocked(c.thread.ID)
				unlock()
				if err != nil {
					t.Fatalf("stopExistingSessionLocked: %v", err)
				}
				f.app.threadRequestsWG.Wait()
			}
			assertEndedMidTurn(t, f, c.thread.ID, token, 0, "Halfway through the changelog.")
		})
	}
}

// replay hands the session the CLI's replay of the user envelopes it was
// written under uuids, as one entry under the last uuid with every
// envelope's blocks in order: a merged-away member's own echo is one uuid,
// the survivor's carries them all.
func (h *heldRequestTarget) replay(t *testing.T, withTimestamp bool, uuids ...string) {
	t.Helper()
	var blocks []any
	// The fake CLI writes what it reads on its own schedule.
	var written []map[string]any
	waitUntil(t, 10*time.Second, func() bool {
		written = recordedUserEnvelopes(t, h.stdinLog)
		for _, uuid := range uuids {
			if !slices.ContainsFunc(written, func(envelope map[string]any) bool { return envelope["uuid"] == uuid }) {
				return false
			}
		}
		return true
	})
	for _, uuid := range uuids {
		found := false
		for _, envelope := range written {
			if envelope["uuid"] != uuid {
				continue
			}
			found = true
			message, _ := envelope["message"].(map[string]any)
			switch content := message["content"].(type) {
			case []any:
				blocks = append(blocks, content...)
			case string:
				blocks = append(blocks, map[string]any{"type": "text", "text": content})
			default:
				t.Fatalf("envelope %s has content %T", uuid, content)
			}
		}
		if !found {
			t.Fatalf("no envelope was written under %s", uuid)
		}
	}
	envelope := map[string]any{
		"type": "user", "isReplay": true, "uuid": uuids[len(uuids)-1],
		"message": map[string]any{"role": "user", "content": blocks},
	}
	if withTimestamp {
		envelope["timestamp"] = "2026-09-16T12:00:00.000Z"
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	events, err := claude.NewParser().ParseLine(h.thread.ID, line)
	if err != nil || len(events) != 1 || events[0].Kind != provider.EventUserText {
		t.Fatalf("replay parsed to %+v (%v), want one user text event", events, err)
	}
	h.frame(t, events[0])
}

// The CLI merges two deferred requests at its queue boundary, and the first's
// merged-away echo arrives before the turn that runs the merged message
// opens, so it opens a turn of its own for the first. The merged turn opens
// with an init (the documented order) or with the survivor's echo. Either
// way the fold empties the first's turn, and both requests are answered by
// the turn that ran them.
func TestAMergeFoldedRequestEchoedIntoATurnOfItsOwnSettlesWithTheSurvivor(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"init", "no init"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			callerMidTurn(t, f)
			h := newHeldRequestTarget(t, f)
			tokens, uuids := twoDeferredRequests(t, f, h)

			h.replay(t, false, uuids[0])
			if receipt := f.receipt(t, tokens[0]); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) {
				t.Fatalf("fixture: first receipt bound to %q, want turn 1, which its echo opened", receipt.TurnID)
			}
			h.lifecycle(t, uuids[0], provider.CommandStarted)
			h.lifecycle(t, uuids[1], provider.CommandStarted)
			if order == "init" {
				h.frame(t, provider.ProviderEvent{Kind: provider.EventInit})
			}
			h.replay(t, true, uuids[0], uuids[1])
			for i, token := range tokens {
				if receipt := f.receipt(t, token); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 2) || !receipt.OwnTurn {
					t.Fatalf("receipt %d bound to %q (own turn %v), want turn 2, which ran the merged message", i, receipt.TurnID, receipt.OwnTurn)
				}
			}
			h.text(t, "a-merged", "Both answered here.")
			h.turnComplete(t)
			for i, token := range tokens {
				if row := f.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "Both answered here." {
					t.Fatalf("request %d = %q %q, want it finished by the merged turn", i, row.State, row.Answer)
				}
			}
			if turn, found, err := f.app.store.GetTurnByThreadIndex(h.thread.ID, 1); err != nil || !found || turn.CompletedAt == nil {
				t.Fatalf("turn 1 = %+v found=%v err=%v, want it ended", turn, found, err)
			}
			if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || found {
				t.Fatalf("active turn = %+v found=%v err=%v, want none left running", turn, found, err)
			}
		})
	}
}

// A reply settles the request after the cancel read it as running and before
// the cancel's own settlement. The reply wins, and the cancel, finding the
// request settled, stops nothing: the turn that answered is not interrupted.
func TestACancelThatLosesToAReplyLeavesTheTurnRunning(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	h := newHeldRequestTarget(t, f)
	const peerDevice = "device-peer"
	token := newThreadRequestToken()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: peerDevice, SourceThreadID: "peer-caller",
		Kind: store.ThreadRequestSend, TargetThreadID: h.thread.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	f.app.bindRequestDelivery(token, h.thread.ID, h.thread.ID+":u0", 0, true)
	f.app.cancelReadForTest = func(read string) {
		if read != token {
			return
		}
		if err := f.app.settleThreadReceipt(token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
			State: store.ThreadReceiptReplied, Answer: []byte("The stall is in the watchdog."), AnswerKind: store.ThreadAnswerReply,
		}); err != nil {
			t.Errorf("settle replied: %v", err)
		}
	}

	origin := threadRequestOrigin{caller: threadtools.Caller{ThreadID: "peer-caller", ComputerID: "peer"}, foreign: true, ownerDevice: peerDevice}
	reply, err := f.app.cancelPeerThreadRequest(t.Context(), origin, token, ThreadPeerCall{Tool: "thread_cancel", Token: token})
	if err != nil {
		t.Fatalf("cancelPeerThreadRequest: %v", err)
	}
	if reply.Effect != threadtools.EffectNothing {
		t.Fatalf("peer effect = %q, want %q", reply.Effect, threadtools.EffectNothing)
	}
	if reply.Request == nil || reply.Request.State != store.ThreadReceiptReplied {
		t.Fatalf("peer request = %+v, want the reply's settlement", reply.Request)
	}
	if n := h.interruptsSent(t); n != 0 {
		t.Fatalf("the turn that answered was interrupted %d times", n)
	}
	if turn, found, err := f.app.store.GetActiveTurn(h.thread.ID); err != nil || !found || turn.TurnIndex != 0 {
		t.Fatalf("the running turn was touched: %+v found=%v err=%v", turn, found, err)
	}
}
