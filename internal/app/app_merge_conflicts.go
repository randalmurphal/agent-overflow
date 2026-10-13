package app

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
)

type PRMergeConflictsResult struct {
	Conflicted bool     `json:"conflicted"`
	TreeOID    string   `json:"treeOID"`
	BaseLabel  string   `json:"baseLabel"`
	HeadLabel  string   `json:"headLabel"`
	Paths      []string `json:"paths"`
	// Notes: per-path merge-tree messages for conflicts with no
	// renderable content (modify/delete, rename/rename, …).
	Notes map[string][]string `json:"notes"`
	// Messages: leftover messages that mention no conflicted path.
	Messages []string `json:"messages"`
}

// GetPRMergeConflicts merges the PR at rev in the referenced clone and
// reports the conflicts.
//
//ao:scope git:operate
func (a *App) GetPRMergeConflicts(ws WorkspaceRef, pr gitops.PRReference, rev gitops.PRRevision, headRefName string) (PRMergeConflictsResult, error) {
	workspace, err := a.prRevisionWorkspace("get PR merge conflicts", ws, pr, rev)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}
	result, err := a.gitCore().MergeTreeConflicts(workspace, rev.BaseSHA, rev.HeadSHA)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}
	baseLabel := "origin/" + rev.BaseRef
	headLabel := strings.TrimSpace(headRefName)
	if headLabel == "" {
		headLabel = fmt.Sprintf("PR #%d head", pr.Number)
	}
	return PRMergeConflictsResult{
		Conflicted: result.Conflicted,
		TreeOID:    result.TreeOID,
		BaseLabel:  baseLabel,
		HeadLabel:  headLabel,
		Paths:      result.Paths,
		Notes:      result.Notes,
		Messages:   result.Messages,
	}, nil
}

//ao:scope git:operate
func (a *App) GetMergeConflictFile(ws WorkspaceRef, treeOID, path string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	workspace, err := a.prCloneWorkspace("get merge conflict file", ws)
	if err != nil {
		return "", err
	}
	return a.gitCore().ShowTreeFile(workspace, treeOID, path)
}
