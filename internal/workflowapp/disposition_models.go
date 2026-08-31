package workflowapp

import (
	"context"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/store"
)

// Git is the consumer-side git surface used by workflow disposition, discard,
// and PR follow-up. It deliberately excludes unrelated repository operations.
type Git interface {
	ListWorktrees(cwd string) ([]gitops.Worktree, error)
	WorkingTreeChanges(cwd string, limit int) ([]string, int, error)
	CountWorkingTreeChanges(cwd string) (int, error)
	RemoveWorktreeForce(cwd, path string, force bool) error
	DeleteBranch(cwd, branch string, force bool) error
	MergeBranch(cwd, base, head string) (gitops.MergeResult, error)
	HeadSHA(cwd string) (string, error)
	PushUnattended(cwd string) error
	CreatePR(cwd, title, body, base string, draft bool) (string, error)
	GetPRDetail(cwd string, ref gitops.PRReference) (gitops.PRDetail, error)
	ListReviewThreads(cwd string, ref gitops.PRReference) ([]gitops.ReviewThread, error)
}

type ListBranchCommitsFunc func(context.Context, string, string, string) ([]gitdiff.Commit, error)

type DispositionProfile struct {
	BaseBranch  string
	Disposition string
}

type TriageThreadInput struct {
	ID, Workspace, Branch, Title, Provider, Model string
	Project                                       store.Project
}

type Digest struct {
	WhatHappened string `json:"whatHappened"`
	WhatItNeeds  string `json:"whatItNeeds"`
}

type DispositionReceipt struct {
	Action        string         `json:"action"`
	Mode          string         `json:"mode,omitempty"`
	SHA           string         `json:"sha,omitempty"`
	PRRef         string         `json:"prRef,omitempty"`
	Base          string         `json:"base,omitempty"`
	CleanupFailed bool           `json:"cleanupFailed,omitempty"`
	Discarded     *DiscardResult `json:"discarded,omitempty"`
	Policy        string         `json:"policy"`
	At            int64          `json:"at"`
}

type DiscardWorktree struct {
	ItemID              string
	UnitID              string
	Path                string
	Branch              string
	Base                string
	Present             bool
	Registered          bool
	DirtyFiles          []string
	DirtyFileCount      int
	UnmergedCommits     []gitdiff.Commit
	UnmergedCommitCount int
	Error               string
}

type DiscardPreview struct {
	ItemID      string
	Members     []string
	LiveMembers []string
	Worktrees   []DiscardWorktree
}

type DiscardResult struct {
	Members          []string `json:"members"`
	Cancelled        []string `json:"cancelled"`
	RemovedWorktrees []string `json:"removedWorktrees"`
	DeletedBranches  []string `json:"deletedBranches"`
}

// TreeLoss is the shared run-tree checkout projection used by run discard and
// project deletion. Exported fields let root's project-deletion adapter reuse
// the exact set without duplicating its ownership rules.
type TreeLoss struct {
	Members []store.WorkItem
	Live    []string
	Targets []WorktreeTarget
}

type WorktreeTarget struct {
	ItemID string
	UnitID string
	Path   string
	Branch string
	Base   string
}

type WorktreeRegistry struct {
	Worktrees []gitops.Worktree
	Present   bool
}

type PRReviewComments struct {
	Count   int
	Threads []gitops.ReviewThread
}

type prCoordinates struct {
	Item    store.WorkItem
	Receipt DispositionReceipt
	Ref     gitops.PRReference
	CWD     string
}
