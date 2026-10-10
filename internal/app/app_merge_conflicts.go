package app

import (
	"errors"
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

//ao:scope git:operate
func (a *App) GetPRMergeConflicts(ws WorkspaceRef, pr gitops.PRReference, baseRef, headRefName string) (PRMergeConflictsResult, error) {
	if a.shuttingDown.Load() {
		return PRMergeConflictsResult{}, ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return PRMergeConflictsResult{}, err
	}
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return PRMergeConflictsResult{}, errors.New("base branch is required")
	}
	if err := gitops.ValidateBranchName(baseRef); err != nil {
		return PRMergeConflictsResult{}, err
	}
	workspace, err := a.prCloneWorkspace("get PR merge conflicts", ws)
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
	workspace, err := a.prCloneWorkspace("get merge conflict file", ws)
	if err != nil {
		return "", err
	}
	return a.gitCore().ShowTreeFile(workspace, treeOID, path)
}
