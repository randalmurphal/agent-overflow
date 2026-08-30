package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// revertFakeScript builds a fake app-server that answers initialize with the
// given userAgent, starts a thread with the given id and historyMode, and
// answers `thread/revert` with revertReply — capturing the request frame and,
// when echo is true, following the response with the `thread/reverted`
// notification a real app-server sends on the same connection.
func revertFakeScript(t *testing.T, userAgent, threadID, historyMode, revertReply, capturePath string, echo bool) string {
	t.Helper()
	echoLine := ""
	if echo {
		echoLine = fmt.Sprintf("        echo \"%s\"\n",
			bashJSON(fmt.Sprintf(`{"jsonrpc":"2.0","method":"thread/reverted","params":{"threadId":%q}}`, threadID)))
	}
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/revert"'; then
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,%s}"
%s    fi
    if echo "$line" | grep -q '"method":"turn/interrupt"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
done
`,
		bashJSON(fmt.Sprintf(`{"userAgent":%q}`, userAgent)),
		bashJSON(fmt.Sprintf(`{"thread":{"id":%q,"historyMode":%q}}`, threadID, historyMode)),
		capturePath,
		bashJSON(revertReply),
		echoLine,
	)

	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// silentRevertFakeScript answers the handshake and then never answers
// `thread/revert` at all — the shape a lost response, a dead connection,
// or a hung handler takes from the client's side.
func silentRevertFakeScript(t *testing.T, userAgent, threadID, historyMode string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
done
`,
		bashJSON(fmt.Sprintf(`{"userAgent":%q}`, userAgent)),
		bashJSON(fmt.Sprintf(`{"thread":{"id":%q,"historyMode":%q}}`, threadID, historyMode)),
	)
	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// turnsListFakeScript answers the handshake and `thread/turns/list`,
// capturing every list request frame. The second page is served whenever
// the request carries a cursor, so one script covers the paged walk.
func turnsListFakeScript(t *testing.T, threadID, page1, page2, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/turns/list"'; then
        echo "$line" >> %q
        if echo "$line" | grep -q '"cursor"'; then
            echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        else
            echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        fi
        continue
    fi
done
`,
		bashJSON(`{"userAgent":"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0"}`),
		bashJSON(fmt.Sprintf(`{"thread":{"id":%q,"historyMode":"paginated"}}`, threadID)),
		capturePath,
		bashJSON(page2),
		bashJSON(page1),
	)
	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

func newRevertSession(t *testing.T, binary string) *Session {
	t.Helper()
	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  binary,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const revertOKReply = `"result":{"thread":{"id":"codex-thread-revert","historyMode":"paginated","turns":[]},` +
	`"turnsBackwardsCursor":"turn-cursor-9","itemsBackwardsCursor":"item-cursor-9"}`

// TestSessionRevertSendsBeforeTurnIdAndAwaitsEcho is the whole happy path: a
// 0.149 app-server, a paginated thread, the exclusive anchor on the wire, and
// the `thread/reverted` echo released before the call returned.
func TestSessionRevertSendsBeforeTurnIdAndAwaitsEcho(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	if got := s.ThreadHistoryMode(); got != "paginated" {
		t.Fatalf("ThreadHistoryMode() = %q, want paginated", got)
	}
	if !s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = false on a 0.149 paginated thread")
	}

	reverted, err := s.Revert(context.Background(), "turn-c")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if reverted.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the same thread back", reverted.ThreadID)
	}
	if !reverted.EchoConfirmed {
		t.Fatal("EchoConfirmed = false, want the thread/reverted echo to have arrived")
	}
	request, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured revert request: %v", err)
	}
	if !strings.Contains(string(request), `"beforeTurnId":"turn-c"`) {
		t.Fatalf("revert request missing beforeTurnId: %s", request)
	}
	if !strings.Contains(string(request), `"threadId":"codex-thread-revert"`) {
		t.Fatalf("revert request missing threadId: %s", request)
	}
	if strings.Contains(string(request), "lastTurnId") {
		t.Fatalf("revert request must not carry the fork's inclusive anchor: %s", request)
	}
}

// TestSessionRevertRefusesBelowTheVersionFloor pins the gate as a PRE-RPC
// one: a 0.147 app-server must never see the request at all, because its
// answer would be an error and the caller's fallback would then have to
// reason about whether anything was mutated.
func TestSessionRevertRefusesBelowTheVersionFloor(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.147.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	if s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = true on a 0.147 app-server")
	}
	_, err := s.Revert(context.Background(), "turn-c")
	if !errors.Is(err, ErrThreadRevertUnsupported) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertUnsupported", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Fatal("a below-floor Revert sent thread/revert anyway")
	}
}

// TestSessionRevertRefusesLegacyHistoryThreads is the gate that matters most
// in practice: AO does not pass `historyMode` on thread/start, upstream
// defaults to legacy, and upstream refuses a legacy revert.
func TestSessionRevertRefusesLegacyHistoryThreads(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "legacy", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	if s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = true on a legacy thread")
	}
	_, err := s.Revert(context.Background(), "turn-c")
	if !errors.Is(err, ErrThreadRevertUnsupported) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertUnsupported", err)
	}
	if _, statErr := os.Stat(capture); statErr == nil {
		t.Fatal("a legacy-history Revert sent thread/revert anyway")
	}
}

// TestSessionRevertTreatsAnAbsentHistoryModeAsUnsupported: an app-server that
// states no history mode (the field is experimental) must fail closed.
func TestSessionRevertTreatsAnAbsentHistoryModeAsUnsupported(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "", revertOKReply, capture, true)
	// historyMode "" is emitted as an empty string rather than an absent
	// key; drop it so the response really lacks the field.
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read mock script: %v", err)
	}
	rewritten := strings.ReplaceAll(string(raw), `,\"historyMode\":\"\"`, "")
	if rewritten == string(raw) {
		t.Fatal("mock rewrite matched nothing — the thread/start response still carries historyMode")
	}
	if err := os.WriteFile(binary, []byte(rewritten), 0o755); err != nil {
		t.Fatalf("rewrite mock script: %v", err)
	}

	s := newRevertSession(t, binary)
	if got := s.ThreadHistoryMode(); got != "" {
		t.Fatalf("ThreadHistoryMode() = %q, want empty", got)
	}
	if _, err := s.Revert(context.Background(), "turn-c"); !errors.Is(err, ErrThreadRevertUnsupported) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertUnsupported", err)
	}
}

