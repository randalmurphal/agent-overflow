package codex

import (
	"context"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
)

// TestLateSpawnDrainsSameSessionChildFromBothQueues runs a same-session child
// through the connection the way the product does: the root's session id is
// known when the route is bound, so the child's thread/started routes to the
// session by that id and waits in the session's quarantine, while the child's
// later frames carry only its threadId and wait in the connection's. The late
// spawn must release both, with the child's answer under the spawn.
func TestLateSpawnDrainsSameSessionChildFromBothQueues(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	answerChildProfileReads(fx)
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := fx.conn.newSession(context.Background(), "ao-thread", Config{ResumeThreadID: "root-provider-thread"}, func(event provider.ProviderEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}, true, nil, nil)
	fresh := fx.conn.registerSession(s, "root-provider-thread")
	s.setRootSessionID("root-session")
	fx.conn.bindSessionRoot(s, fresh)
	t.Cleanup(func() {
		s.mu.Lock()
		for _, timer := range s.childRouting.deferredChildDeadlines {
			timer.Stop()
		}
		s.mu.Unlock()
	})

	connHeld := func() int {
		fx.conn.routesMu.Lock()
		defer fx.conn.routesMu.Unlock()
		return len(fx.conn.deferredChild["child-a"])
	}
	sessionHeld := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.childRouting.deferredChildWireEvents["child-a"])
	}

	fx.feed([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"root-session","parentThreadId":"root-provider-thread"}}}`))
	fx.feed([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-a","turnId":"child-turn","itemId":"answer","delta":"owned"}}`))
	if got, want := [2]int{sessionHeld(), connHeld()}, [2]int{1, 1}; got != want {
		t.Fatalf("held before the spawn (session, connection) = %v, want %v", got, want)
	}
	mu.Lock()
	early := len(events)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("child frames reached the thread before their spawn: %+v", events)
	}

	fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if got := [2]int{sessionHeld(), connHeld()}; got != [2]int{0, 0} {
		t.Fatalf("held after the spawn (session, connection) = %v, want [0 0]", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, event := range events {
		if event.ItemID == "answer" {
			if event.ParentToolUseID != "spawn-a" {
				t.Fatalf("child answer escaped its spawn: %+v", event)
			}
			return
		}
	}
	t.Fatalf("late spawn did not deliver the child answer held on the connection: %+v", events)
}
