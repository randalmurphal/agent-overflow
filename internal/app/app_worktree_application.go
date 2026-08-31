package app

import "agent-overflow/internal/worktreeapp"

func (a *App) worktreeApplication() *worktreeapp.Service {
	a.worktreeAppOnce.Do(func() {
		a.worktreeApp = worktreeapp.New(worktreeapp.Deps{
			Store: a.store,
			Core:  a.gitCore(),
			TransientBusyThreadIDs: func() []string {
				if a.triage == nil {
					return nil
				}
				return a.triage.ThreadIDsWithLiveCodexBackgroundTasks()
			},
			CountBackgroundTasks: a.countRunningBackgroundTasks,
		})
	})
	return a.worktreeApp
}
