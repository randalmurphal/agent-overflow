package triage

import (
	"testing"
)

func TestRegisterAndConsumePendingSend_FIFO(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSend("t1", "user:0", 0)
	router.RegisterPendingSend("t1", "user:1", 1)

	first, ok := router.consumePendingSendHead("t1")
	if !ok {
		t.Fatalf("expected first pending send to consume, got ok=false")
	}
	if first.AOItemID != "user:0" || first.TurnIndex != 0 {
		t.Fatalf("first pop: got %+v, want AOItemID=user:0 TurnIndex=0", first)
	}

	second, ok := router.consumePendingSendHead("t1")
	if !ok {
		t.Fatalf("expected second pending send to consume, got ok=false")
	}
	if second.AOItemID != "user:1" || second.TurnIndex != 1 {
		t.Fatalf("second pop: got %+v, want AOItemID=user:1 TurnIndex=1", second)
	}

	zero, ok := router.consumePendingSendHead("t1")
	if ok {
		t.Fatalf("expected empty FIFO to return ok=false, got entry %+v", zero)
	}
	if zero != (pendingSend{}) {
		t.Fatalf("empty pop should return zero value, got %+v", zero)
	}
}

func TestPendingSendIsolatedPerThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSend("t1", "user:0", 0)

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("t1 should report has-pending after register")
	}
	if router.HasPendingSendForThread("t2") {
		t.Fatalf("t2 should NOT report has-pending — registers are thread-scoped")
	}

	zero, ok := router.consumePendingSendHead("t2")
	if ok {
		t.Fatalf("consume on t2 should return ok=false, got entry %+v", zero)
	}

	// t1's entry must still be intact.
	head, ok := router.consumePendingSendHead("t1")
	if !ok || head.AOItemID != "user:0" {
		t.Fatalf("t1 head after t2 consume attempt: got ok=%v entry=%+v", ok, head)
	}
}

func TestHasPendingSendForThread(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("fresh router should report no pending sends")
	}

	router.RegisterPendingSend("t1", "user:0", 0)

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("expected has-pending after register")
	}

	if _, ok := router.consumePendingSendHead("t1"); !ok {
		t.Fatalf("consume should succeed")
	}

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("expected no pending sends after final consume")
	}
}

func TestClearPendingSendForFailure_RemovesMatchingEntry(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSend("t1", "user:0", 0)
	router.RegisterPendingSend("t1", "user:1", 1)
	router.RegisterPendingSend("t1", "user:2", 2)

	router.ClearPendingSendForFailure("t1", "user:1")

	first, ok := router.consumePendingSendHead("t1")
	if !ok || first.AOItemID != "user:0" {
		t.Fatalf("first pop after clear: ok=%v entry=%+v want user:0", ok, first)
	}

	second, ok := router.consumePendingSendHead("t1")
	if !ok || second.AOItemID != "user:2" {
		t.Fatalf("second pop after clear: ok=%v entry=%+v want user:2", ok, second)
	}

	if _, ok := router.consumePendingSendHead("t1"); ok {
		t.Fatalf("queue should be empty after clearing one + consuming two")
	}
}

func TestClearPendingSendForFailure_NoMatch_NoOp(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSend("t1", "user:0", 0)
	router.RegisterPendingSend("t1", "user:1", 1)

	router.ClearPendingSendForFailure("t1", "user:does-not-exist")

	first, ok := router.consumePendingSendHead("t1")
	if !ok || first.AOItemID != "user:0" {
		t.Fatalf("first pop after no-op clear: ok=%v entry=%+v", ok, first)
	}
	second, ok := router.consumePendingSendHead("t1")
	if !ok || second.AOItemID != "user:1" {
		t.Fatalf("second pop after no-op clear: ok=%v entry=%+v", ok, second)
	}
}

func TestClearPendingSendsForThread_SweepsAll(t *testing.T) {
	router, _, _ := newTestRouter(t)

	router.RegisterPendingSend("t1", "user:0", 0)
	router.RegisterPendingSend("t1", "user:1", 1)
	router.RegisterPendingSend("t1", "user:2", 2)

	router.clearPendingSendsForThread("t1")

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("queue should be empty after sweep")
	}
	if _, ok := router.consumePendingSendHead("t1"); ok {
		t.Fatalf("consume should fail after sweep")
	}
}

func TestMarkWireOnlyUserTextSeen_DedupBehavior(t *testing.T) {
	router, _, _ := newTestRouter(t)

	if !router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("first sighting should return true")
	}
	if router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("second sighting of same id on same thread should return false (dup)")
	}

	if !router.markWireOnlyUserTextSeen("t1", "wire-2") {
		t.Fatalf("different id on same thread should return true (new)")
	}

	if !router.markWireOnlyUserTextSeen("t2", "wire-1") {
		t.Fatalf("same id on different thread should return true (thread-scoped)")
	}
	if router.markWireOnlyUserTextSeen("t2", "wire-1") {
		t.Fatalf("second sighting on t2 should return false (per-thread dedup)")
	}
}

func TestCleanupThread_SweepsPendingSendAndWireOnlySeen(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSend("t1", "user:0", 0)
	router.RegisterPendingSend("t1", "user:1", 1)
	router.markWireOnlyUserTextSeen("t1", "wire-1")
	router.markWireOnlyUserTextSeen("t1", "wire-2")

	if !router.HasPendingSendForThread("t1") {
		t.Fatalf("setup: expected pending sends before cleanup")
	}

	router.CleanupThread("t1")

	if router.HasPendingSendForThread("t1") {
		t.Fatalf("CleanupThread should sweep pending sends")
	}
	if _, ok := router.consumePendingSendHead("t1"); ok {
		t.Fatalf("CleanupThread should leave consume returning ok=false")
	}

	// After cleanup, the wire-only set is gone — a previously-seen id
	// must register as new again.
	if !router.markWireOnlyUserTextSeen("t1", "wire-1") {
		t.Fatalf("CleanupThread should sweep wireOnlyUserTextSeen — first sighting after cleanup should return true")
	}
}
