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
func (s *Service) DeleteGroup(groupID string) (store.ThreadGroup, error) {
	database, err := s.database("delete thread group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	group, err := database.GetThreadGroup(groupID)
	if err != nil {
		return store.ThreadGroup{}, err
	}
	if err := database.DeleteThreadGroup(groupID); err != nil {
		return store.ThreadGroup{}, err
	}
	return group, nil
}

func (s *Service) PinGroup(groupID string) (store.ThreadGroup, error) {
	database, err := s.database("pin thread group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	if err := database.PinThreadGroup(groupID); err != nil {
		return store.ThreadGroup{}, err
	}
	return database.GetThreadGroup(groupID)
}

func (s *Service) UnpinGroup(groupID string) (store.ThreadGroup, error) {
	database, err := s.database("unpin thread group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	if err := database.UnpinThreadGroup(groupID); err != nil {
		return store.ThreadGroup{}, err
	}
	return database.GetThreadGroup(groupID)
}

func (s *Service) SetGroupPinGroup(groupID string, group int) (store.ThreadGroup, error) {
	database, err := s.database("set thread group pin group")
	if err != nil {
		return store.ThreadGroup{}, err
	}
	if err := database.SetThreadGroupPinGroup(groupID, group); err != nil {
		return store.ThreadGroup{}, err
	}
	return database.GetThreadGroup(groupID)
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
