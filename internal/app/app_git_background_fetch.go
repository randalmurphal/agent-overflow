package app

import "context"

// startBackgroundGitFetch launches the app-owned unattended fetch cadence.
// The service joins it before SQLite closes during Shutdown.
func (a *App) startBackgroundGitFetch() {
	a.gitApplication().StartBackgroundFetch(a.lifeCtx(), a.backgroundFetchDisabled)
}

// stopBackgroundGitFetch cancels an in-flight fetch before joining the cadence.
func (a *App) stopBackgroundGitFetch() {
	a.gitApplication().StopBackgroundFetch()
}

// runBackgroundFetchPass performs one pass for focused App integration tests.
func (a *App) runBackgroundFetchPass(ctx context.Context) {
	a.gitApplication().RunBackgroundFetchPass(ctx)
}