// TestSessionRevertAllowsMidTurn pins the upstream operation AO uses for an
// Esc un-send: thread/revert owns active-turn shutdown and history replacement
// as one server-side operation.
func TestSessionRevertAllowsMidTurn(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	s.mu.Lock()
	s.turn.activeTurnID = "turn-live"
	s.mu.Unlock()

	if _, err := s.Revert(context.Background(), "turn-c"); err != nil {
		t.Fatalf("Revert with active turn: %v", err)
	}
	if _, statErr := os.Stat(capture); statErr != nil {
		t.Fatalf("mid-turn Revert did not send thread/revert: %v", statErr)
	}
}

// TestSessionRevertAfterInterruptAckDoesNotWaitForClientQuiescence covers the
// ordered race where another caller won controlMu first. The interrupt answer
// does not promise persistence is idle, but thread/revert owns the remaining
// shutdown barrier and must still be sent without a client-side wait.
func TestSessionRevertAfterInterruptAckDoesNotWaitForClientQuiescence(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	s.mu.Lock()
	s.turn.activeTurnID = "turn-live"
	s.mu.Unlock()
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if _, err := s.Revert(context.Background(), "turn-c"); err != nil {
		t.Fatalf("Revert after interrupt answer: %v", err)
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatalf("thread/revert was not sent after interrupt answer: %v", err)
	}
}

// TestSessionRevertRequiresAnAnchor: an empty beforeTurnId is a lost anchor,
// not "revert nothing".
func TestSessionRevertRequiresAnAnchor(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	if _, err := s.Revert(context.Background(), "  "); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("Revert error = %v, want a missing-anchor refusal", err)
	}
}

