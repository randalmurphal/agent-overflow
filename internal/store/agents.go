package store

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Assistants (docs/specs/standing-agents.md, "Standing agent"): a standing
// agent is a record of its own, keyed on its own id, with one current
// thread and the threads a handoff superseded. Its name is its current
// thread's title, unique among this computer's assistants.

// StandingAgent is one assistant record.
type StandingAgent struct {
	ID             string `json:"id"`
	ThreadID       string `json:"threadId"`
	Home           string `json:"home"`
	NotifyOnTick   bool   `json:"notifyOnTick"`
	CreatorAgentID string `json:"creatorAgentId,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	// AllProjects is set for an assistant that serves every project, those
	// added later included; otherwise it serves the projects
	// AgentProjectIDs names, which may be none once its last one is
	// deleted.
	AllProjects bool `json:"allProjects"`
}

const standingAgentColumns = `id, thread_id, home, notify_on_tick, creator_agent_id, created_at, all_projects`

// OwnerAgentID names the assistant whose current thread, past thread or
// helper this is, "" for any other thread.
func (t Thread) OwnerAgentID() string {
	return cmp.Or(t.AgentID, t.PastAgentID, t.HelperOfAgentID)
}

type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func scanStandingAgent(row interface{ Scan(...any) error }) (StandingAgent, error) {
	var agent StandingAgent
	var notify, all int
	if err := row.Scan(&agent.ID, &agent.ThreadID, &agent.Home, &notify, &agent.CreatorAgentID, &agent.CreatedAt, &all); err != nil {
		return StandingAgent{}, err
	}
	agent.NotifyOnTick = notify == 1
	agent.AllProjects = all == 1
	return agent, nil
}

// ErrAssistantNameEmpty refuses an empty name for an assistant.
var ErrAssistantNameEmpty = errors.New("an assistant needs a name")

// AssistantNameTakenError refuses a name another assistant on this computer
// holds, naming that assistant.
type AssistantNameTakenError struct {
	// Name is the holder's name as its title spells it.
	Name     string
	AgentID  string
	ThreadID string
}

func (e *AssistantNameTakenError) Error() string {
	return fmt.Sprintf("the assistant %q already has that name; names are unique on this computer", e.Name)
}

// SameAssistantName reports whether two names are the same assistant name:
// equal once trimmed, under Unicode simple case folding.
func SameAssistantName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// assistantNameHolder finds the assistant other than exceptAgentID whose
// name is name, or nil.
func assistantNameHolder(q rowQuerier, name, exceptAgentID string) (_ *AssistantNameTakenError, err error) {
	rows, err := q.Query(`SELECT agents.id, agents.thread_id, threads.title FROM agents
		JOIN threads ON threads.id = agents.thread_id WHERE agents.id <> ?`, exceptAgentID)
	if err != nil {
		return nil, fmt.Errorf("read the assistants' names: %w", err)
	}
	defer closeAgentWakeRows(rows, "read the assistants' names", &err)
	for rows.Next() {
		var holder AssistantNameTakenError
		if err := rows.Scan(&holder.AgentID, &holder.ThreadID, &holder.Name); err != nil {
			return nil, fmt.Errorf("read the assistants' names: %w", err)
		}
		if SameAssistantName(holder.Name, name) {
			return &holder, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the assistants' names: %w", err)
	}
	return nil, nil
}

// checkAssistantName refuses an empty name and one another assistant holds.
func checkAssistantName(q rowQuerier, name, exceptAgentID string) error {
	if strings.TrimSpace(name) == "" {
		return ErrAssistantNameEmpty
	}
	holder, err := assistantNameHolder(q, name, exceptAgentID)
	if err != nil {
		return err
	}
	if holder != nil {
		return holder
	}
	return nil
}

// CheckAssistantName is the check a new assistant's name passes before
// anything is created for it. CreateStandingAgent checks it again.
func (s *Store) CheckAssistantName(name string) error {
	return checkAssistantName(s.reader(), name, "")
}

// checkAssistantTitleTx is the name rule on a title write: a write to an
// assistant's current thread is its rename.
func checkAssistantTitleTx(tx *sql.Tx, threadID, title string) error {
	var agentID string
	err := tx.QueryRow(`SELECT id FROM agents WHERE thread_id = ?`, threadID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read the assistant of %s: %w", threadID, err)
	}
	return checkAssistantName(tx, title, agentID)
}

// GetStandingAgent reads the assistant whose current thread is threadID.
func (s *Store) GetStandingAgent(threadID string) (StandingAgent, bool, error) {
	agent, err := scanStandingAgent(s.reader().QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE thread_id = ?`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		return StandingAgent{}, false, nil
	}
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("store: get the assistant of %s: %w", threadID, err)
	}
	return agent, true, nil
}

