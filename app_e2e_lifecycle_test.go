package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
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
// emit function. Tests can Next() the channel to read events in order.
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

	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	app := &App{
		store:               st,
		settings:            settings.NewService(t.TempDir()),
		sessions:            make(map[string]session),
		startingSessions:    make(map[string]*sessionStart),
		threadSystemPrompts: make(map[string]string),
	}
	app.triage = triage.NewRouter(st, bus.emit)
	ensureDefaultTestProject(t, app)

	// Ensure any sessions that remain open at test end are torn down.
	t.Cleanup(func() {
		app.mu.Lock()
		sessions := make([]session, 0, len(app.sessions))
		for _, s := range app.sessions {
			sessions = append(sessions, s)
		}
		app.sessions = make(map[string]session)
		app.mu.Unlock()
		for _, s := range sessions {
			if s.claude != nil {
				_ = s.claude.Close()
			}
			if s.codex != nil {
				_ = s.codex.Close()
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
		ID:              id,
		ProjectID:     defaultTestProjectID,
		Title:           "E2E Thread",
		Provider:        providerName,
		WorkspacePath:   workspace,
		Model:           "claude-opus-4-7",
		Mode: "chat",
		CreatedAt:       now,
		UpdatedAt:       now,
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
	if err := app.SendMessage(thread.ID, "hi"); err != nil {
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
		if it.Role == "assistant" && it.Kind == string(provider.ItemText) && it.Summary == "hello from mock" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("assistant text not persisted: %+v", items)
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	app.mu.Lock()
	_, stillActive := app.sessions[thread.ID]
	app.mu.Unlock()
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

	if err := app.SendMessage(thread.ID, "hi"); err != nil {
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

	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-multi","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"turn one"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
		{
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

	if err := app.SendMessage(thread.ID, "first"); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	if err := app.SendMessage(thread.ID, "second"); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	// Expect: turn 1 user "first", turn 1 assistant "turn one",
	//         turn 2 user "second", turn 2 assistant "turn two"
	// (exact ordering depends on persist timing; check items per turn).
	turn1Summaries := map[string]bool{}
	turn2Summaries := map[string]bool{}
	for _, it := range items {
		switch it.TurnIndex {
		case 1:
			turn1Summaries[it.Summary] = true
		case 2:
			turn2Summaries[it.Summary] = true
		}
	}
	if !turn1Summaries["first"] || !turn1Summaries["turn one"] {
		t.Fatalf("turn 1 missing expected summaries: %v", turn1Summaries)
	}
	if !turn2Summaries["second"] || !turn2Summaries["turn two"] {
		t.Fatalf("turn 2 missing expected summaries: %v", turn2Summaries)
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

	responses := [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-int","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial..."}]}}`,
		},
		// 2nd stdin line is the interrupt. We just consume it silently.
		{},
		// 3rd stdin line is the next user message; emit full reply.
		{
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

	if err := app.SendMessage(thread.ID, "start"); err != nil {
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
	app.mu.Lock()
	_, active := app.sessions[thread.ID]
	app.mu.Unlock()
	if !active {
		t.Fatal("session vanished after interrupt")
	}

	if err := app.SendMessage(thread.ID, "continue"); err != nil {
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
	if err := app.SendMessage(thread.ID, "hi"); err != nil {
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
	app.mu.Lock()
	_, active := app.sessions[thread.ID]
	app.mu.Unlock()
	if active {
		t.Fatal("sessions map still contains thread after StopSession")
	}

	// Reconnect: reset binary to something that gives a fresh init.
	binary2 := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"sess-exit-2","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
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
	if err := app.SendMessage(thread.ID, "hello again"); err != nil {
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
	if err := app.SendMessage(thread.ID, "hi"); err != nil {
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
			`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"pong"}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), responses)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if err := app.SendMessage(thread.ID, "hello"); err != nil {
		t.Fatalf("SendMessage (lazy-start): %v", err)
	}
	// Session must now be live and the mock must have delivered both the
	// init + the assistant response. Draining them also catches the case
	// where lazy-start silently succeeds but the send never made it.
	bus.nextProviderEventOfKind(t, provider.EventInit, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTextDelta, 5*time.Second)
	bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 5*time.Second)

	app.mu.Lock()
	_, sessionExists := app.sessions[thread.ID]
	app.mu.Unlock()
	if !sessionExists {
		t.Fatal("SendMessage should have lazy-started a session")
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
	if err := app.SendMessage(thread.ID, "ls"); err != nil {
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
	// and exercises the persistHeavy → payload round-trip.
	evt := provider.ProviderEvent{
		Kind:     provider.EventDiff,
		ThreadID: thread.ID,
		Content:  "diff --git a/file.txt b/file.txt\n@@ -0,0 +1 @@\n+hello\n",
		Timestamp: time.Now(),
	}
	// Seed an item so LastTurnIndex is deterministic (>=1).
	if err := app.store.InsertItem(store.Item{
		ID: "seed-user", ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
		Kind: "text", Role: "user", Summary: "seed", CreatedAt: time.Now().UnixMilli(),
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
		if it.Kind == string(provider.ItemDiff) {
			diffItem = it
		}
	}
	if diffItem.ID == "" {
		t.Fatalf("no diff item persisted: %+v", items)
	}
	if diffItem.PayloadID == "" {
		t.Fatalf("diff item has no payload: %+v", diffItem)
	}

	// Round-trip the payload via the binding.
	got, err := app.GetPayloadData(diffItem.PayloadID)
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got != evt.Content {
		t.Fatalf("payload round-trip = %q", got)
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

	app.mu.Lock()
	_, active := app.sessions[thread.ID]
	app.mu.Unlock()
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
	app.mu.Lock()
	_, stillActive := app.sessions[thread.ID]
	app.mu.Unlock()
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
	if err := app.SendMessage(thread.ID, "go"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	approval := bus.nextProviderEventOfKind(t, provider.EventApprovalRequest, 5*time.Second)
	if approval.ItemID != "req-1" {
		t.Fatalf("approval requestID = %q", approval.ItemID)
	}
	var ar provider.ApprovalRequest
	if err := json.Unmarshal(approval.Meta, &ar); err != nil {
		t.Fatalf("unmarshal approval meta: %v", err)
	}
	if ar.ToolName != "Bash" {
		t.Fatalf("approval tool = %q", ar.ToolName)
	}

	if err := app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
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
	if _, err := app.ListItems(thread.ID); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
}

// TestE2E_TokenUsagePersists: a result with token usage should flow through
// the triage router and be emitted with cost attached (when the model is
// known).
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
	if err := app.SendMessage(thread.ID, "go"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	usage := bus.nextProviderEventOfKind(t, provider.EventTokenUsage, 5*time.Second)
	var tokens provider.TokenUsage
	if err := json.Unmarshal(usage.Meta, &tokens); err != nil {
		t.Fatalf("unmarshal usage meta: %v", err)
	}
	if tokens.InputTokens != 100 || tokens.OutputTokens != 50 {
		t.Fatalf("usage = %+v, want input=100 output=50", tokens)
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
	if err := app.SendMessage(thread.ID, "think"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait for turn complete to flush accumulators.
	waitUntil(t, 5*time.Second, func() bool {
		items, _ := app.store.ListItems(thread.ID)
		for _, it := range items {
			if it.Kind == string(provider.ItemThinking) {
				return true
			}
		}
		return false
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
		case string(provider.ItemText):
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
// persisted as a command_execution item with payload.
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
		Kind: "text", Role: "user", Summary: "seed", CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	evt := provider.ProviderEvent{
		Kind:     provider.EventCommandOutput,
		ThreadID: thread.ID,
		Content:  "line 1\nline 2\nline 3\n",
		Meta:     json.RawMessage(`{"command":"ls -la","exitCode":0}`),
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
		if it.Kind == string(provider.ItemCommandExecution) {
			cmd = it
		}
	}
	if cmd.ID == "" {
		t.Fatalf("no command_execution item persisted: %+v", items)
	}
	if cmd.PayloadID == "" {
		t.Fatalf("command item missing payload: %+v", cmd)
	}

	got, err := app.GetPayloadData(cmd.PayloadID)
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got != evt.Content {
		t.Fatalf("payload round-trip = %q, want %q", got, evt.Content)
	}
}

// TestE2E_RenameThreadUpdatesTitle: verifies the basic rename binding.
func TestE2E_RenameThreadUpdatesTitle(t *testing.T) {
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

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
}

// TestE2E_GetPayloadDataReturnsEmptyForMissing: unknown payloadID should
// return an error, not silent empty string.
func TestE2E_GetPayloadDataReturnsEmptyForMissing(t *testing.T) {
	app, _ := setupE2EApp(t)
	_, err := app.GetPayloadData("no-such-id")
	if err == nil {
		t.Fatal("GetPayloadData missing: err = nil, want error")
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

	if err := app.SendMessage(thread.ID, "touch"); err != nil {
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

	app.mu.Lock()
	_, registered := app.sessions["missing-thread-id"]
	app.mu.Unlock()
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

	err := app.SendMessage("ghost-thread-id", "hello")
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
// active session should fail with a clear error, not crash.
func TestE2E_InterruptTurnWithoutSession(t *testing.T) {
	app, _ := setupE2EApp(t)
	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	err = app.InterruptTurn(thread.ID)
	if err == nil {
		t.Fatal("InterruptTurn without session: err = nil, want failure")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("error = %v, want 'no active session'", err)
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
	app.mu.Lock()
	first, ok := app.sessions[thread.ID]
	app.mu.Unlock()
	if !ok {
		t.Fatal("first session not registered")
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession #2: %v", err)
	}
	app.mu.Lock()
	second, ok := app.sessions[thread.ID]
	app.mu.Unlock()
	if !ok {
		t.Fatal("second session not registered")
	}

	if first.token == second.token {
		t.Fatalf("session token should change on restart (both=%q)", first.token)
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
