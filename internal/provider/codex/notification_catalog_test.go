package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// captureLog redirects the standard logger for the duration of fn and
// returns everything it wrote.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previous)
		log.SetFlags(flags)
	})
	fn()
	return buf.String()
}

// TestNotificationCatalogPartitionsIntoConsumedAndOptedOut is the
// structural claim the whole opt-out scheme rests on: every catalogued
// method is either something we consume or something we tell Codex to
// stop sending. A method that is in neither bucket would be silently
// received and silently dropped, which is the state item 3.6/9.2 exists
// to eliminate.
func TestNotificationCatalogPartitionsIntoConsumedAndOptedOut(t *testing.T) {
	optOut := sessionOptOutNotificationMethods()
	for _, method := range codexNotificationCatalog {
		consumed := notificationMethodConsumed(method)
		inOptOut := slices.Contains(optOut, method)
		if consumed == inOptOut {
			t.Errorf("%q: consumed=%v optedOut=%v — must be exactly one", method, consumed, inOptOut)
		}
	}
	if len(optOut) == 0 {
		t.Error("opt-out list is empty; the derivation is not running")
	}
}

// TestSessionOptOutNeverDropsAConsumedNotification names the methods
// whose loss would be silent and severe. The generic partition test above
// would still pass if a classifier case were deleted (the method would
// move from consumed to opted-out in lockstep), so the load-bearing ones
// are pinned by name here.
func TestSessionOptOutNeverDropsAConsumedNotification(t *testing.T) {
	mustReceive := []string{
		"error",
		"turn/started",
		"turn/completed",
		"item/started",
		"item/completed",
		"item/agentMessage/delta",
		"item/plan/delta",
		"item/commandExecution/outputDelta",
		"item/commandExecution/terminalInteraction",
		"item/reasoning/textDelta",
		"thread/tokenUsage/updated",
		"thread/settings/updated",
		"thread/compacted",
		"model/rerouted",
		"model/safetyBuffering/updated",
		"account/rateLimits/updated",
		"mcpServer/oauthLogin/completed",
		"mcpServer/startupStatus/updated",
		"serverRequest/resolved",
		"rawResponseItem/completed",
	}
	optOut := sessionOptOutNotificationMethods()
	for _, method := range mustReceive {
		if slices.Contains(optOut, method) {
			t.Errorf("%q is consumed by this package but appears in the initialize opt-out list", method)
		}
		if !slices.Contains(codexNotificationCatalog, method) {
			t.Errorf("%q is missing from codexNotificationCatalog", method)
		}
	}
}

// TestCodexNotificationCatalogHasNoDuplicates keeps the hand-maintained
// half of the derivation honest — a duplicated entry would silently
// double an opt-out list and hide a copy/paste error during the next
// upstream sync.
func TestCodexNotificationCatalogHasNoDuplicates(t *testing.T) {
	seen := map[string]struct{}{}
	for _, method := range codexNotificationCatalog {
		if strings.TrimSpace(method) == "" {
			t.Fatal("catalog contains an empty method name")
		}
		if _, dup := seen[method]; dup {
			t.Errorf("catalog lists %q twice", method)
		}
		seen[method] = struct{}{}
	}
}

// TestClassifiedMethodsAreDecidedByMethodAlone pins the property the
// opt-out derivation relies on: a dispatcher's `handled` answer never
// depends on the params. If someone added a case that returns
// `(nil, false)` for some payloads, the empty-params probe would report a
// method as unconsumed and we would opt out of a notification we handle.
func TestClassifiedMethodsAreDecidedByMethodAlone(t *testing.T) {
	realistic := map[string]string{
		"item/started":                    `{"turnId":"t1","item":{"id":"i1","type":"commandExecution","status":"inProgress","command":"ls"}}`,
		"item/completed":                  `{"turnId":"t1","item":{"id":"i1","type":"commandExecution","status":"completed","exitCode":0}}`,
		"turn/completed":                  `{"turn":{"id":"t1","status":"completed"}}`,
		"turn/diff/updated":               `{"turnId":"t1","diff":"--- a\n+++ b\n"}`,
		"error":                           `{"error":{"message":"boom"},"willRetry":false}`,
		"thread/tokenUsage/updated":       `{"tokenUsage":{"last":{"totalTokens":10},"total":{"totalTokens":10},"modelContextWindow":100}}`,
		"item/agentMessage/delta":         `{"turnId":"t1","itemId":"i1","delta":"hello"}`,
		"model/safetyBuffering/updated":   `{"threadId":"th","turnId":"t1","model":"gpt","reasons":["policy"],"showBufferingUi":true}`,
		"thread/settings/updated":         `{"threadId":"th","threadSettings":{"model":"gpt"}}`,
		"item/autoApprovalReview/started": `{"turnId":"t1","itemId":"i1"}`,
	}
	for method, params := range realistic {
		_, emptyHandled := classifyNotification("th", method, emptyNotificationParams)
		_, richHandled := classifyNotification("th", method, json.RawMessage(params))
		if !emptyHandled || !richHandled {
			t.Errorf("%q: handled with empty params = %v, with realistic params = %v; both must be true",
				method, emptyHandled, richHandled)
		}
	}
}

