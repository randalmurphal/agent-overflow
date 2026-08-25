package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/provider"
)

func TestRolloutSubagentNotificationLineEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-done": "call-collab-1",
			},
		},
	}

	line := rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	})
	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "call-collab-1" {
		t.Fatalf("ItemID = %q, want call-collab-1", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-done" {
		t.Errorf("meta.agent_path = %v, want child-done", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status = %v, want completed", meta["status"])
	}
	if meta["message"] != "detached child finished" {
		t.Errorf("meta.message = %v, want detached child finished", meta["message"])
	}
}

func TestRolloutSubagentNotificationLineEmitsWithoutProviderMapping(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	if !s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-resumed", map[string]any{
		"completed": "detached child finished after resume",
	})) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "" {
		t.Fatalf("ItemID = %q, want empty so triage can resolve persisted launch", events[0].ItemID)
	}
}

func TestRolloutAndRawSubagentNotificationDedupes(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-done": "call-collab-1",
			},
		},
	}
	s.setRootThreadID("parent-provider-thread")

	rawLine := rawUserSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "child-done",
		"status": map[string]any{
			"completed": "detached child finished",
		},
	})
	s.dispatchLine(rawLine)
	s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	}))

	var notificationCount int
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			notificationCount++
		}
	}
	if notificationCount != 1 {
		t.Fatalf("EventSubagentNotification count = %d, want 1; events=%+v", notificationCount, events)
	}
}

