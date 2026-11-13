package app

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
)

// A person's Stop names the agents it was confirmed to kill. These tests
// pin what the confirmation covers: the agents it names, by transcript
// root, and nothing launched after the question was asked.

// An agent launched while the person read the question is not covered:
// the confirmed Stop is refused again naming every live agent, and only a
// confirmation naming both stops the turn.
func TestInterruptTurn_ConfirmationCoversOnlyTheAgentsItNames(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "first", "task-first", "")
	asked := requireRefusal(t, f.app.InterruptTurn(f.thread.ID, nil))
	if got := agentIDs(asked); len(got) != 1 || got[0] != "first=running" {
		t.Fatalf("refusal agents = %v, want the first agent", got)
	}

	f.launchAgent(t, "later", "task-later", "")
	again := requireRefusal(t, f.app.InterruptTurn(f.thread.ID, []string{"first"}))
	if got := agentIDs(again); len(got) != 2 || got[0] != "first=running" || got[1] != "later=running" {
		t.Fatalf("second refusal agents = %v, want both agents", got)
	}
	if n := f.interrupts(t); n != 0 {
		t.Fatalf("a Stop confirmed for one of two agents sent %d interrupts", n)
	}

	if err := f.app.InterruptTurn(f.thread.ID, []string{"first", "later"}); err != nil {
		t.Fatalf("InterruptTurn confirmed for both: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// A confirmed agent that ended before the Stop landed is no reason to ask
// again: the live set is inside the confirmation.
func TestInterruptTurn_AConfirmedAgentThatEndedDoesNotRefuse(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "stays", "task-stays", "")
	f.launchAgent(t, "ends", "task-ends", "")
	requireRefusal(t, f.app.InterruptTurn(f.thread.ID, nil))
	f.stopAgent(t, "ends", "task-ends")

	if err := f.app.InterruptTurn(f.thread.ID, []string{"stays", "ends"}); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// A confirmation names an agent by its transcript root, which a resumed
// round keeps: the round's carrier is the same agent.
func TestInterruptTurn_ConfirmationNamesAResumedAgentByItsRoot(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "root", "task-root", "")
	f.stopAgent(t, "root", "task-root")
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier", ItemType: "SendMessage"},
		map[string]any{"toolName": "SendMessage", "input": map[string]any{"to": "task-root", "message": "continue"}})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier"},
		map[string]any{"task_id": "task-root", "task_type": "local_agent", "resumes_tool_use_id": "root",
			"description": "Agent root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "carrier", Content: "Resuming agent"},
		map[string]any{"is_background": true})

	requireRefusal(t, f.app.InterruptTurn(f.thread.ID, []string{"carrier"}))
	if err := f.app.InterruptTurn(f.thread.ID, []string{"root"}); err != nil {
		t.Fatalf("InterruptTurn confirmed for the root: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}

// The confirmation crosses the real dispatcher as the list the client sent.
func TestInterruptTurn_ConfirmationArrivesOverTheWire(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(f.app, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
		t.Fatal(err)
	}
	method, ok := dispatcher.LookupName("InterruptTurn")
	if !ok {
		t.Fatal("missing method InterruptTurn")
	}
	params := []json.RawMessage{json.RawMessage(strconv.Quote(f.thread.ID)), json.RawMessage(`["agent"]`)}
	if _, frame := dispatcher.Invoke(context.Background(), method, params); frame != nil {
		t.Fatalf("confirmed Stop over the wire: frame %+v", frame)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
}
