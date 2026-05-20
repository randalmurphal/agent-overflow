package codex

import (
	"encoding/json"
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

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"serverName":"linear","success":true}}`)
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

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"serverName":"github","success":false,"error":"user cancelled"}}`)
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

	line := []byte(`{"jsonrpc":"2.0","method":"mcpServer/oauthLogin/completed","params":{"serverName":"linear","success":true}}`)
	s.dispatchLine(line)
}

func TestDispatchMCPOAuthCompletion_MissingServerNameIsDropped(t *testing.T) {
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
		t.Fatalf("handler must not fire when serverName is missing")
	}
}
