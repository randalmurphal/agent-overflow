package codex

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// spawnCards counts the spawn rows (the tool start of the spawn item) a
// thread received for itemID. A metadata update for the card is not a row.
func spawnCards(events []provider.ProviderEvent, itemID string) int {
	n := 0
	for _, event := range events {
		if event.ItemID == itemID && event.Kind == provider.EventToolStart &&
			!strings.Contains(string(event.Meta), `"meta_update_only":true`) {
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

// eventIndex is the position of the first event for itemID of kind, or -1.
func eventIndex(events []provider.ProviderEvent, itemID string, kind provider.EventKind) int {
	for i, event := range events {
		if event.ItemID == itemID && event.Kind == kind {
			return i
		}
	}
	return -1
}

func countItem(events []provider.ProviderEvent, itemID string) int {
	n := 0
	for _, event := range events {
		if event.ItemID == itemID {
			n++
		}
	}
	return n
}

// A V1 child closed and then resumed keeps its one spawn card. Its frames
// between the close and the resume are held and reach the card after the
// resume row; its output and approvals after the resume reach the card; the
// resume row stays its own row and names the child; and the output it gave
// before the close is not delivered again.
func TestAResumedChildDeliversUnderItsOriginalSpawnCard(t *testing.T) {
	t.Parallel()
	fx, s, events := v1RootSession(t)
	deadlines := fx.deadlines()
	v1Spawn(fx, "spawn-a", "child-a")
	s.collabAsyncWG.Wait()
	fx.feed(childDelta("child-a", "before-close", "first"))
	fx.feed(v1CollabLine("close-a", "closeAgent", "completed", "child-a"))
	fx.feed(childDelta("child-a", "while-closed", "held"))
	if got := fx.heldFrames("child-a"); got != 1 {
		t.Fatalf("connection held %d of the closed child's frames, want 1", got)
	}

	fx.feed(v1CollabLine("resume-a", "resumeAgent", "completed", "child-a"))
	fx.feed(childDelta("child-a", "after-resume", "second"))
	fx.feed(childApproval("child-a", 92))
	s.collabAsyncWG.Wait()

	if got := fx.heldFrames("child-a"); got != 0 {
		t.Fatalf("connection still holds %d of the resumed child's frames", got)
	}
	if got := deadlines.count(); got != 1 {
		t.Fatalf("the connection armed %d deadlines, want only the close's hold", got)
	}
	got := events()
	if cards := spawnCards(got, "spawn-a"); cards != 1 {
		t.Fatalf("spawn cards = %d, want 1: %+v", cards, got)
	}
	resumeRow := eventIndex(got, "resume-a", provider.EventToolComplete)
	if resumeRow < 0 {
		t.Fatalf("the resume row was not its own row: %+v", got)
	}
	// The closed child keeps its name, so the resume row names it.
	if meta := string(got[resumeRow].Meta); !strings.Contains(meta, `"agentNickname":"Ada"`) {
		t.Fatalf("the resume row does not name the resumed child: %s", meta)
	}
	held := eventIndex(got, "while-closed", provider.EventTextDelta)
	if held < resumeRow {
		t.Fatalf("the frame held across the close is at %d, the resume row at %d: %+v", held, resumeRow, got)
	}
	for _, itemID := range []string{"before-close", "while-closed", "after-resume", "cmd"} {
		if n := countItem(got, itemID); n != 1 {
			t.Fatalf("%s reached the thread %d times, want once: %+v", itemID, n, got)
		}
		event := got[slicesIndexItem(got, itemID)]
		if event.ParentToolUseID != "spawn-a" {
			t.Fatalf("%s escaped the original spawn card: %+v", itemID, event)
		}
	}
	if approval := got[slicesIndexItem(got, "cmd")]; approval.Kind != provider.EventApprovalRequest {
		t.Fatalf("the resumed child's request is not an approval: %+v", approval)
	}
}

func slicesIndexItem(events []provider.ProviderEvent, itemID string) int {
	for i, event := range events {
		if event.ItemID == itemID {
			return i
		}
	}
	return -1
}

// A resume_agent that Codex reports failed restores nothing: the closed
// child's frames stay held and its request is rejected at the deadline.
func TestAFailedResumeLeavesTheChildClosed(t *testing.T) {
	t.Parallel()
	fx, s, events := v1RootSession(t)
	deadlines := fx.deadlines()
	v1Spawn(fx, "spawn-a", "child-a")
	fx.feed(v1CollabLine("close-a", "closeAgent", "completed", "child-a"))
	fx.feed(v1CollabLine("resume-a", "resumeAgent", "failed", "child-a"))
	fx.feed(childDelta("child-a", "late", "after a failed resume"))
	fx.feed(childApproval("child-a", 93))
	if got := fx.heldFrames("child-a"); got != 2 {
		t.Fatalf("connection held %d of the closed child's frames, want 2", got)
	}

	deadlines.fire(t, 0)
	s.collabAsyncWG.Wait()
	got := events()
	if countItem(got, "late") != 0 || countItem(got, "cmd") != 0 {
		t.Fatalf("the closed child's frame reached the thread: %+v", got)
	}
	if !fx.rejectedRequest(93) {
		t.Fatalf("the closed child's request was not rejected: %q", fx.written(2))
	}
}

// A resume of a child that was never closed changes nothing: the resume row
// is its own row and the child keeps delivering under its spawn card.
func TestAResumeOfALiveChildIsItsOwnRow(t *testing.T) {
	t.Parallel()
	fx, s, events := v1RootSession(t)
	v1Spawn(fx, "spawn-a", "child-a")
	fx.feed(v1CollabLine("resume-a", "resumeAgent", "completed", "child-a"))
	fx.feed(childDelta("child-a", "answer", "still live"))
	s.collabAsyncWG.Wait()
	got := events()
	if eventIndex(got, "resume-a", provider.EventToolComplete) < 0 {
		t.Fatalf("the resume row was not its own row: %+v", got)
	}
	if cards := spawnCards(got, "spawn-a"); cards != 1 {
		t.Fatalf("spawn cards = %d, want 1: %+v", cards, got)
	}
	if i := slicesIndexItem(got, "answer"); i < 0 || got[i].ParentToolUseID != "spawn-a" {
		t.Fatalf("the live child's answer did not reach its spawn card: %+v", got)
	}
}

// A parent reopened after its child was closed and resumed rebuilds the
// child's ownership from the persisted spawn row and routes it, so the
// child's frames reach the original card; a close and resume in the reopened
// session then work as in the first one.
func TestAReopenedParentRoutesAClosedThenResumedChild(t *testing.T) {
	t.Parallel()
	fx, s, events := v1RootSession(t)
	s.rehydrateCollabOwnership([]ResumeCollabLaunch{{
		ItemID: "spawn-a",
		Meta:   []byte(`{"input":{"tool":"spawn_agent","receiverThreadIds":["child-a"]}}`),
	}})
	s.collabAsyncWG.Wait()
	fx.feed(childDelta("child-a", "after-reopen", "routed"))
	fx.feed(v1CollabLine("close-b", "closeAgent", "completed", "child-a"))
	fx.feed(childDelta("child-a", "while-closed", "held"))
	if got := fx.heldFrames("child-a"); got != 1 {
		t.Fatalf("connection held %d of the closed child's frames, want 1", got)
	}
	fx.feed(v1CollabLine("resume-b", "resumeAgent", "completed", "child-a"))
	s.collabAsyncWG.Wait()

	got := events()
	for _, itemID := range []string{"after-reopen", "while-closed"} {
		if i := slicesIndexItem(got, itemID); i < 0 || got[i].ParentToolUseID != "spawn-a" {
			t.Fatalf("%s did not reach the original spawn card: %+v", itemID, got)
		}
	}
	if cards := spawnCards(got, "spawn-a"); cards != 0 {
		t.Fatalf("the reopened parent emitted %d spawn rows for a persisted spawn: %+v", cards, got)
	}
}

// A root turn's end retires the raw tool calls of the root and of closed
// children, whose calls can no longer finish, and keeps a live child's.
func TestARootTurnEndRetiresAClosedChildsRawCalls(t *testing.T) {
	t.Parallel()
	fx, s, _ := v1RootSession(t)
	v1Spawn(fx, "spawn-a", "child-a")
	v1Spawn(fx, "spawn-b", "child-b")
	fx.feed(v1CollabLine("close-b", "closeAgent", "completed", "child-b"))
	s.collabAsyncWG.Wait()
	s.mu.Lock()
	if s.rawCalls.byID == nil {
		s.rawCalls.byID = make(map[string]rawToolCall)
	}
	for _, call := range []rawToolCall{
		{CallID: "root-call", ProviderThreadID: "root-provider-thread"},
		{CallID: "live-call", ProviderThreadID: "child-a"},
		{CallID: "closed-call", ProviderThreadID: "child-b"},
	} {
		s.rawCalls.byID[call.CallID] = call
	}
	s.mu.Unlock()

	fx.feed([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"root-provider-thread","turn":{"id":"root-turn","status":"completed"}}}`))

	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := make([]string, 0, len(s.rawCalls.byID))
	for id := range s.rawCalls.byID {
		remaining = append(remaining, id)
	}
	if len(remaining) != 1 || remaining[0] != "live-call" {
		t.Fatalf("raw calls after the root turn = %v, want only the live child's", remaining)
	}
}

// Stopping a closed child's spawn card reports it already terminal and sends
// Codex nothing: the card is still this session's, and its child has no turn.
func TestStoppingAClosedChildReportsItTerminal(t *testing.T) {
	t.Parallel()
	fx, s, _ := v1RootSession(t)
	v1Spawn(fx, "spawn-a", "child-a")
	fx.feed(v1CollabLine("close-a", "closeAgent", "completed", "child-a"))
	s.collabAsyncWG.Wait()
	before := len(fx.written(0))

	stopped, err := s.InterruptSubagent(context.Background(), "spawn-a")
	if err != nil || stopped {
		t.Fatalf("InterruptSubagent = %v, %v; want false, nil for a closed child", stopped, err)
	}
	if after := fx.written(0); len(after) != before {
		t.Fatalf("stopping a closed child wrote %q", after[before:])
	}
}
