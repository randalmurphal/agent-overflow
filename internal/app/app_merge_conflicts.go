package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
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

//ao:scope git:operate
func (a *App) GetPRMergeConflicts(ws WorkspaceRef, pr gitops.PRReference, baseRef, headRefName string) (PRMergeConflictsResult, error) {
	if a.shuttingDown.Load() {
		return PRMergeConflictsResult{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return PRMergeConflictsResult{}, err
	}
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return PRMergeConflictsResult{}, errors.New("base branch is required")
	}
	if err := gitops.ValidateBranchName(baseRef); err != nil {
		return PRMergeConflictsResult{}, err
	}
	workspace, err := a.conflictWorkspace(ws)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}

	headRef, err := gitops.PRHeadRef(pr.Forge, pr.Number)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}
	core := a.gitCore()
	headOID, err := core.FetchRefOID(workspace, "origin", headRef)
	if err != nil {
		return PRMergeConflictsResult{}, fmt.Errorf("fetch PR head: %w", err)
	}
	if err := core.FetchBranch(workspace, "origin", baseRef); err != nil {
		return PRMergeConflictsResult{}, fmt.Errorf("fetch base branch: %w", err)
	}
	baseLabel := "origin/" + baseRef
	result, err := core.MergeTreeConflicts(workspace, baseLabel, headOID)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}
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
	workspace, err := a.conflictWorkspace(ws)
	if err != nil {
		return "", err
	}
	return a.gitCore().ShowTreeFile(workspace, treeOID, path)
}

func (a *App) conflictWorkspace(ws WorkspaceRef) (string, error) {
	workspace, ok := a.localCloneWorkspace(ws)
	if !ok {
		return "", errors.New("viewing conflicts requires a local clone")
	}
	return workspace, nil
}

// localCloneWorkspace resolves a workspace ref to a real local git clone.
// ok=false means there is none — a ZERO ref (a pr-anchor thread that never
// had a checkout), a ref that fails validation, or a directory that is not a
// repository. Callers decide whether that is an error (conflict viewer) or a
// fall-back-to-the-forge-API signal (PR diff).
func (a *App) localCloneWorkspace(ws WorkspaceRef) (string, bool) {
	if strings.TrimSpace(ws.ProjectID) == "" {
		return "", false
	}
	_, workspace, err := a.gitApplication().ResolveWorkspace(ws)
	if err != nil || strings.TrimSpace(workspace) == "" {
		return "", false
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", false
	}
	return workspace, true
}