// TestSessionRevertRejectsAForeignThreadEcho: the identity echo is the one
// validation this response supports (`turns` is always empty), and it is the
// load-bearing one — the caller keeps its SessionRef pointed at this thread.
func TestSessionRevertTreatsAForeignThreadIdentityAsAnAppliedCut(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	reply := `"result":{"thread":{"id":"some-other-thread","turns":[]}}`
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		// echo=true: a real server sends the notification right after
		// the response, so the mismatch under test is the RESPONSE body,
		// not a missing echo.
		"codex-thread-revert", "paginated", reply, capture, true)

	s := newRevertSession(t, binary)
	reverted, err := s.Revert(context.Background(), "turn-c")
	if err != nil {
		t.Fatalf("Revert: %v, want the mismatched echo treated as a wire fault, not a divergence", err)
	}
	if reverted.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the thread the REQUEST asserted", reverted.ThreadID)
	}
}

// TestSessionRevertTreatsAnUndecodableResponseAsAnAppliedCut is the same
// rule from the other side: upstream only answers with a result after the
// replacement rollout is written and the pointer has moved, so a body AO
// cannot read is a wire fault to log — never a reason to abandon a cut
// that already happened and leave AO's history wider than the provider's.
func TestSessionRevertTreatsAnUndecodableResponseAsAnAppliedCut(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", `"result":{"unexpected":true}`, capture, true)

	s := newRevertSession(t, binary)
	reverted, err := s.Revert(context.Background(), "turn-c")
	if err != nil {
		t.Fatalf("Revert: %v, want an undecodable body treated as an applied cut", err)
	}
	if reverted.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the thread the REQUEST asserted", reverted.ThreadID)
	}
}

