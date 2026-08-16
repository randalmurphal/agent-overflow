package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-overflow/internal/provider"
)

func TestDispatchMCPOAuthCompletion_FiresHandler(t *testing.T) {
	type call struct {
		name    string
		success bool
		errMsg  string
	}
	var got []call
	s := newMCPTestSession(t, nil)
	s.SetMCPOAuthCompletedHandler(func(name string, success bool, errMsg string) {
		got = append(got, call{name, success, errMsg})
	})

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"name":"linear","success":true}}`)
	s.dispatchLine(line)

	if len(got) != 1 {
		t.Fatalf("handler fired %d times, want 1", len(got))
	}
	if got[0] != (call{name: "linear", success: true}) {
		t.Errorf("handler got %+v", got[0])
	}
}

func TestDispatchMCPOAuthCompletion_PropagatesFailure(t *testing.T) {
	var (
		gotName, gotErr string
		gotSuccess      bool
	)
	s := newMCPTestSession(t, func(provider.ProviderEvent) {})
	s.SetMCPOAuthCompletedHandler(func(name string, success bool, errMsg string) {
		gotName, gotSuccess, gotErr = name, success, errMsg
	})

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"name":"github","success":false,"error":"user cancelled"}}`)
	s.dispatchLine(line)

	if gotName != "github" || gotSuccess || gotErr != "user cancelled" {
		t.Fatalf("got name=%q success=%v err=%q", gotName, gotSuccess, gotErr)
	}
}

func TestDispatchMCPOAuthCompletion_NoHandlerIsNoop(t *testing.T) {
	s := newMCPTestSession(t, nil)

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"name":"linear","success":true}}`)
	s.dispatchLine(line)
}

// TestSession_ListMCPServerStatuses_ThreadScopedRoundTrip drives the
// live-session status list end to end against a fake app-server:
// the request must carry the session's root threadId (without it the
// app-server answers for the global config view, omitting
// thread-scoped plugin/project servers) and the tools map must decode
// into names.
func TestSession_ListMCPServerStatuses_ThreadScopedRoundTrip(t *testing.T) {
	capturePath := t.TempDir() + "/list-request.json"
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"codex-thread-mcp\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"mcpServerStatus/list"'; then
        echo "$line" > %q
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"data\":[{\"name\":\"github\",\"authStatus\":\"oAuth\",\"tools\":{\"issues_list\":{},\"pr_read\":{}}}],\"nextCursor\":null}}"
    fi
done
`, capturePath)

	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
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

	list, err := s.ListMCPServerStatuses(context.Background())
	if err != nil {
		t.Fatalf("ListMCPServerStatuses: %v", err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list.Data))
	}
	if got := list.Data[0].ToolNames(); len(got) != 2 || got[0] != "issues_list" || got[1] != "pr_read" {
		t.Errorf("tool names = %v, want [issues_list pr_read]", got)
	}

	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			Detail   string `json:"detail"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode captured request: %v (raw: %s)", err, string(raw))
	}
	if frame.Params.ThreadID != "codex-thread-mcp" {
		t.Errorf("params.threadId = %q, want codex-thread-mcp", frame.Params.ThreadID)
	}
	if frame.Params.Detail != "toolsAndAuthOnly" {
		t.Errorf("params.detail = %q, want toolsAndAuthOnly", frame.Params.Detail)
	}
}

func TestDispatchMCPStartupUpdate_FiresHandler(t *testing.T) {
	var got []MCPStartupUpdate
	s := newMCPTestSession(t, nil)
	s.SetMCPStartupUpdateHandler(func(u MCPStartupUpdate) {
		got = append(got, u)
	})

	cases := []string{
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"ready","error":null}}`,
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"broken","status":"failed","error":"timeout"}}`,
	}
	for _, line := range cases {
		s.dispatchLine([]byte(line))
	}

	if len(got) != 2 {
		t.Fatalf("handler fired %d times, want 2", len(got))
	}
	if got[0].Name != "github" || got[0].State != "ready" {
		t.Errorf("first call: %+v", got[0])
	}
	if got[1].Name != "broken" || got[1].State != "failed" || got[1].Error != "timeout" {
		t.Errorf("second call: %+v", got[1])
	}
}

func TestDispatchMCPStartupUpdate_NoHandlerIsNoop(t *testing.T) {
	s := newMCPTestSession(t, nil)
	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"ready"}}`)
	s.dispatchLine(line)
}

