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

// A send that finds the session gone after the handoff took the carried
// sends and before it ended lands in the successor's composer once.
func TestASendBetweenTheCarryAndTheEndOfTheHandOffLandsOnce(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	if !f.app.assistantSessionLoss.begin(threadID) {
		t.Fatal("begin")
	}
	successor, err := f.app.nextAssistantThread(t.Context(), threadID, true)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	_, sendErr := f.app.SendMessageWithOptions(t.Context(), threadID, "late message", SendMessageOptions{SendID: uuid.NewString()})
	if left := f.app.assistantSessionLoss.finish(threadID); len(left) > 0 {
		f.app.carrySessionLossItems(threadID, successor.ID, left)
	}
	f.app.waitAgentNoticeWork()
	composers := draftContent(t, f.app, successor.ID) + "\n\n" + draftContent(t, f.app, threadID)
	if code, _, _ := errorsx.PublicDetails(sendErr); code == transport.ErrCodeAssistantSessionGone && strings.Count(composers, "late message") != 1 {
		t.Fatalf("told carried, but the composers hold it %d times: %q", strings.Count(composers, "late message"), composers)
	}
}

// Sends that find the session gone while a failed handoff returns the
// carried ones join them, in the old composer, each once; a later handoff
// that succeeds moves them all to the successor's composer.
func TestSendsDuringAFailedReturnEndInTheNextSuccessor(t *testing.T) {
	t.Parallel()
	for run := range 5 {
		f, threadID, _ := lostSessionAssistant(t)
		if !f.app.assistantSessionLoss.begin(threadID) {
			t.Fatal("begin")
		}
		sendFindingTheSessionGone(t, f.app, threadID, "first message")
		sendFindingTheSessionGone(t, f.app, threadID, "second message")
		// The failed path, as recoverAssistantSessionLoss runs it, racing a
		// third send.
		var wg sync.WaitGroup
		wg.Go(func() {
			f.app.returnSessionLossCarries(threadID, f.app.assistantSessionLoss.take(threadID))
		})
		// The client sends what its composer held, and consumes only that.
		_, third := f.app.SendMessageWithOptions(t.Context(), threadID, "third message", SendMessageOptions{SendID: uuid.NewString(), ConsumeDraft: &DraftSnapshot{Content: "third message"}})
		wg.Wait()
		f.app.returnSessionLossCarries(threadID, f.app.assistantSessionLoss.finish(threadID))
		texts := []string{"first message", "second message"}
		if code, _, _ := errorsx.PublicDetails(third); code == transport.ErrCodeAssistantSessionGone {
			texts = append(texts, "third message")
		}
		old := draftContent(t, f.app, threadID)
		for _, text := range texts {
			if strings.Count(old, text) != 1 {
				t.Errorf("run %d: %q in the old composer %d times: %q", run, text, strings.Count(old, text), old)
			}
		}

		// New Thread, Same Assistant, which the failure told the person to
		// use, takes the composer with the assistant.
		next, err := f.app.StartNextAssistantThread(t.Context(), threadID)
		if err != nil {
			t.Fatalf("run %d: hand off: %v", run, err)
		}
		f.app.waitAgentNoticeWork()
		moved := draftContent(t, f.app, next.ID)
		for _, text := range texts {
			if strings.Count(moved, text) != 1 {
				t.Errorf("run %d: %q in the successor's composer %d times: %q", run, text, strings.Count(moved, text), moved)
			}
		}
		if old := draftContent(t, f.app, threadID); old != "" {
			t.Errorf("run %d: past thread's composer = %q, want it empty", run, old)
		}
	}
}

// A failed session-loss handoff puts the carried sends back in the old
// composer while the handoff still runs, so a send that finds the session
// gone meanwhile joins them instead of starting a handoff that could move
// the composer before they land in it.
func TestAFailedSessionLossHandOffReturnsTheTextsBeforeItEnds(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	var mu sync.Mutex
	runningAtReturn := []bool{}
	f.app.testEmitHook = func(name string, data any) {
		if evt, ok := data.(DraftUpdatedEvent); ok && name == "draft:updated" && evt.ThreadID == threadID {
			c := &f.app.assistantSessionLoss
			c.mu.Lock()
			running := c.running[threadID]
			c.mu.Unlock()
			mu.Lock()
			runningAtReturn = append(runningAtReturn, running)
			mu.Unlock()
		}
	}
	// An archived assistant refuses the handoff.
	release := holdSessionLossHandOff(t, f.app, func() {
		if _, _, err := f.app.store.ArchiveThread(threadID); err != nil {
			t.Error(err)
		}
	})
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	release()
	f.app.waitAgentNoticeWork()
	if got := draftContent(t, f.app, threadID); got != "first message" {
		t.Fatalf("old composer = %q, want the carried text", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runningAtReturn) == 0 || !runningAtReturn[0] {
		t.Fatalf("handoff running when the text was returned: %v, want true", runningAtReturn)
	}
}
