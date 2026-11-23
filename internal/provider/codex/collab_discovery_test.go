package codex

import (
	"context"
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func TestRecoverChildOwnershipUsesOriginalSpawnAndKeepsNestedScope(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureWait)
	defer cancel()
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	fx := fixtureFor(t, s)
	threadOf := func(params json.RawMessage) string { return readNestedString(params, "threadId") }
	fx.answer("thread/read", func(params json.RawMessage) any {
		switch threadOf(params) {
		case "helper":
			return `{"thread":{"id":"helper","parentThreadId":"worker"}}`
		case "worker":
			return `{"thread":{"id":"worker","parentThreadId":"root-provider-thread"}}`
		}
		return `{}`
	})
	fx.answer("thread/items/list", func(params json.RawMessage) any {
		switch threadOf(params) {
		case "root-provider-thread":
			return `{"data":[{"turnId":"root-turn","item":{"type":"subAgentActivity","id":"spawn-worker","kind":"started","agentThreadId":"worker","agentPath":"/root/worker"}}]}`
		case "worker":
			return `{"data":[{"turnId":"worker-turn","item":{"type":"subAgentActivity","id":"later-send","kind":"interacted","agentThreadId":"helper","agentPath":"/root/worker/helper"}},{"turnId":"worker-turn","item":{"type":"subAgentActivity","id":"spawn-helper","kind":"started","agentThreadId":"helper","agentPath":"/root/worker/helper"}}]}`
		}
		return `{}`
	})
	s.ctx = ctx
	// This test isolates identity discovery from the independently tested status reader.
	s.collabHistory.visited["worker"] = 1
	s.collabHistory.visited["helper"] = 1
	t.Cleanup(s.collabAsyncWG.Wait)
	if dispatch, held := s.dispatchesWireThread("helper", deferredChildWireEvent{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"child text"}`)}, nil); dispatch || !held {
		t.Fatal("helper event was not held")
	}
	if err := s.recoverChildOwnership(ctx, "helper", make(map[string]bool)); err != nil {
		t.Fatal(err)
	}
	if s.parentToolUseForProviderThread("helper") != "spawn-helper" || s.parentToolUseForProviderThread("worker") != "spawn-worker" {
		t.Fatal("original spawn ownership was not recovered")
	}
	sawText := false
	for _, e := range events {
		if e.ItemID == "spawn-helper" && e.ParentToolUseID != "spawn-worker" {
			t.Fatalf("nested launch escaped scope: %+v", e)
		}
		if e.Kind == provider.EventTextDelta {
			sawText = true
			if e.ParentToolUseID != "spawn-helper" {
				t.Fatalf("child text escaped scope: %+v", e)
			}
		}
	}
	if !sawText {
		t.Fatal("quarantined child text was not drained")
	}
}
