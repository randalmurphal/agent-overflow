package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// Assistants (docs/specs/standing-agents.md, "Standing agent"): a standing
// agent is a record with its own id, a home under the config directory and
// one current thread. The user creates one with CreateAssistant, an
// assistant creates one with thread_new_assistant, and New Thread, Same
// Assistant (StartNextAssistantThread) gives it a new current thread.

// Wake kinds of the app-written messages that start an assistant's threads.
const (
	assistantWakeKindOpening = "assistant"
	assistantWakeKindHandOff = "handoff"
)

// assistantSendIDRoot names the app-written messages that open an
// assistant's thread, `assistant:<kind>:<id>`. They are the assistant's
// orientation: one its session never took in goes back on its queue, held
// (requeueAssistantMessage).
const assistantSendIDRoot = "assistant:"

func assistantSendID(kind, id string) string { return assistantSendIDRoot + kind + ":" + id }

// assistantSendIDKind reads the wake kind back out of an assistant
// message's send id.
func assistantSendIDKind(sendID string) string {
	rest, _ := strings.CutPrefix(sendID, assistantSendIDRoot)
	kind, _, _ := strings.Cut(rest, ":")
	return kind
}

// errNotAssistantThread refuses an assistant action on a thread that is no
// assistant's current thread here.
var errNotAssistantThread = errors.New("this thread is not an assistant's current thread on this computer")

// CreateAssistantOptions is New Assistant's dialog: the name and the
// project and workspace choice a new thread offers.
type CreateAssistantOptions struct {
	ProjectID         string `json:"projectId"`
	Name              string `json:"name"`
	WorktreeBranch    string `json:"worktreeBranch,omitempty"`
	WorktreeBase      string `json:"worktreeBase,omitempty"`
	WorktreeBaseLocal bool   `json:"worktreeBaseLocal,omitempty"`
	WorktreePath      string `json:"worktreePath,omitempty"`
	Branch            string `json:"branch,omitempty"`
}

// CreateAssistant creates an assistant: its thread, titled with its name,
// its record and its home, and queues the opening message that starts the
// discussion of its role. A name another assistant here holds is refused
// naming that assistant, before anything is created.
//
//ao:scope threads:autonomy
//ao:route selected
func (a *App) CreateAssistant(ctx context.Context, opts CreateAssistantOptions) (store.Thread, error) {
	thread, agent, err := a.createAssistant(ctx, assistantSpec{
		name: opts.Name,
		thread: CreateThreadOptions{
			ProjectID:         opts.ProjectID,
			WorktreeBranch:    opts.WorktreeBranch,
			WorktreeBase:      opts.WorktreeBase,
			WorktreeBaseLocal: opts.WorktreeBaseLocal,
			WorktreePath:      opts.WorktreePath,
			Branch:            opts.Branch,
		},
	})
	if err != nil {
		return store.Thread{}, err
	}
	body := agenthome.OpeningMessage(thread.Title, agent.Home)
	if err := a.queueAssistantMessage(thread.ID, assistantSendID(assistantWakeKindOpening, agent.ID), thread.Title, body); err != nil {
		return thread, fmt.Errorf("the assistant was created, but its opening message could not be queued: %w", err)
	}
	return thread, nil
}

// assistantSpec is what an assistant is created from.
type assistantSpec struct {
	name   string
	thread CreateThreadOptions
	// creatorAgentID names the assistant that created this one.
	creatorAgentID string
}

