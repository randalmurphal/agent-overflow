package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// An agent's terminal lives in the agent's own Codex thread, whose process
// table is separate from the root's, so each call names the thread its
// command's process scope names.
func TestBackgroundTerminalRPCsTargetTheScopeThread(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	s.mu.Lock()
	s.collab.childParentByThread = map[string]string{"codex-child-thread": "spawn-1"}
	s.mu.Unlock()

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
	// A thread the session neither runs nor owns an agent of, such as
	// another session's root on the same app-server, is refused too.
	for _, scope := range []string{review, " ", "codex-other-session-thread"} {
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

	// The answer reaches the session as the read loop reads it.
	refuse := func(threadID string) {
		t.Helper()
		_, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
		s.dispatchLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"thread not found: %s"}}`, rpcID, threadID)))
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

// A session that began to close writes no background-terminal request,
// and Close waits for one admitted before it began to be written.
func TestAClosingSessionWritesNoTerminalRequest(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	s.terminalWriteMu.RLock() // a request admitted and still being written
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = s.Close()
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while an admitted request was still being written")
	case <-time.After(50 * time.Millisecond):
	}
	s.terminalWriteMu.RUnlock()
	<-closed
	if err := s.CleanBackgroundTerminals(context.Background(), "codex-root-thread"); err == nil || !strings.Contains(err.Error(), "the session is ending") {
		t.Fatalf("clean after Close = %v, want a refusal because the session is ending", err)
	}
	if frames := readCapturedRawFrames(t, capturePath); len(frames) != 0 {
		t.Fatalf("a closed session wrote %d frames, want none", len(frames))
	}
}

// A closed agent is asked whether Codex still has its thread with the
// smallest request that answers it: one list page of at most one row.
func TestAClosedAgentIsAskedAboutWithOneSmallPage(t *testing.T) {
	t.Parallel()
	s, capturePath := newCapturingSession(t, "codex-root-thread")
	s.mu.Lock()
	s.collab.childParentByThread = map[string]string{"codex-agent-thread": "spawn-1"}
	s.collab.childRuntimeByThread = map[string]childRuntimeState{}
	s.mu.Unlock()
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"codex-root-thread","item":{"id":"close-1","type":"collabAgentToolCall","tool":"closeAgent","receiverThreadIds":["codex-agent-thread"],"status":"completed"}}}`))
	_, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
	s.dispatchLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"data":[{"itemId":"cmd-1","processId":"1"}],"nextCursor":"more"}}`, rpcID)))
	s.collabAsyncWG.Wait()
	waitForCapturedFrames(t, capturePath, 1, backgroundTerminalTestTimeout)
	frames := readCapturedRawFrames(t, capturePath)
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want one list page", len(frames))
	}
	var frame struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Method != "thread/backgroundTerminals/list" || frame.Params["threadId"] != "codex-agent-thread" || frame.Params["limit"] != float64(1) {
		t.Fatalf("probe = %s %v, want one list page of codex-agent-thread with limit 1", frame.Method, frame.Params)
	}
}

// A recovered unload is recorded in the step its revision guards, before it
// is reported: a live status read after it is the later word.
func TestARecoveredUnloadIsRecordedWhereItsRevisionIsChecked(t *testing.T) {
	t.Parallel()
	var (
		s              *Session
		loadedAtReport = true
	)
	s = newMultiAgentV2RoutingSession(t, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventSubagentStatus && strings.Contains(string(evt.Meta), `"notLoaded"`) {
			loadedAtReport = s.childLoaded("codex-child-thread")
		}
	})
	if !s.registerHistoricalChildOwnership("root-provider-thread", "codex-child-thread", "/root/child", "spawn-1") {
		t.Fatal("register ownership")
	}
	s.childLifecycleMu.Lock()
	revision := s.childLifecycleRevision["codex-child-thread"]
	s.childLifecycleMu.Unlock()
	s.emitRecoveredChildUnload("codex-child-thread", "spawn-1", "child-turn", revision)
	if loadedAtReport {
		t.Fatal("the recovered unload was reported before it was recorded")
	}
	if s.childLoaded("codex-child-thread") {
		t.Fatal("the recovered unload was not recorded")
	}
}

// agentTimeline names each agent status and command start in events, in
// order.
func agentTimeline(events []provider.ProviderEvent) []string {
	var out []string
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventSubagentStatus:
			var meta struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(evt.Meta, &meta)
			out = append(out, "status "+meta.Status)
		case provider.EventToolStart:
			out = append(out, evt.ItemID+" start")
		}
	}
	return out
}

// Codex's answer that it has no agent thread loaded is that thread's unload
// at the answer's place in the stream. Codex loading the thread again and
// the thread starting a command after the answer come after the unload, so
// the unload ends neither the new command nor the thread's new residency.
func TestAThreadNotFoundAnswerIsTheUnloadWhereItWasRead(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		events []provider.ProviderEvent
	)
	s, _ := newCapturingSessionEmitting(t, "codex-root-thread", func(evt provider.ProviderEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, evt)
	})
	s.mu.Lock()
	s.collab.childParentByThread = map[string]string{"codex-agent-thread": "spawn-1"}
	s.collab.childRuntimeByThread = map[string]childRuntimeState{}
	s.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := s.TerminateBackgroundTerminal(context.Background(), "codex-agent-thread", "42")
		done <- err
	}()
	_, rpcID := waitForPending(t, s, backgroundTerminalTestTimeout)
	// The read loop reads the answer, then the thread loaded again and
	// starting a command, without waiting for the caller in between.
	s.dispatchLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"thread not found: codex-agent-thread"}}`, rpcID)))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/status/changed","params":{"threadId":"codex-agent-thread","status":{"type":"active"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":` + string(childCommandFrame("codex-agent-thread", "cmd-2", "inProgress")) + `}`))
	if err := <-done; !errors.Is(err, ErrBackgroundTerminalThreadNotLoaded) {
		t.Fatalf("terminate = %v, want ErrBackgroundTerminalThreadNotLoaded", err)
	}
	mu.Lock()
	got := agentTimeline(events)
	mu.Unlock()
	if want := []string{"status notLoaded", "status running", "cmd-2 start"}; !sameOrder(got, want) {
		t.Fatalf("agent timeline = %q, want %q: the unload belongs where its answer was read", got, want)
	}
	if !s.childLoaded("codex-agent-thread") {
		t.Fatal("the agent thread Codex loaded again is recorded unloaded")
	}
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
