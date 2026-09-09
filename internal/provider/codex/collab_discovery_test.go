package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestRecoverChildOwnershipUsesOriginalSpawnAndKeepsNestedScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "sh", Args: []string{"-c", `
 while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$line" in
   *'"method":"thread/read"'*'"threadId":"helper"'*) result='{"thread":{"id":"helper","parentThreadId":"worker"}}';;
   *'"method":"thread/read"'*'"threadId":"worker"'*) result='{"thread":{"id":"worker","parentThreadId":"root-provider-thread"}}';;
   *'"method":"thread/items/list"'*'"threadId":"root-provider-thread"'*) result='{"data":[{"turnId":"root-turn","item":{"type":"subAgentActivity","id":"spawn-worker","kind":"started","agentThreadId":"worker","agentPath":"/root/worker"}}]}';;
   *'"method":"thread/items/list"'*'"threadId":"worker"'*) result='{"data":[{"turnId":"worker-turn","item":{"type":"subAgentActivity","id":"later-send","kind":"interacted","agentThreadId":"helper","agentPath":"/root/worker/helper"}},{"turnId":"worker-turn","item":{"type":"subAgentActivity","id":"spawn-helper","kind":"started","agentThreadId":"helper","agentPath":"/root/worker/helper"}}]}';;
   *) result='{}';;
  esac
  printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$result"
 done
 `}})
	if err != nil {
		t.Fatal(err)
	}
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	s.proc, s.ctx, s.cancel = proc, ctx, cancel
	s.pending = make(map[int64]chan json.RawMessage)
	// This test isolates identity discovery from the independently tested status reader.
	s.collabHistory.visited["worker"] = 1
	s.collabHistory.visited["helper"] = 1
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		if err := proc.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close fake server: %v", err)
		}
		s.collabAsyncWG.Wait()
	})
	s.deferChildWireEvent("helper", deferredChildWireEvent{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"child text"}`)})
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
