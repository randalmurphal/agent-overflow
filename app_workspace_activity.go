package main

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
)

// WorkspaceActivity reports the work that makes a workspace's checkout unsafe
// to move or delete right now, aggregated over EVERY thread that references
// the directory rather than only the one asking.
//
// The entity is the DIRECTORY, not the conversation. Two threads sharing a
// worktree is first-class (project-root threads default to it, and
// implement-plan-in-a-new-thread deliberately shares its source worktree), so
// "am I busy?" is the wrong question to gate a `rm -rf` on — the sibling's
// agent is writing into the same files.
//
// Both counters are thread counts / task counts, never booleans: the frontend
// renders "background tasks are running" and a caller that wanted to say how
// many would otherwise need a second call.
type WorkspaceActivity struct {
	// ActiveTurnThreads is the number of threads in this workspace with an
	// open turn — an agent mid-response, writing into the checkout.
	ActiveTurnThreads int `json:"activeTurnThreads"`
	// RunningBackgroundTasks is the number of live background tasks summed
	// over those threads: persisted Claude/Codex background launches, live
	// Codex subagent launches, and transient Codex unified-exec terminals.
	RunningBackgroundTasks int `json:"runningBackgroundTasks"`
}

// GetWorkspaceActivity answers "is anything running in this directory?" for
// the frontend's workspace-change lock, which gates the destructive workspace
// affordances (remove worktree, env / branch moves).
//
// It is deliberately the same computation the removal gate performs while
// holding the thread locks (removeProjectWorktree →
// threadActivityBlockReason): the same thread-set resolution
// (workspaceRefMatches, symlink-canonical, both path columns) and the same
// per-thread activity reads. An affordance derived from a second, similar
// predicate is an affordance that eventually disagrees with the refusal, and
// the direction it disagrees in is a live button over a running agent.
//
// The candidate set is narrowed by "which threads are busy" BEFORE any path
// work: the busy set is a handful of rows even on a large history, so this
// canonicalizes a few paths per call rather than two per thread ever created.
// That ordering is what makes the call cheap enough to re-run on every
// background-task event.
func (a *App) GetWorkspaceActivity(workspacePath string) (WorkspaceActivity, error) {
	path := strings.TrimSpace(workspacePath)
	if path == "" {
		return WorkspaceActivity{}, fmt.Errorf("workspace activity: workspace path is required")
	}
	refs, err := a.busyThreadWorkspaceRefs()
	if err != nil {
		return WorkspaceActivity{}, fmt.Errorf("workspace activity: list busy threads: %w", err)
	}
	canon := gitops.CanonicalPath(path)

	var activity WorkspaceActivity
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, dup := seen[ref.ID]; dup {
			continue
		}
		if !workspaceRefMatches(ref, canon) {
			continue
		}
		seen[ref.ID] = struct{}{}

		if _, open, err := a.store.GetActiveTurn(ref.ID); err != nil {
			return WorkspaceActivity{}, fmt.Errorf("workspace activity: check active turn for %s: %w", ref.ID, err)
		} else if open {
			activity.ActiveTurnThreads++
		}
		count, err := a.countRunningBackgroundTasks(ref.ID)
		if err != nil {
			return WorkspaceActivity{}, fmt.Errorf("workspace activity: count background tasks for %s: %w", ref.ID, err)
		}
		activity.RunningBackgroundTasks += count
	}
	return activity, nil
}