// GetAgent reads one assistant by its id.
func (s *Store) GetAgent(agentID string) (StandingAgent, bool, error) {
	agent, err := scanStandingAgent(s.reader().QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE id = ?`, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return StandingAgent{}, false, nil
	}
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("store: get assistant %s: %w", agentID, err)
	}
	return agent, true, nil
}

// MovedDraftOf is the composer draft a handoff moved from the past thread
// threadID to the next one, its text and attachment ids; found is false for
// a thread that is no assistant's past thread.
func (s *Store) MovedDraftOf(threadID string) (moved ThreadDraft, found bool, err error) {
	err = s.reader().QueryRow(`SELECT moved_draft, moved_draft_attachments FROM agent_past_threads WHERE thread_id = ?`, threadID).
		Scan(&moved.Content, &moved.Attachments)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadDraft{}, false, nil
	}
	if err != nil {
		return ThreadDraft{}, false, fmt.Errorf("store: read the draft a handoff moved from %s: %w", threadID, err)
	}
	moved.ThreadID = threadID
	return moved, true, nil
}

// AssistantCurrentThreadOf maps a thread a handoff superseded to its
// assistant's current thread. Found is false for every other thread,
// current threads included.
func (s *Store) AssistantCurrentThreadOf(threadID string) (string, bool, error) {
	var current string
	err := s.reader().QueryRow(`SELECT agents.thread_id FROM agent_past_threads
		JOIN agents ON agents.id = agent_past_threads.agent_id
		WHERE agent_past_threads.thread_id = ?`, threadID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the assistant thread %s was superseded for: %w", threadID, err)
	}
	return current, true, nil
}

// ErrAssistantThreadNotInHome refuses an assistant record on a thread that
// is not in the assistant's home: an assistant's thread lives there,
// outside every project, with no group, worktree or branch.
var ErrAssistantThreadNotInHome = errors.New("store: an assistant's thread lives in its home, outside every project")

// ErrAssistantThreadStays refuses a move of an assistant's current thread to
// another computer: the assistant, its home, loops and watches stay on this
// one. A copy is an ordinary conversation there.
var ErrAssistantThreadStays = errors.New("store: an assistant's current thread stays on its computer")

// refuseAssistantMoveTx returns ErrAssistantThreadStays when threadID is an
// assistant's current thread.
func refuseAssistantMoveTx(tx *sql.Tx, threadID string) error {
	var current bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?)`, threadID).Scan(&current); err != nil {
		return fmt.Errorf("store: read whether %s is an assistant's current thread: %w", threadID, err)
	}
	if current {
		return ErrAssistantThreadStays
	}
	return nil
}

