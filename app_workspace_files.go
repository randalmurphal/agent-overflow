package main

import (
	"fmt"

	"agent-overflow/internal/workspacefiles"
)

// WorkspaceFileSearchResult is the RPC shape for workspace-file search hits.
type WorkspaceFileSearchResult struct {
	Files     []workspacefiles.WorkspaceFile `json:"files"`
	Truncated bool                           `json:"truncated"`
	Root      string                         `json:"root"`
}

// SearchWorkspaceFiles returns workspace files matching the query, scoped to
// the workspace of the given thread.
func (a *App) SearchWorkspaceFiles(threadID, query string, limit int) (WorkspaceFileSearchResult, error) {
	if a.workspaceFiles == nil {
		return WorkspaceFileSearchResult{}, fmt.Errorf("workspace file searcher not initialized")
	}
	if a.store == nil {
		return WorkspaceFileSearchResult{}, fmt.Errorf("store not initialized")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return WorkspaceFileSearchResult{}, fmt.Errorf("search workspace files: %w", err)
	}
	if thread.WorkspacePath == "" {
		return WorkspaceFileSearchResult{}, fmt.Errorf("thread %s has no workspace path", threadID)
	}

	files, truncated, err := a.workspaceFiles.Search(thread.WorkspacePath, query, limit)
	if err != nil {
		return WorkspaceFileSearchResult{}, err
	}
	if files == nil {
		files = []workspacefiles.WorkspaceFile{}
	}
	return WorkspaceFileSearchResult{
		Files:     files,
		Truncated: truncated,
		Root:      thread.WorkspacePath,
	}, nil
}
