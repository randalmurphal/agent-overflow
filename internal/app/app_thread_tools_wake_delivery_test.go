package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"

	"github.com/google/uuid"
)

// settledNotifyRequest is a request of the fixture's caller that has settled
// with a final message and owes its wake.
func (f *requestFixture) settledNotifyRequest(t *testing.T, answer string) string {
	t.Helper()
	token := uuid.NewString()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		TargetThreadID: uuid.NewString(), State: store.ThreadRequestUnconfirmed, Notify: true,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte(answer), AnswerKind: store.ThreadAnswerFinal,
	}); err != nil || !settled {
		t.Fatalf("SettleThreadRequest: settled=%v err=%v", settled, err)
	}
	return token
}

// A message is built from rows read before it is written. A request another
// route answers in between (a thread_status that read it, a message of its
// own) is not named by the message that lands: the message is rebuilt from
// what is still owed, under the oldest request left.
func TestAMessageNamingARequestAnsweredMeanwhileIsRebuiltWithoutIt(t *testing.T) {
	t.Parallel()
	for _, answered := range []int{0, 1} {
		t.Run([]string{"its carrier", "a rider"}[answered], func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "mid-thought")
			f.holdCallerTurn(t)
			var refs []threadWakeRef
			for range 3 {
				refs = append(refs, threadWakeRef{token: f.settledNotifyRequest(t, "the turn ended")})
			}
			built := f.app.threadWakeMessages(refs)
			if len(built) != 1 || len(built[0].rows) != 3 {
				t.Fatalf("messages = %+v, want one naming all three", built)
			}
			gone := built[0].rows[answered].Token
			var left []string
			for index, row := range built[0].rows {
				if index != answered {
					left = append(left, row.Token)
				}
			}
			if _, err := f.app.store.MarkThreadRequestDelivered(gone, store.ThreadWakeInline, 0, false); err != nil {
				t.Fatalf("MarkThreadRequestDelivered: %v", err)
			}

			f.app.deliverThreadWakeMessages(built)

			wakes := f.wakesQueuedFor(t, left...)
			if len(wakes) != 1 {
				t.Fatalf("queued wakes = %+v, want one", wakes)
			}
			if wakes[0].SendID != threadWakeSendID(left[0]) {
				t.Errorf("wake send id = %q, want the oldest request still owed", wakes[0].SendID)
			}
			if strings.Contains(wakes[0].Message, gone) {
				t.Errorf("wake = %q names %s, whose answer was already delivered", wakes[0].Message, gone)
			}
			for _, token := range left {
				if !strings.Contains(wakes[0].Message, token) {
					t.Errorf("wake = %q, want it to name %s", wakes[0].Message, token)
				}
			}
			if row := f.request(t, gone); row.DeliveredHow != store.ThreadWakeInline || row.DeliveredWith != "" {
				t.Errorf("the request answered meanwhile = delivered %q with %q", row.DeliveredHow, row.DeliveredWith)
			}
			f.nothingOwed(t)
		})
	}
}

// Archiving the sender disarms its wakes. A message that was already built
// when the archive landed is not written and does not bring the thread back.
func TestAWakeOnItsWayWhenTheSenderIsArchivedIsNotDelivered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	token := f.settledNotifyRequest(t, "the turn ended")
	built := f.app.threadWakeMessages([]threadWakeRef{{token: token}})
	if len(built) != 1 {
		t.Fatalf("messages = %+v, want the one owed", built)
	}
	if err := f.app.ArchiveThread(f.caller.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	f.app.deliverThreadWakeMessages(built)

	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queued = %+v, want nothing for an archived sender", rows)
	}
	if thread, err := f.app.store.GetThread(f.caller.ID); err != nil || !thread.Archived {
		t.Fatalf("caller = archived %v err %v, want it left archived", thread.Archived, err)
	}
	row := f.request(t, token)
	if row.Notify || row.DeliveredAt != 0 || row.WakeIssue != "" {
		t.Fatalf("request = notify %v, delivered at %d, issue %q; want it disarmed, undelivered and not failed", row.Notify, row.DeliveredAt, row.WakeIssue)
	}
	f.nothingOwed(t)
}