// TestSessionRevertReportsAnUnansweredCutAsUnknown: the request was
// written and nothing answered it. `thread/revert` mutates before it
// replies, so this is NOT proof the thread was left alone — the caller
// gets the ambiguous error and the expectation stays armed so the late
// echo of AO's own cut cannot read as a foreign writer's.
func TestSessionRevertReportsAnUnansweredCutAsUnknown(t *testing.T) {
	// A server that answers the handshake and then goes silent on the
	// cut is exactly the lost-response shape.
	binary := silentRevertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated")

	s := newRevertSession(t, binary)
	SetRequestTimeoutForTest(s, 150*time.Millisecond)
	_, err := s.Revert(context.Background(), "turn-c")
	if !errors.Is(err, ErrThreadRevertOutcomeUnknown) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertOutcomeUnknown", err)
	}
	pending := s.pendingRevert.Load()
	if pending == nil {
		t.Fatal("pendingRevert was cleared; a late echo of AO's own cut would now raise the foreign-writer alarm")
	}
	if pending.inFlight.Load() {
		t.Fatal("the settled expectation is still marked in flight; the next Revert would be refused forever")
	}
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"codex-thread-revert"}`))
	if !pending.closed {
		t.Fatal("the late echo was not matched to the unresolved cut")
	}
}

// TestSessionRevertRefusesAConcurrentCut: the `thread/reverted` echo
// carries only a threadId, so two overlapping cuts on one thread are
// indistinguishable on the wire. The second is refused rather than
// allowed to replace the first's expectation.
func TestSessionRevertRefusesAConcurrentCut(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, true)

	s := newRevertSession(t, binary)
	inFlight := &revertExpectation{
		threadID:     s.rootThreadID(),
		beforeTurnID: "turn-b",
		echo:         make(chan struct{}),
	}
	inFlight.inFlight.Store(true)
	s.pendingRevert.Store(inFlight)

	_, err := s.Revert(context.Background(), "turn-c")
	if !errors.Is(err, ErrThreadRevertInFlight) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertInFlight", err)
	}
	if got := s.pendingRevert.Load(); got != inFlight {
		t.Fatal("the in-flight expectation was replaced by the refused cut")
	}

	// A SETTLED expectation is retained for a late echo, not a lock: the
	// next cut must be allowed to replace it.
	inFlight.inFlight.Store(false)
	if _, err := s.Revert(context.Background(), "turn-c"); err != nil {
		t.Fatalf("Revert after the earlier cut settled: %v", err)
	}
}

// TestSessionRevertMapsUpstreamPaginatedRefusal covers the version-skew case
// the local gate cannot: a server that reported a paginated thread but still
// refuses. The refusal is raised before upstream mutates anything, so it maps
// to the state error the caller falls back on.
func TestSessionRevertMapsUpstreamPaginatedRefusal(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	reply := `"error":{"code":-32600,"message":"thread/revert only supports paginated threads"}`
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", reply, capture, false)

	s := newRevertSession(t, binary)
	_, err := s.Revert(context.Background(), "turn-c")
	if !errors.Is(err, ErrThreadRevertUnsupported) {
		t.Fatalf("Revert error = %v, want ErrThreadRevertUnsupported", err)
	}
}

func TestClassifyThreadRevertError(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		unsupported bool
		anchor      bool
	}{
		{"nil", nil, false, false},
		{
			"method not found is a codex below the floor",
			&RPCError{Method: threadRevertMethod, Code: -32601, Message: "method not found"},
			true, false,
		},
		{
			"paginated gate",
			&RPCError{Method: threadRevertMethod, Code: -32600, Message: "thread/revert only supports paginated threads"},
			true, false,
		},
		{
			"a post-shutdown internal error must NOT read as unsupported",
			&RPCError{Method: threadRevertMethod, Code: -32603, Message: "timed out shutting down thread before revert"},
			false, false,
		},
		{
			// The shape a retry takes after a cut whose provider half
			// landed and whose local half did not: upstream has already
			// dropped the turn AO is naming.
			"an anchor upstream cannot resolve is its own answer",
			&RPCError{Method: threadRevertMethod, Code: -32600, Message: "turn not found: turn-9"},
			false, true,
		},
		{
			"an anchor with no persisted rollout position too",
			&RPCError{Method: threadRevertMethod, Code: -32600, Message: "turn turn-9 does not have persisted rollout positions"},
			false, true,
		},
		{
			"and an anchor outside the inherited history",
			&RPCError{Method: threadRevertMethod, Code: -32600, Message: "fork boundary exceeds inherited source history"},
			false, true,
		},
		{
			// Upstream folds every anchor refusal onto invalid_request; a
			// different code is a different failure and must stay hard,
			// because the anchor answer licenses a fork on this thread.
			"the same words under another code stay a hard failure",
			&RPCError{Method: threadRevertMethod, Code: -32602, Message: "turn not found: turn-9"},
			false, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyThreadRevertError(tc.err)
			if errors.Is(got, ErrThreadRevertUnsupported) != tc.unsupported {
				t.Fatalf("classifyThreadRevertError(%v) = %v, unsupported want %v", tc.err, got, tc.unsupported)
			}
			if errors.Is(got, ErrThreadRevertAnchorUnresolvable) != tc.anchor {
				t.Fatalf("classifyThreadRevertError(%v) = %v, anchor want %v", tc.err, got, tc.anchor)
			}
		})
	}
}

func TestParseThreadRevertResponse(t *testing.T) {
	if _, err := parseThreadRevertResponse(json.RawMessage(`{"thread":{}}`)); err == nil {
		t.Fatal("expected a missing thread.id to fail")
	}
	// The pagination cursors are not decoded (see parseThreadRevertResponse);
	// their presence, absence, or null-ness must not affect the identity
	// echo, which is the only thing this response is validated on.
	reverted, err := parseThreadRevertResponse(json.RawMessage(
		`{"thread":{"id":"t-1","turns":[]},"turnsBackwardsCursor":null,"itemsBackwardsCursor":"c-9"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if reverted.ThreadID != "t-1" {
		t.Fatalf("reverted = %+v, want t-1", reverted)
	}
}

// TestDispatchThreadRevertedIgnoresUnsolicitedEchoes: an echo AO did not ask
// for carries no boundary, so it must not release a pending wait belonging to
// another cut, and must not panic when nothing is pending.
func TestDispatchThreadRevertedIgnoresUnsolicitedEchoes(t *testing.T) {
	s := &Session{}
	s.setRootThreadID("codex-thread-revert")
	// No expectation armed at all.
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"codex-thread-revert"}`))
	s.dispatchThreadReverted(json.RawMessage(`{}`))

	pending := &revertExpectation{threadID: "codex-thread-revert", beforeTurnID: "turn-c", echo: make(chan struct{})}
	s.pendingRevert.Store(pending)
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"a-different-thread"}`))
	select {
	case <-pending.echo:
		t.Fatal("a foreign thread's revert echo released this session's wait")
	default:
	}
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"codex-thread-revert"}`))
	select {
	case <-pending.echo:
	default:
		t.Fatal("the matching echo did not release the wait")
	}
	// A duplicate echo must not panic on a second close.
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"codex-thread-revert"}`))
}

