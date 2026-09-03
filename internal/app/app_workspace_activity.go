package app

// WorkspaceActivity reports the work that makes a workspace's checkout unsafe
// to delete right now, or a thread unsafe to move out of it, aggregated over EVERY thread that references
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
	// BusyThreads breaks the counters down per thread. The frontend gates two
	// different actions off one fetch: the DIRECTORY question (is anything
	// running here? — remove worktree) reads the
	// counters; the THREAD question (is this thread running? — moving the
	// thread to another checkout) looks itself up here. Moving an idle
	// thread out of a directory a sibling is working in touches only the
	// idle thread's row, so the directory answer must not gate it. Sorted by
	// id, so the answer is stable across calls.
	BusyThreads []BusyThread `json:"busyThreads"`
}

// BusyThread is one thread's contribution to WorkspaceActivity. At least one
// leg is set; idle threads are not listed.
type BusyThread struct {
	ThreadID               string `json:"threadId"`
	ActiveTurn             bool   `json:"activeTurn"`
	RunningBackgroundTasks int    `json:"runningBackgroundTasks"`
}

// GetWorkspaceActivity answers "is anything running in this directory, and
// which threads are they?" for the frontend's workspace-change lock. The
// counters gate the directory-destructive affordance (remove worktree);
// BusyThreads lets the same fetch gate the thread-scoped ones (moving a
// thread to another checkout) on that thread alone, matching the backend's own ensureThreadChangeAllowed(threadID).
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
//
//ao:scope git:operate
//ao:route selected
func (a *App) GetWorkspaceActivity(workspacePath string) (WorkspaceActivity, error) {
	activity, err := a.worktreeApplication().Activity(workspacePath)
	if err != nil {
		return WorkspaceActivity{}, err
	}
	busy := make([]BusyThread, len(activity.BusyThreads))
	for index := range activity.BusyThreads {
		busy[index] = BusyThread(activity.BusyThreads[index])
	}
	return WorkspaceActivity{
		ActiveTurnThreads:      activity.ActiveTurnThreads,
		RunningBackgroundTasks: activity.RunningBackgroundTasks,
		BusyThreads:            busy,
	}, nil
}
