package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// remoteJobAssistant is an assistant whose thread can launch remote jobs and
// hand off without a provider.
func remoteJobAssistant(t *testing.T) (*App, store.Thread) {
	t.Helper()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	old := remoteWatchThread(t, a, "claude")
	old.Model = "claude-opus-4-7"
	if err := a.store.UpdateThread(old); err != nil {
		t.Fatal(err)
	}
	testAssistant(t, a, old.ID)
	return a, old
}

func finishRemoteJob(t *testing.T, a *App, w store.RemoteWatch, launcher string) {
	t.Helper()
	receipt := store.RemoteJob{ID: w.RequestID, SourceThreadID: launcher, State: "succeeded", Workspace: "/destination/worktree", FinishedAt: time.Now().UnixMilli(), Output: "build passed"}
	if err := a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, "", 0); err != nil {
		t.Fatal(err)
	}
}

// watchPass runs one watcher pass over the watch as the store holds it.
func watchPass(t *testing.T, a *App, w store.RemoteWatch) {
	t.Helper()
	current, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	a.checkRemoteWatch(current)
	a.waitAgentNoticeWork()
}

func completionRows(t *testing.T, a *App, threadID string, w store.RemoteWatch) int {
	t.Helper()
	n := 0
	for _, row := range durableQueueRows(t, a, threadID) {
		if row.SendID == remoteCompletionSendID(w) {
			n++
		}
	}
	return n
}

