package main

import (
	"context"
	"log"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
)

// sweepLegacyCheckpointRefs deletes the hidden refs/agent-overflow/*
// refs that the removed per-message git-checkpoint machinery wrote
// into user repos, across every workspace the DB knows about (project
// roots, thread workspaces, worktrees). Best-effort background
// maintenance: failures are logged, never surfaced — a repo that no
// longer exists or refuses the delete just stays dirty until the next
// boot retries. Workspaces AO once touched but whose threads/projects
// were deleted can't be enumerated; docs/architecture/schema.md notes
// the manual one-liner for those.
func (a *App) sweepLegacyCheckpointRefs() {
	workspaces := map[string]struct{}{}
	add := func(path string) {
		if path != "" {
			workspaces[gitops.CanonicalPath(path)] = struct{}{}
		}
	}
	projects, err := a.store.ListProjects()
	if err != nil {
		log.Printf("legacy checkpoint refs: list projects: %v", err)
	}
	for _, p := range projects {
		add(p.Path)
	}
	threads, err := a.store.ListThreads()
	if err != nil {
		log.Printf("legacy checkpoint refs: list threads: %v", err)
	}
	for _, t := range threads {
		add(t.WorkspacePath)
		add(t.WorktreePath)
	}

	deleted := 0
	for workspace := range workspaces {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n, err := gitdiff.CleanupLegacyCheckpointRefs(ctx, workspace)
		cancel()
		if err != nil {
			log.Printf("legacy checkpoint refs: sweep %s: %v", workspace, err)
			continue
		}
		deleted += n
	}
	if deleted > 0 {
		log.Printf("legacy checkpoint refs: deleted %d refs across %d workspaces", deleted, len(workspaces))
	}
}
