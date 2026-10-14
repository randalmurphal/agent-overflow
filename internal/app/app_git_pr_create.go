package app

import (
	"context"
	"log"
	"time"

	gitops "agent-overflow/internal/git"
)

// prLookupAfterCreateTimeout bounds the forge lookup an agent's PR create
// command prompts.
const prLookupAfterCreateTimeout = time.Minute

// observeCommandSettled re-asks the forge for the thread's open PR when an
// agent's command ran `gh pr create` or `glab mr create`. The push before
// it already cached "no PR", and creating the PR touches nothing on disk,
// so without this the badge waits for that cache to expire and some later
// local change. Runs on the provider read loop: the match is a string
// check, and the rest leaves the loop.
func (a *App) observeCommandSettled(threadID, command string) {
	if !gitops.CreatesPullRequest(command) {
		return
	}
	go a.refreshThreadPR(threadID)
}

// refreshThreadPR schedules a fresh open-PR lookup for the thread's
// workspace. A thread whose workspace cannot be resolved has no PR badge
// to update; that is logged, as nothing else would report it.
func (a *App) refreshThreadPR(threadID string) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("pr lookup after create: thread %s: %v", threadID, err)
		return
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		log.Printf("pr lookup after create: thread %s workspace: %v", threadID, err)
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), prLookupAfterCreateTimeout)
	defer cancel()
	a.gitApplication().RefreshPR(ctx, workspace)
}