// createAssistant makes the thread, the record and the home, and
// broadcasts the row once it is an assistant's. A clash found when the
// record is written deletes the thread again.
func (a *App) createAssistant(ctx context.Context, spec assistantSpec) (store.Thread, store.StandingAgent, error) {
	name := strings.TrimSpace(spec.name)
	if err := a.store.CheckAssistantName(name); err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	if a.configDir == "" {
		return store.Thread{}, store.StandingAgent{}, errors.New("create an assistant: no config directory")
	}
	opts := spec.thread
	opts.Title, opts.Mode, opts.GroupID = name, string(threadmode.ModeChat), ""
	thread, err := a.createThreadRow(ctx, opts, false)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	agent := store.StandingAgent{
		ID:             entityid.New(),
		ThreadID:       thread.ID,
		CreatorAgentID: spec.creatorAgentID,
		CreatedAt:      time.Now().UnixMilli(),
	}
	agent.Home = filepath.Join(a.configDir, "agents", agent.ID)
	undo := func(cause error) (store.Thread, store.StandingAgent, error) {
		if err := a.DeleteThread(thread.ID); err != nil {
			log.Printf("assistants: delete the thread %s of an assistant that was not created: %v", thread.ID, err)
		}
		return store.Thread{}, store.StandingAgent{}, cause
	}
	if err := os.MkdirAll(agent.Home, 0o700); err != nil {
		return undo(fmt.Errorf("create the assistant's home: %w", err))
	}
	if err := a.writeAssistantReadme(agent, thread); err != nil {
		return undo(err)
	}
	if err := a.store.CreateStandingAgent(agent); err != nil {
		if removeErr := os.RemoveAll(agent.Home); removeErr != nil {
			log.Printf("assistants: remove the home %s of an assistant that was not created: %v", agent.Home, removeErr)
		}
		return undo(err)
	}
	created, err := a.store.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, fmt.Errorf("read the new assistant's thread: %w", err)
	}
	a.broadcastThreadRow(triage.ThreadActionListed, created)
	return created, agent, nil
}

// queueAssistantMessage queues an app-written message that opens an
// assistant's thread and starts its session.
func (a *App) queueAssistantMessage(threadID, sendID, name, body string) error {
	kind := assistantSendIDKind(sendID)
	return a.queueAgentNotice(agentNotice{
		threadID:   threadID,
		sendID:     sendID,
		origin:     usermessage.OriginAgentWake,
		originWake: &usermessage.OriginWake{Kind: kind, Label: name},
		prepare:    func() (string, bool, error) { return body, true, nil },
		startFailed: func(err error) string {
			return "This assistant's opening message is queued, but its agent could not start: " + err.Error()
		},
	})
}

// requeueAssistantMessage puts an opening message its session never took in
// back on its thread's queue, held: the next session start sends it. A
// thread that is archived or gone takes none.
func (a *App) requeueAssistantMessage(threadID, sendID, body string) {
	kind := assistantSendIDKind(sendID)
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("assistants: requeue %s on %s: %v", sendID, threadID, err)
		return
	}
	if _, err := a.registerQueueItem(threadID, body, SendMessageOptions{SendID: sendID}, injectedQueueOptions{
		preserveDraft:     true,
		held:              true,
		heldWhileArchived: true,
		origin:            usermessage.OriginAgentWake,
		originWake:        &usermessage.OriginWake{Kind: kind, Label: thread.Title},
	}); err != nil && !errors.Is(err, errThreadArchived) {
		log.Printf("assistants: requeue %s on %s: %v", sendID, threadID, err)
	}
}

// GetStandingAgent returns the assistant whose current thread this is, or
// nil.
//
//ao:scope threads:read
func (a *App) GetStandingAgent(threadID string) (*store.StandingAgent, error) {
	row, found, err := a.store.GetStandingAgent(threadID)
	if err != nil || !found {
		return nil, err
	}
	return &row, nil
}

// assistantHomeForDelete is the thread delete's AssistantHome: deleting an
// assistant's current thread deletes the assistant, and its home goes with
// it. Only the directory creation makes, `<config>/agents/<agent-id>` (a
// migrated record's id is its thread's, so its home is that too), is
// removed; a record naming any other path refuses the delete, naming the
// folder, and nothing is deleted.
func (a *App) assistantHomeForDelete(threadID string) (func() error, error) {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		return nil, fmt.Errorf("read the assistant of %s: %w", threadID, err)
	}
	if !found {
		return nil, nil
	}
	agents := filepath.Join(a.configDir, "agents")
	want := filepath.Join(agents, agent.ID)
	var reason string
	switch {
	case agent.Home == "":
		reason = "its record names no home folder"
	case a.configDir == "":
		reason = "the app has no config directory"
	case filepath.Clean(agent.Home) != want || filepath.Dir(want) != agents:
		reason = fmt.Sprintf("it is not this assistant's folder %s", want)
	}
	if reason != "" {
		return nil, fmt.Errorf("this assistant was not deleted: its home %q cannot be removed with it, because %s", agent.Home, reason)
	}
	return func() error {
		if err := os.RemoveAll(want); err != nil {
			at := want
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) {
				at, err = pathErr.Path, pathErr.Err
			}
			return fmt.Errorf("remove the assistant's home %q (stopped at %s): %w", want, at, err)
		}
		return nil
	}, nil
}

