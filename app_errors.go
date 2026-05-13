package main

import (
	"log"
	"time"

	"agent-overflow/internal/provider"
)

// emitEvent sends an arbitrary event to the frontend. Prefer this over
// reaching for the transport bus directly so tests can intercept
// emissions via emitEventFn without wiring a real transport bus.
func (a *App) emitEvent(eventName string, data any) {
	if a.emitEventFn != nil {
		a.emitEventFn(eventName, data)
		return
	}
	// Route through a.emit so the transport bus stamps its per-channel
	// seq alongside every other wire emission.
	a.emit(eventName, data)
}

// emitErrorToThread persists a provider error item for the given thread
// via triage (which owns the provider:item_event chokepoint). The
// triage-nil path logs instead of emitting — if the router is not wired
// yet, we have neither a store to persist into nor a typed channel to
// drop the error on, so it degrades to an observability breadcrumb.
func (a *App) emitErrorToThread(threadID, content string) {
	evt := provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   content,
		Timestamp: time.Now(),
	}

	if a.triage != nil {
		if err := a.triage.Handle(evt); err != nil {
			log.Printf("emit error to thread %s: %v", threadID, err)
		}
		return
	}
	log.Printf("emit error to thread %s: triage not wired — dropping error %q", threadID, content)
}