// A remote job still running when the assistant moves finishes after the
// handoff. The destination's receipt names the thread that launched it; the
// watch, moved to the successor, still names that thread as the launcher,
// so the watcher queues the completion on the successor, once.
func TestAJobFinishingAfterAHandOffReachesTheSuccessor(t *testing.T) {
	t.Parallel()
	a, old := remoteJobAssistant(t)
	w := registeredRemoteWatch(t, a, old)
	successor, err := a.nextAssistantThread(t.Context(), old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a.waitAgentNoticeWork()
	if moved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || moved.ThreadID != successor.ID || moved.LaunchedIn != old.ID {
		t.Fatalf("watch after the handoff = %+v %v, want owned by the successor and launched in the old thread", moved, err)
	}
	finishRemoteJob(t, a, w, old.ID)

	for range 3 {
		watchPass(t, a, w)
	}
	after, _ := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if got := completionRows(t, a, successor.ID, w); got != 1 || after.Error != "" {
		t.Fatalf("completion rows on the successor %d, watch %s error %q; want one, no error", got, after.Notification, after.Error)
	}
	if got := completionRows(t, a, old.ID, w); got != 0 {
		t.Fatalf("completion rows on the past thread %d, want none", got)
	}
}

// A handoff racing the queueing of a completion the watcher read before it,
// at varied offsets, then two more watcher passes: the completion lands
// once, on the successor, every run.
func TestACompletionRacingAHandOffLandsOnce(t *testing.T) {
	t.Parallel()
	for run := range 20 {
		a, old := remoteJobAssistant(t)
		w := completedRemoteWatch(t, a, old)
		handed := make(chan string, 1)
		a.remoteCompletionReadForTest = func(store.RemoteWatch) {
			go func() {
				next, err := a.nextAssistantThread(t.Context(), old.ID, false)
				if err != nil {
					t.Errorf("run %d: hand off: %v", run, err)
				}
				handed <- next.ID
			}()
			time.Sleep(time.Duration(run%5) * 300 * time.Microsecond)
		}
		a.checkRemoteWatch(w)
		successor := <-handed
		a.waitAgentNoticeWork()
		a.remoteCompletionReadForTest = nil
		for range 2 {
			watchPass(t, a, w)
		}
		final, _ := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
		if completionRows(t, a, successor, w) != 1 || completionRows(t, a, old.ID, w) != 0 {
			t.Errorf("run %d: completion rows successor %d, past %d; watch %s on the successor %v, error %q",
				run, completionRows(t, a, successor, w), completionRows(t, a, old.ID, w), final.Notification, final.ThreadID == successor, final.Error)
		}
	}
}

// A job the successor launched itself names the successor in its receipt
// and in its watch; one whose receipt names any other thread still pauses.
func TestAJobLaunchedInTheSuccessorStillMatches(t *testing.T) {
	t.Parallel()
	a, old := remoteJobAssistant(t)
	successor, err := a.nextAssistantThread(t.Context(), old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a.waitAgentNoticeWork()
	own := registeredRemoteWatch(t, a, successor)
	if w, err := a.store.GetRemoteWatch(own.ComputerID, own.RequestID); err != nil || w.LaunchedIn != successor.ID {
		t.Fatalf("watch = %+v %v, want launched in the successor", w, err)
	}
	finishRemoteJob(t, a, own, successor.ID)
	watchPass(t, a, own)
	if got := completionRows(t, a, successor.ID, own); got != 1 {
		t.Fatalf("completion rows %d, want one", got)
	}

	stray := registeredRemoteWatch(t, a, successor)
	finishRemoteJob(t, a, stray, old.ID)
	watchPass(t, a, stray)
	after, _ := a.store.GetRemoteWatch(stray.ComputerID, stray.RequestID)
	if completionRows(t, a, successor.ID, stray) != 0 || !strings.Contains(after.Error, "different conversation") {
		t.Fatalf("a receipt naming another thread was delivered: watch %s error %q", after.Notification, after.Error)
	}
}

// The successor reads and stops a job its predecessor launched, through
// the same owner check every status, cancel and log read takes; the past
// thread no longer owns it.
func TestTheSuccessorOwnsAMovedJobsReceipts(t *testing.T) {
	t.Parallel()
	a, old := remoteJobAssistant(t)
	w := registeredRemoteWatch(t, a, old)
	successor, err := a.nextAssistantThread(t.Context(), old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a.waitAgentNoticeWork()
	receipt := RemoteCommand{ID: w.RequestID, SourceThreadID: old.ID, State: "running"}
	if err := a.observeRemoteCommand(w.ComputerID, w.RequestID, successor.ID, receipt); err != nil {
		t.Fatalf("the successor's read of a moved job = %v, want it accepted", err)
	}
	if err := a.observeRemoteCommand(w.ComputerID, w.RequestID, old.ID, receipt); err == nil {
		t.Fatal("the past thread still reads a job it no longer owns")
	}
	receipt.SourceThreadID = successor.ID
	if err := a.observeRemoteCommand(w.ComputerID, w.RequestID, successor.ID, receipt); err == nil {
		t.Fatal("accepted a receipt that does not name the thread that launched the job")
	}
}

// Text typed in the assistant's composer and not sent follows the handoff:
// the successor's composer holds it, its attachment copied into the
// successor and its terminal snippet kept, and the past thread's is empty.
func TestAHandOffMovesTheComposerDraft(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok")
	f.assistantCaller(t)
	attStore, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, f.app.store)
	if err != nil {
		t.Fatal(err)
	}
	f.app.attachments = attStore
	att := uploadTestAttachment(t, f.app, f.caller.ID, "note.txt", "text/plain", []byte("hello"))
	chip := TerminalChip{ID: "chip-1", Label: "make test", Preview: "ok", Content: "ok", CreatedAt: 1}
	// A plan link with no thread names a plan in the draft's own thread.
	plan := &SourceProposedPlan{ItemID: "plan-item", Title: "The plan"}
	if err := f.app.SaveDraft(t.Context(), f.caller.ID, "half-typed note", []string{att.ID}, []TerminalChip{chip}, plan); err != nil {
		t.Fatal(err)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.app.waitAgentNoticeWork()

	moved, err := f.app.GetDraft(successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Content != "half-typed note" || len(moved.TerminalChips) != 1 || moved.TerminalChips[0].ID != "chip-1" || len(moved.AttachmentIDs) != 1 {
		t.Fatalf("successor's composer = %+v, want the typed text, its snippet and its attachment", moved)
	}
	if moved.SourceProposedPlan == nil || moved.SourceProposedPlan.ItemID != "plan-item" || moved.SourceProposedPlan.ThreadID != f.caller.ID {
		t.Fatalf("moved plan link = %+v, want the plan in the past thread", moved.SourceProposedPlan)
	}
	if got, found, err := f.app.store.GetAttachment(moved.AttachmentIDs[0]); err != nil || !found || got.ThreadID != successor.ID {
		t.Fatalf("moved attachment = %+v %v %v, want one owned by the successor", got, found, err)
	}
	if _, found, err := f.app.store.GetThreadDraft(f.caller.ID); err != nil || found {
		t.Fatalf("past thread's draft found=%v %v, want it cleared", found, err)
	}
}

// A session-loss handoff puts the carried sends ahead of the text still
// typed in the composer, in the successor's composer.
func TestASessionLossHandOffMovesCarriesThenTheDraft(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	release := holdSessionLossHandOff(t, f.app, nil)
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	if err := f.app.SaveDraft(t.Context(), threadID, "typed after it", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	release()
	successor := awaitCurrentThread(t, f.app, threadID)
	waitUntil(t, 20*time.Second, func() bool {
		return draftContent(t, f.app, successor) == "first message\n\ntyped after it"
	})
	f.app.waitAgentNoticeWork()
	if old := draftContent(t, f.app, threadID); old != "" {
		t.Fatalf("past thread's composer = %q, want it empty", old)
	}
}

// The composer move takes the kept sends and keeps no more: a send after it
// is not the handoff's to place, so its end puts nothing in the past
// thread's composer, and the successor holds the kept send then the draft.
func TestAHandOffKeepsNoSendAfterItsComposerMove(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	agentID := assistantIDOf(t, f.app, threadID)
	if !f.app.assistantHandOffs.begin(agentID, threadID) {
		t.Fatal("begin")
	}
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	if err := f.app.SaveDraft(t.Context(), threadID, "typed after it", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	keptAfterMove := true
	f.app.handOffCommittedForTest = func(old, _ string) {
		keptAfterMove = f.app.assistantHandOffs.add(old, store.Item{ID: uuid.NewString(), ThreadID: old, Kind: "user_text", Role: "user", Summary: "late message"})
	}
	successor, err := f.app.nextAssistantThread(t.Context(), threadID, true)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	f.app.endAssistantHandOff(agentID, threadID)
	f.app.waitAgentNoticeWork()
	if keptAfterMove {
		t.Error("the handoff kept a send after its composer move took the others")
	}
	if got := draftContent(t, f.app, successor.ID); got != "first message\n\ntyped after it" {
		t.Errorf("successor's composer = %q, want the kept send then the draft", got)
	}
	if got := draftContent(t, f.app, threadID); got != "" {
		t.Errorf("past thread's composer = %q, want it empty", got)
	}
}

// A send that finds the session gone while a failed handoff waits to end is
// kept with the others, and the end returns them once, in the order sent; a
// later handoff that succeeds moves them in that order to the successor.
func TestSendsDuringAFailedReturnEndInTheNextSuccessor(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	agentID := assistantIDOf(t, f.app, threadID)
	if !f.app.assistantHandOffs.begin(agentID, threadID) {
		t.Fatal("begin")
	}
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	sendFindingTheSessionGone(t, f.app, threadID, "second message")
	// The failed handoff's end waits on the composer before it takes the
	// kept messages, and the third send finds the session gone meanwhile.
	unlock := f.app.threadApplication().LockMutations(threadID)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		f.app.endAssistantHandOff(agentID, threadID)
	}()
	waitUntil(t, 20*time.Second, func() bool {
		return f.app.threadApplication().MutationRefs(threadID) == 2
	})
	// The client sends what its composer held, and consumes only that.
	_, third := f.app.SendMessageWithOptions(t.Context(), threadID, "third message", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{Content: "third message"}})
	unlock()
	<-ended
	if code, _, _ := errorsx.PublicDetails(third); code != transport.ErrCodeAssistantSessionGone {
		t.Fatalf("third send = %v, want %s", third, transport.ErrCodeAssistantSessionGone)
	}
	const inOrder = "first message\n\nsecond message\n\nthird message"
	if old := draftContent(t, f.app, threadID); old != inOrder {
		t.Errorf("old composer = %q, want %q", old, inOrder)
	}

	// New Thread, Same Assistant, which the failure told the person to
	// use, takes the composer with the assistant.
	next, err := f.app.StartNextAssistantThread(t.Context(), threadID)
	if err != nil {
		t.Fatalf("hand off: %v", err)
	}
	f.app.waitAgentNoticeWork()
	if moved := draftContent(t, f.app, next.ID); moved != inOrder {
		t.Errorf("successor's composer = %q, want %q", moved, inOrder)
	}
	if old := draftContent(t, f.app, threadID); old != "" {
		t.Errorf("past thread's composer = %q, want it empty", old)
	}
}

// A failed handoff's end is one take and one write however many sends it
// kept, and a send that finds the session gone while it writes is no longer
// this handoff's, so a steady sender cannot keep it from returning.
func TestAFailedHandOffEndsAfterOneWrite(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	agentID := assistantIDOf(t, f.app, threadID)
	if !f.app.assistantHandOffs.begin(agentID, threadID) {
		t.Fatal("begin")
	}
	texts := []string{"message 1", "message 2", "message 3", "message 4"}
	for _, text := range texts {
		sendFindingTheSessionGone(t, f.app, threadID, text)
	}
	var mu sync.Mutex
	writes := 0
	f.app.testEmitHook = func(name string, data any) {
		if evt, ok := data.(DraftUpdatedEvent); ok && name == "draft:updated" && evt.ThreadID == threadID {
			mu.Lock()
			writes++
			mu.Unlock()
		}
	}
	keptDuringEnd := 0
	f.app.assistantHandOffEndedForTest = func(string) {
		for i := range 20 {
			if f.app.assistantHandOffs.add(threadID, store.Item{ID: uuid.NewString(), ThreadID: threadID, Kind: "user_text", Role: "user", Summary: "late " + string(rune('a'+i))}) {
				keptDuringEnd++
			}
		}
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		f.app.endAssistantHandOff(agentID, threadID)
	}()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the handoff never ended while sends kept arriving")
	}
	if keptDuringEnd != 0 {
		t.Errorf("the ending handoff kept %d sends that arrived while it wrote", keptDuringEnd)
	}
	if got, want := draftContent(t, f.app, threadID), strings.Join(texts, "\n\n"); got != want {
		t.Errorf("old composer = %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if writes != 1 {
		t.Errorf("composer writes = %d, want 1", writes)
	}
}
