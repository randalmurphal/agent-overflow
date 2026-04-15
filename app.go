package main

import "context"

// App is the primary Wails-bound struct. Its public methods are callable from the frontend.
// Stub methods generate Wails JS/TS bindings. Implementations come from the ralph loop.
type App struct {
	ctx context.Context
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// --- Stubs for Wails binding generation ---
// These produce the wailsjs/go/main/App.{js,d.ts} files.
// The ralph loop replaces them with real implementations.

func (a *App) CreateThread(provider string, workspacePath string, model string) (map[string]any, error) {
	return nil, nil
}

func (a *App) ListThreads() ([]map[string]any, error) {
	return nil, nil
}

func (a *App) GetThread(id string) (map[string]any, error) {
	return nil, nil
}

func (a *App) DeleteThread(id string) error {
	return nil
}

func (a *App) ArchiveThread(id string) error {
	return nil
}

func (a *App) RenameThread(id string, title string) error {
	return nil
}

func (a *App) ListItems(threadID string) ([]map[string]any, error) {
	return nil, nil
}

func (a *App) ListPayloadMetas(threadID string) ([]map[string]any, error) {
	return nil, nil
}

func (a *App) GetPayloadData(payloadID string) (string, error) {
	return "", nil
}

func (a *App) StartSession(threadID string) error {
	return nil
}

func (a *App) SendMessage(threadID string, content string) error {
	return nil
}

func (a *App) InterruptTurn(threadID string) error {
	return nil
}

func (a *App) StopSession(threadID string) error {
	return nil
}

func (a *App) RespondToApproval(threadID string, requestID string, decision string) error {
	return nil
}

func (a *App) GetSettings() (map[string]any, error) {
	return nil, nil
}
