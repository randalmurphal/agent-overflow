package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
)

// newOwnershipHistorySession is a routing session, the only one on its
// connection, whose app-server answers the ownership reads of a root that
// spawned worker, which spawned helper.
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
	conn := newConnWithProcess(proc, cancel, "sh")
	s := newMultiAgentV2RoutingSession(t, onEvent)
	s.conn, s.proc, s.ctx, s.cancel = conn, proc, ctx, cancel
	conn.allSessions[s] = struct{}{}
	conn.byThread[s.rootThreadID()] = s
	conn.loaded[s.rootThreadID()] = struct{}{}
	// These tests isolate identity discovery from the independently tested status reader.
	s.collabHistory.visited["worker"] = 1
	s.collabHistory.visited["helper"] = 1
	go conn.readLoop()
	t.Cleanup(func() {
		cancel()
		if err := proc.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close fake server: %v", err)
		}
		s.collabAsyncWG.Wait()
		conn.routesMu.Lock()
		for _, timer := range conn.deferredChildDeadlines {
			timer.Stop()
		}
		conn.routesMu.Unlock()
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

// A recovery publishes the agent's route on the connection and emits its
// spawn row while it holds the session's dispatch token: a frame of the
// agent that the read loop routes while the row is being emitted queues, and
// the agent's backlog goes out after the whole row, as it does behind a live
// spawn row.
func TestARecoveredAgentsBacklogFollowsItsSpawnRow(t *testing.T) {
	t.Parallel()
	var log eventLog
	var s *Session
	var conn *Conn
	var once sync.Once
	var heldDuringRow bool
	routed := make(chan struct{})
	s, ctx := newOwnershipHistorySession(t, func(e provider.ProviderEvent) {
		if e.ItemID == "spawn-helper" {
			once.Do(func() {
				s.earlyFramesMu.Lock()
				heldDuringRow = s.dispatching
				s.earlyFramesMu.Unlock()
				go func() {
					defer close(routed)
					conn.dispatchNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"second"}`))
				}()
				// The read loop's frame queues behind the token at once. A
				// read loop that took the token instead would wait for this
				// event's emission to end and then deliver the backlog.
				select {
				case <-routed:
				case <-time.After(300 * time.Millisecond):
				}
			})
		}
		log.add(e)
	})
	conn = s.conn
	conn.dispatchNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"helper","turnId":"helper-turn","itemId":"answer","delta":"first"}`))
	if err := s.recoverChildOwnership(ctx, "helper", make(map[string]bool)); err != nil {
		t.Fatal(err)
	}
	<-routed
	if !heldDuringRow {
		t.Fatal("the recovery emitted the spawn row without holding the dispatch token: the read loop could deliver the agent's backlog ahead of it")
	}
	var order []string
	for _, e := range log.snapshot() {
		switch {
		case e.ItemID == "spawn-helper" && e.Kind == provider.EventToolStart:
			order = append(order, "spawn row start")
		case e.ItemID == "spawn-helper" && e.Kind == provider.EventToolComplete:
			order = append(order, "spawn row end")
		case e.Kind == provider.EventTextDelta:
			if e.ParentToolUseID != "spawn-helper" {
				t.Fatalf("the agent's text %+v is not under spawn-helper", e)
			}
			order = append(order, e.Content)
		}
	}
	if want := []string{"spawn row start", "spawn row end", "first", "second"}; !sameOrder(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// A recovery that finds the child's spawn but cannot register it lets the
// dispatch token go and leaves the child unowned, its frames quarantined
// until their deadline as any unowned child's are.
func TestRecoverChildOwnershipThatConflictsLeavesTheFramesQuarantined(t *testing.T) {
	t.Parallel()
	var log eventLog
	s, ctx := newOwnershipHistorySession(t, log.add)
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
	queued := len(s.childRouting.deferredChildWireEvents["helper"])
	s.mu.Unlock()
	if queued != 1 || s.parentToolUseForProviderThread("helper") != "" {
		t.Fatalf("after the conflict: %d frames queued, route %q; want the frame still quarantined and no route", queued, s.parentToolUseForProviderThread("helper"))
	}
	s.dispatchNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"root-turn","itemId":"m1","delta":"root text"}`))
	for _, e := range log.snapshot() {
		if e.Kind == provider.EventTextDelta && e.Content == "root text" {
			return
		}
	}
	t.Fatal("a root frame was not dispatched after the conflict: the recovery kept the dispatch token")
}
