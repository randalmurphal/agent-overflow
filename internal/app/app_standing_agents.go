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
)

// Standing agents (docs/specs/standing-agents.md, "Standing agent"): a thread
// the user promoted. Promotion gives it a home directory under the config
// directory and lets it register watches; demotion takes the row away and
// leaves the home on disk.

// errNotAStandingAgentThread refuses a promotion the thread cannot take.
var errNotAStandingAgentThread = errors.New("this thread cannot be promoted")

// PromoteStandingAgent makes a thread a standing agent: it creates the home
// `<config>/agents/<thread-id>/`, writes the README that names the agent, and
// records the row. Promoting a standing agent again returns its row
// unchanged. Refused for an archived thread, a thread a workflow run that has
// not ended is bound to, and a thread that is not on this computer.
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
	row, _, err := a.store.PromoteStandingAgent(threadID, home, time.Now().UnixMilli())
	return row, err
}

// DemoteStandingAgent removes a thread's standing agent row and cancels its
// watches, which only a standing agent may keep. Its loops stay, since any
// thread may keep one, and its home stays on disk. Demoting a thread that is
// not a standing agent does nothing.
//
//ao:scope threads:operate
func (a *App) DemoteStandingAgent(threadID string) error {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if _, err := a.store.DemoteStandingAgent(threadID); err != nil {
		return err
	}
	ids, err := a.store.CancelAgentWakesForThread(threadID, store.AgentWakeWatch,
		"cancelled: the thread is no longer a standing agent")
	for _, id := range ids {
		a.stopAgentWakeRun(id)
	}
	return err
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
