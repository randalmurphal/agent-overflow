package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The background tray's Stop All is one call per thread. A call per row
// would meet the connection's in-flight bound (transport's
// DefaultMaxConcurrentRPCs) at a hundred agents, and would stop a parked
// agent's shell before the agent, which wakes the agent (claude-wire.md
// §E6b). The call names the launches the person saw, as a confirmed Stop
// does, and each is stopped by its provider's primitive: Claude's
// stop_task per task, Codex's subagent interrupt per agent and one
// terminal cleanup. Every named launch gets one result.

// Background stop outcomes.
const (
	// BackgroundStopStopping: the provider accepted the stop. The row
	// settles when the task's terminal lands.
	BackgroundStopStopping = "stopping"
	// BackgroundStopWithAgent: the task dies with a named agent that owns
	// it, so no stop of its own was sent.
	BackgroundStopWithAgent = "withAgent"
	// BackgroundStopEnded: the task was no longer running.
	BackgroundStopEnded = "ended"
	// BackgroundStopFailed: the stop was refused or failed; Error says why.
	BackgroundStopFailed = "failed"
)

// backgroundStopsInFlight bounds the provider stops one call keeps in
// flight. Each provider answers its control requests in order, so more
// buy no speed.
const backgroundStopsInFlight = 16

// BackgroundTaskStop is the result of stopping one named launch.
type BackgroundTaskStop struct {
	LaunchItemID string `json:"launchItemId"`
	// Outcome is one of the BackgroundStop outcomes.
	Outcome string `json:"outcome"`
	// Error is why a failed stop failed.
	Error string `json:"error,omitempty"`
}

// StopBackgroundTasks stops the named background launches of a thread
// and returns one result per distinct named launch, in the order named.
//
//ao:scope threads:operate
func (a *App) StopBackgroundTasks(threadID string, launchIDs []string) ([]BackgroundTaskStop, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if err := a.store.CheckForkReady(threadID); err != nil {
		return nil, err
	}
	named := distinctLaunchIDs(launchIDs)
	if len(named) == 0 {
		return []BackgroundTaskStop{}, nil
	}
	if a.triage == nil {
		return nil, fmt.Errorf("stop background tasks: thread %s has no event router", threadID)
	}
	kind, _, err := a.store.GetThreadProviderWorkspace(threadID)
	if err != nil {
		return nil, fmt.Errorf("stop background tasks: load thread %s: %w", threadID, err)
	}
	cutoff := time.Now().UnixMilli() - store.BackgroundTaskRetentionMillis
	rows, err := a.store.ListLiveBackgroundTasks(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("stop background tasks: list live background tasks: %w", err)
	}
	switch kind {
	case string(provider.Claude):
		return a.stopClaudeBackgroundTasks(threadID, rows, named)
	case string(provider.Codex):
		return a.stopCodexBackgroundTasks(threadID, rows, named), nil
	}
	return nil, fmt.Errorf("stop background tasks: thread %s has provider %q", threadID, kind)
}

func (a *App) stopClaudeBackgroundTasks(threadID string, rows []store.Item, named []string) ([]BackgroundTaskStop, error) {
	plan, err := planClaudeBackgroundStop(rows, named, func(launch store.Item) (string, error) {
		return a.triage.TranscriptRootID(threadID, launch)
	})
	if err != nil {
		return nil, fmt.Errorf("stop background tasks: %w", err)
	}
	claude := a.claudeAppService()
	runBounded(len(plan.stops), backgroundStopsInFlight, func(j int) {
		stop := plan.stops[j]
		if err := claude.StopTask(threadID, stop.taskID); err != nil {
			plan.results[stop.result] = BackgroundTaskStop{
				LaunchItemID: plan.results[stop.result].LaunchItemID,
				Outcome:      BackgroundStopFailed,
				Error:        err.Error(),
			}
		}
	})
	return plan.results, nil
}

// claudeBackgroundStop is what planClaudeBackgroundStop decided: a result
// per named launch, and the stop_task each result waits on.
type claudeBackgroundStop struct {
	results []BackgroundTaskStop
	stops   []claudeTaskStop
}

type claudeTaskStop struct {
	result int
	taskID string
}

