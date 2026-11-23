package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
)

// An agent's terminal lives in the agent's own Codex thread, whose process
// table is separate from the root's, so each call names the thread its
// command's process scope names.
func TestBackgroundTerminalRPCsTargetTheScopeThread(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")

	done := make(chan error, 1)
	go func() {
		killed, err := s.TerminateBackgroundTerminal(context.Background(), "codex-child-thread", "42")
		if err == nil && !killed {
			err = errors.New("terminated = false, want the wire's true")
		}
		done <- err
	}()
	answerer := newPendingAnswerer(s)
	answerer.answer(t, `{"terminated":true}`)
	if err := <-done; err != nil {
		t.Fatalf("TerminateBackgroundTerminal: %v", err)
	}
	go func() { done <- s.CleanBackgroundTerminals(context.Background(), "codex-child-thread") }()
	answerer.answer(t, `{}`)
	if err := <-done; err != nil {
		t.Fatalf("CleanBackgroundTerminals: %v", err)
	}
	go func() {
		_, err := s.ListBackgroundTerminals(context.Background(), "codex-child-thread")
		done <- err
	}()
	answerer.answer(t, `{"data":[],"nextCursor":null}`)
	if err := <-done; err != nil {
		t.Fatalf("ListBackgroundTerminals: %v", err)
	}

	frames := waitForCapturedFrames(t, capturePath, 3, backgroundTerminalTestTimeout)
	for i, want := range []string{"thread/backgroundTerminals/terminate", "thread/backgroundTerminals/clean", "thread/backgroundTerminals/list"} {
		if frames[i].Method != want || frames[i].Params.ThreadID != "codex-child-thread" {
			t.Errorf("frame %d = %s on %q, want %s on the agent's thread codex-child-thread", i, frames[i].Method, frames[i].Params.ThreadID, want)
		}
	}
}

// A scope no request can reach is refused before anything is sent: a
// review run's processes are the review delegate's, which has no thread
// the app-server resolves, and a command without a scope names none.
func TestBackgroundTerminalRPCsRefuseAnUnaddressableScope(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	review := reviewProcessScope("review-launch").ID
	for _, scope := range []string{review, " "} {
		if _, err := s.TerminateBackgroundTerminal(context.Background(), scope, "42"); err == nil {
			t.Errorf("terminate in scope %q: err = nil, want a refusal", scope)
		}
		if err := s.CleanBackgroundTerminals(context.Background(), scope); err == nil {
			t.Errorf("clean in scope %q: err = nil, want a refusal", scope)
		}
		if _, err := s.ListBackgroundTerminals(context.Background(), scope); err == nil {
			t.Errorf("list in scope %q: err = nil, want a refusal", scope)
		}
	}
	if frames := readCapturedRawFrames(t, capturePath); len(frames) != 0 {
		t.Fatalf("sent %d frames for scopes no request reaches, want none", len(frames))
	}
}

// An agent thread the app-server no longer has loaded answers "thread not
// found". That answer is the thread's unload, reported as its notLoaded
// status naming its process scope, which ends its commands. The route of an
// agent a close item named ends with it; an agent no close named keeps its
// route, because Codex loads an idle agent again when it is addressed. The
// root thread's refusal is a plain error.
func TestBackgroundTerminalRPCsReadAnUnloadedAgentThread(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		events []provider.ProviderEvent
	)
	s, capturePath := newCapturingSessionEmitting(t, "codex-root-thread", func(evt provider.ProviderEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, evt)
	})
	s.mu.Lock()
	s.collab.childParentByThread = map[string]string{"codex-closed-thread": "spawn-1", "codex-idle-thread": "spawn-2"}
	s.collab.childResidency = map[string]childResidency{"codex-closed-thread": {closed: true}}
	s.collab.childRuntimeByThread = map[string]childRuntimeState{}
	s.mu.Unlock()

	refuse := func(threadID string) {
		t.Helper()
		ch, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
		ch <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"thread not found: %s"}}`, rpcID, threadID))
	}
	done := make(chan error, 1)
	for _, threadID := range []string{"codex-closed-thread", "codex-idle-thread"} {
		go func() {
			_, err := s.TerminateBackgroundTerminal(context.Background(), threadID, "42")
			done <- err
		}()
		refuse(threadID)
		if err := <-done; !errors.Is(err, ErrBackgroundTerminalThreadNotLoaded) {
			t.Fatalf("terminate in unloaded %s: err = %v, want ErrBackgroundTerminalThreadNotLoaded", threadID, err)
		}
	}
	mu.Lock()
	got := append([]provider.ProviderEvent(nil), events...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("events = %+v, want one unload per thread", got)
	}
	for i, want := range []struct{ thread, spawn string }{{"codex-closed-thread", "spawn-1"}, {"codex-idle-thread", "spawn-2"}} {
		evt := got[i]
		if evt.Kind != provider.EventSubagentStatus || evt.ItemID != want.spawn || evt.ProcessScope.ID != want.thread || !strings.Contains(string(evt.Meta), `"notLoaded"`) {
			t.Fatalf("event %d = %+v, want %s's notLoaded on %s", i, evt, want.thread, want.spawn)
		}
	}
	if parent := s.parentToolUseForProviderThread("codex-closed-thread"); parent != "" {
		t.Fatalf("closed agent's route = %q after Codex said its thread is gone, want none", parent)
	}
	if parent := s.parentToolUseForProviderThread("codex-idle-thread"); parent != "spawn-2" {
		t.Fatalf("idle agent's route = %q, want spawn-2 kept: no close named it", parent)
	}

	go func() { done <- s.CleanBackgroundTerminals(context.Background(), "codex-root-thread") }()
	refuse("codex-root-thread")
	if err := <-done; err == nil || errors.Is(err, ErrBackgroundTerminalThreadNotLoaded) {
		t.Fatalf("clean refused for the root thread: err = %v, want a plain error", err)
	}
	waitForCapturedFrames(t, capturePath, 3, backgroundTerminalTestTimeout)
}

// The unload "thread not found" reports waits behind the frames still queued
// for the session's dispatch, so it cannot pass a command start that came
// before it.
func TestThreadNotFoundWaitsBehindTheChildBacklog(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(evt provider.ProviderEvent) { events = append(events, evt) })
	if !s.registerHistoricalChildOwnership("root-provider-thread", "codex-child-thread", "/root/child", "spawn-1") {
		t.Fatal("register ownership")
	}
	// The read loop holds the dispatch token, with the agent's command start
	// still queued behind the frame it is dispatching.
	s.earlyFramesMu.Lock()
	s.dispatching = true
	s.earlyFramesMu.Unlock()
	s.enqueueEarlyFrames([]deferredChildWireEvent{{Method: "item/started", Params: childCommandFrame("codex-child-thread", "cmd-1", "inProgress")}})

	s.observeChildThreadGone("codex-child-thread")
	if len(events) != 0 {
		t.Fatalf("the unload passed the queue: %+v", events)
	}
	s.endDispatch()
	if len(events) != 2 || events[0].Kind != provider.EventToolStart || events[0].ItemID != "cmd-1" ||
		events[1].Kind != provider.EventSubagentStatus || events[1].ProcessScope.ID != "codex-child-thread" {
		t.Fatalf("events after the queue = %+v, want the command start, then the thread's unload", events)
	}
}
