package main

import (
	"log"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

func (a *App) generatedWorktreeBranchName(thread store.Thread, message string) (string, error) {
	if a.generateBranchNameFn != nil {
		raw, err := a.generateBranchNameFn(thread, message)
		if err != nil {
			return "", err
		}
		return gitops.BuildGeneratedWorktreeBranchNameWithPrefix(raw, a.worktreeBranchPrefix()), nil
	}
	return gitops.BuildGeneratedWorktreeBranchNameWithPrefix(gitops.BranchFragmentFromUserMessage(message), a.worktreeBranchPrefix()), nil
}

func (a *App) maybeRenameTemporaryWorktreeBranch(threadID, message string) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("send message: load thread for branch rename: %v", err)
		return
	}

	if strings.TrimSpace(thread.WorktreePath) == "" ||
		!gitops.IsTemporaryWorktreeBranchWithPrefix(thread.Branch, a.worktreeBranchPrefix()) {
		return
	}

	target, err := a.generatedWorktreeBranchName(thread, message)
	if err != nil {
		log.Printf("send message: generate worktree branch name: %v", err)
		return
	}
	if target == "" || target == thread.Branch {
		return
	}

	cwd := strings.TrimSpace(thread.WorktreePath)
	if cwd == "" {
		cwd = strings.TrimSpace(thread.WorkspacePath)
	}
	renamed, err := a.gitCore().RenameBranch(cwd, thread.Branch, target)
	if err != nil {
		log.Printf("send message: rename worktree branch: %v", err)
		return
	}

	thread.Branch = renamed
	if err := a.store.UpdateThread(thread); err != nil {
		log.Printf("send message: persist renamed worktree branch: %v", err)
	}
}
