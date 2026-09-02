package app

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
// the referenced checkout. Workspace-keyed because the answer is a property
// of the directory: the composer's @-mention picker asks it from a draft
// placeholder that has no thread row yet.
func (a *App) SearchWorkspaceFiles(ws WorkspaceRef, query string, limit int) (WorkspaceFileSearchResult, error) {
	if a.workspaceFiles == nil {
		return WorkspaceFileSearchResult{}, fmt.Errorf("workspace file searcher not initialized")
	}
	_, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil {
		return WorkspaceFileSearchResult{}, fmt.Errorf("search workspace files: %w", err)
	}

	files, truncated, err := a.workspaceFiles.Search(workspace, query, limit)
	if err != nil {
		return WorkspaceFileSearchResult{}, err
	}
	if files == nil {
		files = []workspacefiles.WorkspaceFile{}
	}
	return WorkspaceFileSearchResult{
		Files:     files,
		Truncated: truncated,
		Root:      workspace,
	}, nil
}
