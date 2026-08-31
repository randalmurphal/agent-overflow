package app

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
)

const globalTurnObserverScope = ""

type turnObserver func(threadID string, evt provider.ProviderEvent)

// subscribeThreadTurnObserver registers observer for provider events from one
// thread. The returned unsubscribe function is idempotent.
func (a *App) subscribeThreadTurnObserver(threadID string, observer turnObserver) func() {
	if threadID == "" {
		panic("turn observer: thread ID cannot be empty")
	}
	return a.subscribeTurnObserver(threadID, observer)
}

// subscribeGlobalTurnObserver registers observer for provider events from all
// threads. The returned unsubscribe function is idempotent.
func (a *App) subscribeGlobalTurnObserver(observer turnObserver) func() {
	return a.subscribeTurnObserver(globalTurnObserverScope, observer)
}

func (a *App) subscribeTurnObserver(scope string, observer turnObserver) func() {
	if observer == nil {
		panic("turn observer: callback cannot be nil")
	}

	a.turnObservers.mu.Lock()
	if a.turnObservers.byThread == nil {
		a.turnObservers.byThread = make(map[string]map[uint64]turnObserver)
	}
	a.turnObservers.nextID++
	id := a.turnObservers.nextID
	bucket := a.turnObservers.byThread[scope]
	if bucket == nil {
		bucket = make(map[uint64]turnObserver)
		a.turnObservers.byThread[scope] = bucket
	}
	bucket[id] = observer
	a.turnObservers.mu.Unlock()

	return func() {
		a.turnObservers.mu.Lock()
		defer a.turnObservers.mu.Unlock()

		bucket := a.turnObservers.byThread[scope]
		delete(bucket, id)
		if len(bucket) == 0 {
			delete(a.turnObservers.byThread, scope)
		}
	}
}

// dispatchTurnObservers snapshots matching callbacks while holding the
// registry lock, then invokes them synchronously after releasing it. A
// callback may therefore subscribe or unsubscribe without deadlocking or
// changing the current snapshot. Separate session read loops can dispatch
// concurrently, so callbacks must still be safe to run concurrently and a
// concurrent dispatch may observe a registry change immediately.
func (a *App) dispatchTurnObservers(threadID string, evt provider.ProviderEvent) {
	a.turnObservers.mu.RLock()
	global := a.turnObservers.byThread[globalTurnObserverScope]
	thread := a.turnObservers.byThread[threadID]
	observerCount := len(global)
	if threadID != globalTurnObserverScope {
		observerCount += len(thread)
	}
	var inlineObservers [4]turnObserver
	observers := inlineObservers[:0]
	if observerCount > len(inlineObservers) {
		observers = make([]turnObserver, 0, observerCount)
	}
	for _, observer := range global {
		observers = append(observers, observer)
	}
	if threadID != globalTurnObserverScope {
		for _, observer := range thread {
			observers = append(observers, observer)
		}
	}
	a.turnObservers.mu.RUnlock()

	for _, observer := range observers {
		observer(threadID, evt)
	}
}

func (a *App) installDiscussionTurnObserver() {
	a.turnObservers.discussionOnce.Do(func() {
		a.subscribeGlobalTurnObserver(func(threadID string, evt provider.ProviderEvent) {
			if evt.Kind != provider.EventTurnComplete {
				return
			}
			if err := a.discussionService().SyncTurn(threadID); err != nil {
				log.Printf("discussion runtime: %v", err)
				// Emit an error event so the UI knows the discussion sync
				// failed. The turn-complete event still propagates (we can't
				// block it), but the error should be visible. Wire variant:
				// this fires on the read loop in response to a wire frame,
				// so it must drop with the rest of a stopped thread's tail.
				a.emitWireErrorToThread(threadID, fmt.Sprintf("discussion sync failed: %v", err))
			}
		})
	})
}
