package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// --- Event capture plumbing for E2E tests ---

// capturedEvent is a single Wails-bound emission intercepted for assertions.
type capturedEvent struct {
	Name string
	Data any
}

// capturedEventBus is a thread-safe sink for events emitted via the triage
// emit function and the router's SetEventHook observer. Tests can Next()
// the channel to read events in order.
//
// Wire emissions go into `ch` under their real event name. Router
// eventHook callbacks are recorded under the synthetic name
// "provider:event" so tests that synchronize on a routing-pipeline step
// (EventTurnComplete, EventInit, EventTextDelta, etc.) can read through
// nextProviderEventOfKind. This keeps the hook private to tests while
// preserving the pre-migration test-bus API.
type capturedEventBus struct {
	mu  sync.Mutex
	ch  chan capturedEvent
	all []capturedEvent
}

func newCapturedEventBus() *capturedEventBus {
	return &capturedEventBus{ch: make(chan capturedEvent, 256)}
}

func (b *capturedEventBus) emit(name string, data any) {
	b.mu.Lock()
	b.all = append(b.all, capturedEvent{Name: name, Data: data})
	b.mu.Unlock()
	// Drop if the channel is full so tests do not block Wails emission.
	select {
	case b.ch <- capturedEvent{Name: name, Data: data}:
	default:
	}
}

// emitChannel is the triage.Router emit callback: same recorder, typed
// channel. Two methods rather than one because testEmitHook observes
// a.emit's wire spelling (a string) while the router emits eventchan
// constants.
func (b *capturedEventBus) emitChannel(name eventchan.Channel, data any) {
	b.emit(name.String(), data)
}

// observeRouterEvent is the router.SetEventHook callback. Each Handle
// call pipes its incoming ProviderEvent through here AFTER the routing
// switch returns, giving tests a deterministic sync point without a
// wire-channel emission. The synthetic "provider:event" name keeps
// nextProviderEventOfKind unchanged.
func (b *capturedEventBus) observeRouterEvent(evt provider.ProviderEvent) {
	b.emit("provider:event", evt)
}

// next reads the next event off the channel with a timeout. Fails the test if
// nothing arrives in time.
func (b *capturedEventBus) next(t *testing.T, d time.Duration) capturedEvent {
	t.Helper()
	select {
	case e := <-b.ch:
		return e
	case <-time.After(d):
		b.mu.Lock()
		recent := append([]capturedEvent(nil), b.all...)
		b.mu.Unlock()
		t.Fatalf("timed out waiting for event (captured so far: %d): %s", len(recent), summarizeEvents(recent))
		return capturedEvent{}
	}
}

// nextOfKind waits until an event of the given name arrives, or fails on
// timeout. Intermediate events are discarded from the channel (but still
// retained in the `all` log for diagnostics).
func (b *capturedEventBus) nextOfKind(t *testing.T, name string, d time.Duration) capturedEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			b.mu.Lock()
			recent := append([]capturedEvent(nil), b.all...)
			b.mu.Unlock()
			t.Fatalf("timed out waiting for event %q (captured: %s)", name, summarizeEvents(recent))
			return capturedEvent{}
		}
		select {
		case e := <-b.ch:
			if e.Name == name {
				return e
			}
		case <-time.After(remaining):
			b.mu.Lock()
			recent := append([]capturedEvent(nil), b.all...)
			b.mu.Unlock()
			t.Fatalf("timed out waiting for event %q (captured: %s)", name, summarizeEvents(recent))
			return capturedEvent{}
		}
	}
}

// nextProviderEventOfKind blocks until a `provider:event` envelope of the
// given EventKind arrives.
func (b *capturedEventBus) nextProviderEventOfKind(t *testing.T, kind provider.EventKind, d time.Duration) provider.ProviderEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for provider:event of kind %q", kind)
			return provider.ProviderEvent{}
		}
		select {
		case e := <-b.ch:
			if e.Name != "provider:event" {
				continue
			}
			evt, ok := e.Data.(provider.ProviderEvent)
			if !ok {
				continue
			}
			if evt.Kind == kind {
				return evt
			}
		case <-time.After(remaining):
			t.Fatalf("timed out waiting for provider:event of kind %q", kind)
			return provider.ProviderEvent{}
		}
	}
}

func (b *capturedEventBus) nextApprovalEvent(t *testing.T, action string, d time.Duration) provider.ApprovalEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			b.mu.Lock()
			recent := append([]capturedEvent(nil), b.all...)
			b.mu.Unlock()
			t.Fatalf("timed out waiting for provider:approval action %q (captured: %s)", action, summarizeEvents(recent))
			return provider.ApprovalEvent{}
		}
		select {
		case e := <-b.ch:
			if e.Name != "provider:approval" {
				continue
			}
			evt, ok := e.Data.(provider.ApprovalEvent)
			if !ok {
				continue
			}
			if evt.Action == action {
				return evt
			}
		case <-time.After(remaining):
			b.mu.Lock()
			recent := append([]capturedEvent(nil), b.all...)
			b.mu.Unlock()
			t.Fatalf("timed out waiting for provider:approval action %q (captured: %s)", action, summarizeEvents(recent))
			return provider.ApprovalEvent{}
		}
	}
}

