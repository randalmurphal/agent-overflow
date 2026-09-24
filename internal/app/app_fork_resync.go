package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

// watchForkStamps has the store report the pointer forks whose stamps a
// committed write to another thread moved (store.OnForkStampsMoved), and
// pushes each one a resync frame: a source's hand-off, span, touch or
// deletion changed what the fork shows or how it is stamped, and no row
// frame of the fork's own says so. A client showing the fork re-syncs its
// window (docs/architecture/thread-replica-sync.md#pointer-fork-stamps).
func (a *App) watchForkStamps() {
	a.store.OnForkStampsMoved(a.emitForkResyncs)
}

func (a *App) emitForkResyncs(forkIDs []string) {
	for _, id := range forkIDs {
		a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamResync(id))
	}
}
