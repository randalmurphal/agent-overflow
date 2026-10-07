package triage

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// sendsPendingProbe checks SendsPending after each step against the answer
// the observer would have published. The observer runs under r.mu, so it
// records the answer as of each mark; the App's worker reads after a mark,
// so the last marked answer is what clients end up showing. A mutation
// without a mark leaves that answer stale, including one that flips the
// answer and back inside a single call.
type sendsPendingProbe struct {
	t        *testing.T
	router   *Router
	threadID string
	mu       sync.Mutex
	marked   bool
}

func newSendsPendingProbe(t *testing.T, router *Router, threadID string) *sendsPendingProbe {
	p := &sendsPendingProbe{t: t, router: router, threadID: threadID}
	router.SetSendsPendingObserver(func(id string) {
		if id != threadID {
			return
		}
		answer := router.sendsPendingLocked(id)
		p.mu.Lock()
		p.marked = answer
		p.mu.Unlock()
	})
	return p
}

func (p *sendsPendingProbe) step(label string, want bool) {
	p.t.Helper()
	if got := p.router.SendsPending(p.threadID); got != want {
		p.t.Fatalf("%s: SendsPending = %v, want %v", label, got, want)
	}
	p.mu.Lock()
	marked := p.marked
	p.mu.Unlock()
	if marked != want {
		p.t.Fatalf("%s: last observer mark saw %v, want %v (a mutation went unreported)", label, marked, want)
	}
}

func TestSendsPendingFollowsTheFlushHandoff(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	probe := newSendsPendingProbe(t, router, "t1")

	entered := make(chan struct{})
	releaseDispatch := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDispatch) }) }
	t.Cleanup(release)
	var calls atomic.Int32
	router.SetFlushDispatcher(func(string, []QueuedFlushItem) {
		if calls.Add(1) == 1 {
			close(entered)
			<-releaseDispatch
		}
	})

	probe.step("idle", false)
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "queued"))
	probe.step("queued", true)

	go router.tryFlushQueue("t1")
	<-entered
	probe.step("claimed by the dispatcher", true)

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "echo-1"})
	release()
	deadline := time.Now().Add(2 * time.Second)
	for router.QueuedFlushItemCount("t1") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("dispatcher never released its claim")
		}
		time.Sleep(2 * time.Millisecond)
	}
	probe.step("flushed, awaiting its echo", true)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", TurnIndex: 1, Content: "queued",
		Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	probe.step("echoed", false)
}