func (b *capturedEventBus) nextUsageEvent(t *testing.T, action string, d time.Duration) provider.UsageEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for provider:usage action %q", action)
			return provider.UsageEvent{}
		}
		select {
		case e := <-b.ch:
			if e.Name != "provider:usage" {
				continue
			}
			evt, ok := e.Data.(provider.UsageEvent)
			if !ok {
				continue
			}
			if evt.Action == action {
				return evt
			}
		case <-time.After(remaining):
			t.Fatalf("timed out waiting for provider:usage action %q", action)
			return provider.UsageEvent{}
		}
	}
}

// allEvents returns a snapshot of every captured event (including ones already
// consumed via next/nextOfKind).
func (b *capturedEventBus) allEvents() []capturedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]capturedEvent, len(b.all))
	copy(out, b.all)
	return out
}

func summarizeEvents(events []capturedEvent) string {
	var b strings.Builder
	for i, e := range events {
		if i > 0 {
			b.WriteString(", ")
		}
		if pe, ok := e.Data.(provider.ProviderEvent); ok {
			fmt.Fprintf(&b, "%s[%s]", e.Name, pe.Kind)
		} else {
			b.WriteString(e.Name)
		}
	}
	return b.String()
}

// --- Integration app builder ---

// setupE2EApp constructs a full App wired against a real SQLite store, a real
// triage router, and a captured event bus (no Wails app, no telemetry). The
// App is safe to call methods on directly; sessions will emit through the
// sessionEventHandler into the triage router, whose emit function pushes
// everything into the returned *capturedEventBus.
func setupE2EApp(t *testing.T) (*App, *capturedEventBus) {
	t.Helper()

	bus := newCapturedEventBus()

	st := storetest.Clone(t)

	app := &App{
		store:    st,
		settings: settings.NewService(t.TempDir()),
	}
	app.triage = triage.NewRouter(st, bus.emitChannel)
	app.triage.SetEventHook(bus.observeRouterEvent)
	ensureDefaultTestProject(t, app)
	// No test may reach a real provider binary or the developer's provider
	// homes; a session-starting test installs a mock over the poisoned
	// defaults. See isolateE2EProviderSpawns.
	isolateE2EProviderSpawns(t, app)

	// Ensure any sessions that remain open at test end are torn down.
	t.Cleanup(func() {
		sessions := make([]session, 0, len(app.sessionManager().runtime.Snapshot()))
		for _, s := range app.sessionManager().runtime.Snapshot() {
			sessions = append(sessions, s)
		}
		app.sessionManager().snapshotAndClear()
		for _, s := range sessions {
			if ps := s.ProviderSession(); ps != nil {
				_ = ps.Close()
			}
		}
	})

	return app, bus
}

