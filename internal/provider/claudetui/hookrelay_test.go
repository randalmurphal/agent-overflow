package claudetui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func newTestRelay(t *testing.T, fed chan<- json.RawMessage) *hookRelay {
	t.Helper()
	relay, err := newHookRelay(
		func(env json.RawMessage) {
			if fed != nil {
				fed <- env
			}
		},
		func(string, string, string) {},
		compactionHooks{},
		func(err error) { t.Errorf("relay error: %v", err) },
	)
	if err != nil {
		t.Fatalf("newHookRelay: %v", err)
	}
	relay.start()
	t.Cleanup(func() { _ = relay.close() })
	return relay
}

func postHook(t *testing.T, relay *hookRelay, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, relay.url(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(hookAuthHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post hook: %v", err)
	}
	return resp
}

// TestHookRelayCompactionDispatch proves the two compaction signals route to
// their hooks: PreCompact arms the capture; PostCompact finalizes it with the
// trigger and the committed summary. Channels carry the calls back so the assert
// is race-safe across the relay's handler goroutine.
func TestHookRelayCompactionDispatch(t *testing.T) {
	armCh := make(chan struct{}, 1)
	finalizeCh := make(chan [2]string, 1)
	relay, err := newHookRelay(
		func(json.RawMessage) {},
		func(string, string, string) {},
		compactionHooks{
			arm:      func() { armCh <- struct{}{} },
			finalize: func(trigger, summary string) { finalizeCh <- [2]string{trigger, summary} },
		},
		func(err error) { t.Errorf("relay error: %v", err) },
	)
	if err != nil {
		t.Fatalf("newHookRelay: %v", err)
	}
	relay.start()
	t.Cleanup(func() { _ = relay.close() })

	pre := postHook(t, relay, relay.authToken(), `{"hook_event_name":"PreCompact","trigger":"auto"}`)
	_ = pre.Body.Close()
	select {
	case <-armCh:
	case <-time.After(time.Second):
		t.Fatal("PreCompact did not arm the compaction capture")
	}

	post := postHook(t, relay, relay.authToken(),
		`{"hook_event_name":"PostCompact","trigger":"auto","compact_summary":"committed summary"}`)
	_ = post.Body.Close()
	select {
	case got := <-finalizeCh:
		if got[0] != "auto" || got[1] != "committed summary" {
			t.Errorf("finalize(%q, %q), want (auto, committed summary)", got[0], got[1])
		}
	case <-time.After(time.Second):
		t.Fatal("PostCompact did not finalize the compaction")
	}
}

// TestIsLoopback covers the peer check that guards BOTH the hook relay and the
// gateway: only loopback peers pass; a LAN/public address is rejected.
func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:8080", true},
		{"127.0.0.5:1", true},
		{"192.168.1.10:443", false},
		{"10.0.0.1:80", false},
		{"8.8.8.8:53", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopback(tc.addr); got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestHookRelayRejectsBadToken(t *testing.T) {
	relay := newTestRelay(t, nil)

	resp := postHook(t, relay, "wrong-token", `{"hook_event_name":"PostToolUse"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", resp.StatusCode)
	}

	ok := postHook(t, relay, relay.authToken(), `{"hook_event_name":"SessionStart"}`)
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("good token status = %d, want 200", ok.StatusCode)
	}
}

func TestHookRelayObserveDispatch(t *testing.T) {
	fed := make(chan json.RawMessage, 4)
	relay := newTestRelay(t, fed)

	body := `{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"toolu_9",` +
		`"tool_response":{"stdout":"hi","exit_code":0}}`
	resp := postHook(t, relay, relay.authToken(), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("observe status = %d, want 200", resp.StatusCode)
	}

	select {
	case env := <-fed:
		s := string(env)
		if !strings.Contains(s, "tool_result") || !strings.Contains(s, "toolu_9") {
			t.Fatalf("fed envelope not a tool_result for toolu_9: %s", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observe dispatch did not feed an envelope")
	}
}

func TestHookRelayAskUserQuestionRoundTrip(t *testing.T) {
	fed := make(chan json.RawMessage, 4)
	relay := newTestRelay(t, fed)

	body := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_use_id":"toolu_q",` +
		`"tool_input":{"questions":[{"question":"Pick a file","header":"File",` +
		`"options":[{"label":"alpha.txt"},{"label":"beta.txt"}]}]}}`

	respCh := make(chan *http.Response, 1)
	go func() {
		respCh <- postHook(t, relay, relay.authToken(), body)
	}()

	// The relay surfaces the control_request through feed; pull the request_id.
	var requestID string
	select {
	case env := <-fed:
		var e struct {
			RequestID string `json:"request_id"`
			Type      string `json:"type"`
		}
		if err := json.Unmarshal(env, &e); err != nil {
			t.Fatalf("decode control_request: %v", err)
		}
		if e.Type != "control_request" || e.RequestID == "" {
			t.Fatalf("unexpected surfaced envelope: %s", env)
		}
		requestID = e.RequestID
	case <-time.After(2 * time.Second):
		t.Fatal("AskUserQuestion did not surface a control_request")
	}

	// Answer it (keyed by question text, the projection's primary key).
	answers := map[string]provider.UserInputAnswer{"Pick a file": {"beta.txt"}}
	if err := relay.respond(requestID, answers); err != nil {
		t.Fatalf("respond: %v", err)
	}

	select {
	case resp := <-respCh:
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		s := string(out)
		if !strings.Contains(s, "hookSpecificOutput") || !strings.Contains(s, "updatedInput") {
			t.Fatalf("answer-back missing hookSpecificOutput/updatedInput: %s", s)
		}
		if !strings.Contains(s, "beta.txt") {
			t.Fatalf("answer-back missing chosen label: %s", s)
		}
		// The original questions must be echoed intact (a partial updatedInput
		// makes the TUI re-prompt).
		if !strings.Contains(s, "Pick a file") {
			t.Fatalf("answer-back dropped the original tool_input: %s", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answered AskUserQuestion hook never returned")
	}

	// A second answer for the same request is a no-op error, not a panic.
	if err := relay.respond(requestID, answers); err == nil {
		t.Error("second respond should fail — request already resolved")
	}
}
