package session

import (
	"context"
	"fmt"
	"sync"

	"agent-overflow/internal/provider"
)

// Binding tracks an active provider session for a thread.
type Binding struct {
	ThreadID     string
	ProviderKind provider.Kind
	Model        string
	ResumeCursor string
}

// Manager tracks active provider sessions and routes operations to the correct adapter.
type Manager struct {
	registry *provider.Registry
	mu       sync.RWMutex
	bindings map[string]*Binding // threadID -> binding
}

// NewManager creates a session manager backed by the given provider registry.
func NewManager(registry *provider.Registry) *Manager {
	return &Manager{
		registry: registry,
		bindings: make(map[string]*Binding),
	}
}

// Start initiates a provider session for a thread.
func (m *Manager) Start(ctx context.Context, threadID string, kind provider.Kind, model string) error {
	adapter, err := m.registry.Get(kind)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if _, exists := m.bindings[threadID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("session already active for thread %s", threadID)
	}
	binding := &Binding{
		ThreadID:     threadID,
		ProviderKind: kind,
		Model:        model,
	}
	m.bindings[threadID] = binding
	m.mu.Unlock()

	if err := adapter.StartSession(ctx, threadID, model, ""); err != nil {
		m.mu.Lock()
		delete(m.bindings, threadID)
		m.mu.Unlock()
		return fmt.Errorf("start session for thread %s: %w", threadID, err)
	}

	return nil
}

// SendTurn routes a user turn to the active provider session.
func (m *Manager) SendTurn(ctx context.Context, threadID string, content string, onEvent func(provider.RuntimeEvent)) error {
	adapter, err := m.adapterForThread(threadID)
	if err != nil {
		return err
	}
	return adapter.SendTurn(ctx, threadID, content, onEvent)
}

// Interrupt cancels an in-progress turn.
func (m *Manager) Interrupt(ctx context.Context, threadID string) error {
	adapter, err := m.adapterForThread(threadID)
	if err != nil {
		return err
	}
	return adapter.InterruptTurn(ctx, threadID)
}

// Stop tears down the provider session for a thread.
func (m *Manager) Stop(ctx context.Context, threadID string) error {
	adapter, err := m.adapterForThread(threadID)
	if err != nil {
		return err
	}

	if err := adapter.StopSession(ctx, threadID); err != nil {
		return fmt.Errorf("stop session for thread %s: %w", threadID, err)
	}

	m.mu.Lock()
	delete(m.bindings, threadID)
	m.mu.Unlock()

	return nil
}

// Binding returns the session binding for a thread, or nil if none exists.
func (m *Manager) GetBinding(threadID string) *Binding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bindings[threadID]
}

func (m *Manager) adapterForThread(threadID string) (provider.Adapter, error) {
	m.mu.RLock()
	binding, ok := m.bindings[threadID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no active session for thread %s", threadID)
	}
	return m.registry.Get(binding.ProviderKind)
}