// planClaudeBackgroundStop decides, from the tray's rows, which named
// launches get a stop_task of their own. A named launch that is not a
// live tray row has ended. A background agent is always stopped itself:
// an async agent launched by another is a task of its own and outlives
// its parent's stop (claude-wire.md §E6b, fixture D). Any other task
// (a shell, a watch, a foreground agent) dies with the nearest background
// agent above it when that agent is stopped here (fixture E), so it is
// not sent a stop, which for a parked agent's shell would wake the agent.
// rootOf resolves an agent's transcript root, the id its rows are
// parented to.
func planClaudeBackgroundStop(rows []store.Item, named []string, rootOf func(store.Item) (string, error)) (claudeBackgroundStop, error) {
	byID := make(map[string]store.Item, len(rows))
	settled := make(map[string]bool)
	for _, row := range rows {
		if row.CompletionOf != "" {
			settled[row.CompletionOf] = true
			continue
		}
		byID[row.ID] = row
	}
	live := func(row store.Item) bool {
		if settled[row.ID] || row.Kind != "tool_call" || row.Status != "running" {
			return false
		}
		state := store.AgentRunState(row)
		return state == "" || interruptKillsAgent(state)
	}
	isAgent := func(row store.Item) bool {
		return row.IsBackground && store.IsAgentTranscriptLaunch(row)
	}

	plan := claudeBackgroundStop{results: make([]BackgroundTaskStop, len(named))}
	// The transcript roots of the background agents stopped here.
	stoppedRoots := make(map[string]string)
	for i, id := range named {
		plan.results[i] = BackgroundTaskStop{LaunchItemID: id, Outcome: BackgroundStopEnded}
		row, ok := byID[id]
		if !ok || !live(row) || !isAgent(row) {
			continue
		}
		root, err := rootOf(row)
		if err != nil {
			return claudeBackgroundStop{}, fmt.Errorf("transcript root of %s: %w", id, err)
		}
		stoppedRoots[root] = id
		if root != id {
			stoppedRoots[id] = id
		}
	}
	for i, id := range named {
		row, ok := byID[id]
		if !ok || !live(row) {
			continue
		}
		if !isAgent(row) {
			if owner := stoppedOwner(row, byID, stoppedRoots, isAgent); owner != "" {
				plan.results[i].Outcome = BackgroundStopWithAgent
				continue
			}
		}
		taskID := triage.TaskIDFromItemMeta(row.Meta)
		if taskID == "" {
			plan.results[i] = BackgroundTaskStop{LaunchItemID: id, Outcome: BackgroundStopFailed,
				Error: "the task has no id to stop it by"}
			continue
		}
		plan.results[i].Outcome = BackgroundStopStopping
		plan.stops = append(plan.stops, claudeTaskStop{result: i, taskID: taskID})
	}
	return plan, nil
}

// stoppedOwner walks up from a task to the nearest background agent that
// owns it and returns that agent's launch when it is stopped here. Rows
// between are foreground calls, which die with it; a parent outside the
// tray is resolved only as a stopped agent's root.
func stoppedOwner(row store.Item, byID map[string]store.Item, stoppedRoots map[string]string, isAgent func(store.Item) bool) string {
	parent := strings.TrimSpace(row.ParentID)
	for range len(byID) + 1 {
		if parent == "" {
			return ""
		}
		if owner, ok := stoppedRoots[parent]; ok {
			return owner
		}
		up, ok := byID[parent]
		if !ok || isAgent(up) {
			return ""
		}
		parent = strings.TrimSpace(up.ParentID)
	}
	return ""
}

// stopCodexBackgroundTasks interrupts each named live Codex subagent and,
// when a named launch is a live background terminal, cleans the thread's
// terminals once: Codex stops a terminal only thread-wide
// (thread/backgroundTerminals/clean) or by process id.
func (a *App) stopCodexBackgroundTasks(threadID string, rows []store.Item, named []string) []BackgroundTaskStop {
	now := time.Now().UnixMilli()
	rows = append(rows, a.triage.ListLiveCodexBackgroundTasks(threadID, now, now-store.BackgroundTaskRetentionMillis)...)
	settled := make(map[string]bool)
	for _, row := range rows {
		if row.CompletionOf != "" {
			settled[row.CompletionOf] = true
		}
	}
	terminals := make(map[string]bool)
	for _, row := range rows {
		if row.CompletionOf == "" && row.Kind == "tool_call" && row.IsBackground && row.Status == "running" && !settled[row.ID] {
			terminals[row.ID] = true
		}
	}
	agents := make(map[string]bool)
	for _, agent := range a.triage.ListLiveCodexAgentTasks(threadID) {
		agents[agent.ID] = true
		delete(terminals, agent.ID)
	}

	results := make([]BackgroundTaskStop, len(named))
	var interrupts, cleaned []int
	for i, id := range named {
		results[i] = BackgroundTaskStop{LaunchItemID: id, Outcome: BackgroundStopEnded}
		switch {
		case agents[id]:
			interrupts = append(interrupts, i)
		case terminals[id]:
			cleaned = append(cleaned, i)
		}
	}
	codex := a.codexAppService()
	runBounded(len(interrupts), backgroundStopsInFlight, func(j int) {
		i := interrupts[j]
		stopped, err := codex.StopSubagent(threadID, named[i])
		switch {
		case err != nil:
			results[i] = BackgroundTaskStop{LaunchItemID: named[i], Outcome: BackgroundStopFailed, Error: err.Error()}
		case stopped:
			results[i].Outcome = BackgroundStopStopping
		}
	})
	if len(cleaned) > 0 {
		err := codex.CleanBackgroundTerminals(threadID)
		for _, i := range cleaned {
			results[i].Outcome = BackgroundStopStopping
			if err != nil {
				results[i] = BackgroundTaskStop{LaunchItemID: named[i], Outcome: BackgroundStopFailed, Error: err.Error()}
			}
		}
	}
	return results
}

// runBounded calls fn(0..n-1) with at most limit calls in flight and
// returns when every call has. Each call may write only its own index's
// state.
func runBounded(n, limit int, fn func(int)) {
	sem := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

// distinctLaunchIDs trims, drops blank and dedupes ids, keeping order.
func distinctLaunchIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
