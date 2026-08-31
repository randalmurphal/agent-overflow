package app

import (
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/gitapp"
)

func (a *App) gitApplication() *gitapp.Service {
	a.gitAppOnce.Do(func() {
		a.gitApp = gitapp.New(gitapp.Deps{
			Store: a.store,
			Core:  a.gitCore(),
			Watch: a.gitWatch,
			BackgroundFetchEnabled: func() bool {
				return a.settings != nil && a.settings.Get().BackgroundGitFetch
			},
			IsShuttingDown: a.shuttingDown.Load,
			EmitStatus: func(event gitapp.StatusEvent) {
				a.emit(eventchan.GitStatus, GitStatusEvent(event))
			},
			Logf:              log.Printf,
			ShuttingDownError: ErrShuttingDown,
			InvalidateWorkspace: func(workspace string) {
				if a.workspaceFiles != nil {
					a.workspaceFiles.Invalidate(workspace)
				}
			},
		})
	})
	return a.gitApp
}