// TestAwaitRevertEchoIsNonFatal pins the contract that a missing echo is a
// warning: the wait ends, and the caller keeps the successful response.
func TestAwaitRevertEchoIsNonFatal(t *testing.T) {
	s := &Session{}
	if s.awaitRevertEcho(canceledContext(), &revertExpectation{echo: make(chan struct{})}) {
		t.Fatal("awaitRevertEcho reported a confirmation it never received")
	}
}

// TestSessionRevertKeepsTheExpectationArmedForALateEcho: the cut
// succeeded, so a `thread/reverted` that arrives after the wait gave up
// is AO's own echo, not a foreign writer's — the expectation has to
// survive the timeout or the drift alarm cries wolf on every slow one.
func TestSessionRevertKeepsTheExpectationArmedForALateEcho(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	// echo=false: the mock answers the RPC and never sends the
	// notification, which is exactly the timed-out shape.
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", revertOKReply, capture, false)

	s := newRevertSession(t, binary)
	// A cancelled context stands in for the elapsed timeout: the
	// contract under test is what happens AFTER the wait gives up, not
	// how long it waits.
	pending := &revertExpectation{
		threadID:     s.rootThreadID(),
		beforeTurnID: "turn-c",
		echo:         make(chan struct{}),
	}
	s.pendingRevert.Store(pending)
	if s.awaitRevertEcho(canceledContext(), pending) {
		t.Fatal("awaitRevertEcho reported an echo that never came")
	}
	if got := s.pendingRevert.Load(); got != pending {
		t.Fatal("the expectation was cleared while the cut it describes had already happened")
	}
	s.dispatchThreadReverted(json.RawMessage(`{"threadId":"codex-thread-revert"}`))
	if !pending.closed {
		t.Fatal("the late echo was not matched to the pending cut")
	}
}

// TestSessionRevertForgetsTheExpectationWhenTheCutFailed: no cut
// happened, so a later `thread/reverted` really is somebody else's and
// must reach the alarm.
func TestSessionRevertForgetsTheExpectationWhenTheCutFailed(t *testing.T) {
	capture := t.TempDir() + "/revert-request.json"
	reply := `"error":{"code":-32600,"message":"thread/revert only supports paginated threads"}`
	binary := revertFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-revert", "paginated", reply, capture, false)

	s := newRevertSession(t, binary)
	if _, err := s.Revert(context.Background(), "turn-c"); err == nil {
		t.Fatal("expected the refusal to fail the call")
	}
	if got := s.pendingRevert.Load(); got != nil {
		t.Fatalf("pendingRevert = %+v after a failed cut, want it cleared", got)
	}
}

