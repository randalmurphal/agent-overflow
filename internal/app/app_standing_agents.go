package app

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// Standing agents (docs/specs/standing-agents.md, "Standing agent"): a thread
// the user promoted. Promotion gives it a home directory under the config
// directory and lets it register watches; demotion takes the row away and
// leaves the home on disk.

// errNotAStandingAgentThread refuses a promotion the thread cannot take.
var errNotAStandingAgentThread = errors.New("this thread cannot be promoted")

// PromoteStandingAgent makes a thread a standing agent: it creates the home
// `<config>/agents/<thread-id>/`, writes the README that names the agent, and
// records the row. The thread and the threads it spawned leave their sidebar
// groups, since the agent's row is where they nest, and a group that leaves
// empty is deleted; every row that moved is broadcast. Promoting a standing
// agent again returns its row unchanged. Refused for an archived thread, a
// thread a workflow run that has not ended is bound to, a thread that is not
// a chat or plan thread, and a thread that is not on this computer.
//
//ao:scope threads:autonomy
func (a *App) PromoteStandingAgent(threadID string) (store.StandingAgent, error) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.StandingAgent{}, fmt.Errorf("%w: the thread is not on this computer", errNotAStandingAgentThread)
	}
	if err := a.store.CheckThreadTransferAccess(threadID); err != nil {
		var moved *store.ThreadTransferError
		if errors.As(err, &moved) {
			return store.StandingAgent{}, fmt.Errorf("%w: the thread is not on this computer", errNotAStandingAgentThread)
		}
		return store.StandingAgent{}, err
	}
	if thread.ForkPreparing {
		return store.StandingAgent{}, fmt.Errorf("%w: the fork is still being prepared", errNotAStandingAgentThread)
	}
	if thread.Archived {
		return store.StandingAgent{}, fmt.Errorf("%w: the thread is archived", errNotAStandingAgentThread)
	}
	if thread.Mode != threadmode.ModeChat && thread.Mode != threadmode.ModePlan {
		return store.StandingAgent{}, fmt.Errorf("%w: only a chat or plan thread can be one", errNotAStandingAgentThread)
	}
	if live, err := a.store.ThreadHasLiveWorkflowRun(threadID); err != nil {
		return store.StandingAgent{}, err
	} else if live {
		return store.StandingAgent{}, fmt.Errorf("%w: a workflow run that has not ended is bound to it", errNotAStandingAgentThread)
	}
	if a.configDir == "" {
		return store.StandingAgent{}, errors.New("promote a standing agent: no config directory")
	}
	home := filepath.Join(a.configDir, "agents", threadID)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return store.StandingAgent{}, fmt.Errorf("create the agent's home: %w", err)
	}
	if err := a.writeStandingAgentReadme(home, thread); err != nil {
		return store.StandingAgent{}, err
	}
	promotion, err := a.store.PromoteStandingAgent(threadID, home, time.Now().UnixMilli())
	if err != nil {
		return store.StandingAgent{}, err
	}
	if promotion.Promoted {
		// The agent's own row carries agentPromotedAt now, and every live
		// thread it spawned carries spawnedByAgent, grouped before or not;
		// each is re-read as it stands after the ungrouping. The group
		// frames go last: a client drops a group only once no row names it.
		a.broadcastThreadRowByID(threadID)
		a.broadcastSpawnedThreadRows(threadID)
		for _, group := range promotion.DeletedGroups {
			a.emitThreadGroup("delete", group)
		}
	}
	return promotion.Agent, nil
}

// GetStandingAgent returns a thread's standing agent row, or nil when the
// thread is not a standing agent.
//
//ao:scope threads:read
func (a *App) GetStandingAgent(threadID string) (*store.StandingAgent, error) {
	row, found, err := a.store.GetStandingAgent(threadID)
	if err != nil || !found {
		return nil, err
	}
	return &row, nil
}

// DemoteStandingAgent removes a thread's standing agent row and cancels its
// watches, which only a standing agent may keep, withdrawing their queued
// wakes. Its loops stay, since any thread may keep one, and its home stays on
// disk. Demoting a thread that is not a standing agent does nothing.
//
//ao:scope threads:operate
func (a *App) DemoteStandingAgent(threadID string) error {
	unlock := a.threadLocks().Lock(threadID)
	if _, err := a.store.DemoteStandingAgent(threadID); err != nil {
		unlock()
		return err
	}
	ids, err := a.store.CancelAgentWakesForThread(threadID, store.AgentWakeWatch,
		"cancelled: the thread is no longer a standing agent")
	for _, id := range ids {
		a.stopAgentWakeRun(id)
	}
	unlock()
	// The row's agentPromotedAt and archivedChildCount are gone with the
	// agent row, and its spawned threads' spawnedByAgent with them; the
	// sidebar renders them all as plain threads again.
	a.broadcastThreadRowByID(threadID)
	a.broadcastSpawnedThreadRows(threadID)
	if len(ids) > 0 {
		a.emitAgentWakesRemoved(threadID, ids)
	}
	if len(ids) > 0 {
		err = errors.Join(err, a.withdrawCancelledAgentWakes(threadID, ids...))
	}
	return err
}

