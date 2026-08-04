package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"agent-overflow/internal/provider"
)

func TestDispatchMCPOAuthCompletion_FiresHandler(t *testing.T) {
	type call struct {
		name    string
		success bool
		errMsg  string
	}
	var got []call
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent: func(evt provider.ProviderEvent) {
			t.Fatalf("oauth-completed should not flow through onEvent; got %+v", evt)
		},
	}
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
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent:                func(provider.ProviderEvent) {},
	}
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
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent: func(evt provider.ProviderEvent) {
			t.Fatalf("no event should be emitted; got %+v", evt)
		},
	}

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
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent: func(evt provider.ProviderEvent) {
			t.Fatalf("startup-update should not flow through onEvent; got %+v", evt)
		},
	}
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
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent: func(evt provider.ProviderEvent) {
			t.Fatalf("no event should be emitted; got %+v", evt)
		},
	}
	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"name":"github","status":"ready"}}`)
	s.dispatchLine(line)
}

func TestDispatchMCPStartupUpdate_MissingNameIsDropped(t *testing.T) {
	called := false
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent:                func(provider.ProviderEvent) {},
	}
	s.SetMCPStartupUpdateHandler(func(MCPStartupUpdate) { called = true })
	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"status":"ready"}}`)
	s.dispatchLine(line)
	if called {
		t.Fatalf("handler must not fire when name is missing")
	}
}

func TestDispatchMCPOAuthCompletion_MissingNameIsDropped(t *testing.T) {
	called := false
	s := &Session{
		threadID:               "thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{},
		childParentByAgentPath: map[string]string{},
		agentPathByThread:      map[string]string{},
		onEvent:                func(provider.ProviderEvent) {},
	}
	s.SetMCPOAuthCompletedHandler(func(name string, success bool, errMsg string) {
		called = true
	})

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"success":true}}`)
	s.dispatchLine(line)

	if called {
		t.Fatalf("handler must not fire when name is missing")
	}
}
