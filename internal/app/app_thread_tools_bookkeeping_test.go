package app

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// The delivery a failed write kept is what a request's turn is until the
// write lands. These tests refuse one receipt write and check that every
// reader that picks the request's turn reads the kept delivery.

// The CLI merges two deferred requests, and the survivor's turn takes the
// first from the turn its own echo opened. The rebind's write is refused.
// The first still settles with the turn that ran it, not the emptied one.
func TestARefusedRebindSettlesWithTheTurnItMovedTo(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	callerMidTurn(t, f)
	h := newHeldRequestTarget(t, f)
	tokens, uuids := twoDeferredRequests(t, f, h)
	h.replay(t, false, uuids[0])
	if receipt := f.receipt(t, tokens[0]); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) {
		t.Fatalf("fixture: first receipt bound to %q, want turn 1, which its echo opened", receipt.TurnID)
	}
	execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_rebind BEFORE UPDATE OF turn_id ON thread_request_receipts WHEN OLD.turn_id != '' AND NEW.turn_id != OLD.turn_id BEGIN SELECT RAISE(ABORT, 'injected rebind failure'); END`)
	h.lifecycle(t, uuids[0], provider.CommandStarted)
	h.lifecycle(t, uuids[1], provider.CommandStarted)
	h.frame(t, provider.ProviderEvent{Kind: provider.EventInit})
	h.replay(t, true, uuids[0], uuids[1])
	if receipt := f.receipt(t, tokens[0]); receipt.TurnID != threadRequestTurnKey(h.thread.ID, 1) {
		t.Fatalf("receipt moved to %q through the refused write", receipt.TurnID)
	}
	h.text(t, "a-merged", "Both answered here.")
	h.turnComplete(t)
	execOnStore(t, f.dbPath, `DROP TRIGGER refuse_rebind`)
	for i, token := range tokens {
		if row := f.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "Both answered here." {
			t.Fatalf("request %d = %q %q, want it finished by the merged turn", i, row.State, row.Answer)
		}
	}
}

// Each reader that decides from a receipt's turn whether its message was
// read, or which turn it answers, reads a delivery whose binding failed to
// write as the turn that read it.
func TestEveryTurnReaderReadsAKeptBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		check func(t *testing.T, f *requestFixture, threadID, token string)
	}{
		{"give-back", func(t *testing.T, f *requestFixture, threadID, token string) {
			if f.adapter().returnRequestNotTakenIn(threadID, token) {
				t.Fatal("a delivered request was taken back for its queue")
			}
		}},
		{"not-read settlement", func(t *testing.T, f *requestFixture, threadID, token string) {
			f.app.settleUndeliveredReceipt(threadID, token, threadtools.InterruptSessionEndedUnread)
			f.app.threadRequestsWG.Wait()
			if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptRunning {
				t.Fatalf("receipt = %q, want a delivered request still running", receipt.State)
			}
		}},
		{"person's Stop", func(t *testing.T, f *requestFixture, threadID, token string) {
			if !f.app.turnSettlesAReceipt(threadID, 1) {
				t.Fatal("the turn that read the request settles no receipt")
			}
		}},
		{"handoff", func(t *testing.T, f *requestFixture, threadID, token string) {
			if settled := f.app.settleReadRequestsForHandOff(threadID); len(settled) != 1 || settled[0] != token {
				t.Fatalf("handoff settled %v, want the delivered request", settled)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := f.app.store.InsertTurn(store.Turn{
				TurnID: "target-open", ThreadID: target.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
			}); err != nil {
				t.Fatalf("InsertTurn: %v", err)
			}
			execOnStore(t, f.dbPath, `CREATE TRIGGER refuse_bind BEFORE UPDATE OF turn_id ON thread_request_receipts WHEN NEW.turn_id != '' BEGIN SELECT RAISE(ABORT, 'injected bind failure'); END`)
			f.app.bindRequestDelivery(token, target.ID, target.ID+":u0", 1, true)
			if receipt := f.receipt(t, token); receipt.TurnID != "" {
				t.Fatalf("fixture: receipt bound to %q through the refused write", receipt.TurnID)
			}
			tc.check(t, f, target.ID, token)
		})
	}
}
