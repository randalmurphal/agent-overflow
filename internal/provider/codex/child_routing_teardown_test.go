package codex

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// deferOneChildNotification quarantines a single request-less child
// notification, which is the shape whose expiry writes no JSON-RPC response
// and therefore needs no live process.
func deferOneChildNotification(t *testing.T, s *Session, providerThreadID string) {
	t.Helper()
	if !s.deferChildWireEvent(providerThreadID, deferredChildWireEvent{
		Method: "item/started",
		Params: json.RawMessage(`{"threadId":"` + providerThreadID + `"}`),
	}) {
		t.Fatalf("deferChildWireEvent refused the event")
	}
}

// TestExpireDeferredChildWireEventsRunsWhileOpen is the control for the two
// teardown tests below: with the session open, the deadline drops the queue
// and raises the routing warning.
func TestExpireDeferredChildWireEventsRunsWhileOpen(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) {
		events = append(events, e)
	})
	deferOneChildNotification(t, s, "child-thread")

	s.expireDeferredChildWireEvents("child-thread")

	if len(events) != 1 {
		t.Fatalf("expected one routing warning, got %d", len(events))
	}
	s.mu.Lock()
	remaining := len(s.deferredChildWireEvents["child-thread"])
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("queue not drained: %d events remain", remaining)
	}
}

// TestExpireDeferredChildWireEventsIsInertOnceClosing pins the half of the
// teardown race that the closing check answers: a deadline that reaches the
// take after Close latched `closing` must not consume the queue, because
// consuming it commits to a rejection and a warning it will not deliver.
func TestExpireDeferredChildWireEventsIsInertOnceClosing(t *testing.T) {
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) {
		events = append(events, e)
	})
	deferOneChildNotification(t, s, "child-thread")

	s.closing.Store(true)
	s.expireDeferredChildWireEvents("child-thread")

	if len(events) != 0 {
		t.Fatalf("closing session emitted %d events during expiry", len(events))
	}
	s.mu.Lock()
	remaining := len(s.deferredChildWireEvents["child-thread"])
	s.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("closing expiry consumed the queue: %d events remain, want 1", remaining)
	}
}

// TestDeferredChildDeadlineIsRefusedAfterCloseLatch pins the other half: once
// Close has latched collabAsyncClosing, a timer that fires afterwards finds no
// worker to run on at all, so nothing races the teardown that follows.
func TestDeferredChildDeadlineIsRefusedAfterCloseLatch(t *testing.T) {
	restore := deferredChildOwnershipTimeout
	deferredChildOwnershipTimeout = time.Millisecond
	t.Cleanup(func() { deferredChildOwnershipTimeout = restore })

	emitted := make(chan provider.ProviderEvent, 4)
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) {
		emitted <- e
	})
	s.collabAsyncMu.Lock()
	s.collabAsyncClosing = true
	s.collabAsyncMu.Unlock()

	deferOneChildNotification(t, s, "child-thread")

	select {
	case e := <-emitted:
		t.Fatalf("deadline ran after the close latch: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	s.collabAsyncWG.Wait()
}

// TestDeferredChildDeadlineIsJoinedByCollabAsyncWG pins the reason the work is
// registered rather than merely started: Close's collabAsyncWG.Wait() must not
// return while an already-firing deadline is still emitting.
func TestDeferredChildDeadlineIsJoinedByCollabAsyncWG(t *testing.T) {
	restore := deferredChildOwnershipTimeout
	deferredChildOwnershipTimeout = time.Millisecond
	t.Cleanup(func() { deferredChildOwnershipTimeout = restore })

	emitting := make(chan struct{})
	release := make(chan struct{})
	s := newMultiAgentV2RoutingSession(t, func(provider.ProviderEvent) {
		close(emitting)
		<-release
	})
	deferOneChildNotification(t, s, "child-thread")

	select {
	case <-emitting:
	case <-time.After(5 * time.Second):
		t.Fatal("deadline never fired")
	}

	waited := make(chan struct{})
	go func() {
		s.collabAsyncWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("collabAsyncWG.Wait() returned while the deadline was still emitting")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("collabAsyncWG.Wait() never returned after the deadline finished")
	}
}
