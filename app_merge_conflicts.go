package main

import (
	"context"
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
	Messages   []string `json:"messages"`
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
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("view merge conflicts: %w", err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil || strings.TrimSpace(workspace) == "" {
		return "", errors.New("viewing conflicts requires a local clone")
	}
	if !a.checkpointStore().IsGitRepository(context.Background(), workspace) {
		return "", errors.New("viewing conflicts requires a local clone")
	}
	return workspace, nil
}
