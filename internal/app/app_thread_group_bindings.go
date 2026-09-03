package app

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// ThreadGroupUpdateEvent is the wire shape for thread-group:updated.
//
// Three actions, and the payload is the whole row in all of them:
//   - "create" — a group that did not exist,
//   - "patch"  — a rename or a pin move,
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

// emitThreadReplaced pushes a whole thread row on thread:updated. The
// frontend's applyThreadUpdated syncs any non-patch action that carries a
// thread, so a mutation that changes several fields at once (a group move
// strips the pin as it sets the group) lands atomically instead of as a
// patch per field.
func (a *App) emitThreadReplaced(thread store.Thread) {
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
		Action: "full", ID: thread.ID, Thread: &thread,
	})
}

// ListThreadGroups returns every sidebar thread group. The frontend loads
// it once at boot beside ListThreads and buckets by project itself.
func (a *App) ListThreadGroups() ([]store.ThreadGroup, error) {
	return a.threadApplication().ListGroups()
}

// CreateThreadGroup adds an empty group to a project. The name is trimmed
// and a blank one is refused — a nameless row is the one state the sidebar
// cannot render.
func (a *App) CreateThreadGroup(projectID string, name string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().CreateGroup(projectID, name)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("create", group)
	return group, nil
}

// RenameThreadGroup overwrites the display name and returns the refreshed
// row.
func (a *App) RenameThreadGroup(id string, name string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().RenameGroup(id, name)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("patch", group)
	return group, nil
}

// DeleteThreadGroup removes the group and ungroups its members, active and
// archived alike. It never deletes a thread.
//
// The members' own rows are not re-emitted: a client that drops the group
// on this frame has nowhere left to render a member under, and the next
// ListThreads (or the row's own next update) carries the cleared groupId.
func (a *App) DeleteThreadGroup(id string) error {
	group, err := a.threadApplication().DeleteGroup(id)
	if err != nil {
		return err
	}
	a.emitThreadGroup("delete", group)
	return nil
}

// PinThreadGroup places the group on the front burner. A pinned group sits
// in the pin block and never consumes a preview slot.
func (a *App) PinThreadGroup(id string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().PinGroup(id)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("patch", group)
	return group, nil
}

// UnpinThreadGroup clears the group's pin fields.
func (a *App) UnpinThreadGroup(id string) (store.ThreadGroup, error) {
	group, err := a.threadApplication().UnpinGroup(id)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("patch", group)
	return group, nil
}

// SetThreadGroupPinGroup moves an already-pinned group between the front
// and back burners.
func (a *App) SetThreadGroupPinGroup(id string, group int) (store.ThreadGroup, error) {
	updated, err := a.threadApplication().SetGroupPinGroup(id, group)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	a.emitThreadGroup("patch", updated)
	return updated, nil
}

// SetThreadGroup moves threads into groupID, or out of any group when it
// is empty. It returns every row the call touched — the discussion
// children that travelled with a named root included — and emits one
// thread:updated "full" frame per row, because a move rewrites the
// group and strips the pin together.
func (a *App) SetThreadGroup(threadIDs []string, groupID string) ([]store.Thread, error) {
	moved, err := a.threadApplication().SetGroup(threadIDs, groupID)
	if err != nil {
		return nil, err
	}
	for _, thread := range moved {
		a.emitThreadReplaced(thread)
	}
	return moved, nil
}
