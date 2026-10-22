package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// Assistants (docs/specs/standing-agents.md, "Standing agent"): a standing
// agent is a record with its own id, a home under the config directory and
// one current thread. The user creates one with CreateAssistant, an
// assistant creates one with thread_new_assistant, and New Thread, Same
// Assistant (StartNextAssistantThread) gives it a new current thread.

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
// projects the assistant serves, one or more, or all of them.
type CreateAssistantOptions struct {
	Name        string   `json:"name"`
	ProjectIDs  []string `json:"projectIds"`
	AllProjects bool     `json:"allProjects,omitempty"`
}

// CreateAssistant creates an assistant: its thread, titled with its name,
// outside every project with its home as the workspace, its record with the
// projects it serves and its home, and queues the opening message that
// starts the discussion of its role. A name another assistant here holds,
// and an empty or unknown choice of projects, are refused before anything
// is created.
//
//ao:scope threads:autonomy
//ao:route selected
func (a *App) CreateAssistant(ctx context.Context, opts CreateAssistantOptions) (store.Thread, error) {
	thread, agent, err := a.createAssistant(ctx, assistantSpec{
		name:     opts.Name,
		projects: assistantProjects{all: opts.AllProjects, ids: opts.ProjectIDs},
	})
	if err != nil {
		return store.Thread{}, refuseTakenAssistantName(err)
	}
	body := agenthome.OpeningMessage(thread.Title, agent.Home)
	if err := a.queueAssistantMessage(thread.ID, assistantSendID(usermessage.OriginWakeKindOpening, agent.ID), thread.Title, body); err != nil {
		return thread, fmt.Errorf("the assistant was created, but its opening message could not be queued: %w", err)
	}
	return thread, nil
}

// refuseTakenAssistantName gives a name another assistant holds the code a
// client branches on to show the refusal where the name was typed, with a
// sentence naming that assistant.
func refuseTakenAssistantName(err error) error {
	var taken *store.AssistantNameTakenError
	if errors.As(err, &taken) {
		return errorsx.Public(transport.ErrCodeAssistantNameTaken,
			fmt.Sprintf("The assistant %q already has that name; names are unique on this computer.", taken.Name), err)
	}
	return err
}

// assistantSpec is what an assistant is created from.
type assistantSpec struct {
	name string
	// provider, model, effort and runtimeMode are the settings its thread
	// starts with: an assistant that creates one passes on its own, as a
	// spawn does (spawnSettings). Empty takes a new thread's defaults.
	provider, model, effort, runtimeMode string
	projects                             assistantProjects
	// creatorAgentID names the assistant that created this one.
	creatorAgentID string
}

// assistantProjects is the choice of projects an assistant serves: every
// project, those added later included, or the ones ids names.
type assistantProjects struct {
	all bool
	ids []string
}

// checkAssistantProjects refuses an empty choice and a project this
// computer does not have, and returns the ids without repeats.
func (a *App) checkAssistantProjects(choice assistantProjects) (assistantProjects, error) {
	if choice.all {
		return assistantProjects{all: true}, nil
	}
	var ids []string
	for _, id := range choice.ids {
		id = strings.TrimSpace(id)
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		if _, err := a.store.GetProject(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return assistantProjects{}, errorsx.Public(transport.ErrCodeBadParams, fmt.Sprintf("There is no project %s on this computer.", id), err)
			}
			return assistantProjects{}, fmt.Errorf("read project %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return assistantProjects{}, errorsx.Public(transport.ErrCodeBadParams, "Choose at least one project for the assistant, or all projects.", nil)
	}
	return assistantProjects{ids: ids}, nil
}

// createAssistant makes the home, the thread outside every project in it
// and the record, and broadcasts the row once it is an assistant's. A
// clash found when the record is written deletes the thread and the home
// again.
func (a *App) createAssistant(ctx context.Context, spec assistantSpec) (store.Thread, store.StandingAgent, error) {
	name := strings.TrimSpace(spec.name)
	if err := a.store.CheckAssistantName(name); err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	projects, err := a.checkAssistantProjects(spec.projects)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	endWork, err := a.workAdmission.begin(ctx)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	defer endWork()
	thread, agent, err := a.recordNewAssistant(func(home string) (store.Thread, error) {
		return a.createThreadRow(ctx, CreateThreadOptions{
			Title:           name,
			Mode:            string(threadmode.ModeChat),
			Provider:        spec.provider,
			Model:           spec.model,
			ReasoningEffort: spec.effort,
			RuntimeMode:     spec.runtimeMode,
			home:            home,
		}, false)
	}, func(thread store.Thread) {
		if err := a.DeleteThread(thread.ID); err != nil {
			log.Printf("assistants: delete the thread %s of an assistant that was not created: %v", thread.ID, err)
		}
	}, projects, spec.creatorAgentID)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, err
	}
	created, err := a.store.GetThread(thread.ID)
	if err != nil {
		return store.Thread{}, store.StandingAgent{}, fmt.Errorf("read the new assistant's thread: %w", err)
	}
	a.announceThread(triage.ThreadActionListed, created.ID)
	return created, agent, nil
}

