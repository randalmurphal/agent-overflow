package codex

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestUnrelatedThreadDoesNotWarnOrConsumeChildQuarantine(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) { events = append(events, event) })
	s.setRootSessionID("root-session")

	// A notification can beat thread/started. Its ownership stays unknown until
	// the provider identifies the session tree, then the whole queue is released.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"memory-child","turnId":"memory-turn","itemId":"message","delta":"private"}}`))
	if s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("unknown child event was not quarantined")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"memory-child","sessionId":"memory-session","parentThreadId":"memory-root"}}}`))
	if s.childRouting.deferredChildWireCount != 0 || len(s.childRouting.deferredChildWireEvents) != 0 {
		t.Fatal("unrelated thread retained quarantined events")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"memory-child","turn":{"id":"memory-turn","status":"completed"}}}`))
	s.expireDeferredChildWireEvents("memory-child")
	if len(events) != 0 || s.childRouting.warned {
		t.Fatalf("unrelated memory events reached the thread: %+v", events)
	}
}

func TestSameSessionStillNeedsSpawnOwnership(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) { events = append(events, event) })
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"root-session","parentThreadId":"root-provider-thread"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-a","turnId":"child-turn","itemId":"answer","delta":"owned"}}`))
	if len(events) != 0 || s.childRouting.deferredChildWireCount != 2 {
		t.Fatalf("same-session child bypassed ownership: events=%+v queued=%d", events, s.childRouting.deferredChildWireCount)
	}
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if s.parentToolUseForProviderThread("child-a") != "spawn-a" || s.childRouting.deferredChildWireCount != 0 {
		t.Fatal("late spawn did not claim and drain child events")
	}
	answerSeen := false
	for _, event := range events {
		if event.ItemID == "answer" {
			answerSeen = true
			if event.ParentToolUseID != "spawn-a" {
				t.Fatalf("child answer escaped its spawn: %+v", event)
			}
		}
	}
	if !answerSeen {
		t.Fatal("late spawn did not emit the child answer")
	}
}

func TestRouteChildWireEventAfterOwnershipDoesNotRequeue(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, nil)
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register child ownership")
	}
	if route := s.routeChildWireEvent("child-a", deferredChildWireEvent{Method: "turn/completed"}); route != childWireRoutable {
		t.Fatalf("owned child route = %v", route)
	}
	if s.childRouting.deferredChildWireCount != 0 {
		t.Fatal("owned child event was requeued after the spawn drained")
	}
}

func TestMissingSessionIdentityKeepsExistingWarning(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) { events = append(events, event) })
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"unknown-child","parentThreadId":"other-root"}}}`))
	s.expireDeferredChildWireEvents("unknown-child")
	if len(events) != 1 || events[0].Kind != provider.EventNotification {
		t.Fatalf("missing identity suppressed an unresolved-child warning: %+v", events)
	}
}

func TestParentLinkPreservesChildWithConflictingSessionID(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) { events = append(events, event) })
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"legacy-child-session","parentThreadId":"root-provider-thread"}}}`))
	if s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("child with a direct parent link was silently discarded")
	}
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if s.parentToolUseForProviderThread("child-a") != "spawn-a" || s.childRouting.deferredChildWireCount != 0 {
		t.Fatal("direct child did not attach to its late spawn")
	}
}

