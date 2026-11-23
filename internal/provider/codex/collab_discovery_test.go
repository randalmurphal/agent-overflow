package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
)

// A recovery publishes a child's route off the read loop while the read
// loop keeps routing that child's frames. A frame the read loop gets after
// the route exists, a command's completion, must not overtake the backlog
// that holds the command's start: it waits behind the backlog, and the
// drain delivers both in wire order before the route opens.
func TestRecoveredChildRouteDeliversTheBacklogBeforeLaterFrames(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	start := json.RawMessage(`{"threadId":"child-a","turnId":"child-turn","item":{"id":"cmd-1","type":"commandExecution","source":"unifiedExecStartup","status":"inProgress","processId":"77","command":"sleep 60"}}`)
	complete := json.RawMessage(`{"threadId":"child-a","turnId":"child-turn","item":{"id":"cmd-1","type":"commandExecution","source":"unifiedExecStartup","status":"completed","processId":"77","command":"sleep 60","exitCode":0}}`)
	if s.routeChildWireEvent("child-a", deferredChildWireEvent{Method: "item/started", Params: start}) != childWireDeferred {
		t.Fatal("the unowned child's start was not quarantined")
	}

	s.gateChildBacklog("child-a")
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register recovered ownership")
	}
	if got := s.routeChildWireEvent("child-a", deferredChildWireEvent{Method: "item/completed", Params: complete}); got != childWireDeferred {
		t.Fatalf("a frame routed while the backlog waits = %v, want it queued behind the backlog", got)
	}
	s.drainDeferredChildWireEvents("child-a")

	var order []provider.EventKind
	for _, e := range events {
		if e.ItemID == "cmd-1" && (e.Kind == provider.EventToolStart || e.Kind == provider.EventToolComplete) {
			if e.ParentToolUseID != "spawn-a" || e.ProcessScope != (provider.ProcessScope{ID: "child-a", Addressable: true}) {
				t.Fatalf("drained %s = %+v, want it under spawn-a in child-a's process scope", e.Kind, e)
			}
			order = append(order, e.Kind)
		}
	}
	if len(order) != 2 || order[0] != provider.EventToolStart || order[1] != provider.EventToolComplete {
		t.Fatalf("drained command frames = %v, want the start, then the completion", order)
	}
	if got := s.routeChildWireEvent("child-a", deferredChildWireEvent{Method: "item/completed", Params: complete}); got != childWireRoutable {
		t.Fatalf("route after the drain = %v, want routable", got)
	}
}

// newOwnershipHistorySession is a routing session whose app-server answers
// the ownership reads of a root that spawned worker, which spawned helper.
func newOwnershipHistorySession(t *testing.T, onEvent func(provider.ProviderEvent)) (*Session, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	proc, err := provider.Spawn(ctx, providertest.Lifetime(), provider.SpawnConfig{Binary: "sh", Args: []string{"-c", `
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
	s := newMultiAgentV2RoutingSession(t, onEvent)
	s.proc, s.ctx, s.cancel = proc, ctx, cancel
	s.pending = make(map[int64]chan json.RawMessage)
	// These tests isolate identity discovery from the independently tested status reader.
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
	return s, ctx
}

func TestRecoverChildOwnershipUsesOriginalSpawnAndKeepsNestedScope(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s, ctx := newOwnershipHistorySession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	if s.routeChildWireEvent("helper", deferredChildWireEvent{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"child text"}`)}) != childWireDeferred {
		t.Fatal("helper event was not deferred")
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

// A recovery that finds the child's spawn but cannot register it leaves no
// backlog gate behind: the child stays unowned, its frames quarantined
// until their deadline as any unowned child's are.
func TestRecoverChildOwnershipThatConflictsLeavesNoBacklogGate(t *testing.T) {
	t.Parallel()
	s, ctx := newOwnershipHistorySession(t, func(provider.ProviderEvent) {})
	s.mu.Lock()
	s.collab.agentPathByThread = map[string]string{"helper": "/root/elsewhere"}
	s.mu.Unlock()
	if s.routeChildWireEvent("helper", deferredChildWireEvent{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"child text"}`)}) != childWireDeferred {
		t.Fatal("helper event was not deferred")
	}
	if err := s.recoverChildOwnership(ctx, "helper", make(map[string]bool)); err == nil || !strings.Contains(err.Error(), "conflicting recovered ownership") {
		t.Fatalf("recover = %v, want the conflict", err)
	}
	s.mu.Lock()
	gated, queued := s.childRouting.backlogOpen["helper"], len(s.childRouting.deferredChildWireEvents["helper"])
	s.mu.Unlock()
	if gated || queued != 1 {
		t.Fatalf("after the conflict: gate %v, %d frames queued; want no gate and the frame still quarantined", gated, queued)
	}
}
