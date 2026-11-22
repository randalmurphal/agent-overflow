package triage

import (
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// refuseOlderTurnEndSQL makes the store refuse the write of turn 1's end
// alone, the turn a later one replaces.
const refuseOlderTurnEndSQL = `CREATE TRIGGER refuse_older_settle BEFORE UPDATE OF completed_at ON turns
	WHEN NEW.completed_at IS NOT NULL AND OLD.turn_index = %d BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`

// olderEndFrames counts t1's provider:turn_end_unrecorded frames.
func olderEndFrames(f *endsFixture) int {
	n := 0
	for _, e := range f.emissions.snapshot() {
		if e.eventName != eventchan.ProviderTurnEndUnrecorded.String() {
			continue
		}
		if event, ok := e.data.(TurnEndUnrecordedEvent); ok && event.ThreadID == "t1" {
			n++
		}
	}
	return n
}

// olderTurnOpen fails the test unless turnID's row is still open: the
// refused write the test is about happened.
func olderTurnOpen(t *testing.T, f *endsFixture, turnID string) {
	t.Helper()
	turn, found, err := f.st.GetTurn(turnID)
	if err != nil || !found || turn.CompletedAt != nil {
		t.Fatalf("turn %s = %+v, %v, %v; want its row left open by the refused end", turnID, turn, found, err)
	}
}

// olderTurnSettled fails the test unless turnID's row settled.
func olderTurnSettled(t *testing.T, f *endsFixture, turnID string) {
	t.Helper()
	turn, found, err := f.st.GetTurn(turnID)
	if err != nil || !found || turn.CompletedAt == nil {
		t.Fatalf("turn %s = %+v, %v, %v; want it settled", turnID, turn, found, err)
	}
}

// A turn a queued message's echo replaces is settled by that echo; when the
// store refuses that end, the thread's newest turn is the echo's, and once
// it ends well nothing reads the thread as having a turn that ended
// unrecorded: no client is told so at any point.
func TestAQueuedEchosRefusedPredecessorLeavesAThreadWhoseNewestTurnEndedWell(t *testing.T) {
	f := newEndsFixture(t)
	f.exec(t, fmt.Sprintf(refuseOlderTurnEndSQL, 0))
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0}, 0)
	f.router.openQueuedEchoTurn("t1", 1, f.base.Add(time.Second).UnixMilli(), -1, "")
	olderTurnOpen(t, f, "t1:0")
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()}, 2*time.Second)
	olderTurnSettled(t, f, "t1:1")
	if n := olderEndFrames(f); n != 0 {
		t.Fatalf("turn_end_unrecorded frames = %d; want none, the newest turn ended well", n)
	}
}

// A merge fold ends the turn it emptied, an older one than the survivor's;
// when the store refuses that end, and the end its successor's opening
// writes, the thread's newest turn is the survivor's, and once it ends
// well nothing reads the thread as having a turn that ended unrecorded.
func TestAMergeFoldsRefusedEndLeavesAThreadWhoseNewestTurnEndedWell(t *testing.T) {
	for _, order := range []string{"init", "no init"} {
		t.Run(order, func(t *testing.T) {
			f := newEndsFixture(t)
			f.router.MarkThreadActive("t1", "session-1")
			handle := func(evt provider.ProviderEvent) {
				t.Helper()
				evt.ThreadID = "t1"
				evt.SessionToken = "session-1"
				evt.Timestamp = time.Now()
				if err := f.router.Handle(evt); err != nil {
					t.Fatalf("%s: %v", evt.Kind, err)
				}
			}
			handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
			handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "turn 0 response"})
			handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
			for i, send := range []struct{ uuid, text, sendID string }{
				{"uuid-a", "check the changelog", "thread-request:tok-a"},
				{"uuid-b", "and the notes", "thread-request:tok-b"},
			} {
				turn := i + 1
				digest, err := claude.UserMessageBlockDigest(send.text, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				meta, err := usermessage.MarshalMeta(usermessage.Meta{SendID: send.sendID})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UnixMilli()
				f.router.RegisterPendingFlushSendWithExpectation("t1", fmt.Sprintf("queue:%d", turn), store.Item{
					ID: fmt.Sprintf("user:%d:flush:%d", turn, turn), ThreadID: "t1", TurnIndex: turn,
					Kind: "user_text", Role: "user", Status: "completed", Summary: send.text, Meta: meta, CreatedAt: now, UpdatedAt: now,
				}, now, PendingSendExpectation{ProviderItemID: send.uuid, ContentBlockDigest: digest})
			}

			handle(mergeEcho(t, "t1", "uuid-a", textBlocks("check the changelog"), false))
			f.exec(t, fmt.Sprintf(refuseOlderTurnEndSQL, 1))
			handle(commandLifecycleEvent("t1", "uuid-a", provider.CommandStarted))
			handle(commandLifecycleEvent("t1", "uuid-b", provider.CommandStarted))
			if order == "init" {
				handle(provider.ProviderEvent{Kind: provider.EventInit})
			}
			handle(mergeEcho(t, "t1", "uuid-b", textBlocks("check the changelog", "and the notes"), true))
			olderTurnOpen(t, f, "t1:1")
			handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
			survivor, found, err := f.st.GetTurnByThreadIndex("t1", 2)
			if err != nil || !found || survivor.CompletedAt == nil {
				t.Fatalf("survivor's turn = %+v, %v, %v; want it settled", survivor, found, err)
			}
			if n := olderEndFrames(f); n != 0 {
				t.Fatalf("turn_end_unrecorded frames = %d; want none, the newest turn ended well", n)
			}
		})
	}
}