// TestVerifyRevertBoundaryReportsAnUncutThread: the anchor AO wanted gone
// is still in the thread's durable turns, so no cut is in effect and the
// caller must not converge in place.
func TestVerifyRevertBoundaryReportsAnUncutThread(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	page := `{"data":[{"id":"turn-c"},{"id":"turn-b"},{"id":"turn-a"}],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page, page, capture))

	verified, applied, err := s.VerifyRevertBoundary(context.Background(), "turn-b", "turn-c")
	if err != nil {
		t.Fatalf("VerifyRevertBoundary: %v", err)
	}
	if applied {
		t.Fatal("applied = true while the cut anchor is still in the thread's history")
	}
	if verified.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the probed thread", verified.ThreadID)
	}
	request, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured turns/list request: %v", err)
	}
	for _, want := range []string{
		`"threadId":"codex-thread-revert"`,
		// Turn shells only: the probe reads ids, never content.
		`"itemsView":"notLoaded"`,
		// Newest first, so an uncut thread is answered by page one.
		`"sortDirection":"desc"`,
	} {
		if !strings.Contains(string(request), want) {
			t.Fatalf("turns/list request missing %s: %s", want, request)
		}
	}
	if strings.Count(string(request), "thread/turns/list") != 1 {
		t.Fatalf("want the walk to stop at the anchor it found, got: %s", request)
	}
}

// TestVerifyRevertBoundaryReportsAnAlreadyCutThread: the anchor is gone
// and the last kept turn survives — the boundary AO asked for is already
// the thread's boundary, which is what lets an unresolved cut converge in
// place instead of paying for a fork. The answer costs a full walk, so it
// also covers the paged one.
func TestVerifyRevertBoundaryReportsAnAlreadyCutThread(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	page1 := `{"data":[{"id":"turn-b"}],"nextCursor":"cursor-1"}`
	page2 := `{"data":[{"id":"turn-a"}],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page1, page2, capture))

	verified, applied, err := s.VerifyRevertBoundary(context.Background(), "turn-b", "turn-c")
	if err != nil {
		t.Fatalf("VerifyRevertBoundary: %v", err)
	}
	if !applied {
		t.Fatal("applied = false while the anchor is gone and the last kept turn survives")
	}
	if verified.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the probed thread", verified.ThreadID)
	}
	request, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured turns/list request: %v", err)
	}
	if !strings.Contains(string(request), `"cursor":"cursor-1"`) {
		t.Fatalf("the walk did not follow nextCursor: %s", request)
	}
}

// TestVerifyRevertBoundaryRefusesAKeptTurnThatIsNotTheTail: the anchor is
// gone and the last kept turn survives — but the thread has grown PAST it,
// so its durable tail is not the prefix AO asked for.
//
// This is the gap membership alone cannot see. The reachable path: the
// revert to turn-b succeeds on the provider, AO's local truncation then
// fails, another writer appends turn-x, and the retry finds the anchor
// unresolvable because the cut already happened. Reporting "applied" there
// makes AO truncate SQLite to turn-b while the provider holds turn-b +
// turn-x — two histories that silently disagree on a thread AO believes it
// just converged.
//
// It is NOT an error: the caller's fallback is `thread/fork { lastTurnId:
// turn-b }`, which is anchored on the surviving turn and produces exactly
// the requested prefix whether or not turn-x exists.
func TestVerifyRevertBoundaryRefusesAKeptTurnThatIsNotTheTail(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	// Descending, so the FIRST id is the thread's newest durable turn.
	page1 := `{"data":[{"id":"turn-x"},{"id":"turn-b"}],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page1, page1, capture))

	verified, applied, err := s.VerifyRevertBoundary(context.Background(), "turn-b", "turn-c")
	if err != nil {
		t.Fatalf("VerifyRevertBoundary: %v — a tail that moved on is a caller decision, not a failure", err)
	}
	if applied {
		t.Fatal("applied = true while the provider tail is turn-x, not the requested turn-b")
	}
	if verified.ThreadID != "codex-thread-revert" {
		t.Fatalf("ThreadID = %q, want the probed thread so the caller can fork on it", verified.ThreadID)
	}
}

// The same shape with NO kept turn named: AO cannot say what the tail
// should be, so the anchor's absence is all the proof there is and the
// verdict stays "applied". Pinned so the tail test above cannot be
// tightened into refusing every cut whose kept turn AO never recorded.
func TestVerifyRevertBoundaryAcceptsAnAnchorGoneCutWithNoKeptTurn(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	page := `{"data":[{"id":"turn-x"},{"id":"turn-b"}],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page, page, capture))

	if _, applied, err := s.VerifyRevertBoundary(context.Background(), "", "turn-c"); err != nil || !applied {
		t.Fatalf("VerifyRevertBoundary = (%v, %v), want the anchor-gone verdict", applied, err)
	}
}