// A late reply that lands while the first answer's message is being written
// replaces it: the first message is not sent after the reply.
func TestALateReplySupersedesAFirstAnswerStillBeingWritten(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	token := f.settledNotifyRequest(t, "I started the tests")
	built := f.app.threadWakeMessages([]threadWakeRef{{token: token}})
	if len(built) != 1 {
		t.Fatalf("messages = %+v, want the first answer's", built)
	}
	if stored, err := f.app.store.StoreThreadRequestLateReply(token, []byte("all green"), time.Now().UnixMilli(), true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	f.app.deliverThreadWakes([]threadWakeRef{{token: token, late: true}})
	awaitQueuedWake(t, f.app, f.caller.ID, threadWakeLateSendID(token))

	f.app.deliverThreadWakeMessages(built)

	rows := durableQueueRows(t, f.app, f.caller.ID)
	if len(rows) != 1 || !strings.Contains(rows[0].Message, "all green") {
		t.Fatalf("queued = %+v, want the reply alone", rows)
	}
	if row := f.request(t, token); row.DeliveredAt != 0 {
		t.Errorf("the superseded first answer was marked delivered at %d", row.DeliveredAt)
	}
	f.nothingOwed(t)
}

// Requests made at different moments keep different poll clocks. When one of
// them is due, the poll asks the other computer about every request its
// thread still has open there, so what one event answered arrives here as
// one message.
func TestOnePollCollectsEveryOpenRequestOnAComputer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	const answer = "the other computer answered"
	gate := filepath.Join(t.TempDir(), "answer")
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":` + quoteJSON(answer) + `}]}}`,
		testutil.MockWaitForFile(gate),
		`{"type":"result","subtype":"success","is_error":false}`,
	}})
	// The wake starts the caller's session to read it; the open turn keeps
	// the message queued where the test can see it.
	installMockClaudeReplies(t, pair.source, "read the wake")
	if err := pair.source.store.InsertTurn(store.Turn{
		TurnID: pair.caller.ID + "-open", ThreadID: pair.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	first := pair.spawnThere(t, "one", map[string]any{"notify": true})
	second := pair.spawnThere(t, "two", map[string]any{"notify": true})
	// Only the first is due; the second's clock is ten minutes out.
	now := time.Now()
	if err := pair.source.store.RescheduleThreadRequest(first.Token, now.Add(-time.Second).UnixMilli(), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := pair.source.store.RescheduleThreadRequest(second.Token, now.Add(10*time.Minute).UnixMilli(), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first.Token, second.Token} {
		waitUntilE2E(t, 30*time.Second, "the destination settles "+token, func() bool {
			receipt, found, err := pair.dest.store.GetThreadRequestReceipt(token)
			return err == nil && found && receipt.SettledAt != 0
		})
	}

	pair.source.pollRemoteThreadRequests(now)
	pair.source.threadRequestsWG.Wait()

	for _, token := range []string{first.Token, second.Token} {
		if row := pair.request(t, token); row.State != store.ThreadRequestFinished || row.DeliveredHow != store.ThreadWakeQueued {
			t.Fatalf("request %s = %q delivered %q, want it collected and queued by the one pass", token, row.State, row.DeliveredHow)
		}
	}
	var wakes []store.FlushQueueItem
	for _, queued := range durableQueueRows(t, pair.source, pair.caller.ID) {
		if strings.HasPrefix(queued.SendID, threadWakeSendID("")) {
			wakes = append(wakes, queued)
		}
	}
	if len(wakes) != 1 {
		t.Fatalf("queued wakes = %+v, want one message for the two answers", wakes)
	}
	for _, token := range []string{first.Token, second.Token} {
		if !strings.Contains(wakes[0].Message, token) {
			t.Errorf("wake = %q, want it to name %s", wakes[0].Message, token)
		}
	}
}

// The requests that ride along with a due poll are the same threads' other
// open requests on that computer. Another thread's keep their own clock, as
// do requests on another computer and ones whose call has not returned.
func TestAPollAddsOnlyTheDueThreadsOtherOpenRequests(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	other, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the other caller: %v", err)
	}
	computer, elsewhere := uuid.NewString(), uuid.NewString()
	seed := func(caller, computerID string, accepted bool) store.ThreadRequest {
		t.Helper()
		token := uuid.NewString()
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSend, State: store.ThreadRequestUnconfirmed,
			TargetComputerID: computerID, TargetThreadID: uuid.NewString(), NextCheck: time.Now().Add(time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatalf("InsertThreadRequest: %v", err)
		}
		if accepted {
			if applied, err := f.app.store.AdvanceThreadRequestState(token, store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted); err != nil || !applied {
				t.Fatalf("accept %s: applied=%v err=%v", token, applied, err)
			}
		}
		return f.request(t, token)
	}
	due := seed(f.caller.ID, computer, true)
	sibling := seed(f.caller.ID, computer, true)
	seed(f.caller.ID, computer, false)
	seed(f.caller.ID, elsewhere, true)
	seed(other.ID, computer, true)

	var polled []string
	for _, row := range f.app.withOpenThreadRequestsOn(computer, []store.ThreadRequest{due}) {
		polled = append(polled, row.Token)
	}
	if len(polled) != 2 || polled[0] != due.Token || polled[1] != sibling.Token {
		t.Fatalf("polled = %v, want the due request then its thread's other open one (%s, %s)", polled, due.Token, sibling.Token)
	}
}
