package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// The request lifecycle under its own locks: what settles a request when the
// thread answering it is destroyed, what a wait and a settlement owe each
// other when they end at the same moment, and what happens to an answer whose
// delivery is lost.

// TestDeletingAScratchThreadSettlesItsOwnRequest pins the delete port's
// re-entrancy. Deleting an ask's scratch thread settles the request that
// thread was answering, from inside the deletion and under that thread's own
// action lock: the settlement must not try to delete the same thread again.
func TestDeletingAScratchThreadSettlesItsOwnRequest(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "reading the log")
	target := f.forkableThread(t, "scratch-delete")

	ack, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{
		ThreadID: target.ID, Question: "what did the log say?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})

	done := make(chan error, 1)
	go func() { done <- f.app.DeleteThread(ack.ThreadID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteThread(%s): %v", ack.ThreadID, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("deleting the scratch thread never returned: its settlement took that thread's action lock again")
	}

	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestErrored)
	if !strings.Contains(string(row.Answer), "deleted") {
		t.Fatalf("answer = %q, want an account of the deletion", row.Answer)
	}
	if _, err := f.app.store.GetThread(ack.ThreadID); err == nil {
		t.Fatalf("scratch thread %s survived its own deletion", ack.ThreadID)
	}
}

// TestAWaitArmsItsWakeUnderTheTokensSettleLock pins the serialization a wait
// and a settlement owe each other: the wait re-reads the row and arms the
// wake under the token's settle lock, so a settlement cannot decide there is
// nobody to deliver to while the wait is deciding the answer will arrive as a
// message.
func TestAWaitArmsItsWakeUnderTheTokensSettleLock(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "parked")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "when you get to it", WaitSeconds: 0, Notify: false,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	unlock := f.app.threadRequestSettleLock(ack.Token)
	done := make(chan error, 1)
	go func() {
		_, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
			Tokens: []string{ack.Token}, WaitSeconds: 1,
		})
		done <- err
	}()
	waitUntil(t, 10*time.Second, func() bool { return f.waitersOn(ack.Token) == 1 })

	// The wait runs out after a second. Everything it does afterwards waits
	// for this lock.
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the wait finished while the token's settle lock was held: %v", err)
	case <-time.After(2500 * time.Millisecond):
	}
	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RequestStates: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never finished after the settle lock was released")
	}
	if row := f.request(t, ack.Token); !row.Notify {
		t.Fatal("the timed-out wait left the request with no way to deliver its answer")
	}
}

// TestABlockedWaitArmsEveryTokenItWaitedOn pins the other ends of a positive
// wait. A target that stopped to ask a person ends the wait without settling
// anything, and every token that call was waiting on is still open: all of
// them owe their answer as a message, not only the one that ended the wait.
func TestABlockedWaitArmsEveryTokenItWaitedOn(t *testing.T) {
	f := newRequestFixture(t)
	blocked := f.busyThread(t, "waiting-on-a-person")
	quiet := f.busyThread(t, "still-working")

	first, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: blocked.ID, Message: "rerun the deploy", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send(blocked): %v", err)
	}
	second, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: quiet.ID, Message: "and check the logs", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send(quiet): %v", err)
	}
	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-1", Kind: "permission"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: blocked.ID, ItemID: "approval-1",
		Meta: approval, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}

	report, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{
		Tokens: []string{first.Token, second.Token}, WaitSeconds: 10,
	})
	if err != nil {
		t.Fatalf("RequestStates: %v", err)
	}
	if report.WokeOn != first.Token {
		t.Fatalf("report = %+v, want the blocked target to end the wait", report)
	}
	for _, token := range []string{first.Token, second.Token} {
		if row := f.request(t, token); !row.Notify {
			t.Errorf("request %s ended a positive wait unsettled with no way to deliver", token)
		}
	}
}