// TestVerifyRevertBoundaryRefusesAHistoryNarrowerThanTheBoundary: neither
// anchor survives, so the fork the caller would fall back to has nothing
// to anchor on either. Saying so beats an obscure refusal a round trip
// later.
func TestVerifyRevertBoundaryRefusesAHistoryNarrowerThanTheBoundary(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	page := `{"data":[{"id":"turn-a"}],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page, page, capture))

	if _, applied, err := s.VerifyRevertBoundary(context.Background(), "turn-b", "turn-c"); err == nil || applied {
		t.Fatalf("VerifyRevertBoundary = (%v, %v), want an error naming the missing kept turn", applied, err)
	}
}

// TestVerifyRevertBoundaryRequiresAnAnchor pins the same refusal Revert
// makes: an empty anchor is not "verify nothing".
func TestVerifyRevertBoundaryRequiresAnAnchor(t *testing.T) {
	capture := t.TempDir() + "/turns-list.json"
	page := `{"data":[],"nextCursor":null}`
	s := newRevertSession(t, turnsListFakeScript(t, "codex-thread-revert", page, page, capture))

	if _, _, err := s.VerifyRevertBoundary(context.Background(), "turn-b", "  "); err == nil {
		t.Fatal("VerifyRevertBoundary accepted an empty anchor")
	}
	if _, err := os.Stat(capture); err == nil {
		t.Fatal("the refusal reached the wire; it must be pre-RPC")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// startFakeScript builds a fake app-server that captures the `thread/start`
// request frame. When refuseFirstPaginatedStart is set the first paginated
// start is answered with the store-shaped refusal a codex with no SQLite
// state database raises, so the client's downgrade retry is exercised.
func startFakeScript(t *testing.T, userAgent, threadID, historyMode, capturePath string, refuseFirstPaginatedStart bool) string {
	t.Helper()
	refusal := ""
	if refuseFirstPaginatedStart {
		refusal = fmt.Sprintf(`        if echo "$line" | grep -q '"historyMode"' && [ ! -f %q.refused ]; then
            touch %q.refused
            echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"error\":{\"code\":-32600,\"message\":\"paginated threads require thread/turns/list and thread/items/list support\"}}"
            continue
        fi
`, capturePath, capturePath)
	}
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "$line" >> %q
%s        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
        continue
    fi
done
`,
		bashJSON(fmt.Sprintf(`{"userAgent":%q}`, userAgent)),
		capturePath,
		refusal,
		bashJSON(fmt.Sprintf(`{"thread":{"id":%q,"historyMode":%q}}`, threadID, historyMode)),
	)
	path := t.TempDir() + "/codex"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return path
}

// readCapturedStarts returns every `thread/start` frame the mock recorded, in
// the order it saw them.
func readCapturedStarts(t *testing.T, capturePath string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured thread/start frames: %v", err)
	}
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame struct {
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("decode captured thread/start frame %q: %v", line, err)
		}
		frames = append(frames, frame.Params)
	}
	return frames
}

// TestThreadStartRequestsPaginatedHistoryOnCodex0149 is the opt-in itself: a
// thread AO creates on a 0.148+ app-server is born with the history contract
// `thread/revert` needs, so the in-place cut is available for the rest of its
// life. Without this the field is absent, upstream defaults to legacy, and
// every AO thread refuses the revert forever.
func TestThreadStartRequestsPaginatedHistoryOnCodex0149(t *testing.T) {
	capture := t.TempDir() + "/thread-start.jsonl"
	binary := startFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-paginated", "paginated", capture, false)

	s := newRevertSession(t, binary)

	frames := readCapturedStarts(t, capture)
	if len(frames) != 1 {
		t.Fatalf("thread/start frames = %d, want 1", len(frames))
	}
	if got := frames[0]["historyMode"]; got != "paginated" {
		t.Fatalf("thread/start historyMode = %v, want paginated", got)
	}
	if got := s.ThreadHistoryMode(); got != "paginated" {
		t.Fatalf("ThreadHistoryMode() = %q, want paginated", got)
	}
	if !s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = false on a 0.149 paginated thread; the opt-in bought nothing")
	}
}

