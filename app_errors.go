package main

import (
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
)

func appendError(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
}

func wrapLifecycleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// emitEvent sends an arbitrary Wails event to the frontend. Prefer this over
// calling a.app.Event.Emit directly so tests can intercept emissions via
// emitEventFn without wiring a full Wails application.
func (a *App) emitEvent(eventName string, data any) {
	if a.emitEventFn != nil {
		a.emitEventFn(eventName, data)
		return
	}
	// Route through a.emit so every wire emission carries a monotonic
	// seq envelope matching the one applied by the triage router.
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
