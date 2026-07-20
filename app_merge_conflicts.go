package main

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

func (a *App) GetPRMergeConflicts(threadID string, pr gitops.PRReference, baseRef, headRefName string) (PRMergeConflictsResult, error) {
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
	workspace, err := a.conflictWorkspace(threadID)
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

func (a *App) GetMergeConflictFile(threadID, treeOID, path string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	workspace, err := a.conflictWorkspace(threadID)
	if err != nil {
		return "", err
	}
	return a.gitCore().ShowTreeFile(workspace, treeOID, path)
}

func (a *App) conflictWorkspace(threadID string) (string, error) {
	workspace, ok := a.localCloneWorkspace(threadID)
	if !ok {
		return "", errors.New("viewing conflicts requires a local clone")
	}
	return workspace, nil
}

// localCloneWorkspace resolves the thread's workspace when it is a real
// local git clone. ok=false means no clone is available (a pr-anchor
// thread with no matching local checkout); callers decide whether that is
// an error (conflict viewer) or a fall-back-to-API signal (PR diff).
func (a *App) localCloneWorkspace(threadID string) (string, bool) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", false
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil || strings.TrimSpace(workspace) == "" {
		return "", false
	}
	if !gitdiff.IsGitRepository(context.Background(), workspace) {
		return "", false
	}
	return workspace, true
}
