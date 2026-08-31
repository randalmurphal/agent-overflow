package app

import (
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/triage"
)

func TestTurnObserversDispatchByScope(t *testing.T) {
	app := &App{}
	var received []string

	app.subscribeGlobalTurnObserver(func(threadID string, evt provider.ProviderEvent) {
		received = append(received, "global:"+threadID+":"+evt.Content)
	})
	app.subscribeThreadTurnObserver("thread-a", func(threadID string, evt provider.ProviderEvent) {
		received = append(received, "thread-a:"+threadID+":"+evt.Content)
	})
	app.subscribeThreadTurnObserver("thread-b", func(threadID string, evt provider.ProviderEvent) {
		received = append(received, "thread-b:"+threadID+":"+evt.Content)
	})

	app.dispatchTurnObservers("thread-a", provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "first"})
	app.dispatchTurnObservers("thread-c", provider.ProviderEvent{Kind: provider.EventThinking, Content: "second"})

	want := []string{
		"global:thread-a:first",
		"thread-a:thread-a:first",
		"global:thread-c:second",
	}
	if !slices.Equal(received, want) {
		t.Fatalf("received = %v, want %v", received, want)
	}
}

func TestTurnObserverUnsubscribeIsIdempotentAndCleansBucket(t *testing.T) {
	app := &App{}
	var calls int
	unsubscribe := app.subscribeThreadTurnObserver("thread-a", func(string, provider.ProviderEvent) {
		calls++
	})

	unsubscribe()
	unsubscribe()
	app.dispatchTurnObservers("thread-a", provider.ProviderEvent{Kind: provider.EventTextDelta})

	if calls != 0 {
		t.Fatalf("calls after unsubscribe = %d, want 0", calls)
	}
	app.turnObservers.mu.Lock()
	defer app.turnObservers.mu.Unlock()
	if len(app.turnObservers.byThread) != 0 {
		t.Fatalf("turn observer buckets after unsubscribe = %d, want 0", len(app.turnObservers.byThread))
	}
}

func TestTurnObserverCanUnsubscribeAndRegisterDuringCallback(t *testing.T) {
	app := &App{}
	var selfCalls, addedCalls int
	var unsubscribe func()
	unsubscribe = app.subscribeThreadTurnObserver("thread-a", func(string, provider.ProviderEvent) {
		selfCalls++
		unsubscribe()
		app.subscribeThreadTurnObserver("thread-a", func(string, provider.ProviderEvent) {
			addedCalls++
		})
	})

	app.dispatchTurnObservers("thread-a", provider.ProviderEvent{Kind: provider.EventTextDelta})
	if selfCalls != 1 || addedCalls != 0 {
		t.Fatalf("calls after first dispatch = self %d, added %d; want 1, 0", selfCalls, addedCalls)
	}

	app.dispatchTurnObservers("thread-a", provider.ProviderEvent{Kind: provider.EventTextDelta})
	if selfCalls != 1 || addedCalls != 1 {
		t.Fatalf("calls after second dispatch = self %d, added %d; want 1, 1", selfCalls, addedCalls)
	}
}

func TestTurnObserversDispatchAfterTriage(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-observer-order")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var order []string
	app.triage.SetEventHook(func(provider.ProviderEvent) {
		order = append(order, "triage")
	})
	app.subscribeThreadTurnObserver(thread.ID, func(string, provider.ProviderEvent) {
		order = append(order, "observer")
	})

	handler := app.sessionEventHandler(thread.ID, "session-token", string(provider.Codex))
	handler(provider.ProviderEvent{
		Kind:     provider.EventModelFallback,
		ThreadID: thread.ID,
		ItemID:   "model-fallback:observer-order",
		Meta:     json.RawMessage(`{"fallbackModel":"gpt-5.4-mini"}`),
	})

	if want := []string{"triage", "observer"}; !slices.Equal(order, want) {
		t.Fatalf("dispatch order = %v, want %v", order, want)
	}
}

func TestDiscussionTurnObserverPreservesWireErrorMessage(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-discussion-observer-error")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	emitted := collectErrorItemUpserts(t, app, 4)

	closedStore, err := store.New(storetest.ClonePath(t))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := closedStore.Close(); err != nil {
		t.Fatalf("close failure store: %v", err)
	}
	_, lookupErr := closedStore.GetThread(thread.ID)
	if lookupErr == nil {
		t.Fatal("closed store GetThread error = nil")
	}
	app.store = closedStore

	handler := app.sessionEventHandler(thread.ID, "session-token", "")
	handler(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     thread.ID,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})

	want := "discussion sync failed: " + lookupErr.Error()
	for len(emitted) > 0 {
		item := <-emitted
		if item.ThreadID == thread.ID && item.Summary == want {
			return
		}
	}
	t.Fatalf("discussion observer did not emit exact wire error %q", want)
}

func TestTurnObserversConcurrentRegisterDispatchAndUnsubscribe(t *testing.T) {
	app := &App{}
	const (
		workers    = 64
		iterations = 100
	)

	start := make(chan struct{})
	var calls atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wg.Done()
			<-start
			threadID := "thread-a"
			if worker%2 == 1 {
				threadID = "thread-b"
			}
			for range iterations {
				unsubscribe := app.subscribeThreadTurnObserver(threadID, func(string, provider.ProviderEvent) {
					calls.Add(1)
				})
				app.dispatchTurnObservers(threadID, provider.ProviderEvent{Kind: provider.EventTextDelta})
				unsubscribe()
				unsubscribe()
			}
		}()
	}
	close(start)
	wg.Wait()

	if calls.Load() == 0 {
		t.Fatal("concurrent dispatch invoked no observers")
	}
	app.turnObservers.mu.Lock()
	defer app.turnObservers.mu.Unlock()
	if len(app.turnObservers.byThread) != 0 {
		t.Fatalf("turn observer buckets after concurrent unsubscribe = %d, want 0", len(app.turnObservers.byThread))
	}
}