// ListArchivedSpawnedThreads lists the archived threads an assistant's
// current thread spawned on this computer and the assistant's archived past
// threads, newest activity first: what the agent row's done footer opens.
//
//ao:scope threads:read
//ao:route thread
func (a *App) ListArchivedSpawnedThreads(threadID string) ([]store.Thread, error) {
	return a.store.ListArchivedSpawnedThreads(threadID)
}

// broadcastSpawnedThreadRows re-emits the live threads spawnerID spawned:
// their rows' spawnedByAgent follows whether the spawner is an assistant's
// current thread, so creation, a handoff or a move of it changes what their
// menus and drags allow. A delete passes the ids itself
// (broadcastThreadRowsByID), since the cascade has already dropped the
// spawn rows this reads. Log-and-continue, as broadcastThreadRowByID.
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

// broadcastSpawnerRow re-emits the assistant that spawned child, or whose
// past thread child is: the agent row's archivedChildCount moves when such
// a thread is archived, unarchived or deleted. Any other spawner carries no
// count, so nothing is said for it. Log-and-continue, as
// broadcastThreadRowByID: the child's change is already durable and the
// sidebar converges on the next ListThreads.
func (a *App) broadcastSpawnerRow(child store.Thread) {
	parentID := child.SpawnedByThreadID
	if child.PastAgentID != "" {
		agent, found, err := a.store.GetAgent(child.PastAgentID)
		if err != nil {
			log.Printf("broadcast the assistant of past thread %s: %v", child.ID, err)
			return
		}
		if found {
			parentID = agent.ThreadID
		}
	}
	if parentID == "" {
		return
	}
	parent, err := a.store.GetThread(parentID)
	if err != nil {
		log.Printf("broadcast spawner %s of thread %s: %v", parentID, child.ID, err)
		return
	}
	if parent.AgentID == "" {
		return
	}
	a.broadcastThreadRow(triage.ThreadActionFull, parent)
}

// SetStandingAgentNotifyOnTick turns an assistant's tick notifications on or
// off. Off, the default, a turn only its loops and watches started raises no
// notification at rest; on, it notifies like any other turn. Refused for a
// thread that is no assistant's current thread.
//
//ao:scope threads:operate
func (a *App) SetStandingAgentNotifyOnTick(threadID string, on bool) (store.StandingAgent, error) {
	row, _, found, err := a.store.SetStandingAgentNotifyOnTick(threadID, on)
	if err != nil {
		return store.StandingAgent{}, err
	}
	if !found {
		return store.StandingAgent{}, errNotAssistantThread
	}
	return row, nil
}

// assistantCurrentThread maps a thread a handoff superseded to its
// assistant's current thread; ok is false for every other thread. A failed
// read is logged and reads as not superseded, so the message goes where it
// was addressed, as before handoffs existed.
func (a *App) assistantCurrentThread(threadID string) (string, bool) {
	current, found, err := a.store.AssistantCurrentThreadOf(threadID)
	if err != nil {
		log.Printf("assistants: %v", err)
		return "", false
	}
	return current, found
}

// writeAssistantReadme writes the home's README unless one is there.
func (a *App) writeAssistantReadme(agent store.StandingAgent, thread store.Thread) error {
	project := thread.ProjectID
	if row, err := a.store.GetProject(thread.ProjectID); err == nil {
		project = fmt.Sprintf("%s (%s)", row.Name, row.Path)
	} else {
		log.Printf("assistants: read project %s for the README of %s: %v", thread.ProjectID, agent.ID, err)
	}
	file, err := os.OpenFile(filepath.Join(agent.Home, "README.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("write the assistant's README: %w", err)
	}
	_, writeErr := file.WriteString(assistantReadme(agent, thread, project))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write the assistant's README: %w", err)
	}
	return nil
}