// TestOneShotOptOutKeepsOnlyNamedMethods covers both one-shot shapes: the
// login client that waits on exactly one notification, and the
// response-only probes that wait on none.
func TestOneShotOptOutKeepsOnlyNamedMethods(t *testing.T) {
	loginOptOut := oneShotOptOutNotificationMethods("account/login/completed")
	if slices.Contains(loginOptOut, "account/login/completed") {
		t.Error("login opted out of the completion notification it blocks on")
	}
	if len(loginOptOut) != len(codexNotificationCatalog)-1 {
		t.Errorf("login opt-out has %d entries, want %d", len(loginOptOut), len(codexNotificationCatalog)-1)
	}
	// A one-shot must not inherit the Session's consumed set: it never
	// runs the classifier, so "the Session handles item/completed" is no
	// reason for a probe to keep receiving it.
	if !slices.Contains(loginOptOut, "item/completed") {
		t.Error("one-shot opt-out kept a Session-consumed method")
	}

	probeOptOut := oneShotOptOutNotificationMethods()
	if len(probeOptOut) != len(codexNotificationCatalog) {
		t.Errorf("response-only opt-out has %d entries, want the whole catalog (%d)",
			len(probeOptOut), len(codexNotificationCatalog))
	}
}

// TestCodexInitializeParamsShape pins the handshake every entry point in
// the package now shares.
func TestCodexInitializeParamsShape(t *testing.T) {
	params := codexInitializeParams("agent_overflow_test", []string{"fs/changed"})
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal initialize params: %v", err)
	}
	var decoded struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool     `json:"experimentalApi"`
			OptOut          []string `json:"optOutNotificationMethods"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if decoded.ClientInfo.Name != "agent_overflow_test" || decoded.ClientInfo.Title != "Agent Overflow" {
		t.Errorf("clientInfo = %+v", decoded.ClientInfo)
	}
	if !decoded.Capabilities.ExperimentalAPI {
		t.Error("experimentalApi must stay set: thread/settings/updated, item/plan/delta and the background-terminal RPCs are all gated on it")
	}
	if len(decoded.Capabilities.OptOut) != 1 || decoded.Capabilities.OptOut[0] != "fs/changed" {
		t.Errorf("optOutNotificationMethods = %v", decoded.Capabilities.OptOut)
	}

	// An empty list must omit the key rather than send `null`/`[]` — the
	// field is `Option<Vec<String>>` upstream and "no preference" is the
	// absent case.
	bare, err := json.Marshal(codexInitializeParams("agent_overflow_test", nil))
	if err != nil {
		t.Fatalf("marshal bare initialize params: %v", err)
	}
	if strings.Contains(string(bare), "optOutNotificationMethods") {
		t.Errorf("empty opt-out list must be omitted, got %s", string(bare))
	}
}

// TestWarnUnclaimedNotificationLogsOncePerMethod covers the 9.2 dedup
// transitions: first delivery logs, repeats stay silent, and a second
// distinct method logs on its own.
func TestWarnUnclaimedNotificationLogsOncePerMethod(t *testing.T) {
	s := &Session{}
	output := captureLog(t, func() {
		s.warnUnclaimedNotification("future/notification")
		s.warnUnclaimedNotification("future/notification")
		s.warnUnclaimedNotification("future/notification")
		s.warnUnclaimedNotification("other/notification")
	})
	if got := strings.Count(output, `"future/notification"`); got != 1 {
		t.Errorf("first method logged %d times, want exactly 1:\n%s", got, output)
	}
	if got := strings.Count(output, `"other/notification"`); got != 1 {
		t.Errorf("second method logged %d times, want exactly 1:\n%s", got, output)
	}
}

// TestWarnUnclaimedNotificationStaysSilentForConsumedMethods is the other
// half of the contract: the drift log must mean "we have never seen this
// method", not "we chose not to render it". A method consumed inline
// (item/plan/delta) or by a side channel produces no classifier events and
// would otherwise trip the alarm on every session.
func TestWarnUnclaimedNotificationStaysSilentForConsumedMethods(t *testing.T) {
	s := &Session{}
	output := captureLog(t, func() {
		for _, method := range []string{
			"item/plan/delta",
			"mcpServer/startupStatus/updated",
			"mcpServer/oauthLogin/completed",
			"thread/started",
			"thread/settings/updated",
			"",
		} {
			s.warnUnclaimedNotification(method)
		}
	})
	if strings.TrimSpace(output) != "" {
		t.Errorf("consumed methods must not reach the drift log, got:\n%s", output)
	}
}

// TestWarnUnclaimedNotificationBoundsItsDedupSet pins the overflow
// transition. The dedup key is server-supplied, so an app-server emitting
// unique method names must neither grow the map without limit nor keep
// logging; it gets one suppression notice and then silence.
func TestWarnUnclaimedNotificationBoundsItsDedupSet(t *testing.T) {
	s := &Session{}
	overflowOutput := captureLog(t, func() {
		for i := 0; i < maxUnclaimedNotificationMethods+10; i++ {
			s.warnUnclaimedNotification("drift/method" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		}
	})
	if got := strings.Count(overflowOutput, "suppressing further drift warnings"); got != 1 {
		t.Errorf("suppression notice logged %d times, want exactly 1:\n%s", got, overflowOutput)
	}
	s.mu.Lock()
	size := len(s.unclaimedNotifications)
	s.mu.Unlock()
	if size != maxUnclaimedNotificationMethods {
		t.Errorf("dedup set grew to %d entries, want it capped at %d", size, maxUnclaimedNotificationMethods)
	}

	// Post-overflow deliveries are fully silent, including the notice.
	afterOutput := captureLog(t, func() {
		s.warnUnclaimedNotification("drift/yet-another")
	})
	if strings.TrimSpace(afterOutput) != "" {
		t.Errorf("post-overflow delivery must be silent, got:\n%s", afterOutput)
	}
}

// TestDispatchNotificationReportsUnknownMethod proves the wiring rather
// than the helper: an unrecognized notification arriving on the real
// dispatch path reaches the drift log and emits nothing.
func TestDispatchNotificationReportsUnknownMethod(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: testThread,
		onEvent:  func(e provider.ProviderEvent) { events = append(events, e) },
	}
	output := captureLog(t, func() {
		s.dispatchNotification("thread/somethingBrandNew", json.RawMessage(`{"threadId":"th"}`))
	})
	if !strings.Contains(output, "thread/somethingBrandNew") {
		t.Errorf("unknown notification did not reach the drift log, got:\n%s", output)
	}
	if len(events) != 0 {
		t.Errorf("unknown notification must not synthesize events, got %+v", events)
	}
}

// TestNewSessionSendsOptOutNotificationMethods proves the wiring end to
// end against a fake app-server: the real handshake carries the derived
// opt-out list, and the list it carries is the one the derivation
// produced — not an empty array, and not something containing a method
// the session depends on.
func TestNewSessionSendsOptOutNotificationMethods(t *testing.T) {
	capturePath := t.TempDir() + "/initialize.json"
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-optout\"}}}"
    fi
done
`, capturePath)

	scriptPath := t.TempDir() + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured initialize: %v", err)
	}
	var frame struct {
		Params struct {
			Capabilities struct {
				ExperimentalAPI bool     `json:"experimentalApi"`
				OptOut          []string `json:"optOutNotificationMethods"`
			} `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode captured initialize: %v (raw %s)", err, string(raw))
	}
	if !frame.Params.Capabilities.ExperimentalAPI {
		t.Error("session handshake dropped experimentalApi")
	}
	got := frame.Params.Capabilities.OptOut
	if !slices.Equal(got, sessionOptOutNotificationMethods()) {
		t.Errorf("handshake opt-out list diverged from the derivation:\n got: %v\nwant: %v",
			got, sessionOptOutNotificationMethods())
	}
	if slices.Contains(got, "item/completed") || slices.Contains(got, "turn/completed") {
		t.Errorf("handshake opted out of a lifecycle notification: %v", got)
	}
}

// TestCatalogCoversThe0149Additions pins the five methods the 0.147→0.149
// sync added, and — more importantly — which side of the consumed/opt-out
// line each one landed on. The generic partition test above is satisfied by
// ANY assignment; this one is the record of the decision.
func TestCatalogCoversThe0149Additions(t *testing.T) {
	added := []struct {
		method   string
		consumed bool
		why      string
	}{
		{
			"thread/queue/changed", true,
			"an external `codex queue` write is about to inject a turn into a thread AO owns; " +
				"opting out would make that turn arrive with no explanation",
		},
		{
			"autoApprovalReview/strictReviewRequired", true,
			"the `auto` tier's reviewer switched to the slow path; without the notice the session looks stalled",
		},
		{
			"thread/reverted", true,
			"AO cuts history in place with thread/revert wherever the thread supports it " +
				"(session_revert.go); the echo is what releases the RPC's wait, and an " +
				"UNsolicited one is the only signal that a foreign writer cut a thread AO caches",
		},
		{
			"project/changed", false,
			"upstream's project surface is not consumed; AO owns its own project rows",
		},
		{
			"thread/project/updated", false,
			"same — a thread's upstream project binding is not a thing AO reads",
		},
	}
	optOut := sessionOptOutNotificationMethods()
	for _, entry := range added {
		if !slices.Contains(codexNotificationCatalog, entry.method) {
			t.Errorf("%q missing from codexNotificationCatalog", entry.method)
			continue
		}
		if got := notificationMethodConsumed(entry.method); got != entry.consumed {
			t.Errorf("%q consumed = %v, want %v (%s)", entry.method, got, entry.consumed, entry.why)
		}
		if got := slices.Contains(optOut, entry.method); got == entry.consumed {
			t.Errorf("%q in opt-out list = %v, want %v", entry.method, got, !entry.consumed)
		}
	}
}
