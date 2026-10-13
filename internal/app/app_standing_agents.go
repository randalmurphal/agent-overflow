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
	_, _, err := a.demoteStandingAgent(threadID)
	return err
}

// DemoteStandingAgentAndDeleteHome demotes a standing agent as
// DemoteStandingAgent does and then deletes its home: exactly the directory
// its row recorded, and only when that is `<config>/agents/<thread-id>`. The
// demotion stands when the deletion fails, and the error names the path.
// Nothing is deleted for a thread that is not a standing agent.
//
//ao:scope threads:autonomy
func (a *App) DemoteStandingAgentAndDeleteHome(threadID string) error {
	home, found, err := a.demoteStandingAgent(threadID)
	if !found {
		return err
	}
	deleteErr := a.deleteStandingAgentHome(threadID, home)
	if err == nil {
		return deleteErr
	}
	return errors.Join(err, deleteErr)
}

// demoteStandingAgent removes the row and cancels the watches, returning the
// home the removed row recorded; found is false when there was no row.
func (a *App) demoteStandingAgent(threadID string) (home string, found bool, err error) {
	unlock := a.threadLocks().Lock(threadID)
	row, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		unlock()
		return "", false, err
	}
	if _, err := a.store.DemoteStandingAgent(threadID); err != nil {
		unlock()
		return "", false, err
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
	return row.Home, found, err
}

// deleteStandingAgentHome removes a demoted agent's home. The recorded path
// must be the one promotion creates for this thread; any other path is
// refused rather than deleted. A failure's innermost layer, the one a toast
// shows, says the demotion stood; the layer around it names the folder.
func (a *App) deleteStandingAgentHome(threadID, home string) error {
	var reason error
	agents := filepath.Join(a.configDir, "agents")
	want := filepath.Join(agents, threadID)
	switch {
	case home == "":
		reason = errors.New("its row records no home")
	case a.configDir == "":
		reason = errors.New("no config directory")
	case filepath.Clean(home) != want || filepath.Dir(want) != agents:
		reason = fmt.Errorf("it is not %s", want)
	default:
		reason = os.RemoveAll(want)
	}
	if reason == nil {
		return nil
	}
	return fmt.Errorf("delete the agent's home %q: %w", home,
		fmt.Errorf("the thread was demoted, but its home folder was not deleted: %v", reason))
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

// standingAgentPrompt is the block a standing agent's sessions carry beside
// the app guide (docs/specs/standing-agents.md, "Standing agent"): its home,
// its thread id and that the home is its own. "" for any other thread. It is
// read at every start and resume, so a promotion reaches the next session and
// a demotion leaves it out.
func (a *App) standingAgentPrompt(thread store.Thread) (string, error) {
	row, found, err := a.store.GetStandingAgent(thread.ID)
	if err != nil {
		return "", fmt.Errorf("read the standing agent row of %s: %w", thread.ID, err)
	}
	if !found {
		return "", nil
	}
	return standingAgentPromptText(row), nil
}

func standingAgentPromptText(row store.StandingAgent) string {
	return fmt.Sprintf("You are a standing agent: the user promoted this thread (id %s). Your home is %s. You own that directory and keep your durable state there.",
		row.ThreadID, row.Home)
}

// joinPromptParts joins the non-empty parts of an appended prompt with a
// blank line, the way the model reads paragraphs.
func joinPromptParts(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
}