func TestDispatchMCPStartupUpdate_MissingNameIsDropped(t *testing.T) {
	called := false
	s := newMCPTestSession(t, func(provider.ProviderEvent) {})
	s.SetMCPStartupUpdateHandler(func(MCPStartupUpdate) { called = true })
	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"status":"ready"}}`)
	s.dispatchLine(line)
	if called {
		t.Fatalf("handler must not fire when name is missing")
	}
}

// TestDispatchMCPStartupUpdate_RetainsStatePerServer pins last-write-wins
// retention: the app's MCP listing consults these instead of re-deriving
// a lifecycle from a probe, so a later update must be able to talk the
// session out of an earlier one in BOTH directions.
func TestDispatchMCPStartupUpdate_RetainsStatePerServer(t *testing.T) {
	s := newMCPTestSession(t, nil)
	s.SetMCPStartupUpdateHandler(func(MCPStartupUpdate) {})

	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"starting"}}`,
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"failed","error":"invalid_grant: Invalid refresh token","failureReason":null}}`,
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"failed","error":"boom"}}`,
		`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"ready"}}`,
	} {
		s.dispatchLine([]byte(line))
	}

	states := s.MCPStartupStates()
	if len(states) != 2 {
		t.Fatalf("retained %d servers, want 2: %+v", len(states), states)
	}
	atl := states["atlassian"]
	if atl.State != "failed" || atl.Error != "invalid_grant: Invalid refresh token" || atl.FailureReason != "" {
		t.Errorf("atlassian retained %+v, want the failed update with a null failureReason", atl)
	}
	if gh := states["github"]; gh.State != "ready" || gh.Error != "" {
		t.Errorf("github retained %+v, want the later ready update", gh)
	}
}

// TestDispatchMCPStartupUpdate_RetainsWithoutHandler: retention must not
// depend on an observer being wired up. The app registers the handler
// after NewSession returns, and a session that dropped its startup
// history in that window would answer a later MCP listing with an
// inference instead of what it saw.
func TestDispatchMCPStartupUpdate_RetainsWithoutHandler(t *testing.T) {
	s := newMCPTestSession(t, nil)
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"failed","error":"invalid_grant"}}`))

	states := s.MCPStartupStates()
	if got := states["atlassian"]; got.State != "failed" || got.Error != "invalid_grant" {
		t.Fatalf("retained %+v with no handler registered, want the failed update", got)
	}
}

func TestDispatchMCPStartupUpdate_MissingNameIsNotRetained(t *testing.T) {
	s := newMCPTestSession(t, nil)
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"status":"ready"}}`))
	if states := s.MCPStartupStates(); len(states) != 0 {
		t.Fatalf("retained %+v, want nothing for a nameless update", states)
	}
}

// TestMCPStartupStates_ReturnsACopy: the caller merges these against a
// list response and is free to mutate what it got back.
func TestMCPStartupStates_ReturnsACopy(t *testing.T) {
	s := newMCPTestSession(t, nil)
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"failed","error":"invalid_grant"}}`))

	first := s.MCPStartupStates()
	delete(first, "atlassian")
	first["injected"] = MCPStartupUpdate{Name: "injected", State: "ready"}

	second := s.MCPStartupStates()
	if len(second) != 1 {
		t.Fatalf("session state = %+v, want exactly the retained atlassian entry", second)
	}
	if got := second["atlassian"]; got.State != "failed" {
		t.Fatalf("session state = %+v, want the retained failed update", got)
	}
}

// TestForgetMCPStartupState covers the transitions, not just the
// states: retain → forget → nothing; forget of an unknown name is a
// no-op; a later update re-retains after a forget.
func TestForgetMCPStartupState(t *testing.T) {
	s := newMCPTestSession(t, nil)
	failedLine := []byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"failed","error":"invalid_grant"}}`)

	s.ForgetMCPStartupState("atlassian") // nothing retained yet — must not panic
	s.dispatchLine(failedLine)
	s.ForgetMCPStartupState("atlassian")
	if states := s.MCPStartupStates(); len(states) != 0 {
		t.Fatalf("retained %+v after forget, want nothing", states)
	}

	s.dispatchLine(failedLine)
	if got := s.MCPStartupStates()["atlassian"]; got.State != "failed" {
		t.Fatalf("retained %+v after re-dispatch, want the failed update back", got)
	}
	s.ForgetMCPStartupState("github") // unrelated name leaves atlassian alone
	if got := s.MCPStartupStates()["atlassian"]; got.State != "failed" {
		t.Fatalf("retained %+v after unrelated forget, want atlassian untouched", got)
	}
}

