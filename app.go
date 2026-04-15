package main

import (
	"context"

	"agent-overflow/internal/domain"
	"agent-overflow/internal/orchestration"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/session"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the primary Wails-bound struct. Its public methods are callable from the frontend.
type App struct {
	ctx      context.Context
	engine   *orchestration.Engine
	sessions *session.Manager
}

func NewApp() *App {
	registry := provider.NewRegistry()
	return &App{
		engine:   orchestration.NewEngine(),
		sessions: session.NewManager(registry),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Push domain events to the frontend via Wails event system.
	a.engine.Subscribe(func(evt domain.Event) {
		runtime.EventsEmit(a.ctx, "domain:event", evt)
	})
}

// --- Bindings exposed to the frontend ---

// CreateThread creates a new conversation thread and returns its ID.
func (a *App) CreateThread(title string) (string, error) {
	threadID := uuid.NewString()
	_, err := a.engine.Dispatch(domain.Command{
		CommandID: uuid.NewString(),
		Kind:      domain.CmdCreateThread,
		ThreadID:  threadID,
		Payload:   map[string]any{"title": title},
	})
	if err != nil {
		return "", err
	}
	return threadID, nil
}

// SendMessage sends a user message to a thread.
func (a *App) SendMessage(threadID string, content string) error {
	_, err := a.engine.Dispatch(domain.Command{
		CommandID: uuid.NewString(),
		Kind:      domain.CmdSendMessage,
		ThreadID:  threadID,
		Payload: map[string]any{
			"role":    string(domain.RoleUser),
			"content": content,
		},
	})
	return err
}

// GetThread returns the current state of a thread.
func (a *App) GetThread(threadID string) *domain.Thread {
	return a.engine.Thread(threadID)
}

// ListThreads returns all threads.
func (a *App) ListThreads() []domain.Thread {
	return a.engine.Threads()
}

// DeleteThread removes a thread.
func (a *App) DeleteThread(threadID string) error {
	_, err := a.engine.Dispatch(domain.Command{
		CommandID: uuid.NewString(),
		Kind:      domain.CmdDeleteThread,
		ThreadID:  threadID,
	})
	return err
}