func TestSendsPendingCountsOnlyFlushMarkers(t *testing.T) {
	row := func(id string) *store.Item { return &store.Item{ID: id, ThreadID: "t1", Kind: "user_text"} }
	for _, tc := range []struct {
		name    string
		pending pendingSend
		want    bool
	}{
		{"direct send", pendingSend{AOItemID: "user:1", Shape: sendShapeDirect, DeferredItem: row("user:1")}, false},
		{"deferred flush", pendingSend{AOItemID: "f:1", Shape: sendShapeFlush, DeferredItem: row("f:1")}, true},
		{"quiet flush", pendingSend{AOItemID: "f:2", Shape: sendShapeFlush, QuietItem: row("f:2")}, true},
		{"flush with its row on screen", pendingSend{AOItemID: "f:3", Shape: sendShapeFlush}, false},
		{"anchored at an interrupt", pendingSend{AOItemID: "f:4", Shape: sendShapeFlush, QuietItem: row("f:4"), AnchoredAtInterrupt: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			router.mu.Lock()
			state := router.state("t1")
			state.pendingSends = append(state.pendingSends, tc.pending)
			router.mu.Unlock()
			if got := router.SendsPending("t1"); got != tc.want {
				t.Fatalf("SendsPending = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSendsPendingClearsOnEveryExit(t *testing.T) {
	quiet := func(router *Router, st *store.Store) {
		t.Helper()
		now := time.Now().UnixMilli()
		row := store.Item{ID: "user:0:flush:1", ThreadID: "t1", Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", Meta: `{}`, CreatedAt: now, UpdatedAt: now}
		if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", "queue-1", row, 1, now, PendingSendExpectation{ProviderItemID: "echo-1"}); err != nil {
			t.Fatal(err)
		}
	}
	deferred := func(router *Router, _ *store.Store) {
		router.RegisterPendingFlushSendWithExpectation("t1", "queue-1", store.Item{
			ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
			Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
		}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "echo-1"})
	}
	queued := func(router *Router, _ *store.Store) {
		router.RegisterQueueItem("t1", makeQueueItem("queue-1", "queued"))
	}
	for _, tc := range []struct {
		name  string
		setup func(*Router, *store.Store)
		exit  func(*testing.T, *Router, *store.Store)
	}{
		{"queued item removed", queued, func(t *testing.T, r *Router, _ *store.Store) {
			if _, ok := r.RemoveQueuedFlushItem("t1", "queue-1"); !ok {
				t.Fatal("queued item not removed")
			}
		}},
		{"queue drained at session end", queued, func(t *testing.T, r *Router, _ *store.Store) {
			if got := r.DrainUnconfirmedFlushItems("t1"); len(got) != 1 {
				t.Fatalf("drained %d items, want 1", len(got))
			}
		}},
		{"quiet send promoted at an interrupt", quiet, func(t *testing.T, r *Router, _ *store.Store) {
			if len(promoteQuietForTest(r, "t1")) != 1 {
				t.Fatal("promotion failed")
			}
		}},
		{"deferred send persisted at an interrupt", deferred, func(t *testing.T, r *Router, st *store.Store) {
			seedOpenTurn(t, r, st, "t1", 0)
			if len(eagerPersistForTest(r, "t1", 0)) != 1 {
				t.Fatal("eager persist failed")
			}
		}},
		{"send failed", deferred, func(_ *testing.T, r *Router, _ *store.Store) {
			r.ClearPendingSendForFailure("t1", "user:1:flush:1")
		}},
		{"thread cleaned up", deferred, func(_ *testing.T, r *Router, _ *store.Store) {
			r.CleanupThread("t1")
		}},
		{"claimed batch released with no send registered", queued, func(t *testing.T, r *Router, _ *store.Store) {
			if !r.tryFlushQueue("t1") {
				t.Fatal("queue did not dispatch")
			}
		}},
		{"sends cleared from a reverted turn", deferred, func(_ *testing.T, r *Router, _ *store.Store) {
			r.ClearPendingSendsFromTurn("t1", 1)
		}},
		{"send anchored at an interrupt", quiet, func(_ *testing.T, r *Router, _ *store.Store) {
			r.MarkPendingSendAnchoredAtInterrupt("t1", "user:0:flush:1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			router.SetFlushDispatcher(func(string, []QueuedFlushItem) {})
			probe := newSendsPendingProbe(t, router, "t1")
			tc.setup(router, st)
			probe.step("pending", true)
			tc.exit(t, router, st)
			probe.step("exited", false)
		})
	}
}

// A transition that drops the composer marker and then fails puts the
// marker back. The rollback is a change of answer of its own, which the
// observer must hear, or clients keep the intermediate idle.
func TestSendsPendingFollowsFailedTransitionRollbacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *Router, *store.Store)
	}{
		{"eager persist fails", func(t *testing.T, r *Router, st *store.Store) {
			// Malformed meta defeats the persist (json_valid CHECK).
			r.RegisterPendingFlushSendWithExpectation("t1", "queue-1", store.Item{
				ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
				Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", Meta: "{not-json",
			}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "echo-1"})
			seedOpenTurn(t, r, st, "t1", 0)
			if len(eagerPersistForTest(r, "t1", 0)) != 0 {
				t.Fatal("eager persist of an unpersistable row succeeded")
			}
		}},
		{"quiet promotion fails", func(t *testing.T, r *Router, _ *store.Store) {
			now := time.Now().UnixMilli()
			// Registered without its row, so the bump finds nothing.
			r.RegisterPendingQuietFlushSendWithExpectation("t1", "queue-1", store.Item{
				ID: "user:0:flush:1", ThreadID: "t1", Kind: "user_text", Role: "user",
				Status: "completed", Summary: "never persisted", CreatedAt: now, UpdatedAt: now,
			}, 1, now, PendingSendExpectation{ProviderItemID: "echo-1"})
			if len(promoteQuietForTest(r, "t1")) != 0 {
				t.Fatal("promotion of a missing row succeeded")
			}
		}},
		{"popped send reinserted", func(t *testing.T, r *Router, _ *store.Store) {
			r.RegisterPendingFlushSendWithExpectation("t1", "queue-1", store.Item{
				ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Summary: "queued",
			}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "echo-1"})
			r.mu.Lock()
			popped := r.popPendingSendAtLocked("t1", 0)
			r.mu.Unlock()
			if r.SendsPending("t1") {
				t.Fatal("popped send still pending")
			}
			r.reinsertPendingSendHead("t1", popped)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			probe := newSendsPendingProbe(t, router, "t1")
			tc.run(t, router, st)
			probe.step("rolled back", true)
		})
	}
}
