package main

import appservice "agent-overflow/internal/app"

// App is the stable Wails service registered as main.App. Its promoted method
// set comes from the importable application shell in internal/app; keeping this
// named root wrapper preserves every existing main.App.<Method> binding ID.
type App struct {
	*appservice.App
}

func NewApp() *App {
	return &App{App: appservice.NewAppWithVersion(version)}
}