// ListArchivedSpawnedThreads lists the archived threads a standing agent
// spawned on this computer, newest activity first: what the agent row's
// finished count opens.
//
//ao:scope threads:read
//ao:route thread
func (a *App) ListArchivedSpawnedThreads(threadID string) ([]store.Thread, error) {
	return a.store.ListArchivedSpawnedThreads(threadID)
}

// broadcastSpawnedThreadRows re-emits the live threads spawnerID spawned:
// their rows' spawnedByAgent follows the spawner's standing agent row, so
// a promotion, demotion or move of the spawner changes what their menus
// and drags allow. A delete passes the ids itself (broadcastThreadRowsByID),
// since the cascade has already dropped the spawn rows this reads.
// Log-and-continue, as broadcastThreadRowByID.
func (a *App) broadcastSpawnedThreadRows(spawnerID string) {
	ids, err := a.store.ListSpawnedThreadIDs(spawnerID)
	if err != nil {
		log.Printf("broadcast the threads spawned by %s: %v", spawnerID, err)
		return
	}
	a.broadcastThreadRowsByID(ids)
}

func (a *App) broadcastThreadRowsByID(ids []string) {
	for _, id := range ids {
		a.broadcastThreadRowByID(id)
	}
}

// broadcastSpawnerRow re-emits the standing agent that spawned child: the
// agent row's archivedChildCount moves when a spawned thread is archived,
// unarchived or deleted. A spawner that is not a standing agent carries no
// count, so nothing is said for it. Log-and-continue, as
// broadcastThreadRowByID: the child's change is already durable and the
// sidebar converges on the next ListThreads.
func (a *App) broadcastSpawnerRow(child store.Thread) {
	if child.SpawnedByThreadID == "" {
		return
	}
	spawner, err := a.store.GetThread(child.SpawnedByThreadID)
	if err != nil {
		log.Printf("broadcast spawner %s of thread %s: %v", child.SpawnedByThreadID, child.ID, err)
		return
	}
	if spawner.AgentPromotedAt == nil {
		return
	}
	a.broadcastThreadRow(triage.ThreadActionFull, spawner)
}

// SetStandingAgentNotifyOnTick turns a standing agent's tick notifications on
// or off. Off, the default, a turn only its loops and watches started raises
// no notification at rest; on, it notifies like any other turn. Refused for a
// thread that is not a standing agent.
//
//ao:scope threads:operate
func (a *App) SetStandingAgentNotifyOnTick(threadID string, on bool) (store.StandingAgent, error) {
	row, _, found, err := a.store.SetStandingAgentNotifyOnTick(threadID, on)
	if err != nil {
		return store.StandingAgent{}, err
	}
	if !found {
		return store.StandingAgent{}, fmt.Errorf("thread %s is not a standing agent", threadID)
	}
	return row, nil
}

// writeStandingAgentReadme writes the home's README unless one is there: a
// home kept from an earlier promotion is the agent's, README included.
func (a *App) writeStandingAgentReadme(home string, thread store.Thread) error {
	project := thread.ProjectID
	if row, err := a.store.GetProject(thread.ProjectID); err == nil {
		project = fmt.Sprintf("%s (%s)", row.Name, row.Path)
	} else {
		log.Printf("standing agents: read project %s for the README of %s: %v", thread.ProjectID, thread.ID, err)
	}
	file, err := os.OpenFile(filepath.Join(home, "README.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("write the agent's README: %w", err)
	}
	_, writeErr := file.WriteString(standingAgentReadme(thread, project))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write the agent's README: %w", err)
	}
	return nil
}

func standingAgentReadme(thread store.Thread, project string) string {
	title := strings.TrimSpace(thread.Title)
	if title == "" {
		title = "Standing agent"
	}
	return fmt.Sprintf(`# %s

This directory is the home of a standing agent in Agent Overflow. The agent
owns it and keeps its durable state here; no layout is prescribed.

- Agent: %s
- Thread id: %s
- Project: %s

## What it can register

- thread_loop: wake the thread every interval with a note.
- thread_watch: run a script on an interval, or keep it running, and wake the
  thread with what it prints. Watches run with this directory in
  AO_AGENT_HOME and no Agent Overflow credentials. Keep a script's cursor
  here: a restart runs it from scratch.
- thread_remind: wake the thread once, later.

thread_status lists them, and thread_cancel with a label stops one.

A script outside the app can message the agent:

    agent-overflow thread send %s "text"
`, title, title, thread.ID, project, thread.ID)
}