// CreateStandingAgent records an assistant on its thread, serving
// projectIDs unless agent.AllProjects is set: the thread must exist, be no
// assistant's, current, past or helper, and be in agent.Home outside every
// project, with no group, worktree or branch (ErrAssistantThreadNotInHome),
// as a thread created with a home is (PlaceThreadInHome moves an existing
// one there). Its title is the assistant's name, which is checked here, in
// the insert's transaction, so two creations cannot both take one name.
func (s *Store) CreateStandingAgent(agent StandingAgent, projectIDs ...string) (err error) {
	action := fmt.Sprintf("store: create assistant %s on %s", agent.ID, agent.ThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	var title string
	var taken, inHome bool
	if err := tx.QueryRow(`SELECT title, EXISTS(SELECT 1 FROM agents WHERE thread_id = threads.id)
		OR EXISTS(SELECT 1 FROM agent_past_threads WHERE thread_id = threads.id)
		OR EXISTS(SELECT 1 FROM agent_helpers WHERE thread_id = threads.id),
		project_id IS NULL AND workspace_path = ?2 AND group_id IS NULL
		AND COALESCE(worktree_path, '') = '' AND COALESCE(branch, '') = ''
		FROM threads WHERE id = ?1`, agent.ThreadID, agent.Home).Scan(&title, &taken, &inHome); err != nil {
		return fmt.Errorf("%s: read the thread: %w", action, err)
	}
	if taken {
		return fmt.Errorf("%s: the thread is already an assistant's", action)
	}
	if err := checkAssistantName(tx, title, agent.ID); err != nil {
		return err
	}
	if !inHome {
		return fmt.Errorf("%s: %w", action, ErrAssistantThreadNotInHome)
	}
	notify := 0
	if agent.NotifyOnTick {
		notify = 1
	}
	if _, err := tx.Exec(`INSERT INTO agents (`+standingAgentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		agent.ID, agent.ThreadID, agent.Home, notify, agent.CreatorAgentID, agent.CreatedAt, agent.AllProjects); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := setAgentProjectsTx(tx, agent.ID, agent.AllProjects, projectIDs); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// PlaceThreadInHome moves a thread into home, outside every project, with
// no group, worktree, branch or recorded git origin, as a thread created
// with a home is: how the harness makes an existing thread ready to be an
// assistant's (CreateStandingAgent). A thread that is an assistant's,
// current or past, or a helper, is refused.
func (s *Store) PlaceThreadInHome(threadID, home string) error {
	result, err := s.db.Exec(`UPDATE threads SET project_id = NULL, workspace_path = ?2, worktree_path = NULL, branch = NULL,
		group_id = NULL, placement = 'standalone',
		created_branch = '', created_remote_url = '', created_head_commit = ''
		WHERE id = ?1
		  AND NOT EXISTS (SELECT 1 FROM agents WHERE thread_id = ?1)
		  AND NOT EXISTS (SELECT 1 FROM agent_past_threads WHERE thread_id = ?1)
		  AND NOT EXISTS (SELECT 1 FROM agent_helpers WHERE thread_id = ?1)`, threadID, home)
	if err != nil {
		return fmt.Errorf("store: place thread %s in %s: %w", threadID, home, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: place thread %s in %s: no such thread outside every assistant", threadID, home))
}

// SetStandingAgentNotifyOnTick turns tick notifications on or off for the
// assistant whose current thread is threadID. Changed is false when the
// row already said so; found is false when the thread is no assistant's
// current thread.
func (s *Store) SetStandingAgentNotifyOnTick(threadID string, on bool) (_ StandingAgent, changed, found bool, _ error) {
	notify := 0
	if on {
		notify = 1
	}
	changed, err := s.changeAgentWake(fmt.Sprintf("store: set notify_on_tick of %s", threadID),
		`UPDATE agents SET notify_on_tick = ? WHERE thread_id = ? AND notify_on_tick IS NOT ?`,
		notify, threadID, notify)
	if err != nil {
		return StandingAgent{}, false, false, err
	}
	row, found, err := s.GetStandingAgent(threadID)
	if err != nil || !found {
		return StandingAgent{}, false, false, err
	}
	return row, changed, true, nil
}

// StandingAgentSpawnerOf names the assistant thread that spawned a thread on
// this computer: the caller of a local `spawn` request whose target is the
// thread, while that caller is an assistant's current thread and not
// archived. A handoff moves its predecessors' spawn rows to the current
// thread, so a worker an earlier thread spawned names the current one.
// Found is false for every other thread. A worker past the request ledger's
// retention still names its agent: the sweep keeps its spawn row as lineage
// (spawnLineageSQL), so its prompts keep routing there.
func (s *Store) StandingAgentSpawnerOf(threadID string) (agentThreadID, title string, found bool, err error) {
	err = s.reader().QueryRow(`SELECT t.id, t.title FROM agents a
		JOIN thread_requests r ON r.caller_thread_id = a.thread_id
		JOIN threads t ON t.id = a.thread_id
		WHERE r.kind = '`+ThreadRequestSpawn+`' AND r.target_computer_id = '' AND r.target_thread_id = ?
		  AND t.archived = 0
		ORDER BY r.created_at DESC LIMIT 1`, threadID).Scan(&agentThreadID, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read the standing agent that spawned %s: %w", threadID, err)
	}
	return agentThreadID, title, true, nil
}

// AssistantListing is one assistant as the thread tools list and address
// it: the record, its name and whether its current thread is archived.
type AssistantListing struct {
	Agent    StandingAgent
	Name     string
	Archived bool
}

// ListAssistants lists this computer's assistants by name.
func (s *Store) ListAssistants() (_ []AssistantListing, err error) {
	action := "store: list assistants"
	rows, err := s.reader().Query(`SELECT agents.id, agents.thread_id, agents.home, agents.notify_on_tick,
		agents.creator_agent_id, agents.created_at, agents.all_projects, threads.title, threads.archived
		FROM agents JOIN owned_threads AS threads ON threads.id = agents.thread_id
		ORDER BY threads.title COLLATE NOCASE, agents.id`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	out := []AssistantListing{}
	for rows.Next() {
		var listing AssistantListing
		var notify, all int
		if err := rows.Scan(&listing.Agent.ID, &listing.Agent.ThreadID, &listing.Agent.Home, &notify,
			&listing.Agent.CreatorAgentID, &listing.Agent.CreatedAt, &all, &listing.Name, &listing.Archived); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		listing.Agent.NotifyOnTick = notify == 1
		listing.Agent.AllProjects = all == 1
		out = append(out, listing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// AssistantDependentThreadIDs are the threads that live under an assistant
// besides its current thread: its past threads, then its helpers, each in
// the order they were made.
func (s *Store) AssistantDependentThreadIDs(agentID string) (_ []string, err error) {
	action := fmt.Sprintf("store: read the threads under assistant %s", agentID)
	rows, err := s.reader().Query(`SELECT thread_id FROM (
		SELECT agent_past_threads.thread_id, 0 AS kind, superseded_at AS at FROM agent_past_threads WHERE agent_id = ?1
		UNION ALL
		SELECT agent_helpers.thread_id, 1, threads.created_at FROM agent_helpers
		  JOIN threads ON threads.id = agent_helpers.thread_id WHERE agent_helpers.agent_id = ?1
	) ORDER BY kind, at, thread_id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return ids, nil
}

// AssistantDependentsOf counts the threads AssistantDependentThreadIDs
// lists for the assistant whose current thread is threadID, past threads
// and helpers apart; zero for any other thread.
func (s *Store) AssistantDependentsOf(threadID string) (pastThreads, helpers int, err error) {
	dependents, err := assistantDependentsOf(s.reader(), threadID)
	return dependents.PastThreads, dependents.Helpers, err
}

// AssistantDependentsError refuses to drop the record of an assistant that
// still has past threads or helpers: they live outside every project, so
// without their assistant no sidebar section would list them. Every writer
// that drops a record checks it (deleteAgentRecordTx), and a thread delete
// checks it before its mark (BeginThreadDelete).
type AssistantDependentsError struct {
	AgentID              string
	PastThreads, Helpers int
}

func (e *AssistantDependentsError) Error() string {
	return fmt.Sprintf("store: assistant %s still has %d past threads and %d helpers under it", e.AgentID, e.PastThreads, e.Helpers)
}

// assistantDependentsOf counts the past threads and helpers of the
// assistant whose current thread is threadID, as an AssistantDependentsError
// that names none for any other thread.
func assistantDependentsOf(q transferQuerier, threadID string) (AssistantDependentsError, error) {
	var dependents AssistantDependentsError
	err := q.QueryRow(`SELECT id,
		(SELECT COUNT(*) FROM agent_past_threads WHERE agent_past_threads.agent_id = agents.id),
		(SELECT COUNT(*) FROM agent_helpers WHERE agent_helpers.agent_id = agents.id)
		FROM agents WHERE thread_id = ?`, threadID).Scan(&dependents.AgentID, &dependents.PastThreads, &dependents.Helpers)
	if errors.Is(err, sql.ErrNoRows) {
		return AssistantDependentsError{}, nil
	}
	if err != nil {
		return AssistantDependentsError{}, fmt.Errorf("store: count the threads under the assistant of %s: %w", threadID, err)
	}
	return dependents, nil
}

// requireNoAssistantDependents refuses, with *AssistantDependentsError, a
// write that would drop the record of the assistant whose current thread is
// threadID while threads still live under it.
func requireNoAssistantDependents(q transferQuerier, threadID string) error {
	dependents, err := assistantDependentsOf(q, threadID)
	if err != nil {
		return err
	}
	if dependents.PastThreads > 0 || dependents.Helpers > 0 {
		return &dependents
	}
	return nil
}

// AgentProjectIDs are the projects an assistant serves when it does not
// serve all of them, sorted.
func (s *Store) AgentProjectIDs(agentID string) (_ []string, err error) {
	action := fmt.Sprintf("store: read the projects of assistant %s", agentID)
	rows, err := s.reader().Query(`SELECT project_id FROM agent_projects WHERE agent_id = ? ORDER BY project_id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return ids, nil
}

// ServedProjects are the projects an assistant serves, by name: every
// unarchived project when it serves all of them, otherwise the ones it
// names, archived or not.
func (s *Store) ServedProjects(agentID string) (_ []Project, err error) {
	action := fmt.Sprintf("store: read the projects assistant %s serves", agentID)
	rows, err := s.reader().Query(`SELECT `+projectColumns+` FROM projects
		WHERE EXISTS (SELECT 1 FROM agents WHERE agents.id = ?1 AND agents.all_projects = 1 AND projects.archived = 0)
		   OR EXISTS (SELECT 1 FROM agent_projects WHERE agent_projects.agent_id = ?1 AND agent_projects.project_id = projects.id)
		ORDER BY name COLLATE NOCASE, id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeAgentWakeRows(rows, action, &err)
	out := []Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// SetAgentProjects sets the projects an assistant serves: every one when
// all is set, otherwise projectIDs. Found is false when there is no such
// assistant.
func (s *Store) SetAgentProjects(agentID string, all bool, projectIDs []string) (agent StandingAgent, found bool, err error) {
	action := fmt.Sprintf("store: set the projects of assistant %s", agentID)
	tx, err := s.db.Begin()
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	result, err := tx.Exec(`UPDATE agents SET all_projects = ? WHERE id = ?`, all, agentID)
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if n == 0 {
		return StandingAgent{}, false, nil
	}
	if err := setAgentProjectsTx(tx, agentID, all, projectIDs); err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: %w", action, err)
	}
	agent, err = scanStandingAgent(tx.QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE id = ?`, agentID))
	if err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: read it back: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return StandingAgent{}, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return agent, true, nil
}

// setAgentProjectsTx replaces an assistant's project rows: none when it
// serves all projects, otherwise one per id. An id that names no project
// fails the foreign key.
func setAgentProjectsTx(tx *sql.Tx, agentID string, all bool, projectIDs []string) error {
	if _, err := tx.Exec(`DELETE FROM agent_projects WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	if all {
		return nil
	}
	for _, id := range projectIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_projects (agent_id, project_id) VALUES (?, ?)`, agentID, id); err != nil {
			return fmt.Errorf("serve project %s: %w", id, err)
		}
	}
	return nil
}

// AssistantHandOff is what a handoff changed.
type AssistantHandOff struct {
	// Agent is the record, now on the successor.
	Agent StandingAgent
	// Old is the superseded thread as it stands, archived.
	Old Thread
	// Archived is false when the old thread was already archived.
	Archived bool
	// MovedWakeIDs are the live loops and watches now on the successor.
	MovedWakeIDs []string
	// MovedRequests counts the requests now the successor's.
	MovedRequests int64
}

// ErrNotAssistantCurrentThread refuses a handoff of a thread that is no
// assistant's current thread.
var ErrNotAssistantCurrentThread = errors.New("the thread is not an assistant's current thread")

// HandOffAssistant makes newThreadID the current thread of the assistant
// whose current thread is oldThreadID, in one transaction: the record moves,
// the old thread is recorded as superseded at now and archived, its live
// loops and watches and the remote jobs still owed to it move, and every request it is the caller of moves with
// the thread that made it kept in made_in_thread_id (a reminder's target
// moves with it). The successor must be outside every project, as an
// assistant's thread is, and no assistant's thread or helper yet; the old thread keeps
// its placement, which a thread outside every project does not use.
// Receipts addressed to the old thread are not moved here:
// the messages they wait on are in the old thread's queue, and the app
// queues each again on the successor as it takes it back.
func (s *Store) HandOffAssistant(oldThreadID, newThreadID string, now int64) (handOff AssistantHandOff, err error) {
	action := fmt.Sprintf("store: hand assistant thread %s off to %s", oldThreadID, newThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	agent, err := scanStandingAgent(tx.QueryRow(`SELECT `+standingAgentColumns+` FROM agents WHERE thread_id = ?`, oldThreadID))
	if errors.Is(err, sql.ErrNoRows) {
		return AssistantHandOff{}, ErrNotAssistantCurrentThread
	}
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: read the assistant: %w", action, err)
	}
	var successorTaken, successorInProject bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?1)
		OR EXISTS(SELECT 1 FROM agent_past_threads WHERE thread_id = ?1)
		OR EXISTS(SELECT 1 FROM agent_helpers WHERE thread_id = ?1), project_id IS NOT NULL
		FROM threads WHERE id = ?1`, newThreadID).Scan(&successorTaken, &successorInProject); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: read the successor: %w", action, err)
	}
	if successorTaken {
		return AssistantHandOff{}, fmt.Errorf("%s: the successor is already an assistant's thread", action)
	}
	if successorInProject {
		return AssistantHandOff{}, fmt.Errorf("%s: the successor is in a project: %w", action, ErrAssistantThreadNotInHome)
	}
	if _, err := tx.Exec(`UPDATE agents SET thread_id = ? WHERE id = ?`, newThreadID, agent.ID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the record: %w", action, err)
	}
	agent.ThreadID = newThreadID
	if _, err := tx.Exec(`INSERT INTO agent_past_threads (thread_id, agent_id, superseded_at) VALUES (?, ?, ?)`,
		oldThreadID, agent.ID, now); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: record the old thread: %w", action, err)
	}
	handOff.Agent = agent
	if handOff.MovedWakeIDs, err = queryIDs(tx, `UPDATE agent_wakes SET thread_id = ?
		WHERE thread_id = ? AND state IN ('armed','running') RETURNING id`, newThreadID, oldThreadID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the loops and watches: %w", action, err)
	}
	// A remote job still owed to the old thread reports to the successor:
	// the watcher skips an archived thread's watches.
	if _, err := tx.Exec(`UPDATE remote_watches SET thread_id = ?
		WHERE thread_id = ? AND (notification = 'pending' OR (notification = 'queued' AND confirmed_at IS NULL))`,
		newThreadID, oldThreadID); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the remote jobs: %w", action, err)
	}
	moved, err := tx.Exec(`UPDATE thread_requests SET
		made_in_thread_id = CASE WHEN made_in_thread_id = '' THEN caller_thread_id ELSE made_in_thread_id END,
		target_thread_id = CASE WHEN kind = '`+ThreadRequestRemind+`' AND target_computer_id = '' AND target_thread_id = ?1
			THEN ?2 ELSE target_thread_id END,
		caller_thread_id = ?2
		WHERE caller_thread_id = ?1`, oldThreadID, newThreadID)
	if err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the requests: %w", action, err)
	}
	if handOff.MovedRequests, err = moved.RowsAffected(); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: move the requests: %w", action, err)
	}
	if handOff.Old, handOff.Archived, err = applyThreadRowWriteTx(tx, threadArchivedWrite(oldThreadID, true)); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: archive the old thread: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return AssistantHandOff{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{CallerThreadID: oldThreadID})
	s.noteThreadRequestChanged(ThreadRequestChange{CallerThreadID: newThreadID})
	return handOff, nil
}

// RetargetThreadReceipt points a receipt that waits in `accepted` on a
// superseded thread at its assistant's current thread, with the request's
// own row when the request was made on this computer. False when the
// receipt is not in that state on fromThreadID.
func (s *Store) RetargetThreadReceipt(token, fromThreadID, toThreadID string) (moved bool, err error) {
	action := fmt.Sprintf("store: move receipt %s to %s", token, toThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	result, err := tx.Exec(`UPDATE thread_request_receipts SET target_thread_id = ?
		WHERE token = ? AND target_thread_id = ? AND state = '`+ThreadReceiptAccepted+`'`, toThreadID, token, fromThreadID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE thread_requests SET target_thread_id = ?
		WHERE token = ? AND target_computer_id = '' AND target_thread_id = ?`, toThreadID, token, fromThreadID); err != nil {
		return false, fmt.Errorf("%s: the request's row: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteThreadRequestChanged(ThreadRequestChange{Token: token})
	return true, nil
}

// OnAgentRecordDeleted sets the function the store calls, after it
// commits, with the home of each assistant record a write deleted: a
// thread delete, which deletes the thread or retires it to a holder. Every
// writer that drops an agents row goes through deleteAgentRecordTx. fn must
// not block. nil clears it.
func (s *Store) OnAgentRecordDeleted(fn func(home string)) {
	if fn == nil {
		s.agentRecordDeleted.Store(nil)
		return
	}
	s.agentRecordDeleted.Store(&fn)
}

func (s *Store) noteAgentRecordDeleted(home string) {
	if home == "" {
		return
	}
	if fn := s.agentRecordDeleted.Load(); fn != nil {
		(*fn)(home)
	}
}

// deleteAgentRecordTx deletes the assistant record of threadID, if it has
// one, and returns its home, which the caller reports after its commit
// (noteAgentRecordDeleted). A record with past threads or helpers is
// refused (*AssistantDependentsError): its foreign keys would delete their
// rows and leave the threads with no owner.
func deleteAgentRecordTx(tx *sql.Tx, threadID string) (string, error) {
	if err := requireNoAssistantDependents(tx, threadID); err != nil {
		return "", err
	}
	var home string
	err := tx.QueryRow(`DELETE FROM agents WHERE thread_id = ? RETURNING home`, threadID).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: delete the assistant record of %s: %w", threadID, err)
	}
	return home, nil
}
