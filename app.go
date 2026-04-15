package main

import "context"

// App is the primary Wails-bound struct. Its public methods are callable from the frontend.
type App struct {
	ctx context.Context
}

func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}
