package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// ThreadGroupUpdateEvent is the wire shape for thread-group:updated.
//
// Three actions, and the payload is the whole row in all of them:
//   - "create" — a group that did not exist,
//   - "patch"  — a rename,
//   - "delete" — the group is gone; the row is the one that was removed,
//     so a client that never saw it can still resolve the id.
//
// A group has no heavy fields, so there is no partial-frame variant to
// maintain: the whole row is cheaper than the rule for merging half of it.
type ThreadGroupUpdateEvent struct {
	Action string            `json:"action"`
	Group  store.ThreadGroup `json:"group"`
}

func (a *App) emitThreadGroup(action string, group store.ThreadGroup) {
	a.emitEvent(eventchan.ThreadGroupUpdated, ThreadGroupUpdateEvent{Action: action, Group: group})
}

// ListThreadGroups returns every sidebar thread group. The frontend loads
// it once at boot beside ListThreads and buckets by project itself.
//
//ao:scope threads:read
//ao:route all
func (a *App) ListThreadGroups() ([]store.ThreadGroup, error) {
	return a.threadApplication().ListGroups()
}

// CreateThreadGroup adds an empty group to a project, or returns the one
// that name already names there: a group name identifies a group inside its
// project, and the agent tools resolve by name, so a second row of the same
// name would be a row nobody can tell from the first. The name is trimmed
// and a blank one is refused, because a nameless row is the one state the
// sidebar cannot render.
//
//ao:scope threads:operate
func (a *App) CreateThreadGroup(projectID string, name string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().CreateGroup(projectID, name)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("create", group)
	return group, nil
}

// The id-keyed group mutators below all route `home`: group ids join
// the client's id-family index (methodFamilies.ts), so a group on another
// machine resolves to that machine, and home is the fallback for an id the
// index has never seen.

// RenameThreadGroup overwrites the display name and returns the refreshed
// row.
//
//ao:scope threads:operate
//ao:route home
func (a *App) RenameThreadGroup(id string, name string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().RenameGroup(id, name)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("patch", group)
	return group, nil
}

// DeleteThreadGroup removes the group and ungroups its members, active and
// archived alike. It never deletes a thread. A member leaving the group
// loses its pin.
//
// Each former member is announced with a thread:updated "full" frame before
// the group's own "delete" frame, so no client holds a row naming a group
// it has already dropped, and the cleared pins reach every client.
//
//ao:scope threads:operate
//ao:route home
func (a *App) DeleteThreadGroup(id string) error {
	group, members, err := a.threadApplication().DeleteGroup(id)
	if err != nil {
		return err
	}
	a.announceMovedThreads(members)
	a.emitThreadGroup("delete", group)
	return nil
}

// SetThreadGroup moves threads into groupID, or out of any group when it
// is empty. It returns every row the call touched and emits one
// thread:updated frame per row, because a move rewrites the group and
// clears the pin of a row that changes group; an owned thread a move
// groups joins the sidebar (announceMovedThreads).
//
// The client routes this from the thread ids (methodFamilies.ts); home is
// the single-backend fallback.
//
//ao:scope threads:operate
//ao:route home
func (a *App) SetThreadGroup(threadIDs []string, groupID string) ([]store.Thread, error) {
	moved, err := a.threadApplication().SetGroup(threadIDs, groupID)
	if err != nil {
		return nil, err
	}
	a.announceMovedThreads(moved)
	return moved, nil
}

// SetThreadPlacement makes threads owned by the thread that spawned them,
// with no sidebar row, or standalone in their project, out of any group
// they were in; a move into a group is SetThreadGroup. Show in Sidebar and
// Remove from Sidebar are its two calls. It returns every row the call
// touched and emits one thread:updated frame per row: `listed` for a row
// the sidebar now lists, `full` for an owned one, so a pane showing the
// thread stays open (docs/specs/standing-agents.md, "Owned workers").
// Owned is refused for a thread nothing on this computer spawned, and any
// placement for an assistant's own thread or one outside every project.
//
//ao:scope threads:operate
//ao:route home
func (a *App) SetThreadPlacement(threadIDs []string, placement string) ([]store.Thread, error) {
	placed, err := a.threadApplication().SetPlacement(threadIDs, placement)
	if err != nil {
		return nil, err
	}
	a.announceMovedThreads(placed)
	return placed, nil
}

// announceMovedThreads announces each row a group or placement move wrote,
// as a row that may have joined or left the sidebar (announcePlaced).
func (a *App) announceMovedThreads(rows []store.Thread) {
	for _, thread := range rows {
		a.announcePlaced(thread.ID)
	}
}
