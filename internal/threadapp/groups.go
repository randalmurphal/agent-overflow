package threadapp

import (
	"agent-overflow/internal/store"
)

// Thread groups are pure persistence: a group owns no process, no
// workspace, and no provider session, so these methods are the store's
// surface plus the read-back every mutator owes its caller. Nothing here
// takes a thread lock — the lock registry serializes ACTIONS on a live
// thread (send, restart, fork), and moving a sidebar row between groups
// is neither.

func (s *Service) ListGroups() ([]store.ThreadGroup, error) {
	database, err := s.database("list thread groups")
	if err != nil {
		return nil, err
	}
	return database.ListThreadGroups()
}

func (s *Service) CreateGroup(projectID, name string) (store.ThreadGroup, error) {
	database, err := s.database("create thread group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	return database.CreateThreadGroup(projectID, name)
}

func (s *Service) RenameGroup(groupID, name string) (store.ThreadGroup, error) {
	database, err := s.database("rename thread group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	if err := database.RenameThreadGroup(groupID, name); err != nil {
		return store.ThreadGroup{}, err
	}
	return database.GetThreadGroup(groupID)
}

// DeleteGroup reads the row BEFORE the delete: the deletion event carries
// the group it removed, and after the DELETE there is nothing left to read.
// It also returns the former members as they now stand, ungrouped and
// unpinned, so the caller can announce every row the delete changed.
func (s *Service) DeleteGroup(groupID string) (store.ThreadGroup, []store.Thread, error) {
	database, err := s.database("delete thread group")
	if err != nil {
		return store.ThreadGroup{}, nil, err
	}
	group, err := database.GetThreadGroup(groupID)
	if err != nil {
		return store.ThreadGroup{}, nil, err
	}
	members, err := database.DeleteThreadGroup(groupID)
	if err != nil {
		return store.ThreadGroup{}, nil, err
	}
	return group, members, nil
}

// SetGroup moves threads into groupID, or out of any group when it is "".
// The rows come back from the store's own transaction, so they are exactly
// the rows written — discussion children the caller never named included.
func (s *Service) SetGroup(threadIDs []string, groupID string) ([]store.Thread, error) {
	database, err := s.database("set thread group")
	if err != nil {
		return nil, err
	}
	return database.SetThreadGroup(threadIDs, groupID)
}