func assistantReadme(agent store.StandingAgent, thread store.Thread, project string) string {
	return fmt.Sprintf(`# %s

This directory is the home of an assistant (a standing agent) in Agent
Overflow. The assistant owns it and keeps its durable state here; no layout
is prescribed. Its current thread changes when the user starts a new
thread for it; this home stays.

- Assistant: %s
- Assistant id: %s
- First thread: %s
- Project: %s

## What it can register

- thread_loop: wake the thread every interval with a note.
- thread_watch: run a script on an interval, or keep it running, and wake the
  thread with what it prints. Watches run with this directory in
  AO_AGENT_HOME and no Agent Overflow credentials. Keep a script's cursor
  here: a restart runs it from scratch.
- thread_remind: wake the thread once, later.

thread_status lists them, and thread_cancel with a label stops one.

A script outside the app can message the assistant's thread:

    agent-overflow thread send <thread-id> "text"
`, thread.Title, thread.Title, agent.ID, thread.ID, project)
}

// standingAgentPrompt is the block an assistant's sessions carry beside the
// app guide (docs/specs/standing-agents.md, "Agent home contents"): who it
// is, read from the record (name, id, current thread, home, the creator it
// reports to), the skill sentence, then the text of the home's
// instructions.md and MEMORY.md under their headings
// (agenthome.ReadSessionBlock). "" for any other thread, a past one
// included. It is read at every start and resume and never while a session
// runs, so a rename or a rewrite of either file reaches the next session and
// a session's prompt is fixed for its life. Problems are the file reads that
// failed for a reason other than absence; the start proceeds without that
// file and the caller surfaces them.
func (a *App) standingAgentPrompt(thread store.Thread) (block string, problems []error, err error) {
	row, found, err := a.store.GetStandingAgent(thread.ID)
	if err != nil {
		return "", nil, fmt.Errorf("read the assistant of %s: %w", thread.ID, err)
	}
	if !found {
		return "", nil, nil
	}
	identity := agenthome.Identity{Name: thread.Title, AgentID: row.ID, ThreadID: thread.ID, Home: row.Home}
	if row.CreatorAgentID != "" {
		identity.CreatorName, identity.CreatorThreadID = a.assistantName(row.CreatorAgentID)
	}
	sentence := agenthome.IdentitySentence(identity)
	if row.Home == "" {
		return sentence, nil, nil
	}
	home := agenthome.ReadSessionBlock(row.Home)
	return joinPromptParts(sentence, home.Text), home.Problems, nil
}

// assistantName reads an assistant's name and current thread, empty when it
// is gone or unreadable.
func (a *App) assistantName(agentID string) (name, threadID string) {
	agent, found, err := a.store.GetAgent(agentID)
	if err != nil {
		log.Printf("assistants: read assistant %s: %v", agentID, err)
	}
	if !found {
		return "", ""
	}
	thread, err := a.store.GetThread(agent.ThreadID)
	if err != nil {
		log.Printf("assistants: read the thread of assistant %s: %v", agentID, err)
		return "", agent.ThreadID
	}
	return thread.Title, agent.ThreadID
}

// reportStandingAgentHomeProblems logs each failed read of a standing agent's
// home files with its path and puts it on the thread as a warning row, after
// the start succeeds or fails: the session runs, or would have run, without
// that file, and the person watching the agent has to know.
func (a *App) reportStandingAgentHomeProblems(threadID string, problems []error) {
	for _, problem := range problems {
		log.Printf("standing agent %s: %v", threadID, problem)
		if err := a.emitNoticeToThread(threadID, noticeKindWarning, "One of this standing agent's home files could not be read for its session start: "+problem.Error()+"."); err != nil {
			log.Printf("standing agent %s: %v", threadID, err)
		}
	}
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
