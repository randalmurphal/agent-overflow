package claude

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

const testThread = "thread-test"

// assistantMessageStartLine builds the stream_event.message_start that
// precedes a normal streamed assistant message. Its id marks the message
// as already-streamed, so the coalesced `assistant` snapshot carrying the
// same id drops its text/thinking blocks (anti-double-render) instead of
// recovering them. Production always emits this before a snapshot
// (--include-partial-messages); the never-streamed recovery path is
// covered by the partial_messages_test.go suite.
func assistantMessageStartLine(id string) []byte {
	return []byte(`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"` + id + `","role":"assistant","content":[]}}}`)
}

func isFlagToken(arg string) bool { return len(arg) >= 2 && arg[0] == '-' }

// -- Session lifecycle tests using cat subprocess --

// newTestClaudeSession creates a Session backed by `cat`, which echoes
// stdin to stdout. This lets us exercise readLoop, Send, etc. without
// a real Claude CLI binary.
func newTestClaudeSession(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

func waitEvent(t *testing.T, ch <-chan provider.ProviderEvent) provider.ProviderEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
		return provider.ProviderEvent{}
	}
}

func waitCapturedLines(t *testing.T, path string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= want && lines[0] != "" {
				return lines
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read capture file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %d captured lines in %s; got %q", want, path, string(data))
	return nil
}

func (s *Session) maybeHandleFullAccessToolRequest(line []byte) (bool, error) {
	var raw controlRequestEnvelope
	if err := json.Unmarshal(line, &raw); err != nil {
		return false, err
	}
	return s.handleFullAccessToolRequest(raw)
}

// interruptResponderScript is a bash fake-CLI that reads stdin line by
// line and writes a canned control_response for every interrupt
// request it sees. Mirrors stopTaskResponderScript:
//   - "success": subtype=success, echoes back the request_id
//   - "error":   subtype=error with a provider-side message
//   - "silent":  drops the line; never responds (timeout/kill path)
func interruptResponderScript(mode string) string {
	// The case alternation accepts either field order because
	// json.Marshal on a map[string]any sorts keys alphabetically — the
	// "type" field can land either before or after "subtype" depending
	// on what other keys are present.
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "success":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`
	case "error":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"no active turn"}}\n' "$reqid"`
	case "silent":
		body = `            : # drop the line deliberately to exercise the timeout fallback`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

// newInterruptResponderSession spawns a Session backed by the fake-CLI
// script returned by interruptResponderScript. Wraps the boilerplate
// shared by the Interrupt round-trip tests, mirroring
// newStopTaskResponderSession.
func newInterruptResponderSession(t *testing.T, mode string, interruptTimeout time.Duration) *Session {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(interruptResponderScript(mode)), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: interruptTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

// newTestClaudeSessionWithPendingRequests wires up a cat-backed session
// for tests that need a live read loop plus pending interactive requests.
func newTestClaudeSessionWithPendingRequests(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 200)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// stopTaskResponderScript is a bash fake-CLI that reads stdin line by
// line and writes a canned control_response for every stop_task
// request it sees. The response shape is parameterised by mode:
//   - "success": subtype=success, echoes back the request_id
//   - "error":   subtype=error with a provider-side message
//   - "silent":  drops the line; never responds (timeout path)
//   - "stray":   writes a control_response with a different request_id
//     (unknown-id-dropped path)
//
// The script terminates when stdin closes (Session.Close → proc.Close
// shuts the pipe). Written to a temp file so the test doesn't rely on
// a specific shell quoting pattern.
func stopTaskResponderScript(mode string) string {
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"stop_task"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "success":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`
	case "error":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"task not found"}}\n' "$reqid"`
	case "silent":
		body = `            : # drop the line deliberately to exercise the timeout path`
	case "stray":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"not-the-real-one","response":{}}}\n'`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

// newStopTaskResponderSession spawns a Session backed by the fake-CLI
// script returned by stopTaskResponderScript. Wraps the boilerplate
// shared by the four StopTask tests.
func newStopTaskResponderSession(t *testing.T, mode string, stopTimeout time.Duration) *Session {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(stopTaskResponderScript(mode)), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(evt provider.ProviderEvent) { _ = evt },
		cancel:   cancel,
		readDone: make(chan struct{}),
		// Short timeout so a "silent" mode doesn't stall the suite.
		controlRequestTimeout: stopTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}
