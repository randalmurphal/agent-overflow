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
	return database.GetOwnedThread(threadID)
}

// Archive, Unarchive, MarkRead, MarkUnread, Pin, SetPinGroup and Unpin all
// return the row the write produced plus whether the write moved anything.
// Root broadcasts the row on `thread:updated` so a second attached client
// converges without a refresh, and the flag is what keeps a write that
// changed nothing silent. The row comes back from the write's own
// transaction, so none of these pays a follow-up GetThread.
func (s *Service) Archive(threadID string) (store.Thread, bool, error) {
	database, err := s.database("archive thread")
	if err != nil {
		return store.Thread{}, false, err
	}
	return database.ArchiveThread(threadID)
}

func (s *Service) Unarchive(threadID string) (store.Thread, bool, error) {
	database, err := s.database("unarchive thread")
	if err != nil {
		return store.Thread{}, false, err
	}
	row, changed, err := database.UnarchiveThread(threadID)
	return rowOrCurrent(database, threadID, row, changed, err)
}

// rowOrCurrent keeps "the Service always hands back the current row" true for
// writes whose no-change path has no row to return. The caller still has to
// render something; the flag alone gates the broadcast. Costs a read only on
// the no-change path, which for these writes is the rare one.
func rowOrCurrent(
	database *store.Store, threadID string, row store.Thread, changed bool, err error,
) (store.Thread, bool, error) {
	if err != nil || changed {
		return row, changed, err
	}
	current, err := database.GetThread(threadID)
	return current, false, err
}

func (s *Service) Rename(threadID, title string) error {
	database, err := s.database("rename thread")
	if err != nil {
		return err
	}
	return database.UpdateTitle(threadID, title)
}

func (s *Service) MarkRead(ctx context.Context, threadID string) (store.Thread, bool, error) {
	database, err := s.database("mark thread read")
	if err != nil {
		return store.Thread{}, false, err
	}
	return database.MarkThreadReadNow(ctx, threadID)
}

func (s *Service) MarkUnread(threadID string) (store.Thread, bool, error) {
	database, err := s.database("mark thread unread")
	if err != nil {
		return store.Thread{}, false, err
	}
	return database.MarkThreadUnread(threadID)
}

func (s *Service) Pin(threadID string) (store.Thread, bool, error) {
	database, err := s.database("pin thread")
	if err != nil {
		return store.Thread{}, false, err
	}
	return database.PinThread(threadID)
}

func (s *Service) SetPinGroup(threadID string, group int) (store.Thread, bool, error) {
	database, err := s.database("set thread pin group")
	if err != nil {
		return store.Thread{}, false, err
	}
	row, changed, err := database.SetThreadPinGroup(threadID, group)
	return rowOrCurrent(database, threadID, row, changed, err)
}

func (s *Service) Unpin(threadID string) (store.Thread, bool, error) {
	database, err := s.database("unpin thread")
	if err != nil {
		return store.Thread{}, false, err
	}
	row, changed, err := database.UnpinThread(threadID)
	return rowOrCurrent(database, threadID, row, changed, err)
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
	spellings, err := storedSpellingsOf(database, workspacePath)
	if err != nil {
		return nil, err
	}
	return database.UpdateBranchForWorkspace(spellings, branch)
}

// storedSpellingsOf answers every spelling of workspacePath a thread row can
// be keyed under: the caller's own, its canonical form, and every stored
// workspace_path that resolves to the same directory. Thread rows keep the
// spelling that was current when they were created, and the store matches
// exactly (a compare-and-swap, never a scan), so the resolution happens
// here, once per distinct stored spelling rather than once per row. Two
// fixed spellings missed a row under a third one: on macOS a thread created
// at `/var/...` sat unre-branched while git named `/private/var/...`.
func storedSpellingsOf(database *store.Store, workspacePath string) ([]string, error) {
	canonical := gitops.CanonicalPath(workspacePath)
	spellings := []string{workspacePath, canonical}
	stored, err := database.ListThreadWorkspacePaths()
	if err != nil {
		return nil, fmt.Errorf("update branch: %w", err)
	}
	for _, path := range stored {
		if path == workspacePath || path == canonical {
			continue
		}
		if gitops.CanonicalPath(path) == canonical {
			spellings = append(spellings, path)
		}
	}
	return spellings, nil
}
