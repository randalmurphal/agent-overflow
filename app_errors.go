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

func (a *App) emitProviderEvent(evt provider.ProviderEvent) {
	if a.emitProviderEventFn != nil {
		a.emitProviderEventFn(evt)
		return
	}
	if a.app != nil {
		a.app.Event.Emit("provider:event", evt)
	}
}

// emitEvent sends an arbitrary Wails event to the frontend. Prefer this over
// calling a.app.Event.Emit directly so tests can intercept emissions via
// emitEventFn without wiring a full Wails application.
func (a *App) emitEvent(eventName string, data any) {
	if a.emitEventFn != nil {
		a.emitEventFn(eventName, data)
		return
	}
	if a.app != nil {
		a.app.Event.Emit(eventName, data)
	}
}

// emitErrorToThread sends a provider error event for the given thread through
// the triage layer if available, falling back to direct Wails event emission.
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
	a.emitProviderEvent(evt)
}