// recordNewAssistant makes a new assistant's id and its home under the
// config directory, has place put the assistant's current thread in that
// home, writes the README and records the assistant serving projects. A
// failure after the home exists removes it again, and one after place
// succeeded hands the thread to unplace first.
func (a *App) recordNewAssistant(
	place func(home string) (store.Thread, error), unplace func(store.Thread),
	projects assistantProjects, creatorAgentID string,
) (store.Thread, store.StandingAgent, error) {
	if a.configDir == "" {
		return store.Thread{}, store.StandingAgent{}, errors.New("create an assistant: no config directory")
	}
	agent := store.StandingAgent{ID: entityid.New(), CreatorAgentID: creatorAgentID, AllProjects: projects.all, CreatedAt: time.Now().UnixMilli()}
	agent.Home = filepath.Join(a.configDir, "agents", agent.ID)
	if err := os.MkdirAll(agent.Home, 0o700); err != nil {
		return store.Thread{}, store.StandingAgent{}, fmt.Errorf("create the assistant's home: %w", err)
	}
	thread, err := place(agent.Home)
	if err == nil {
		agent.ThreadID = thread.ID
		if err = a.writeAssistantReadme(agent, thread); err == nil {
			err = a.store.CreateStandingAgent(agent, projects.ids...)
		}
		if err != nil {
			unplace(thread)
		}
	}
	if err != nil {
		if removeErr := os.RemoveAll(agent.Home); removeErr != nil {
			log.Printf("assistants: remove the home %s of an assistant that was not created: %v", agent.Home, removeErr)
		}
		return store.Thread{}, store.StandingAgent{}, err
	}
	return thread, agent, nil
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

// AssistantDependents counts the threads that live under an assistant
// besides its current thread, which deleting it deletes too (ruling 39).
type AssistantDependents struct {
	PastThreads int `json:"pastThreads"`
	Helpers     int `json:"helpers"`
}

// GetAssistantDependents counts the past threads and helpers of the
// assistant whose current thread this is, for its delete confirmation;
// zero for any other thread.
//
//ao:scope threads:read
func (a *App) GetAssistantDependents(threadID string) (AssistantDependents, error) {
	past, helpers, err := a.store.AssistantDependentsOf(threadID)
	if err != nil {
		return AssistantDependents{}, err
	}
	return AssistantDependents{PastThreads: past, Helpers: helpers}, nil
}

// deleteAssistantDependents is the thread delete's AssistantDependents: it
// deletes what lives under the assistant whose current thread threadID is,
// besides that thread, its past threads and its helpers (ruling 39), each
// through DeleteThread under its own lock. One a delete would refuse (a
// thread being moved) refuses them all before any is deleted. Nothing for
// any other thread.
func (a *App) deleteAssistantDependents(threadID string) error {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil || !found {
		return err
	}
	dependents, err := a.store.AssistantDependentThreadIDs(agent.ID)
	if err != nil {
		return err
	}
	for _, id := range dependents {
		if err := a.threadApplication().CheckMutable(id); err != nil {
			return assistantDependentError(id, err)
		}
	}
	for _, id := range dependents {
		if err := a.DeleteThread(id); err != nil {
			return assistantDependentError(id, err)
		}
	}
	return nil
}

// assistantDependentError says which thread under an assistant stopped its
// delete. A transfer's refusal is stated, not wrapped: the dispatcher
// reports a wrapped one as the assistant's own transfer.
func assistantDependentError(threadID string, err error) error {
	var transfer *store.ThreadTransferError
	if errors.As(err, &transfer) {
		return fmt.Errorf("thread %s, which lives under this assistant, is being transferred to another computer (%s), so the assistant was not deleted; finish or cancel that transfer first", threadID, transfer.OperationID)
	}
	return fmt.Errorf("delete thread %s, which lives under the assistant: %w", threadID, err)
}

// assistantRoles is the role-line cache. Its first use subscribes it to
// the store's deletion of assistant records, so the home of every
// assistant deleted or retired is forgotten whichever writer dropped the
// record; an entry exists only after that first use.
func (a *App) assistantRoles() *agenthome.RoleCache {
	a.agentRolesOnce.Do(func() { a.store.OnAgentRecordDeleted(a.agentRoles.Forget) })
	return &a.agentRoles
}

// assistantHomeForDelete is the thread delete's AssistantHome: deleting an
// assistant's current thread deletes the assistant, and its home goes with
// it. Only the directory creation makes, `<config>/agents/<agent-id>` (a
// migrated record's id is its thread's, so its home is that too), is
// removed; a record naming any other path refuses the delete, and nothing is
// deleted. A failure's innermost layer, the one a toast shows, names the
// home folder; the layer around it names the expected folder or where the
// removal stopped.
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
		reason = "its home " + agent.Home + " is not its own folder in the app's agents folder"
	}
	if reason != "" {
		return nil, fmt.Errorf("delete the assistant's home %q (expected %s): %w", agent.Home, want,
			fmt.Errorf("this assistant was not deleted: %s", reason))
	}
	return func() error {
		err := os.RemoveAll(want)
		if err == nil {
			return nil
		}
		at := want
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			at, err = pathErr.Path, pathErr.Err
		}
		return fmt.Errorf("remove the assistant's home %q (stopped at %s): %w", want, at,
			fmt.Errorf("the assistant's home folder %s could not be removed, so the delete is not finished: %v", want, err))
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
// current thread, so creation or a handoff of it changes what their menus
// and drags allow. A delete passes the ids itself
// (broadcastThreadRowsByID), since the cascade has already dropped the
// spawn rows this reads. Log-and-continue, as announceThread.
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
		a.announceThread(triage.ThreadActionFull, id)
	}
}

