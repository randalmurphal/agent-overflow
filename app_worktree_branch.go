package main

import (
	"log"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

func (a *App) generatedWorktreeBranchName(thread store.Thread, message string) (string, error) {
	if a.generateBranchNameFn != nil {
		raw, err := a.generateBranchNameFn(thread, message)
		if err != nil {
			return "", err
		}
		return gitops.BuildGeneratedWorktreeBranchName(raw), nil
	}
	return gitops.BuildGeneratedWorktreeBranchName(branchNameFromUserMessage(message)), nil
}

func (a *App) maybeRenameTemporaryWorktreeBranch(threadID, message string) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("send message: load thread for branch rename: %v", err)
		return
	}

	if strings.TrimSpace(thread.WorktreePath) == "" || !gitops.IsTemporaryWorktreeBranch(thread.Branch) {
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
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		log.Printf("send message: persist renamed worktree branch: %v", err)
	}
}

func branchNameFromUserMessage(content string) string {
	title := strings.TrimSpace(content)
	if title == "" {
		return "update"
	}
	if line, _, ok := strings.Cut(title, "\n"); ok {
		title = line
	}
	title = firstSentenceFromMessage(title)
	title = strings.Join(strings.Fields(title), " ")
	title = strings.TrimSpace(title)
	if title == "" {
		return "update"
	}
	return title
}

func firstSentenceFromMessage(content string) string {
	for i, r := range content {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		return strings.TrimSpace(content[:i+1])
	}
	return content
}