func TestNestedParentLinkPreservesChildWithConflictingSessionID(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	s.setRootSessionID("root-session")
	if !s.registerChildOwnership("root-provider-thread", "parent-child", "/root/parent", "spawn-parent") {
		t.Fatal("register parent child")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"nested-child","sessionId":"legacy-child-session","parentThreadId":"parent-child"}}}`))
	if s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("nested child with a known parent was silently discarded")
	}
}

func TestLaterOwnedIdentityOverridesUnrelatedClassification(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"other-session","parentThreadId":"other-root"}}}`))
	if !s.isUnrelatedProviderThread("child-a") {
		t.Fatal("initial unrelated identity was not retained")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"root-session","parentThreadId":"root-provider-thread"}}}`))
	if s.isUnrelatedProviderThread("child-a") || s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("corrected identity failed to restore ordinary child routing")
	}
}

func TestClearingRootSessionIdentityClearsUnrelatedClassification(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"other-child","sessionId":"other-session"}}}`))
	if !s.isUnrelatedProviderThread("other-child") {
		t.Fatal("foreign thread was not classified")
	}
	s.setRootSessionID("")
	if s.isUnrelatedProviderThread("other-child") || len(s.childRouting.unrelatedOrder) != 0 {
		t.Fatal("cleared session identity retained foreign-thread cache")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"other-child","turn":{"id":"turn-a"}}}`))
	if s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("unknown identity failed to restore quarantine behavior")
	}
}

func TestTypedSpawnOverridesUnrelatedClassification(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(event provider.ProviderEvent) { events = append(events, event) })
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"other-session","parentThreadId":"other-root"}}}`))
	s.dispatchLine(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if s.isUnrelatedProviderThread("child-a") || s.parentToolUseForProviderThread("child-a") != "spawn-a" {
		t.Fatal("typed spawn did not restore child ownership")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-a","turnId":"child-turn","itemId":"answer","delta":"owned"}}`))
	for _, event := range events {
		if event.ItemID == "answer" {
			if event.ParentToolUseID != "spawn-a" {
				t.Fatalf("answer escaped typed spawn: %+v", event)
			}
			return
		}
	}
	t.Fatal("owned child answer was hidden")
}

func TestUnrelatedThreadRequestsAreRejected(t *testing.T) {
	s, capturePath := newCapturingSession(t, "root-provider-thread")
	s.setRootSessionID("root-session")
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":41,"method":"item/commandExecution/requestApproval","params":{"threadId":"memory-child","turnId":"memory-turn","itemId":"command-a","command":"go test ./..."}}`))
	if s.childRouting.deferredChildWireCount != 1 {
		t.Fatal("request did not wait for child identity")
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"memory-child","sessionId":"memory-session","parentThreadId":"memory-root"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"threadId":"memory-child","turnId":"memory-turn","itemId":"command-b","command":"go test ./..."}}`))
	frames := waitForCapturedRawFrames(t, capturePath, 2, 3*time.Second)
	for i, frame := range frames {
		var response struct {
			ID    int64 `json:"id"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame, &response); err != nil {
			t.Fatal(err)
		}
		if response.ID != int64(i+41) || response.Error.Code != -32000 {
			t.Fatalf("unrelated request response %d = %s", i, frame)
		}
	}
	if s.childRouting.deferredChildWireCount != 0 {
		t.Fatal("unrelated request retained in child quarantine")
	}
}

func TestOversizedUnrelatedThreadIDIsNotRetained(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	s.setRootSessionID("root-session")
	id := strings.Repeat("x", maxDeferredChildThreadIDBytes+1)
	params, err := json.Marshal(map[string]any{"thread": map[string]string{"id": id, "sessionId": "other-session"}})
	if err != nil {
		t.Fatal(err)
	}
	s.dispatchNotification("thread/started", params)
	if len(s.childRouting.unrelatedThreads) != 0 {
		t.Fatal("oversized thread ID retained by unrelated cache")
	}
}

func TestForeignThreadIDsStayBounded(t *testing.T) {
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {})
	s.setRootSessionID("root-session")
	for i := 0; i < maxRememberedUnrelatedThreads+1; i++ {
		params, err := json.Marshal(map[string]any{"thread": map[string]string{"id": "foreign-" + strconv.Itoa(i), "sessionId": "other-session"}})
		if err != nil {
			t.Fatal(err)
		}
		s.dispatchNotification("thread/started", params)
	}
	if len(s.childRouting.unrelatedThreads) != maxRememberedUnrelatedThreads {
		t.Fatalf("unrelated-thread cache size = %d", len(s.childRouting.unrelatedThreads))
	}
	if !s.isUnrelatedProviderThread("foreign-1") || !s.isUnrelatedProviderThread("foreign-256") || s.isUnrelatedProviderThread("foreign-0") {
		t.Fatal("cache did not retain the newest unrelated thread IDs")
	}
	s.dispatchNotification("thread/started", json.RawMessage(`{"thread":{"id":"foreign-1","sessionId":"root-session"}}`))
	if s.isUnrelatedProviderThread("foreign-1") {
		t.Fatal("corrected session identity stayed cached as unrelated")
	}
	s.dispatchNotification("thread/started", json.RawMessage(`{"thread":{"id":"foreign-257","sessionId":"other-session"}}`))
	if !s.isUnrelatedProviderThread("foreign-256") || !s.isUnrelatedProviderThread("foreign-257") {
		t.Fatal("corrected identity left a stale entry that evicted a live one")
	}
}
