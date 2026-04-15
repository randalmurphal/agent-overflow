package orchestration

import (
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/domain"

	"github.com/google/uuid"
)

// Engine is the command/event core. It validates commands against the read model,
// produces events, persists them, and notifies subscribers.
type Engine struct {
	mu        sync.RWMutex
	threads   map[string]*domain.Thread
	events    []domain.Event
	sequence  uint64
	listeners []func(domain.Event)
}

// NewEngine creates an orchestration engine with empty state.
func NewEngine() *Engine {
	return &Engine{
		threads: make(map[string]*domain.Thread),
	}
}

// Subscribe registers a listener that is called for every new event.
func (e *Engine) Subscribe(fn func(domain.Event)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listeners = append(e.listeners, fn)
}

// Dispatch validates a command and, if valid, produces and persists events.
func (e *Engine) Dispatch(cmd domain.Command) ([]domain.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	produced, err := e.decide(cmd)
	if err != nil {
		return nil, fmt.Errorf("dispatch %s: %w", cmd.Kind, err)
	}

	for i := range produced {
		e.sequence++
		produced[i].Sequence = e.sequence
		produced[i].OccurredAt = time.Now()
		if produced[i].EventID == "" {
			produced[i].EventID = uuid.NewString()
		}
		e.apply(produced[i])
		e.events = append(e.events, produced[i])
	}

	for _, listener := range e.listeners {
		for _, evt := range produced {
			listener(evt)
		}
	}

	return produced, nil
}

// Thread returns the read model for a thread, or nil if it doesn't exist.
func (e *Engine) Thread(threadID string) *domain.Thread {
	e.mu.RLock()
	defer e.mu.RUnlock()
	t, ok := e.threads[threadID]
	if !ok {
		return nil
	}
	// Return a copy so callers can't mutate internal state.
	cp := *t
	cp.Messages = make([]domain.Message, len(t.Messages))
	copy(cp.Messages, t.Messages)
	return &cp
}

// Threads returns all threads.
func (e *Engine) Threads() []domain.Thread {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]domain.Thread, 0, len(e.threads))
	for _, t := range e.threads {
		cp := *t
		cp.Messages = make([]domain.Message, len(t.Messages))
		copy(cp.Messages, t.Messages)
		out = append(out, cp)
	}
	return out
}

// EventsFrom returns all events starting from the given sequence number.
func (e *Engine) EventsFrom(seq uint64) []domain.Event {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []domain.Event
	for _, evt := range e.events {
		if evt.Sequence >= seq {
			out = append(out, evt)
		}
	}
	return out
}
