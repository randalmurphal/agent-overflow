package app

import (
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// `/side-chat` and Keep, from the bindings the pane calls. The fixture is
// the request fixture because it already builds a Claude thread a fork can
// cut (session file on disk, a stamped user message) under kerneltest
// isolation with a mock provider.

// A side chat keeps the source's runtime mode: a person is present in the
// pane, unlike the read-only fork an agent's ask runs in.
func TestForkSideChatKeepsTheSourcesRuntimeMode(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-source")
	if err := f.app.store.UpdateRuntimeMode(source.ID, string(provider.RuntimeFullAccess)); err != nil {
		t.Fatalf("UpdateRuntimeMode: %v", err)
	}

	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}
	if fork.ID == source.ID {
		t.Fatal("ForkSideChat returned the source thread")
	}
	if fork.Mode != threadmode.ModeScratch {
		t.Errorf("fork mode = %q, want %q", fork.Mode, threadmode.ModeScratch)
	}
	if fork.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Errorf("fork runtime mode = %q, want the source's full-access", fork.RuntimeMode)
	}
	if fork.Title != "Side chat: "+source.Title {
		t.Errorf("fork title = %q, want %q", fork.Title, "Side chat: "+source.Title)
	}
	if fork.Provider != source.Provider || fork.Model != source.Model || fork.WorkspacePath != source.WorkspacePath {
		t.Errorf("fork left its source's identity: %+v", fork)
	}
	row, found, err := f.app.store.GetScratchThread(fork.ID)
	if err != nil || !found {
		t.Fatalf("GetScratchThread: found=%v err=%v", found, err)
	}
	if row.SourceThreadID != source.ID {
		t.Errorf("scratch source = %q, want %q", row.SourceThreadID, source.ID)
	}
	if row.ReturnMode != threadmode.ModeChat {
		t.Errorf("return mode = %q, want the source's chat", row.ReturnMode)
	}
	if row.RequestToken != "" {
		t.Errorf("side chat carried a request token %q; no request owns it", row.RequestToken)
	}
	// The fork's timeline is the source's, which is what makes the side
	// chat a continuation rather than a new thread.
	items, err := f.app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("the side chat cloned no history")
	}
}

// A side chat forked out of a plan thread returns to plan when it is kept.
func TestForkSideChatRecordsThePlanReturnMode(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-plan")
	if _, err := f.app.threadApplication().UpdateMode(source.ID, threadmode.ModePlan); err != nil {
		t.Fatalf("UpdateMode: %v", err)
	}

	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}
	row, found, err := f.app.store.GetScratchThread(fork.ID)
	if err != nil || !found {
		t.Fatalf("GetScratchThread: found=%v err=%v", found, err)
	}
	if row.ReturnMode != threadmode.ModePlan {
		t.Errorf("return mode = %q, want plan", row.ReturnMode)
	}
}

// The pane is offered mid-turn, so the fork has to be the tail fork that
// includes the running turn rather than a refusal.
func TestForkSideChatForksThroughARunningTurn(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-midturn")
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "side-chat-open", ThreadID: source.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat mid-turn: %v", err)
	}
	if fork.Mode != threadmode.ModeScratch {
		t.Errorf("fork mode = %q, want %q", fork.Mode, threadmode.ModeScratch)
	}
	// The source keeps streaming under its own session: the fork takes a
	// snapshot, it does not interrupt.
	if _, active, err := f.app.store.GetActiveTurn(source.ID); err != nil || !active {
		t.Fatalf("the source's turn was disturbed: active=%v err=%v", active, err)
	}
	// The fork's copy of that turn settles interrupted, the way every
	// mid-turn fork's does, so the side chat opens at rest.
	if turn, active, err := f.app.store.GetActiveTurn(fork.ID); err != nil || active {
		t.Errorf("fork turn %s stayed open: active=%v err=%v", turn.TurnID, active, err)
	}
}

