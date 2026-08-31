package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/projectapp"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The `project:updated` broadcast chokepoint.
//
// Every persisted project-row mutation broadcasts the row it changed, so a
// second attached client converges without a refresh — the RPC's return value
// only ever reaches the client that issued it. The rules are
// app_thread_broadcast.go's, unchanged:
//
//   - One emit per changed row. A drag-reorder moves several rows and sends
//     several frames; a reorder that put everything back sends none.
//   - A write that changed nothing emits nothing. The store hands back a
//     changed flag rather than a rows-affected count, because SQLite counts a
//     row as affected when the assignment restates the value it held.
//   - The row broadcast is the row the RPC returns. The initiator may apply
//     its own result optimistically and then receive its own echo; both carry
//     the same post-write row, so the echo converges to the state the
//     optimistic apply already reached instead of flickering through another.
//
// The action vocabulary is triage.ProjectUpdateEvent's, shared with
// thread:updated for the same reason: sidebar membership is not derivable from
// the row, because the list holds only non-archived projects.

// broadcastProjectRow emits one changed row.
func (a *App) broadcastProjectRow(action string, row store.Project) {
	a.emitEvent(eventchan.ProjectUpdated, triage.ProjectUpdateEvent{Action: action, Project: &row})
}

// broadcastProjectWrite is the guard every mutation binding routes through:
// the write's changed flag decides whether anything is said at all.
func (a *App) broadcastProjectWrite(action string, write projectapp.Write) {
	if !write.Changed {
		return
	}
	a.broadcastProjectRow(action, write.Project)
}

// broadcastProjectDeleted announces a row that no longer exists in SQLite. It
// carries the id alone: there is no row left to send, and a client holding one
// drops it.
//
// The threads that went with the project are announced separately, one
// thread:updated "deleted" per row, by the thread teardown DeleteProject runs
// first — so a client learns to close those panes before it learns the project
// is gone, which is the order the deleting client's own sequence produces.
func (a *App) broadcastProjectDeleted(projectID string) {
	a.emitEvent(eventchan.ProjectUpdated, triage.ProjectUpdateEvent{
		Action: triage.ProjectActionDeleted,
		ID:     projectID,
	})
}