// e2eThread builds a thread record suitable for E2E tests. Caller must insert
// it via app.store.CreateThread.
func e2eThread(id, providerName, workspace string) store.Thread {
	now := time.Now().UnixMilli()
	return store.Thread{
		ID:            id,
		ProjectID:     defaultTestProjectID,
		Title:         "E2E Thread",
		Provider:      providerName,
		WorkspacePath: workspace,
		Model:         "claude-opus-4-7",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// --- Part B: End-to-end lifecycle ---

// TestE2E_FullClaudeSessionHappyPath: CreateThread → StartSession → SendMessage
// → mock emits init + text + result → verify events arrive in order and
// assistant text is persisted.
func TestE2E_FullClaudeSessionHappyPath(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Mock: after first user message, emit init, assistant text, result.
	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-happy","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello from mock"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hello from mock"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Expect init then text_delta then turn_complete (post triage processing).
	initEvt := bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	if initEvt.ThreadID != thread.ID {
		t.Fatalf("init threadID = %q", initEvt.ThreadID)
	}

	textEvt := bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	if textEvt.Content != "hello from mock" {
		t.Fatalf("text content = %q", textEvt.Content)
	}

	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// Assistant text should be persisted as a text item for turn 1 (SendMessage
	// incremented the turn index).
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var foundText bool
	for _, it := range items {
		if it.Role == "assistant" && it.Kind == "assistant_text" && it.Summary == "hello from mock" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("assistant text not persisted: %+v", items)
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	_, stillActive := app.sessionManager().get(thread.ID)
	if stillActive {
		t.Fatalf("sessions[%s] should be removed after StopSession", thread.ID)
	}
}

// TestE2E_FullCodexSessionHappyPath: same flow for Codex, verifying the
// provider-agnostic triage handles Codex notifications end-to-end.
func TestE2E_FullCodexSessionHappyPath(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	binary := testutil.WriteMockCodexSession(t, t.TempDir(), map[string]string{
		`"method":"initialize"`:   `{"jsonrpc":"2.0","id":%s,"result":{}}`,
		`"method":"thread/start"`: `{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-thread-happy"}}}`,
		`"method":"turn/start"`:   `{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-1"}}}`,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Codex emits EventInit synchronously from NewSession after the thread/start
	// handshake. Verify we captured it.
	initEvt := bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	if initEvt.ThreadID != thread.ID {
		t.Fatalf("init threadID = %q", initEvt.ThreadID)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(initEvt.Meta, &info); err != nil {
		t.Fatalf("unmarshal init meta: %v", err)
	}
	if info.SessionID != "codex-thread-happy" {
		t.Fatalf("init sessionID = %q", info.SessionID)
	}

	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Note: our mock does not send notifications, so no TurnStart/TextDelta
	// will reach triage. What we can assert is that the user item persisted.
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userMessage bool
	for _, it := range items {
		if it.Role == "user" && it.Summary == "hi" {
			userMessage = true
		}
	}
	if !userMessage {
		t.Fatalf("user message not persisted: %+v", items)
	}
}

// TestE2E_MultiTurnClaude: two consecutive SendMessage calls, each triggers a
// result/turn_complete. Verify turn indices are 1 and 2 and items route to the
// correct turn.
func TestE2E_MultiTurnClaude(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.UpdateThreadRuntimeMode(thread.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}
	storedThread, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if storedThread.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Fatalf("runtime mode = %q, want approval-required", storedThread.RuntimeMode)
	}

	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-multi","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"turn one"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"turn one"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
		{
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"turn two"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"turn two"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := app.SendMessage(thread.ID, "first", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	if err := app.SendMessage(thread.ID, "second", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	// Expect: turn 0 user "first", turn 0 assistant "turn one",
	//         turn 1 user "second", turn 1 assistant "turn two"
	// (exact ordering depends on persist timing; check items per turn).
	turn0Summaries := map[string]bool{}
	turn1Summaries := map[string]bool{}
	for _, it := range items {
		switch it.TurnIndex {
		case 0:
			turn0Summaries[it.Summary] = true
		case 1:
			turn1Summaries[it.Summary] = true
		}
	}
	if !turn0Summaries["first"] || !turn0Summaries["turn one"] {
		t.Fatalf("turn 0 missing expected summaries: %v", turn0Summaries)
	}
	if !turn1Summaries["second"] || !turn1Summaries["turn two"] {
		t.Fatalf("turn 1 missing expected summaries: %v", turn1Summaries)
	}

	_ = app.StopSession(thread.ID)
}

// TestE2E_InterruptMidTurn: send a message that the mock is going to respond to
// with a slow partial text, then call InterruptTurn. Next SendMessage should
// still work.
func TestE2E_InterruptMidTurn(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.UpdateThreadRuntimeMode(thread.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}

	// The mock script auto-acks interrupt control_requests with a
	// success response and does not advance the index counter, so
	// responses[i] tracks user-message turns 1:1 instead of every
	// stdin line.
	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-int","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial..."}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial..."}]}}`,
		},
		{
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"after interrupt"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"after interrupt"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := app.SendMessage(thread.ID, "start", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait for the partial text delta before interrupting.
	text := bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	if text.Content != "partial..." {
		t.Fatalf("partial content = %q", text.Content)
	}

	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}

	// Session should still be active.
	_, active := app.sessionManager().get(thread.ID)
	if !active {
		t.Fatal("session vanished after interrupt")
	}

	if err := app.SendMessage(thread.ID, "continue", nil); err != nil {
		t.Fatalf("SendMessage after interrupt: %v", err)
	}
	after := bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	if after.Content != "after interrupt" {
		t.Fatalf("post-interrupt content = %q", after.Content)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	_ = app.StopSession(thread.ID)
}

// TestE2E_ProviderExitsMidTurn: the mock exits after emitting one line,
// which should surface as a session disconnect; state is still recoverable.
func TestE2E_ProviderExitsMidTurn(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Emit init, then exit immediately on first stdin line.
	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-exit","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)

	// When the mock's stdin-read loop exits (after echoing responses and
	// returning), the session reader sees EOF and emits disconnected.
	// Give the mock time to exit naturally: scripted responses loop ends after
	// one iteration, then the while-read exits on next EOF.
	// Close stdin by telling StopSession to clean up — that triggers a
	// session_status disconnected event.
	_ = app.StopSession(thread.ID)

	// After StopSession the session is gone from the map; but the disconnected
	// event may be in flight. Drain to verify eventually.
	// Accept either a disconnected SessionStatus or a session already removed.
	_, active := app.sessionManager().get(thread.ID)
	if active {
		t.Fatal("sessions map still contains thread after StopSession")
	}

	// Reconnect: reset binary to something that gives a fresh init.
	binary2 := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-exit-2","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"back"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"back"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary2}); err != nil {
		t.Fatalf("set binary2: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("restart session: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hello again", nil); err != nil {
		t.Fatalf("SendMessage after restart: %v", err)
	}
	txt := bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	if txt.Content != "back" {
		t.Fatalf("reconnect text = %q", txt.Content)
	}
	_ = app.StopSession(thread.ID)
}

// TestE2E_ReconnectToExistingSession: Start → Stop → StartSession (resume
// via SessionRef). Verify the stored SessionRef gets passed to the new Claude
// session as --resume.
func TestE2E_ReconnectToExistingSession(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// First binary emits init then receives and records the args via a marker
	// file. We write the second binary such that it writes its --resume value
	// to a sentinel file.
	sentinel := filepath.Join(t.TempDir(), "resume-arg.txt")
	resumeScript := fmt.Sprintf(`#!/bin/bash
# Record args to sentinel so the test can check --resume was forwarded.
printf '%%s\n' "$*" > %s
printf '%%s\n' '{"type":"system","subtype":"init","session_id":"sess-reconnected","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
read -r _ || true
printf '%%s\n' '{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}'
printf '%%s\n' '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"resumed"}}}'
printf '%%s\n' '{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}'
printf '%%s\n' '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"resumed"}]}}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false}'
while read -r _; do :; done
exit 0
`, shellQuoteForTest(sentinel))

	binary := filepath.Join(t.TempDir(), "claude-reconnect.sh")
	if err := writeExec(t, binary, resumeScript); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// First session: write a SessionRef manually since the mock's session id
	// flows through triage's UpdateSessionRef.
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	thread.SessionRef = "stored-session-id"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// Wait briefly for the mock to spawn so the sentinel file is written.
	waitUntil(t, 5*time.Second, func() bool {
		_, err := os.Stat(sentinel)
		return err == nil
	})

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// Read the forwarded args and verify --resume stored-session-id appears.
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	args := string(data)
	if !strings.Contains(args, "--resume stored-session-id") {
		t.Fatalf("resume arg missing, got: %q", args)
	}
}

// TestE2E_ParseErrorRecovery: mock emits a garbage line, then a valid event.
// Verify the parse failure logs but the valid event still reaches the router.
func TestE2E_ParseErrorRecovery(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	responses := [][]string{
		{
			`this is not json at all`,
			`{"type":"system","subtype":"init","session_id":"parse-err","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	txt := bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	if txt.Content != "ok" {
		t.Fatalf("text after garbage = %q", txt.Content)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)
	_ = app.StopSession(thread.ID)
}

// TestE2E_SendMessageBeforeSessionStart: a thread created without an
// explicit StartSession must still accept the user's first message —
// SendMessage lazy-starts the provider subprocess before forwarding.
// This replaces the prior "returns error" expectation; drafts spawn the
// subprocess on first send so the "New Thread → type → send" path works
// without a disconnected banner or an explicit Start step.
func TestE2E_SendMessageBeforeSessionStart(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"lazy-start","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"pong"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"pong"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.SendMessage(thread.ID, "hello", nil); err != nil {
		t.Fatalf("SendMessage (lazy-start): %v", err)
	}
	// Session must now be live and the mock must have delivered both the
	// init + the assistant response. Draining them also catches the case
	// where lazy-start silently succeeds but the send never made it.
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	_, sessionExists := app.sessionManager().get(thread.ID)
	if !sessionExists {
		t.Fatal("SendMessage should have lazy-started a session")
	}
	_ = app.StopSession(thread.ID)
}

func TestE2E_ClaudeQueuedFlushRepairsRiskyAdvisorContextBeforeSend(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// The repair restart passes an explicit --resume-session-at cursor
	// (a-final, tracked from the wire by the live leaf tracker), and the
	// spawn path validates explicit cursors against the session JSONL's
	// active parentUuid branch before honoring them (invariant 28). A
	// real CLI maintains that file as it streams; the mock script can't,
	// so stage the transcript rows matching what the script streams
	// below, with a-final as the file's last transcript row — i.e. the
	// branch tip — so the wire-derived cursor survives validation
	// exactly as it would in production.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeProjectSession(t, home, workspace, "risky-sess", `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"risky-sess","message":{"role":"user","content":"trigger risky advisor"}}
{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","sessionId":"risky-sess","message":{"id":"msg-advisor","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}
{"type":"assistant","uuid":"a-final","parentUuid":"u1","sessionId":"risky-sess","message":{"id":"msg-final","role":"assistant","content":[{"type":"text","text":"done"}]}}
`)

	scriptDir := t.TempDir()
	argsLog := filepath.Join(scriptDir, "args.log")
	countFile := filepath.Join(scriptDir, "count")
	script := `#!/bin/bash
set -e
count=0
if [ -f ` + shellQuoteForTest(countFile) + ` ]; then
  count=$(cat ` + shellQuoteForTest(countFile) + `)
fi
count=$((count + 1))
printf '%s' "$count" > ` + shellQuoteForTest(countFile) + `
printf '%s\n' "$*" >> ` + shellQuoteForTest(argsLog) + `
if [ "$count" = "1" ]; then
  read -r _ || true
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"risky-sess","model":"claude-opus-4-7","cwd":"/tmp","tools":["advisor"],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"msg-advisor","role":"assistant","model":"claude-opus-4-7","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}'
  printf '%s\n' '{"type":"assistant","uuid":"a-final","parentUuid":"u1","message":{"id":"msg-final","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"done"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
  while read -r _; do :; done
else
  read -r _ || true
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"risky-sess","model":"claude-opus-4-7","cwd":"/tmp","tools":["advisor"],"claude_code_version":"1.0"}'
  while read -r _; do :; done
fi
`
	binary := filepath.Join(scriptDir, "mock-claude-risky.sh")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock claude: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.SendMessage(thread.ID, "trigger risky advisor", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	app.dispatchFlush(thread.ID, []triage.QueuedFlushItem{{
		ID:      "queue:repair",
		Message: "queued after risky advisor",
		Payload: json.RawMessage(`{}`),
	}})

	argsText := waitForFileText(t, argsLog, func(text string) bool {
		return strings.Contains(text, "--resume risky-sess") &&
			strings.Contains(text, "--resume-session-at a-final")
	})
	if !strings.Contains(argsText, "--resume-session-at a-final") {
		t.Fatalf("args log missing resume-at repair: %s", argsText)
	}
	_ = app.StopSession(thread.ID)
}

// TestE2E_ToolCallFullCycle: mock emits a tool_use event (via assistant) and
// a subsequent tool_result. Verify item ordering + payloads.
func TestE2E_ToolCallFullCycle(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Assistant emits a tool_use then text; result closes the turn.
	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-tool","model":"claude-opus-4-7","cwd":"/tmp","tools":["Bash"],"claude_code_version":"1.0"}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}}]}}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "ls", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait for tool_start + tool text + turn complete.
	toolEvt := bus.nextProviderEventOfKind(t, provider.EventToolStart, 5*time.Second)
	if toolEvt.ItemType != "Bash" {
		t.Fatalf("tool item type = %q", toolEvt.ItemType)
	}
	if toolEvt.ItemID != "tu-1" {
		t.Fatalf("tool itemID = %q", toolEvt.ItemID)
	}
	bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// The assistant text is persisted on turn_complete.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var assistantText bool
	for _, it := range items {
		if it.Role == "assistant" && it.Summary == "done" {
			assistantText = true
		}
	}
	if !assistantText {
		t.Fatalf("assistant text not persisted: %+v", items)
	}
	_ = app.StopSession(thread.ID)
}

// TestE2E_DiffItemPersistsWithPayload verifies that a diff event persists both
// the item and the payload round-trip.
func TestE2E_DiffItemPersistsWithPayload(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Synthesise a diff event directly: this mirrors what Claude would emit
	// and exercises the diff-payload persistence round-trip.
	evt := provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  thread.ID,
		Content:   "diff --git a/file.txt b/file.txt\n@@ -0,0 +1 @@\n+hello\n",
		Timestamp: time.Now(),
	}
	// Seed an item so LastTurnIndex is deterministic (>=1).
	if err := app.store.InsertItem(store.Item{
		ID: "seed-user", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "seed", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := app.triage.Handle(evt); err != nil {
		t.Fatalf("Handle diff: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var diffItem store.Item
	for _, it := range items {
		if it.Kind == "tool_call" && it.PayloadID != "" {
			diffItem = it
		}
	}
	if diffItem.ID == "" {
		t.Fatalf("no tool_call with diff payload persisted: %+v", items)
	}
	if diffItem.PayloadID == "" {
		t.Fatalf("tool_call has no diff payload: %+v", diffItem)
	}

	// Round-trip the payload via the binding.
	got, err := app.GetPayloadData(thread.ID, diffItem.PayloadID)
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got.Data != evt.Content {
		t.Fatalf("payload round-trip = %q", got.Data)
	}
}

// TestE2E_ThreadModelSwitchMidSession: updating a thread's model while the
// session is active restarts the session. After restart, the new model is what
// the thread stores.
func TestE2E_ThreadModelSwitchMidSession(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Binary that stays alive until stdin closes. No events needed for this
	// test — we only care about the restart path and the DB write.
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	_, active := app.sessionManager().get(thread.ID)
	if !active {
		t.Fatal("session not registered after StartSession")
	}

	updated, err := app.UpdateThreadModel(thread.ID, "claude-sonnet")
	if err != nil {
		t.Fatalf("UpdateThreadModel: %v", err)
	}
	if updated.Model != "claude-sonnet" {
		t.Fatalf("updated model = %q", updated.Model)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Model != "claude-sonnet" {
		t.Fatalf("stored model = %q", stored.Model)
	}

	// UpdateThreadModel restarts the session; the map should still hold a
	// session after the call returns.
	_, stillActive := app.sessionManager().get(thread.ID)
	if !stillActive {
		t.Fatal("session not re-registered after model switch")
	}

	_ = app.StopSession(thread.ID)
}

// TestE2E_ClaudeApprovalRoundTrip: mock emits can_use_tool → backend surfaces
// approval → RespondToApproval writes a control_response. We verify the
// approval reached triage and that RespondToApproval doesn't error.
func TestE2E_ClaudeApprovalRoundTrip(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.UpdateThreadRuntimeMode(thread.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatalf("UpdateThreadRuntimeMode: %v", err)
	}

	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-approve","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	approval := bus.nextApprovalEvent(t, "request", 5*time.Second)
	if approval.Request == nil {
		t.Fatal("approval request missing payload")
	}
	if approval.Request.RequestID != "req-1" {
		t.Fatalf("approval requestID = %q", approval.Request.RequestID)
	}
	if approval.Request.ToolName != "Bash" {
		t.Fatalf("approval tool = %q", approval.Request.ToolName)
	}

	if err := app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	resolved := bus.nextApprovalEvent(t, "resolve", 5*time.Second)
	if resolved.RequestID != "req-1" || resolved.Decision != "approved" {
		t.Fatalf("resolved approval = %+v, want requestId=req-1 decision=approved", resolved)
	}

	_ = app.StopSession(thread.ID)
}

// TestE2E_StopSessionWithoutStartIsClean: calling StopSession on a thread
// that never had a session must not error; it also must clean up any
// lingering triage state. Covers the no-op path explicitly.
func TestE2E_StopSessionWithoutStartIsClean(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession no-op: %v", err)
	}

	// App still usable.
	if _, err := app.ListItems(thread.ID, true); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
}

// TestE2E_TokenUsagePersists: assistant usage should flow through the triage
// router as a context-window snapshot. Output tokens are turn spend, not
// current context occupancy, so they are excluded from the meter value.
func TestE2E_TokenUsagePersists(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	resp := [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-usage","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
	}}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), resp)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	usage := bus.nextUsageEvent(t, "usage", 5*time.Second)
	if usage.UsedTokens != 100 {
		t.Fatalf("usage = %+v, want usedTokens=100", usage)
	}
	_ = app.StopSession(thread.ID)
}

// TestE2E_ThinkingBlockPersistsAsItem: a thinking block should end up in the
// store with a thinking item + payload.
func TestE2E_ThinkingBlockPersistsAsItem(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	resp := [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-think","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		// Thinking block (index 0) streamed as content_block_delta — the
		// assistant envelope's thinking block is skipped by the parser.
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}}`,
		`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reasoning about the problem"}}}`,
		`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
		// Text block (index 1) streamed as content_block_delta.
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}}`,
		`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":1}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"reasoning about the problem"},{"type":"text","text":"answer"}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), resp)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "think", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait until BOTH the thinking and the assistant-text items are
	// persisted. With stream_event deltas, the thinking row appears
	// first (as soon as the first thinking_delta lands), then the text
	// row after the next content_block_start. Waiting on only one
	// leaves the assertion racing the second block.
	waitUntil(t, 5*time.Second, func() bool {
		items, _ := app.store.ListItems(thread.ID)
		var gotThinking, gotText bool
		for _, it := range items {
			if it.Kind == string(provider.ItemThinking) {
				gotThinking = true
			}
			if it.Kind == "assistant_text" {
				gotText = true
			}
		}
		return gotThinking && gotText
	})

	items, _ := app.store.ListItems(thread.ID)
	var thinking, textCnt int
	for _, it := range items {
		switch it.Kind {
		case string(provider.ItemThinking):
			thinking++
			if it.PayloadID == "" {
				t.Fatalf("thinking item has no payload: %+v", it)
			}
		case "assistant_text":
			textCnt++
		}
	}
	if thinking == 0 {
		t.Fatalf("no thinking items persisted; items = %+v", items)
	}
	if textCnt == 0 {
		t.Fatalf("no text item persisted; items = %+v", items)
	}

	_ = app.StopSession(thread.ID)
}

// TestE2E_CommandOutputPersistsToPayload: a command_output heavy event is
// persisted as a tool_call item with payload.
func TestE2E_CommandOutputPersistsToPayload(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a turn so LastTurnIndex > 0.
	if err := app.store.InsertItem(store.Item{
		ID: "seed", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "seed", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  thread.ID,
		Content:   "line 1\nline 2\nline 3\n",
		Meta:      json.RawMessage(`{"command":"ls -la","exitCode":0}`),
		Timestamp: time.Now(),
	}
	if err := app.triage.Handle(evt); err != nil {
		t.Fatalf("triage.Handle: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var cmd store.Item
	for _, it := range items {
		if it.Kind == "tool_call" && it.ToolName == "command_execution" {
			cmd = it
		}
	}
	if cmd.ID == "" {
		t.Fatalf("no command tool_call persisted: %+v", items)
	}
	if cmd.PayloadID == "" {
		t.Fatalf("command item missing payload: %+v", cmd)
	}

	got, err := app.GetPayloadData(thread.ID, cmd.PayloadID)
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got.Data != evt.Content {
		t.Fatalf("payload round-trip = %q, want %q", got.Data, evt.Content)
	}
}

// TestE2E_RenameThreadUpdatesTitle: verifies the basic rename binding.
func TestE2E_RenameThreadUpdatesTitle(t *testing.T) {
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before := thread.UpdatedAt

	if err := app.RenameThread(thread.ID, "Refactored Flow"); err != nil {
		t.Fatalf("RenameThread: %v", err)
	}
	got, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.Title != "Refactored Flow" {
		t.Fatalf("Title = %q, want %q", got.Title, "Refactored Flow")
	}
	if got.UpdatedAt != before {
		t.Fatalf("UpdatedAt = %d, want %d", got.UpdatedAt, before)
	}
}

// TestE2E_GetPayloadDataReturnsEmptyForMissing: unknown payloadID should
// return an error, not silent empty string.
func TestE2E_GetPayloadDataReturnsEmptyForMissing(t *testing.T) {
	app, _ := setupE2EApp(t)
	_, err := app.GetPayloadData("thread-missing", "no-such-id")
	if err == nil {
		t.Fatal("GetPayloadData missing: err = nil, want error")
	}
}

// TestE2E_GetPayloadDataReturnsRawCommandOutput verifies command_output
// payloads round-trip as raw bytes; rendering is a frontend projection.
func TestE2E_GetPayloadDataReturnsRawCommandOutput(t *testing.T) {
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.store.InsertItem(store.Item{
		ID: "seed-cmd-output", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "seed", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	evt := provider.ProviderEvent{
		Kind:      provider.EventCommandOutput,
		ThreadID:  thread.ID,
		Content:   "\x1b[31merror: boom\x1b[0m",
		Meta:      json.RawMessage(`{"command":"run","exitCode":1}`),
		Timestamp: time.Now(),
	}
	if err := app.triage.Handle(evt); err != nil {
		t.Fatalf("triage.Handle: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var cmd store.Item
	for _, it := range items {
		if it.Kind == "tool_call" && it.ToolName == "command_execution" {
			cmd = it
		}
	}
	if cmd.PayloadID == "" {
		t.Fatalf("command item missing payload: %+v", cmd)
	}

	got, err := app.GetPayloadData(thread.ID, cmd.PayloadID)
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got.Data != evt.Content {
		t.Fatalf("payload data round-trip = %q", got.Data)
	}
}

// TestE2E_SendMessageTouchesThreadUpdatedAt: sending a message must bump the
// thread's updated_at so the sidebar reshuffles.
func TestE2E_SendMessageTouchesThreadUpdatedAt(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	before, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	// Ensure clock ticks. UnixMilli has 1ms resolution.
	time.Sleep(2 * time.Millisecond)

	if err := app.SendMessage(thread.ID, "touch", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	after, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread post: %v", err)
	}
	if after.UpdatedAt <= before.UpdatedAt {
		t.Fatalf("updated_at not bumped: before=%d after=%d", before.UpdatedAt, after.UpdatedAt)
	}
	_ = app.StopSession(thread.ID)
}

// TestE2E_StartSessionUnknownThread: StartSession against a non-existent
// thread ID must fail cleanly without registering anything in app.sessions.
func TestE2E_StartSessionUnknownThread(t *testing.T) {
	app, _ := setupE2EApp(t)

	err := app.StartSession("missing-thread-id")
	if err == nil {
		t.Fatal("StartSession unknown thread: err = nil, want failure")
	}

	_, registered := app.sessionManager().get("missing-thread-id")
	if registered {
		t.Fatal("session map should not contain failed-start thread")
	}
}

// TestE2E_SendMessageUnknownThread: calling SendMessage with a thread ID
// that does not exist must report a clear error and not crash the app.
// Since SendMessage now lazy-starts the session, the failure surface
// moves from "no active session" (old manual-start flow) to the
// lazy-start trying to load the thread row — either way the caller
// receives a user-visible error and the app stays healthy.
func TestE2E_SendMessageUnknownThread(t *testing.T) {
	app, _ := setupE2EApp(t)

	err := app.SendMessage("ghost-thread-id", "hello", nil)
	if err == nil {
		t.Fatal("SendMessage unknown thread: err = nil, want failure")
	}
	if !strings.Contains(err.Error(), "ghost-thread-id") {
		t.Fatalf("error = %v, want mention of the missing thread id", err)
	}

	if _, err := app.ListThreads(); err != nil {
		t.Fatalf("ListThreads after failed send: %v", err)
	}
}

// TestE2E_InterruptTurnWithoutSession: InterruptTurn on a thread with no
// active session should silently no-op. The FE may call interrupt during
// recovery (after a crashed/disconnected session) and surfacing an error
// banner during recovery is worse than dropping the call — the session is
// already gone and there is nothing to interrupt.
func TestE2E_InterruptTurnWithoutSession(t *testing.T) {
	app, bus := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	before := len(bus.allEvents())

	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatalf("InterruptTurn without session: err = %v, want nil (silent no-op)", err)
	}

	// The no-op contract is stronger than just "returns nil": the call
	// MUST NOT synthesize a turn_completed, a stop marker, or an error
	// event behind the user's back. A regression that swallows the
	// missing-session check but accidentally fans out a spurious event
	// (e.g. by routing through triage's MarkUserInterrupt before the
	// session-presence guard) would pass an err-only assertion.
	after := bus.allEvents()
	if len(after) != before {
		t.Fatalf("InterruptTurn without session emitted %d unexpected events: %s",
			len(after)-before, summarizeEvents(after[before:]))
	}
}

// TestE2E_StartSessionTwiceReplacesOldSession: calling StartSession twice on
// the same thread should tear down the first session and register the second.
// The session token in the map must change.
func TestE2E_StartSessionTwiceReplacesOldSession(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession #1: %v", err)
	}
	first, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("first session not registered")
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession #2: %v", err)
	}
	second, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("second session not registered")
	}

	if first.Token == second.Token {
		t.Fatalf("session token should change on restart (both=%q)", first.Token)
	}
	_ = app.StopSession(thread.ID)
}

// --- Utilities private to this file ---

func shellQuoteForTest(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func writeExec(t *testing.T, path, contents string) error {
	t.Helper()
	return os.WriteFile(path, []byte(contents), 0o755)
}

// waitUntil polls until the predicate returns true or the deadline passes.
// File-creation from an external process can't be observed without polling
// — we keep the interval short.
func waitUntil(t *testing.T, d time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitUntil: predicate still false after %v", d)
}

func waitForFileText(t *testing.T, path string, predicate func(string) bool) string {
	t.Helper()
	var last string
	waitUntil(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		last = string(data)
		return predicate(last)
	})
	return last
}

// TestE2E_ClaudeStoppedThreadPreInitErrorResultSurfaces reproduces the
// 2026-06-10 wedged-thread incident end-to-end: a thread whose session
// was stopped (StopSession → CleanupThread sets the stopped-thread
// marker, exactly what revert-on-interrupt does) is restarted by a
// send, and the replacement Claude process dies during startup —
// emitting a single result{error_during_execution} BEFORE system/init,
// then lingering alive (the real CLI does not exit after a failed
// --resume-session-at validation).
//
// Pre-fix, every link failed silently: the stopped marker dropped the
// error result (only EventInit cleared it, and init never came), no
// error item was persisted, the lingering process stayed registered,
// and the frontend showed "Working" forever. Post-fix:
//
//  1. startSessionNow re-admits the thread (MarkThreadActive) before
//     spawning, so the pre-init error result routes.
//  2. handleTurnComplete's orphan branch persists an error item on the
//     send's turn (the provider:item_event upsert is what clears the
//     frontend's optimistic pending-send state).
//  3. sessionEventHandler reaps the dead-on-arrival session, so the
//     lingering subprocess is closed and the next send starts fresh.
//  4. No session_died banner row — the error item is the single surface.
func TestE2E_ClaudeStoppedThreadPreInitErrorResultSurfaces(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Turn 1: a healthy session completes a normal turn.
	happy := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-incident","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"2.1"}`,
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"all good"}}}`,
			`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
			`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"all good"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), happy)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	if err := app.SendMessage(thread.ID, "hi", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// Stop the session — the incident's revert path reduces to this
	// (teardownAndCloseSession → CleanupThread → stopped marker set).
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// Replacement binary: dies during startup. Its only output is the
	// incident-shaped error result (emitted pre-init), then it lingers
	// reading stdin like the real CLI.
	doa := [][]string{
		{
			`{"type":"result","subtype":"error_during_execution","duration_ms":1313,"is_error":true,"num_turns":0,"errors":["No message found with message.uuid of: ea6789cd-390e-4ab9-aff7-902c09f1ed98"]}`,
		},
	}
	doaBinary := testutil.WriteMockClaudeScript(t, t.TempDir(), doa)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": doaBinary}); err != nil {
		t.Fatalf("swap binary: %v", err)
	}

	// The retry send lazy-starts the replacement session.
	if err := app.SendMessage(thread.ID, "are you there?", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}

	// The pre-init error result must route (not be dropped by the
	// stopped marker)...
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	// ...and persist an error item carrying the wire message on the
	// retry's turn.
	deadline := time.Now().Add(5 * time.Second)
	var errorItem *store.Item
	for errorItem == nil {
		items, listErr := app.store.ListItems(thread.ID)
		if listErr != nil {
			t.Fatalf("ListItems: %v", listErr)
		}
		for i := range items {
			if items[i].Kind == "error" && strings.Contains(items[i].Summary, "No message found") {
				errorItem = &items[i]
				break
			}
		}
		if errorItem == nil {
			if time.Now().After(deadline) {
				items, _ := app.store.ListItems(thread.ID)
				t.Fatalf("no error item persisted for the pre-init failure; items = %+v", items)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The retry's user message itself persisted normally.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var sawRetryUserRow bool
	for _, it := range items {
		if it.Kind == "user_text" && strings.Contains(it.Summary, "are you there?") {
			sawRetryUserRow = true
		}
		if strings.HasPrefix(it.ID, "session_died:") {
			t.Fatalf("session_died banner row persisted — the error item must be the single surface: %+v", it)
		}
	}
	if !sawRetryUserRow {
		t.Fatalf("retry user row missing; items = %+v", items)
	}
	// Exact attribution: the thread's first send lands on turn 0
	// (nextSendTurnIndex returns LastTurnIndex unchanged for an empty
	// thread), so the retry's dispatcher-stamped pending send — the
	// orphan branch's attribution source — is turn 1.
	if errorItem.TurnIndex != 1 {
		t.Fatalf("error item turn = %d, want the retry send's turn 1", errorItem.TurnIndex)
	}

	// The dead-on-arrival session must be reaped (async goroutine —
	// poll). Pre-fix the lingering process stayed registered forever.
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, stillRegistered := app.sessionManager().get(thread.ID)
		if !stillRegistered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dead pre-init session still registered after 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