// Keep restores the recorded mode, drops the scratch record, and tells every
// attached client the thread belongs in the sidebar now.
func TestPromoteScratchThreadListsTheKeptThread(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-keep")
	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}
	events := &emitRecorder{}
	f.app.testEmitHook = events.capture

	promoted, err := f.app.PromoteScratchThread(t.Context(), fork.ID)
	if err != nil {
		t.Fatalf("PromoteScratchThread: %v", err)
	}
	if promoted.Mode != threadmode.ModeChat {
		t.Errorf("promoted mode = %q, want chat", promoted.Mode)
	}
	stored, err := f.app.store.GetThread(fork.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Mode != threadmode.ModeChat {
		t.Errorf("stored mode = %q, want chat", stored.Mode)
	}
	if _, found, err := f.app.store.GetScratchThread(fork.ID); err != nil || found {
		t.Errorf("scratch record survived the promotion: found=%v err=%v", found, err)
	}
	listed := false
	for _, call := range events.snapshot() {
		if call.Channel != eventchan.ThreadUpdated.String() {
			continue
		}
		event, ok := call.Data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated carried %T, want triage.ThreadUpdateEvent", call.Data)
		}
		if event.Action == triage.ThreadActionListed && event.Thread != nil && event.Thread.ID == fork.ID {
			listed = true
			if event.Thread.Mode != threadmode.ModeChat {
				t.Errorf("broadcast row mode = %q, want chat", event.Thread.Mode)
			}
		}
	}
	if !listed {
		t.Error("the kept thread was never broadcast as listed")
	}
}

// A thread nobody forked into a side chat has nothing to keep, and the
// refusal is client-safe prose rather than a store error.
func TestPromoteScratchThreadRefusesAnOrdinaryThread(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-refuse")

	_, err := f.app.PromoteScratchThread(t.Context(), source.ID)
	if err == nil {
		t.Fatal("PromoteScratchThread accepted an ordinary thread")
	}
	code, message, ok := errorsx.PublicDetails(err)
	if !ok || code != sideChatNotScratchCode {
		t.Fatalf("refusal = %v (code %q public=%v), want %q", err, code, ok, sideChatNotScratchCode)
	}
	if !strings.Contains(message, "side chat") {
		t.Errorf("refusal message = %q", message)
	}
	after, err := f.app.store.GetThread(source.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.Mode != threadmode.ModeChat {
		t.Errorf("the refused thread's mode moved to %q", after.Mode)
	}
}

// Keeping twice is refused the second time: the first promotion took the
// record with it, so there is no return mode left to restore.
func TestPromoteScratchThreadRefusesASecondKeep(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-twice")
	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}
	if _, err := f.app.PromoteScratchThread(t.Context(), fork.ID); err != nil {
		t.Fatalf("PromoteScratchThread: %v", err)
	}
	if _, err := f.app.PromoteScratchThread(t.Context(), fork.ID); err == nil {
		t.Fatal("a second Keep was accepted")
	}
}

// Closing the pane is DeleteThread, and the scratch record goes with the
// row. Nothing else has to remember to clean it up.
func TestDeleteThreadDropsTheScratchRecord(t *testing.T) {
	f := newRequestFixture(t)
	source := f.forkableThread(t, "side-chat-delete")
	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}

	if err := f.app.DeleteThread(fork.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if _, err := f.app.store.GetThread(fork.ID); err == nil {
		t.Errorf("side chat thread %s survived its delete", fork.ID)
	}
	if _, found, err := f.app.store.GetScratchThread(fork.ID); err != nil || found {
		t.Errorf("scratch record survived the delete: found=%v err=%v", found, err)
	}
	// The source is untouched: a side chat is a fork, and closing it says
	// nothing about the conversation it came from.
	if _, err := f.app.store.GetThread(source.ID); err != nil {
		t.Errorf("the source thread went with its side chat: %v", err)
	}
}