// TestThreadStartOmitsHistoryModeBelowTheFloor: a 0.147 app-server has no
// `thread/revert`, so a paginated thread would carry the behavioral
// differences with none of the benefit. Say nothing and take the server's
// default, exactly as before the opt-in existed.
func TestThreadStartOmitsHistoryModeBelowTheFloor(t *testing.T) {
	capture := t.TempDir() + "/thread-start.jsonl"
	binary := startFakeScript(t,
		"codex_cli_rs/0.147.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-legacy", "legacy", capture, false)

	s := newRevertSession(t, binary)

	frames := readCapturedStarts(t, capture)
	if len(frames) != 1 {
		t.Fatalf("thread/start frames = %d, want 1", len(frames))
	}
	if _, present := frames[0]["historyMode"]; present {
		t.Fatalf("thread/start carried historyMode on a 0.147 server: %v", frames[0])
	}
	if s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = true below the 0.148 floor")
	}
}

// TestThreadStartFallsBackWhenPaginatedHistoryIsRefused: the refusal AO can
// actually provoke on a new-enough binary is store-shaped — an app-server
// whose thread store has no SQLite state database cannot serve
// `thread/turns/list`. Upstream raises it while destructuring the params,
// before any thread is created, so the retry is a retry and not a second
// half-start. A session that failed here would be a hard regression: the user
// could not start a codex thread at all.
func TestThreadStartFallsBackWhenPaginatedHistoryIsRefused(t *testing.T) {
	capture := t.TempDir() + "/thread-start.jsonl"
	binary := startFakeScript(t,
		"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) some/1.0",
		"codex-thread-legacy", "legacy", capture, true)

	s := newRevertSession(t, binary)

	frames := readCapturedStarts(t, capture)
	if len(frames) != 2 {
		t.Fatalf("thread/start frames = %d, want 2 (paginated then the downgrade retry)", len(frames))
	}
	if got := frames[0]["historyMode"]; got != "paginated" {
		t.Fatalf("first thread/start historyMode = %v, want paginated", got)
	}
	if _, present := frames[1]["historyMode"]; present {
		t.Fatalf("retry still carried historyMode: %v", frames[1])
	}
	if got := s.rootThreadID(); got != "codex-thread-legacy" {
		t.Fatalf("RootThreadID() = %q, want the thread the retry started", got)
	}
	if s.SupportsThreadRevert() {
		t.Fatal("SupportsThreadRevert() = true on the legacy thread the downgrade produced")
	}
}

// TestThreadStartHistoryMode pins the gate itself, including the fail-closed
// answer for an app-server whose build could not be read.
func TestThreadStartHistoryMode(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"", ""},
		{"0.147.0", ""},
		{"0.147.9", ""},
		{"0.148.0", "paginated"},
		{"0.149.0", "paginated"},
		{"1.0.0", "paginated"},
	} {
		if got := threadStartHistoryMode(tc.version); got != tc.want {
			t.Errorf("threadStartHistoryMode(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// TestIsHistoryPaginationUnsupported keeps the downgrade predicate matched to
// upstream's own (codex-rs/tui/src/app_server_session.rs:175) and, more
// importantly, keeps it NARROW: a start that failed for any other reason must
// stay failed rather than silently retrying as a legacy thread.
func TestIsHistoryPaginationUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not an rpc error", errors.New("boom"), false},
		{"method not found", &RPCError{Method: "thread/start", Code: -32601, Message: "unknown"}, true},
		{
			"store cannot serve paginated lists",
			&RPCError{Method: "thread/start", Code: -32600, Message: "paginated threads require thread/turns/list and thread/items/list support"},
			true,
		},
		{
			"unknown field",
			&RPCError{Method: "thread/start", Code: -32602, Message: "unknown field `historyMode`"},
			true,
		},
		{
			"unknown enum variant",
			&RPCError{Method: "thread/start", Code: -32600, Message: "unknown variant `paginated`, expected `legacy`"},
			true,
		},
		{
			"a real start failure stays a failure",
			&RPCError{Method: "thread/start", Code: -32600, Message: "thread 123 already has an active writer"},
			false,
		},
		{
			"an unrelated internal error stays a failure",
			&RPCError{Method: "thread/start", Code: -32603, Message: "historyMode"},
			false,
		},
	} {
		if got := isHistoryPaginationUnsupported(tc.err); got != tc.want {
			t.Errorf("isHistoryPaginationUnsupported(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