// TestCancellingADispatchedRequestStopsItsTurn pins what a cancel does when
// the message it meant to take back has already reached the provider: there
// is nothing on the queue, and the turn that message started is what the
// cancel has to stop. Reporting that there was nothing to stop would leave
// the cancelled work running and unreported.
func TestCancellingADispatchedRequestStopsItsTurn(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working on it")
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}

	// The receipt stays `accepted`: this dispatch puts the message in front
	// of the provider without ever binding its turn, which is the window the
	// cancel has to cover.
	token := newThreadRequestToken()
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
	if _, err := f.app.sendMessageWithOptions(t.Context(), target.ID, "look at the crash report", sendMessageOptions{
		SendID:            threadRequestSendID(token),
		ReconcileBySendID: true,
		QueueIfActive:     true,
		PreserveDraft:     true,
	}); err != nil {
		t.Fatalf("dispatch the request message: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		live, err := f.adapter().LiveState(t.Context(), target.ID)
		return err == nil && live.ActiveTurn
	})

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadCancelInterrupted {
		t.Fatalf("cancel effect = %q, want the dispatched turn interrupted", report.Effect)
	}
}

// TestAnInterruptIsFencedOnTheTurnItNames pins the other half of a cancel: it
// names the turn it is entitled to stop, and a thread that has moved on to a
// later turn is left alone.
func TestAnInterruptIsFencedOnTheTurnItNames(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "start the sweep")

	receipt := f.receipt(t, ack.Token)
	index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
	if !ok {
		t.Fatalf("receipt names no turn of its own thread: %+v", receipt)
	}
	// The fence reads the turn the thread is running; until the turn start
	// has been processed there is no turn to fence against.
	waitUntil(t, 10*time.Second, func() bool {
		return f.app.triage.OpenTurnIndex(receipt.TargetThreadID) == index
	})
	interrupted, err := f.app.interruptTurnAtIndex(t.Context(), receipt.TargetThreadID, index+1)
	if err != nil {
		t.Fatalf("interruptTurnAtIndex(a later turn): %v", err)
	}
	if interrupted {
		t.Fatal("a turn this request does not own was interrupted")
	}
	if live, err := f.adapter().LiveState(t.Context(), receipt.TargetThreadID); err != nil || !live.ActiveTurn {
		t.Fatalf("live state = %+v err=%v, want the turn still running", live, err)
	}
	interrupted, err = f.app.interruptTurnAtIndex(t.Context(), receipt.TargetThreadID, index)
	if err != nil {
		t.Fatalf("interruptTurnAtIndex(its own turn): %v", err)
	}
	if !interrupted {
		t.Fatal("the request's own turn was not interrupted")
	}
}

// TestBootRedeliversASettledRequestsLostWake pins the recovery net behind the
// wake. A settlement and its wake are two transactions; a crash or a failed
// delivery between them leaves an answer nobody is ever told about, and
// nothing else revisits a settled row.
func TestBootRedeliversASettledRequestsLostWake(t *testing.T) {
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	token := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn,
		Notify: true, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	// Settled, the wake owed, and nothing handed over: what a crash between
	// the settlement and the queue insert leaves behind.
	settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestFinished,
		Answer:     []byte("the sweep found nothing"),
		AnswerKind: store.ThreadAnswerFinal,
	})
	if err != nil || !settled {
		t.Fatalf("SettleThreadRequest: settled=%v err=%v", settled, err)
	}

	after := f.restart(t)
	after.app.sweepThreadRequestsAtBoot()

	var wake store.FlushQueueItem
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, after.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(token) {
				wake = queued
				return true
			}
		}
		return false
	})
	if !strings.Contains(wake.Message, "the sweep found nothing") {
		t.Fatalf("wake body = %q, want the answer it lost", wake.Message)
	}
	if row := f.request(t, token); row.DeliveredAt == 0 {
		t.Fatalf("the redelivered wake was not marked: %+v", row)
	}
}