// TestDispatchMCPStartupUpdate_RetentionBounds drives the three bounds
// at the retention chokepoint over transitions: an oversized name drops
// the whole update (handler included), an oversized error is clamped on
// a rune boundary before it reaches the heap, and a full map keeps
// admitting updates for names it already knows while refusing new ones.
func TestDispatchMCPStartupUpdate_RetentionBounds(t *testing.T) {
	var handled []string
	s := newMCPTestSession(t, nil)
	s.SetMCPStartupUpdateHandler(func(u MCPStartupUpdate) { handled = append(handled, u.Name) })

	longName := strings.Repeat("n", mcpStartupNameMaxBytes+1)
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"` + longName + `","status":"failed"}}`))
	if len(handled) != 0 {
		t.Fatalf("oversized name reached the handler: %v", handled)
	}
	if states := s.MCPStartupStates(); len(states) != 0 {
		t.Fatalf("oversized name was retained: %+v", states)
	}

	// 'é' is two bytes; build an error whose cap lands mid-rune.
	longErr := strings.Repeat("é", mcpStartupErrorMaxBytes/2) + "xx"
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"failed","error":"` + longErr + `"}}`))
	got := s.MCPStartupStates()["atlassian"].Error
	if len(got) > mcpStartupErrorMaxBytes {
		t.Fatalf("retained error is %d bytes, cap is %d", len(got), mcpStartupErrorMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("retained error was cut mid-rune")
	}

	for i := 0; len(s.MCPStartupStates()) < mcpStartupStateMaxEntries; i++ {
		s.dispatchLine([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"srv-%d","status":"starting"}}`, i)))
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"one-too-many","status":"failed"}}`))
	if _, ok := s.MCPStartupStates()["one-too-many"]; ok {
		t.Fatalf("full map admitted a new name")
	}
	// A known name still updates at the cap — a chatty peer must not be
	// able to freeze real lifecycle state out.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"atlassian","status":"ready"}}`))
	if got := s.MCPStartupStates()["atlassian"]; got.State != "ready" {
		t.Fatalf("known name at cap retained %+v, want the ready update", got)
	}
}

func TestDispatchMCPOAuthCompletion_MissingNameIsDropped(t *testing.T) {
	called := false
	s := newMCPTestSession(t, func(provider.ProviderEvent) {})
	s.SetMCPOAuthCompletedHandler(func(name string, success bool, errMsg string) {
		called = true
	})

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"success":true}}`)
	s.dispatchLine(line)

	if called {
		t.Fatalf("handler must not fire when name is missing")
	}
}

// newMCPTestSession builds the minimal Session the MCP dispatch tests
// drive lines through. A nil onEvent fails the test on any provider
// event — none of these dispatch paths may emit one; tests that only
// need the retention side effect pass an explicit no-op.
func newMCPTestSession(t *testing.T, onEvent func(provider.ProviderEvent)) *Session {
	t.Helper()
	if onEvent == nil {
		onEvent = func(evt provider.ProviderEvent) {
			t.Fatalf("mcp dispatch must not flow through onEvent; got %+v", evt)
		}
	}
	return &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent:                onEvent,
	}
}
