package main

import (
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// emitEvent sends an arbitrary event to the frontend. Prefer this over
// reaching for the transport bus directly so tests can intercept
// emissions via emitEventFn without wiring a real transport bus.
// The channel is typed for the same reason a.emit's is; emitEventFn
// stays string-typed because it is a test observation seam, not an emit
// site of its own.
func (a *App) emitEvent(eventName eventchan.Channel, data any) {
	if a.emitEventFn != nil {
		a.emitEventFn(string(eventName), data)
		return
	}
	// Route through a.emit so the transport bus stamps its per-channel
	// seq alongside every other wire emission.
	a.emit(eventName, data)
}

// emitErrorToThread persists a provider error item for the given thread
// via triage (which owns the provider:item_event chokepoint). Routed
// through HandleSynthetic because several callers fire exactly while
// the thread is stopped — reconnect failures most of all — and the
// stopped-thread gate would eat the one error the user needs to see.
// Errors that originate ON a provider read loop must use
// emitWireErrorToThread instead.
func (a *App) emitErrorToThread(threadID, content string) {
	a.routeErrorToThread(threadID, content, true)
}

// emitWireErrorToThread is emitErrorToThread for errors triggered by a
// wire event on the provider read loop (e.g. the discussion-sync
// failure in sessionEventHandler). These must respect the
// stopped-thread gate exactly like the frame that triggered them: a
// torn-down session's read loop keeps draining after CleanupThread
// runs, and bypassing the gate would let its tail persist items under
// the stopped thread (Bug B5 / invariant 29).
func (a *App) emitWireErrorToThread(threadID, content string) {
	a.routeErrorToThread(threadID, content, false)
}

// routeErrorToThread builds the EventError and hands it to triage on
// the route the caller picked. The triage-nil path logs instead of
// emitting — if the router is not wired yet, we have neither a store
// to persist into nor a typed channel to drop the error on, so it
// degrades to an observability breadcrumb.
func (a *App) routeErrorToThread(threadID, content string, synthetic bool) {
	if a.triage == nil {
		log.Printf("emit error to thread %s: triage not wired — dropping error %q", threadID, content)
		return
	}
	evt := provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  threadID,
		Content:   content,
		Timestamp: time.Now(),
	}
	var err error
	if synthetic {
		err = a.triage.HandleSynthetic(evt)
	} else {
		err = a.triage.Handle(evt)
	}
	if err != nil {
		log.Printf("emit error to thread %s: %v", threadID, err)
	}
}
