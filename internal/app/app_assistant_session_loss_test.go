package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// lostSessionAssistant is an assistant on a mock Claude whose recorded
// session has no transcript, so its next start finds the session gone.
func lostSessionAssistant(t *testing.T) (f *requestFixture, threadID, stdinLog string) {
	t.Helper()
	f = newAssistantFixture(t)
	stdinLog = f.mockClaudeRecording(t, "picked up")
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, thread.ID)
	if _, err := f.app.store.UpdateSessionRef(thread.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return f, thread.ID, stdinLog
}

func sendFindingTheSessionGone(t *testing.T, a *App, threadID, text string) {
	t.Helper()
	_, err := a.SendMessageWithOptions(t.Context(), threadID, text, SendMessageOptions{SendID: uuid.NewString()})
	if code, _, ok := errorsx.PublicDetails(err); !ok || code != transport.ErrCodeAssistantSessionGone {
		t.Fatalf("send %q = %v, want %s", text, err, transport.ErrCodeAssistantSessionGone)
	}
}

// assistantIDOf is the id of the assistant whose current thread threadID is.
func assistantIDOf(t *testing.T, a *App, threadID string) string {
	t.Helper()
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil || !found {
		t.Fatalf("assistant of %s: %+v %v %v", threadID, agent, found, err)
	}
	return agent.ID
}

func handOffRunning(a *App, agentID string) bool {
	c := &a.assistantHandOffs
	c.mu.Lock()
	defer c.mu.Unlock()
	_, running := c.leaving[agentID]
	return running
}

func carriesOf(a *App, threadID string) []store.Item {
	c := &a.assistantHandOffs
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[threadID]
}

func draftContent(t *testing.T, a *App, threadID string) string {
	t.Helper()
	draft, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		t.Fatal(err)
	}
	return draft.Content
}

// holdSessionLossHandOff holds the handoff a session loss starts until
// release, and returns once the handoff is waiting.
func holdSessionLossHandOff(t *testing.T, a *App, atHold func()) (release func()) {
	t.Helper()
	reached := make(chan struct{})
	gate := make(chan struct{})
	a.sessionLossHandOffForTest = func(string) {
		close(reached)
		<-gate
		if atHold != nil {
			atHold()
		}
	}
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return func() {
		select {
		case <-reached:
		case <-time.After(20 * time.Second):
			t.Fatal("the session-loss handoff never began")
		}
		open()
	}
}

// Two sends that find the session gone while one handoff runs both wait in
// the successor's composer, in the order they were sent, one blank line
// between, and the opening block counts them.
func TestSessionLossCarriesEverySendInOrder(t *testing.T) {
	t.Parallel()
	f, threadID, stdinLog := lostSessionAssistant(t)
	release := holdSessionLossHandOff(t, f.app, nil)

	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	sendFindingTheSessionGone(t, f.app, threadID, "second message")
	release()

	successor := awaitCurrentThread(t, f.app, threadID)
	waitUntil(t, 20*time.Second, func() bool { return draftContent(t, f.app, successor) == "first message\n\nsecond message" })
	if old := draftContent(t, f.app, threadID); old != "" {
		t.Fatalf("past thread's composer = %q, want it empty", old)
	}
	opening := awaitSent(t, stdinLog, func(text string) bool { return strings.HasPrefix(text, `You are "`) })
	if !strings.Contains(opening, "The 2 messages the user sent when the session was found gone") {
		t.Fatalf("opening block:\n%s", opening)
	}
}

// Two sends racing into a lost session: each text the app told its client
// it carried is in a composer, once.
func TestConcurrentSendsDuringSessionLossKeepBothTexts(t *testing.T) {
	t.Parallel()
	for run := range 5 {
		f, threadID, _ := lostSessionAssistant(t)
		texts := []string{"first message", "second message"}
		errs := make([]error, len(texts))
		var wg sync.WaitGroup
		for i := range texts {
			wg.Go(func() {
				_, errs[i] = f.app.SendMessageWithOptions(t.Context(), threadID, texts[i], SendMessageOptions{SendID: uuid.NewString()})
			})
		}
		wg.Wait()
		successor := awaitCurrentThread(t, f.app, threadID)
		f.app.waitAgentNoticeWork()
		composers := draftContent(t, f.app, successor) + "\n\n" + draftContent(t, f.app, threadID)
		for i, text := range texts {
			if code, _, _ := errorsx.PublicDetails(errs[i]); code != transport.ErrCodeAssistantSessionGone {
				continue // the client keeps a text the app did not carry
			}
			if n := strings.Count(composers, text); n != 1 {
				t.Errorf("run %d: %q was carried but is in the composers %d times: %q", run, text, n, composers)
			}
		}
	}
}

// The person sends again right after the first send was carried: neither
// text is lost, whether the second also finds the session gone or reaches
// the successor.
func TestASecondSendRightAfterSessionLossKeepsBothTexts(t *testing.T) {
	t.Parallel()
	for run := range 5 {
		f, threadID, _ := lostSessionAssistant(t)
		sendFindingTheSessionGone(t, f.app, threadID, "first message")
		_, second := f.app.SendMessageWithOptions(t.Context(), threadID, "second message", SendMessageOptions{SendID: uuid.NewString()})
		successor := awaitCurrentThread(t, f.app, threadID)
		f.app.waitAgentNoticeWork()
		draft := draftContent(t, f.app, successor)
		if !strings.HasPrefix(draft, "first message") {
			t.Errorf("run %d: successor's composer = %q, want the first message first", run, draft)
		}
		if code, _, _ := errorsx.PublicDetails(second); code == transport.ErrCodeAssistantSessionGone && draft != "first message\n\nsecond message" {
			t.Errorf("run %d: second send carried, successor's composer = %q", run, draft)
		}
	}
}

