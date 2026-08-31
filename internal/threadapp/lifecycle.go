package threadapp

import (
	"context"
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

func (s *Service) List() ([]store.Thread, error) {
	database, err := s.database("list threads")
	if err != nil {
		return nil, err
	}
	return database.ListThreadsWithItems()
}

func (s *Service) ListArchived() ([]store.Thread, error) {
	database, err := s.database("list archived threads")
	if err != nil {
		return nil, err
	}
	return database.ListArchivedThreads()
}

func (s *Service) Get(threadID string) (store.Thread, error) {
	database, err := s.database("get thread")
	if err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) Archive(threadID string) error {
	database, err := s.database("archive thread")
	if err != nil {
		return err
	}
	return database.ArchiveThread(threadID)
}

func (s *Service) Unarchive(threadID string) (store.Thread, error) {
	database, err := s.database("unarchive thread")
	if err != nil {
		return store.Thread{}, err
	}
	if err := database.UnarchiveThread(threadID); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) Rename(threadID, title string) error {
	database, err := s.database("rename thread")
	if err != nil {
		return err
	}
	return database.UpdateTitle(threadID, title)
}

func (s *Service) MarkRead(ctx context.Context, threadID string) error {
	database, err := s.database("mark thread read")
	if err != nil {
		return err
	}
	return database.MarkThreadReadNow(ctx, threadID)
}

func (s *Service) MarkUnread(threadID string) error {
	database, err := s.database("mark thread unread")
	if err != nil {
		return err
	}
	return database.MarkThreadUnread(threadID)
}

func (s *Service) Pin(threadID string) (store.Thread, error) {
	database, err := s.database("pin thread")
	if err != nil {
		return store.Thread{}, err
	}
	if err := database.PinThread(threadID); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) SetPinGroup(threadID string, group int) (store.Thread, error) {
	database, err := s.database("set thread pin group")
	if err != nil {
		return store.Thread{}, err
	}
	if err := database.SetThreadPinGroup(threadID, group); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) Unpin(threadID string) (store.Thread, error) {
	database, err := s.database("unpin thread")
	if err != nil {
		return store.Thread{}, err
	}
	if err := database.UnpinThread(threadID); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) UpdateBranch(workspacePath, branch string) ([]store.Thread, error) {
	database, err := s.database("update branch")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspacePath) == "" {
		return nil, fmt.Errorf("update branch: workspace path is required")
	}
	if branch != "" {
		if err := gitops.ValidateBranchName(branch); err != nil {
			return nil, fmt.Errorf("update branch: %w", err)
		}
	}
	return database.UpdateBranchForWorkspace(
		workspacePath, gitops.CanonicalPath(workspacePath), branch,
	)
}
