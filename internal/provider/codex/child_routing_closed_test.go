package codex

import (
	"testing"

	"agent-overflow/internal/provider"
)

// spawnCards counts the spawn rows (the tool start of the spawn item) a
// thread received for itemID.
func spawnCards(events []provider.ProviderEvent, itemID string) int {
	n := 0
	for _, event := range events {
		if event.ItemID == itemID && event.Kind == provider.EventToolStart {
			n++
		}
	}
	return n
}

// A child's frames that arrive after close_agent ended its ownership are held
// and expire. Before, the session received them on the child's old route,
// recovered the closed child from history, emitted its spawn row a second
// time and showed the frame under it. Recovery is scripted to succeed here,
// so a path that still reached it would show the frame.
func TestLateFrameAfterCloseAgent(t *testing.T) {
	t.Parallel()
	fx, s, events := recordingRootSession(t)
	deadlines := fx.deadlines()
	version := "0.169.0"
	s.appServerVersion.Store(&version)
	fx.answer("thread/read", `{"thread":{"id":"child-a","parentThreadId":"root-provider-thread"}}`)
	fx.answer("thread/items/list", `{"data":[{"turnId":"root-turn","item":{"type":"subAgentActivity","id":"spawn-a","kind":"started","agentThreadId":"child-a","agentPath":"/root/child-a"}}]}`)

	fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if got := spawnCards(events(), "spawn-a"); got != 1 {
		t.Fatalf("spawn cards after the spawn = %d, want 1", got)
	}
	fx.feed([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"root-provider-thread","turnId":"root-turn","item":{"id":"close-a","type":"collabAgentToolCall","tool":"closeAgent","receiverThreadIds":["child-a"],"status":"completed"}}}`))
	fx.feed(childDelta("child-a", "late", "after close"))
	fx.feed(childApproval("child-a", 91))
	if got := fx.heldFrames("child-a"); got != 2 {
		t.Fatalf("connection held %d of the closed child's frames, want 2", got)
	}

	deadlines.fire(t, 0)
	s.collabAsyncWG.Wait()
	got := events()
	for _, event := range got {
		if event.ItemID == "late" || event.ItemID == "cmd" {
			t.Fatalf("the closed child's late frame reached the thread: %+v", got)
		}
	}
	if cards := spawnCards(got, "spawn-a"); cards != 1 {
		t.Fatalf("spawn cards = %d, want 1: the closed child was recovered again", cards)
	}
	if !fx.rejectedRequest(91) {
		t.Fatalf("the closed child's late request was not rejected: %q", fx.written(1))
	}
}

// A frame for a child whose spawn item never reached this connection: the
// connection holds it, no session sees it, and the deadline rejects its
// request and drops the rest.
func TestChildWithoutASpawnNeverReachesASession(t *testing.T) {
	t.Parallel()
	fx, s, events := recordingRootSession(t)
	deadlines := fx.deadlines()
	version := "0.169.0"
	s.appServerVersion.Store(&version)
	fx.feed(childDelta("ghost", "m", "x"))
	fx.feed(childApproval("ghost", 71))
	if got := fx.heldFrames("ghost"); got != 2 {
		t.Fatalf("connection held %d frames, want 2", got)
	}
	deadlines.fire(t, 0)
	s.collabAsyncWG.Wait()
	if len(events()) != 0 {
		t.Fatalf("a session saw the frame: %+v", events())
	}
	if !fx.rejectedRequest(71) {
		t.Fatalf("held request 71 was not rejected: %q", fx.written(1))
	}
}