// TestAFinishedTurnSettlesWhenItsReceiptCatchesUp pins the fast turn: the
// message was answered and the turn ended before the receipt could be seen
// as running, so no turn end is coming for it. The binding itself has to
// notice, because nothing else is watching any more.
func TestAFinishedTurnSettlesWhenItsReceiptCatchesUp(t *testing.T) {
	f := newRequestFixture(t)
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	turnID := target.ID + "-fast"
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: turnID, ThreadID: target.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := f.app.store.UpsertItem(store.Item{
		ID: target.ID + ":a0", ThreadID: target.ID, TurnIndex: 0, ItemIndex: 1,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "the backfill touched 41,220 rows", CreatedAt: 1, UpdatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	if err := f.app.store.UpdateTurnCompleted(turnID, time.Now().UnixMilli(), "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}

	token := newThreadRequestToken()
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

	f.adapter().markRequestRunning(token, target.ID, store.Item{ID: target.ID + ":u0", TurnIndex: 0})

	receipt := f.receipt(t, token)
	if receipt.State != store.ThreadReceiptFinished {
		t.Fatalf("receipt = %q, want it settled on the turn that had already ended", receipt.State)
	}
	if !strings.Contains(string(receipt.Answer), "41,220 rows") {
		t.Fatalf("answer = %q, want the turn's last message", receipt.Answer)
	}
	if row := f.request(t, token); row.State != store.ThreadRequestFinished {
		t.Fatalf("request state = %q, want finished", row.State)
	}
}

// TestTheObserverKeepsTheGateOfAnAcceptedReceipt pins the other half of the
// same race: a turn end observed while the receipt is still `accepted` must
// leave the gate alone, because the dispatch that set it is about to need it.
func TestTheObserverKeepsTheGateOfAnAcceptedReceipt(t *testing.T) {
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
	f.app.noteReceiptRunning(target.ID, token)

	if err := f.app.settleReceiptOnTurnEnd(target.ID, token); err != nil {
		t.Fatalf("settleReceiptOnTurnEnd: %v", err)
	}
	tokens, ok := f.app.runningReceiptTokens(target.ID)
	if !ok || len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("gate after the observer = %v (%v), want the accepted receipt still watched", tokens, ok)
	}
}

// TestAPeerCannotReplayAnOutboundToken pins the token's ownership. A request
// this computer sent elsewhere has a source row here and no receipt, so a
// forwarded call reusing its token must be refused before anything is
// accepted: otherwise a paired computer could settle this computer's own
// request by colliding with its token.
func TestAPeerCannotReplayAnOutboundToken(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "victim-target")
	token := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestAsk,
		TargetComputerID: "the-other-laptop", State: store.ThreadRequestUnconfirmed,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	args, err := json.Marshal(threadtools.SendCall{ThreadID: target.ID, Message: "run this instead"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.app.runThreadPeerRequest(t.Context(), "attacker-device", ThreadPeerCall{
		Tool:  "thread_send",
		Args:  args,
		Token: token,
		Source: threadtools.Caller{
			ThreadID: "their-thread", ComputerID: "attacker-computer", ComputerName: "Their Mac",
		},
	})
	if code := publicCode(t, err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("replayed token code = %q, want the call refused as not theirs", code)
	}
	if row := f.request(t, token); row.State != store.ThreadRequestUnconfirmed || len(row.Answer) != 0 {
		t.Fatalf("a peer wrote this computer's outbound request: %+v", row)
	}
	if _, found, err := f.app.store.GetThreadRequestReceipt(token); err != nil || found {
		t.Fatalf("a receipt was minted for a replayed token: found=%v err=%v", found, err)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("a replayed token queued work: %+v", rows)
	}
}

// TestAFailedForwardedRequestSettlesItsReceipt pins the other side of a
// refusal. A request a paired computer made has no source row here, so a
// failure after the receipt was accepted has only the receipt to record it.
// Left open it would keep that computer polling forever and keep this thread
// serving the tools for work nobody is doing.
func TestAFailedForwardedRequestSettlesItsReceipt(t *testing.T) {
	f := newRequestFixture(t)
	// A thread with no provider session on disk cannot be forked, so the ask
	// fails after its receipt has been accepted.
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	token := newThreadRequestToken()
	args, err := json.Marshal(threadtools.AskCall{ThreadID: target.ID, Question: "what is the retry budget?"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.app.runThreadPeerRequest(t.Context(), "peer-device", ThreadPeerCall{
		Tool:  "thread_ask",
		Args:  args,
		Token: token,
		Source: threadtools.Caller{
			ThreadID: "their-thread", ComputerID: "peer-computer", ComputerName: "Their Mac",
		},
	}); err == nil {
		t.Fatal("asking a thread that cannot be forked was accepted")
	}
	receipt, found, err := f.app.store.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("GetThreadRequestReceipt: found=%v err=%v", found, err)
	}
	if receipt.State != store.ThreadReceiptErrored {
		t.Fatalf("receipt state = %q, want the failure settled on the receipt", receipt.State)
	}
	if len(receipt.Answer) == 0 {
		t.Fatal("the settled receipt carries no account of the failure")
	}
}

// TestAFallbackAnswerIsStoredWhole pins what a request keeps when its turn
// ended without a thread_reply: the whole last message. The 24 KB preview
// belongs to the wake body, which is rendered from this; clipping here would
// lose the rest for good.
func TestAFallbackAnswerIsStoredWhole(t *testing.T) {
	f := newThreadToolsFixture(t)
	thread := f.thread(t, "long-answer")
	body := strings.Repeat("the migration log said a great deal. ", 2000)
	if len(body) <= threadtools.WakePreviewBytes {
		t.Fatalf("the fixture body is %d bytes, which is no longer than a wake preview", len(body))
	}
	f.turn(t, thread.ID, 0, 1000, payloadAssistantItem("a0", body))
	turn, found, err := f.app.store.GetTurnByThreadIndex(thread.ID, 0)
	if err != nil || !found {
		t.Fatalf("GetTurnByThreadIndex: found=%v err=%v", found, err)
	}

	settlement, err := f.app.turnSettlement(thread.ID, turn)
	if err != nil {
		t.Fatalf("turnSettlement: %v", err)
	}
	if len(settlement.Answer) != len(body) {
		t.Fatalf("answer = %d bytes, want the whole %d", len(settlement.Answer), len(body))
	}
}

// payloadAssistantItem builds an assistant message whose body lives in a
// payload, which is where a long answer is stored.
func payloadAssistantItem(id, body string) store.Item {
	return store.Item{
		ID: id, Kind: "assistant_text", Role: "assistant", Summary: body[:64],
		PayloadID: id + "-payload", PayloadKind: "assistant_text", PayloadMeta: body,
		CreatedAt: 1, UpdatedAt: 1,
	}
}

// TestSettlementReturnsAResponderToItsSwitch pins the end of the responder's
// exception. A thread serves the thread tools with this computer's switch off
// only while a paired computer's request is open; once that request settles
// the live session goes back to what the switch says.
func TestSettlementReturnsAResponderToItsSwitch(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	app.appCtx, app.appCancel = ctx, cancel
	t.Cleanup(cancel)
	thread, token := remoteMCPThread(t, app, string(provider.Claude))
	if _, err := app.threadMCPConfigForThread(thread, token); err != nil {
		t.Fatalf("threadMCPConfigForThread: %v", err)
	}
	captureDir := t.TempDir()
	sess, err := claude.NewSession(ctx, thread.ID, claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: thread.WorkspacePath,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Token: token, Provider: string(provider.Claude), Claude: sess})

	requestToken := "peer-request-token"
	if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: requestToken, OwnerDeviceID: "peer-device", SourceComputerID: "peer-computer",
		SourceThreadID: "their-thread", Kind: store.ThreadRequestSend, TargetThreadID: thread.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"threadToolsEnabled": false}); err != nil {
		t.Fatalf("settings update: %v", err)
	}
	app.setThreadToolsEnabled(false)
	if !app.threadToolsEnabledFor(thread.ID) {
		t.Fatal("the open foreign request did not keep the tools on with the switch off")
	}

	if err := app.settleThreadReceipt(requestToken, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadReceiptCancelled,
		Answer:     []byte("The sender cancelled this request."),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		t.Fatalf("settleThreadReceipt: %v", err)
	}
	if app.threadToolsEnabledFor(thread.ID) {
		t.Fatal("the settled request still holds the tools open")
	}
	awaitClaudeMcpToggle(t, captureDir, threadMCPName, false, 10*time.Second)
}

// awaitClaudeMcpToggle waits for one server's toggle to reach the live
// session with the wanted value. The capture holds every toggle the session
// was sent, and switching off with an open foreign request writes one of its
// own first.
func awaitClaudeMcpToggle(t *testing.T, captureDir, server string, want bool, deadline time.Duration) {
	t.Helper()
	path := filepath.Join(captureDir, "capture.jsonl")
	end := time.Now().Add(deadline)
	var seen []string
	for time.Now().Before(end) {
		raw, err := os.ReadFile(path)
		if err == nil {
			seen = seen[:0]
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var envelope claudeControlRequestEnvelope
				if err := json.Unmarshal([]byte(line), &envelope); err != nil {
					continue
				}
				seen = append(seen, line)
				if name, _ := envelope.Request["serverName"].(string); name != server {
					continue
				}
				if enabled, ok := envelope.Request["enabled"].(bool); ok && enabled == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s toggle with enabled=%v reached the session within %s: %v", server, want, deadline, seen)
}