// broadcastSpawnerRow re-emits the assistant that spawned child, or whose
// past thread or helper child is: the agent row's archivedChildCounts move
// when such a thread is archived, unarchived or deleted. Any other spawner
// carries no count, so nothing is said for it. Log-and-continue, as
// announceThread: the child's change is already durable and the sidebar
// converges on the next ListThreads.
func (a *App) broadcastSpawnerRow(child store.Thread) {
	parentID := child.SpawnedByThreadID
	if owner := cmp.Or(child.PastAgentID, child.HelperOfAgentID); owner != "" {
		agent, found, err := a.store.GetAgent(owner)
		if err != nil {
			log.Printf("broadcast the assistant of thread %s: %v", child.ID, err)
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
	a.announceThread(triage.ThreadActionFull, parent.ID)
}

// SetAssistantProjects sets the projects the assistant whose current
// thread this is serves: every project when all is set, otherwise the ones
// named, at least one. The next session start reads them; the thread's row
// is broadcast so the sidebar card names them.
//
//ao:scope threads:autonomy
func (a *App) SetAssistantProjects(threadID string, all bool, projectIDs []string) (store.StandingAgent, error) {
	agent, found, err := a.store.GetStandingAgent(threadID)
	if err != nil {
		return store.StandingAgent{}, err
	}
	if !found {
		return store.StandingAgent{}, errNotAssistantThread
	}
	choice, err := a.checkAssistantProjects(assistantProjects{all: all, ids: projectIDs})
	if err != nil {
		return store.StandingAgent{}, err
	}
	agent, found, err = a.store.SetAgentProjects(agent.ID, choice.all, choice.ids)
	if err != nil {
		return store.StandingAgent{}, err
	}
	if !found {
		return store.StandingAgent{}, errNotAssistantThread
	}
	a.announceThread(triage.ThreadActionFull, threadID)
	return agent, nil
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
	file, err := os.OpenFile(filepath.Join(agent.Home, "README.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("write the assistant's README: %w", err)
	}
	_, writeErr := file.WriteString(assistantReadme(agent, thread))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write the assistant's README: %w", err)
	}
	return nil
}

func assistantReadme(agent store.StandingAgent, thread store.Thread) string {
	return fmt.Sprintf(`# %s

This directory is the home of an assistant (a standing agent) in Agent
Overflow. The assistant owns it and keeps its durable state here; no layout
is prescribed. Its current thread changes when the user starts a new
thread for it; this home stays.

- Assistant: %s
- Assistant id: %s
- First thread: %s

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

What pinning, groups, archive, the done fold and unread dots do in the
sidebar is in the Organizing paragraph of the thread tools guide every
session carries.
`, thread.Title, thread.Title, agent.ID, thread.ID)
}

// standingAgentSession is what an assistant's sessions start with beside the
// app guide (docs/specs/standing-agents.md, "Agent home contents").
type standingAgentSession struct {
	// Block is agenthome.SessionBlock, read from the record (name, id,
	// current thread, home, the creator it reports to). It changes only
	// with the record, so a home file's edit keeps the provider's prompt
	// cache; the home files reach the session in its memory
	// (readSessionMemory).
	Block string
	// Agent is the assistant; nil for any other thread.
	Agent *store.StandingAgent
}

// readStandingAgentSession reads what the thread's sessions start with as an
// assistant's current thread; the zero value for any other thread, a past
// one included. It is read at every start and resume, so a rename reaches
// the next session.
func (a *App) readStandingAgentSession(thread store.Thread) (standingAgentSession, error) {
	row, found, err := a.store.GetStandingAgent(thread.ID)
	if err != nil {
		return standingAgentSession{}, fmt.Errorf("read the assistant of %s: %w", thread.ID, err)
	}
	if !found {
		return standingAgentSession{}, nil
	}
	identity := agenthome.Identity{Name: thread.Title, AgentID: row.ID, ThreadID: thread.ID, Home: row.Home}
	if row.CreatorAgentID != "" {
		identity.CreatorName, identity.CreatorThreadID = a.assistantName(row.CreatorAgentID)
	}
	return standingAgentSession{Block: agenthome.SessionBlock(identity), Agent: &row}, nil
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