// A handoff that fails puts every carried text back in the old thread's
// composer, in the order sent, ahead of what was typed there since.
func TestAFailedSessionLossHandOffReturnsEveryText(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	// An archived assistant refuses the handoff.
	release := holdSessionLossHandOff(t, f.app, func() {
		if _, _, err := f.app.store.ArchiveThread(threadID); err != nil {
			t.Error(err)
		}
	})
	sendFindingTheSessionGone(t, f.app, threadID, "first message")
	sendFindingTheSessionGone(t, f.app, threadID, "second message")
	release()
	f.app.waitAgentNoticeWork()

	if got := draftContent(t, f.app, threadID); got != "first message\n\nsecond message" {
		t.Fatalf("old thread's composer = %q, want both messages in order", got)
	}
	if agent, found, err := f.app.store.GetStandingAgent(threadID); err != nil || !found || agent.ThreadID != threadID {
		t.Fatalf("assistant = %+v %v %v, want it still on its thread", agent, found, err)
	}
	running, left := handOffRunning(f.app, assistantIDOf(t, f.app, threadID)), carriesOf(f.app, threadID)
	if running || len(left) != 0 {
		t.Fatalf("handoff running %v with carries %+v, want neither", running, left)
	}
}

// A session loss found while the app stops runs no handoff, so the send
// fails as any start failure does, its client keeps the text, and nothing
// waits for a handoff that will never take it.
func TestASessionLossWhileStoppingCarriesNothing(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	f.app.stopAgentNoticeWork()

	_, err := f.app.SendMessageWithOptions(t.Context(), threadID, "Anything new?", SendMessageOptions{SendID: uuid.NewString()})
	if err == nil {
		t.Fatal("send succeeded, want the start failure")
	}
	if code, _, _ := errorsx.PublicDetails(err); code == transport.ErrCodeAssistantSessionGone {
		t.Fatalf("send = %v, told the client its text was carried with no handoff to carry it", err)
	}
	running, left := handOffRunning(f.app, assistantIDOf(t, f.app, threadID)), carriesOf(f.app, threadID)
	if running || len(left) != 0 {
		t.Fatalf("handoff running %v with carries %+v, want neither", running, left)
	}
}

// A carried send's attachment waits in the successor's composer, copied
// into the successor.
func TestSessionLossCarriesAttachments(t *testing.T) {
	t.Parallel()
	f, threadID, _ := lostSessionAssistant(t)
	attStore, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, f.app.store)
	if err != nil {
		t.Fatal(err)
	}
	f.app.attachments = attStore
	att := uploadTestAttachment(t, f.app, threadID, "note.txt", "text/plain", []byte("hello"))
	_, err = f.app.SendMessageWithOptions(t.Context(), threadID, "See the note", SendMessageOptions{SendID: uuid.NewString(), AttachmentIDs: []string{att.ID}})
	if code, _, ok := errorsx.PublicDetails(err); !ok || code != transport.ErrCodeAssistantSessionGone {
		t.Fatalf("send = %v", err)
	}
	successor := awaitCurrentThread(t, f.app, threadID)
	var draft store.ThreadDraft
	waitUntil(t, 20*time.Second, func() bool {
		d, found, err := f.app.store.GetThreadDraft(successor)
		draft = d
		return err == nil && found && d.Content == "See the note"
	})
	var ids []string
	if err := json.Unmarshal([]byte(draft.Attachments), &ids); err != nil || len(ids) != 1 {
		t.Fatalf("successor draft attachments = %s %v, want one", draft.Attachments, err)
	}
	got, found, err := f.app.store.GetAttachment(ids[0])
	if err != nil || !found || got.ThreadID != successor {
		t.Fatalf("carried attachment = %+v %v %v, want one owned by %s", got, found, err, successor)
	}
}

// A remote completion the watcher read before a handoff committed, and
// queues after it, reaches the assistant's current thread.
func TestARemoteCompletionReadBeforeAHandOffReachesTheSuccessor(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	old := remoteWatchThread(t, a, string(provider.Claude))
	old.Model = "claude-opus-4-7"
	if err := a.store.UpdateThread(old); err != nil {
		t.Fatal(err)
	}
	testAssistant(t, a, old.ID)
	w := completedRemoteWatch(t, a, old)

	var successor store.Thread
	a.remoteCompletionReadForTest = func(read store.RemoteWatch) {
		if read.ThreadID != old.ID {
			t.Errorf("watcher read the watch on %s, want the old thread", read.ThreadID)
		}
		next, err := a.nextAssistantThread(t.Context(), old.ID, false)
		if err != nil {
			t.Errorf("hand off: %v", err)
		}
		successor = next
	}
	a.checkRemoteWatch(w)
	a.waitAgentNoticeWork()
	if successor.ID == "" {
		t.Fatal("the handoff did not run in the gap")
	}

	if rows := durableQueueRows(t, a, successor.ID); !hasSendID(rows, remoteCompletionSendID(w)) {
		t.Fatalf("current thread's queue = %+v, want the completion", rows)
	}
	if rows := durableQueueRows(t, a, old.ID); len(rows) != 0 {
		t.Fatalf("past thread's queue = %+v, want nothing", rows)
	}
	if moved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || moved.ThreadID != successor.ID || moved.Notification != "queued" {
		t.Fatalf("watch = %+v %v, want it queued on the successor", moved, err)
	}
}

func hasSendID(rows []store.FlushQueueItem, sendID string) bool {
	for _, row := range rows {
		if row.SendID == sendID {
			return true
		}
	}
	return false
}