func TestWatchRolloutSubagentNotificationsEmitsSplitLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}

	events := make(chan provider.ProviderEvent, 1)
	s := &Session{
		threadID: "parent-thread",
		readDone: make(chan struct{}),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
	}
	s.setRootThreadID("parent-provider-thread")
	path, offset, err := prepareRolloutSubagentNotificationObserver(path, "parent-provider-thread")
	if err != nil {
		t.Fatalf("prepare rollout observer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchRolloutSubagentNotifications(ctx, path, offset)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("rollout watcher did not exit after cancel")
		}
	})

	line := append(rolloutUserSubagentNotificationLine(t, "child-resumed", "completed"), '\n')
	split := len(line) / 2
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(line[:split]); err != nil {
		file.Close()
		t.Fatalf("append first half: %v", err)
	}
	select {
	case evt := <-events:
		t.Fatalf("watcher emitted before newline: %+v", evt)
	case <-time.After(rolloutSubagentNotificationPollInterval * 2):
	}
	if _, err := file.Write(line[split:]); err != nil {
		file.Close()
		t.Fatalf("append second half: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	select {
	case evt := <-events:
		if evt.Kind != provider.EventSubagentNotification {
			t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventSubagentNotification)
		}
		if evt.ItemID != "" {
			t.Fatalf("ItemID = %q, want empty for persisted triage resolution", evt.ItemID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rollout notification event")
	}
}

func TestReadRolloutAppendStartsAfterExistingHistory(t *testing.T) {
	const threadID = "0199c0de-dead-beef-cafe-000000000001"
	path := filepath.Join(t.TempDir(), "rollout-2026-08-24T00-00-00-"+threadID+".jsonl")
	historical := append(rolloutUserSubagentNotificationLine(t, "child-old", "completed"), '\n')
	if err := os.WriteFile(path, historical, 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	// The production entry point is the observer's own preparation step: the
	// start offset IS the file's current size, so everything already on disk
	// is history and only appends are read.
	resolved, offset, err := prepareRolloutSubagentNotificationObserver(path, threadID)
	if err != nil {
		t.Fatalf("prepareRolloutSubagentNotificationObserver: %v", err)
	}
	if resolved != path {
		t.Fatalf("resolved path = %q, want %q", resolved, path)
	}
	if offset != int64(len(historical)) {
		t.Fatalf("offset = %d, want %d", offset, len(historical))
	}

	fresh := append(rolloutUserSubagentNotificationLine(t, "child-fresh", "completed"), '\n')
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(fresh); err != nil {
		file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	chunk, _, err := readRolloutAppend(path, offset)
	if err != nil {
		t.Fatalf("readRolloutAppend: %v", err)
	}
	if string(chunk) != string(fresh) {
		t.Fatalf("chunk = %q, want only fresh line %q", string(chunk), string(fresh))
	}
}

func TestPrepareRolloutSubagentNotificationObserverValidatesPath(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(valid, []byte("history\n"), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	path, offset, err := prepareRolloutSubagentNotificationObserver(valid, "parent-provider-thread")
	if err != nil {
		t.Fatalf("valid rollout rejected: %v", err)
	}
	if path != filepath.Clean(valid) {
		t.Fatalf("path = %q, want %q", path, filepath.Clean(valid))
	}
	if offset != int64(len("history\n")) {
		t.Fatalf("offset = %d, want history length", offset)
	}

	mismatch := filepath.Join(dir, "rollout-2026-06-16T00-01-18-other-thread.jsonl")
	if err := os.WriteFile(mismatch, nil, 0644); err != nil {
		t.Fatalf("write mismatch rollout: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(mismatch, "parent-provider-thread"); err == nil {
		t.Fatal("expected mismatched thread id path to be rejected")
	}

	symlink := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread-link.jsonl")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(symlink, "parent-provider-thread"); err == nil {
		t.Fatal("expected symlink rollout path to be rejected")
	}
}

// --- Arming: who gets a rollout tail at all ---

// TestResumeArmsRolloutTailOnlyWithUnresolvedSubagents pins the relevance gate.
// The tail exists for ONE gap: a resumed thread cannot opt into raw events
// (`experimentalRawEvents` is a thread/start field held in the app-server's
// in-memory ThreadState), so a detached child's mailbox delivery is invisible
// on its wire. A thread with nothing outstanding cannot hit that gap, and
// polling its rollout file every 150ms for the life of the session buys
// nothing — which is what this thread's resume used to do unconditionally.
func TestResumeArmsRolloutTailOnlyWithUnresolvedSubagents(t *testing.T) {
	cases := []struct {
		name       string
		unresolved bool
		wantEvent  bool
	}{
		{name: "unresolved spawn children arm the tail", unresolved: true, wantEvent: true},
		{name: "nothing outstanding leaves it unarmed", unresolved: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const providerThreadID = "mock-thread-123"
			dir := t.TempDir()
			rollout := filepath.Join(dir, "rollout-2026-08-25T00-00-00-"+providerThreadID+".jsonl")
			if err := os.WriteFile(rollout, nil, 0o644); err != nil {
				t.Fatalf("write rollout: %v", err)
			}

			events := make(chan provider.ProviderEvent, 8)
			cfg := Config{
				Binary: codexReviewerEchoScript(t, filepath.Join(dir, "codex-stdin.log"),
					fmt.Sprintf(`{\"thread\":{\"id\":\"%s\",\"path\":\"%s\"}}`, providerThreadID, rollout)),
				WorkDir:                      dir,
				ResumeThreadID:               providerThreadID,
				ResumeHasUnresolvedSubagents: tc.unresolved,
			}
			s, err := NewSession(context.Background(), testThread, cfg, func(evt provider.ProviderEvent) {
				events <- evt
			})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			if got := rolloutTailStarted(s); got != tc.unresolved {
				t.Fatalf("rollout tail started = %v, want %v", got, tc.unresolved)
			}
			appendRolloutLine(t, rollout, rolloutUserSubagentNotificationLine(t, "child-detached", "completed"))

			if got := awaitSubagentNotification(t, events, tc.wantEvent); got != tc.wantEvent {
				t.Fatalf("observed rollout notification = %v, want %v", got, tc.wantEvent)
			}
		})
	}
}

// TestSpawnObservedMidSessionArmsRolloutTail covers the other half of the gate:
// a resume that legitimately started unarmed still has to catch a child spawned
// AFTERWARDS, whose mailbox delivery this session is just as blind to. The
// signal is the typed ownership registration both spawn shapes funnel through
// (V1 `collabAgentToolCall spawn_agent`, V2 `subAgentActivity kind:"started"`),
// never a string sniff of the wire.
func TestSpawnObservedMidSessionArmsRolloutTail(t *testing.T) {
	const providerThreadID = "0199c0de-dead-beef-cafe-000000000002"
	dir := t.TempDir()
	rollout := filepath.Join(dir, "rollout-2026-08-25T00-00-00-"+providerThreadID+".jsonl")
	if err := os.WriteFile(rollout, nil, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	events := make(chan provider.ProviderEvent, 16)
	s := newUnarmedRolloutTailSession(t, providerThreadID, rollout, events)

	if rolloutTailStarted(s) {
		t.Fatal("rollout tail armed before any spawn was observed")
	}
	s.dispatchLine(subAgentActivityStartedLine(t, providerThreadID, "call-collab-1", "child-thread-9"))
	if !rolloutTailStarted(s) {
		t.Fatal("a spawn observed on the wire did not arm the rollout tail")
	}

	appendRolloutLine(t, rollout, rolloutUserSubagentNotificationLine(t, "child-thread-9", "completed"))
	if !awaitSubagentNotification(t, events, true) {
		t.Fatal("armed rollout tail did not emit the appended notification")
	}
}

// TestRolloutTailIsNeverArmedOnAFreshThread is the structural half of "a
// thread/start session keeps its raw events": no rollout path is ever recorded
// for it, so every arming site is a no-op rather than a judgement call at each
// call site.
func TestRolloutTailIsNeverArmedOnAFreshThread(t *testing.T) {
	events := make(chan provider.ProviderEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Session{
		threadID: testThread,
		ctx:      ctx,
		cancel:   cancel,
		readDone: make(chan struct{}),
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(evt provider.ProviderEvent) { events <- evt },
	}
	s.setRootThreadID("fresh-provider-thread")

	s.armRolloutSubagentNotificationTail("test")
	if rolloutTailStarted(s) {
		t.Fatal("arming a session with no recorded rollout path started a tail")
	}
}

// --- Buffer reuse ---

// TestRetainRolloutPartialLineReusesBufferAcrossTicks pins the allocation fix:
// an incomplete line is copied BACK INTO the same buffer every tick instead of
// into a fresh one (measured ~28MB/h of churn at the 150ms poll). The carry
// aliases the buffer's own array, so the copy is also the case a naive
// implementation gets wrong.
func TestRetainRolloutPartialLineReusesBufferAcrossTicks(t *testing.T) {
	// Sized like the steady state the loop settles into: a buffer that has
	// already grown past the fragment it is asked to hold.
	buf := retainRolloutPartialLine(make([]byte, 0, 256), []byte(`{"type":"resp`))
	if string(buf) != `{"type":"resp` {
		t.Fatalf("first retain = %q", string(buf))
	}
	first := unsafe.SliceData(buf)

	// The tick that follows assembles buf+chunk in place and carries a SUFFIX
	// of that same array back — exactly what the watch loop does.
	assembled := append(buf, []byte("onse_item\",\"payload\":{}}\nleft")...)
	if unsafe.SliceData(assembled) != first {
		t.Fatalf("assembly reallocated at cap %d; widen the fixture", cap(buf))
	}
	carry := assembled[len(assembled)-len("left"):]
	buf = retainRolloutPartialLine(assembled, carry)
	if string(buf) != "left" {
		t.Fatalf("aliased retain = %q, want %q", string(buf), "left")
	}
	if got := unsafe.SliceData(buf); got != first {
		t.Fatalf("aliased retain reallocated the partial buffer (%p -> %p)", first, got)
	}

	// A completed line leaves the buffer empty but KEEPS it, so the next
	// partial costs no allocation at all.
	buf = retainRolloutPartialLine(buf, nil)
	if len(buf) != 0 {
		t.Fatalf("completed-line retain left %q", string(buf))
	}
	if got := unsafe.SliceData(buf); got != first {
		t.Fatalf("completed-line retain dropped a reusable buffer (%p -> %p)", first, got)
	}
}

// TestRetainRolloutPartialLineShedsOversizedCapacity pins the counterweight to
// the reuse above: one pathological line can grow the buffer toward
// rolloutSubagentNotificationMaxLineBytes, and keeping that capacity would pin
// it for the rest of the session on the strength of a single record.
func TestRetainRolloutPartialLineShedsOversizedCapacity(t *testing.T) {
	huge := make([]byte, 0, rolloutSubagentNotificationPartialKeepBytes*4)

	// While the long line is still incomplete the buffer must survive — the
	// shed is a boundary rule, not a size cap on the line itself.
	huge = retainRolloutPartialLine(huge, bytes.Repeat([]byte("x"), rolloutSubagentNotificationPartialKeepBytes*2))
	if cap(huge) <= rolloutSubagentNotificationPartialKeepBytes {
		t.Fatalf("incomplete long line shed its buffer early (cap %d)", cap(huge))
	}

	if shed := retainRolloutPartialLine(huge, nil); shed != nil {
		t.Fatalf("completed long line kept a %d-byte buffer, want it dropped", cap(shed))
	}

	small := make([]byte, 0, rolloutSubagentNotificationPartialKeepBytes)
	if kept := retainRolloutPartialLine(small, nil); kept == nil || cap(kept) != cap(small) {
		t.Fatalf("an at-bound buffer was dropped instead of reused")
	}
}

// TestWatchRolloutSubagentNotificationsAssemblesAcrossManyTicks drives the real
// loop over a line fragmented across four poll ticks, with the tail of the
// fourth carrying the start of a second record. Reusing the partial buffer must
// not disturb either boundary.
func TestWatchRolloutSubagentNotificationsAssemblesAcrossManyTicks(t *testing.T) {
	const providerThreadID = "0199c0de-dead-beef-cafe-000000000003"
	path := filepath.Join(t.TempDir(), "rollout-2026-08-25T00-00-00-"+providerThreadID+".jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	events := make(chan provider.ProviderEvent, 8)
	s := &Session{
		threadID: testThread,
		readDone: make(chan struct{}),
		onEvent:  func(evt provider.ProviderEvent) { events <- evt },
	}
	s.setRootThreadID(providerThreadID)
	resolved, offset, err := prepareRolloutSubagentNotificationObserver(path, providerThreadID)
	if err != nil {
		t.Fatalf("prepare rollout observer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchRolloutSubagentNotifications(ctx, resolved, offset)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("rollout watcher did not exit after cancel")
		}
	})

	first := append(rolloutUserSubagentNotificationLine(t, "child-split", "completed"), '\n')
	second := append(rolloutUserSubagentNotificationLine(t, "child-second", "errored"), '\n')
	quarter := len(first) / 4
	fragments := [][]byte{
		first[:quarter],
		first[quarter : quarter*2],
		first[quarter*2 : quarter*3],
		append(append([]byte(nil), first[quarter*3:]...), second[:len(second)/2]...),
		second[len(second)/2:],
	}
	for index, fragment := range fragments {
		appendRolloutBytes(t, path, fragment)
		if index < 3 {
			// Nothing may surface before the newline lands.
			select {
			case evt := <-events:
				t.Fatalf("fragment %d emitted early: %+v", index, evt)
			case <-time.After(rolloutSubagentNotificationPollInterval * 2):
			}
		}
	}

	for _, want := range []string{"child-split", "child-second"} {
		select {
		case evt := <-events:
			if evt.Kind != provider.EventSubagentNotification {
				t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventSubagentNotification)
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if meta["agent_path"] != want {
				t.Fatalf("meta.agent_path = %v, want %s", meta["agent_path"], want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for the %s notification", want)
		}
	}
}

// --- helpers ---

func rolloutTailStarted(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rolloutTail.started
}

// newUnarmedRolloutTailSession builds a session that has RESUMED (its rollout
// path is recorded) but was told nothing was outstanding, so the tail is idle
// until a spawn arrives. proc stays nil, which is what keeps the collab
// metadata read off the wire.
func newUnarmedRolloutTailSession(t *testing.T, providerThreadID, rollout string, events chan provider.ProviderEvent) *Session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		threadID: testThread,
		ctx:      ctx,
		cancel:   cancel,
		readDone: make(chan struct{}),
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(evt provider.ProviderEvent) { events <- evt },
	}
	s.setRootThreadID(providerThreadID)
	s.prepareRolloutSubagentNotificationTail(rollout)
	t.Cleanup(func() {
		cancel()
		s.rolloutObserverWG.Wait()
	})
	return s
}

func subAgentActivityStartedLine(t *testing.T, threadID, itemID, childThreadID string) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "item/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "turn-1",
			"item": map[string]any{
				"type":          "subAgentActivity",
				"id":            itemID,
				"kind":          "started",
				"agentThreadId": childThreadID,
				"agentPath":     "/root/reviewer",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal subAgentActivity line: %v", err)
	}
	return line
}

func appendRolloutLine(t *testing.T, path string, line []byte) {
	t.Helper()
	appendRolloutBytes(t, path, append(append([]byte(nil), line...), '\n'))
}

func appendRolloutBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open rollout for append: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}
}

// awaitSubagentNotification waits for a rollout-sourced notification. want=false
// callers pay a short bounded wait instead of a long one: the assertion is that
// nothing is polling the file at all, and the poll interval is 150ms.
func awaitSubagentNotification(t *testing.T, events <-chan provider.ProviderEvent, want bool) bool {
	t.Helper()
	budget := rolloutSubagentNotificationPollInterval * 8
	if want {
		budget = 5 * time.Second
	}
	deadline := time.After(budget)
	for {
		select {
		case evt := <-events:
			if evt.Kind == provider.EventSubagentNotification {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
