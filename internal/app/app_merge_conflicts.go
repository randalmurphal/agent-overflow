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

// GetPRMergeConflicts merges rev's head with the tip rev.BaseRef has on
// origin now, read by this call and fetched when the clone lacks it, in the referenced clone and reports the
// conflicts. rev.BaseSHA is not merged: it is the base the forge recorded,
// which the head usually contains, so merging with it never conflicts.
//
// userInitiated is true when a person opened the conflict view and false
// for the view's automatic re-reads (a forge poll that changed what the
// merge depends on, a retry after a failed read). Only a user-initiated
// read may ask for git credentials or arm the PR pump's retry after a
// failure; an automatic read fails fast and reports the failure.
//
//ao:scope git:operate
func (a *App) GetPRMergeConflicts(ws WorkspaceRef, pr gitops.PRReference, rev gitops.PRRevision, headRefName string, userInitiated bool) (PRMergeConflictsResult, error) {
	const action = "get PR merge conflicts"
	workspace, err := a.prRevisionWorkspace(action, ws, pr, rev, userInitiated)
	if err != nil {
		return PRMergeConflictsResult{}, err
	}
	remoteBranchTip := a.gitCore().RemoteBranchTipUnattended
	if userInitiated {
		remoteBranchTip = a.gitCore().RemoteBranchTip
	}
	baseTip, err := remoteBranchTip(workspace, rev.BaseRef)
	if err != nil {
		if userInitiated {
			a.notePRReadFailed(pr)
		}
		return PRMergeConflictsResult{}, fmt.Errorf("%s: %w", action, err)
	}
	result, err := a.gitCore().MergeTreeConflicts(workspace, baseTip, rev.HeadSHA)
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
