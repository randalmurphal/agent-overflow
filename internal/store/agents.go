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
	CreatorAgentID string `json:"creatorAgentId,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	// AllProjects is set for an assistant that serves every project, those
	// added later included; otherwise it serves the projects
	// AgentProjectIDs names, which may be none once its last one is
	// deleted.
	AllProjects bool `json:"allProjects"`
}

const standingAgentColumns = `id, thread_id, home, creator_agent_id, created_at, all_projects`

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
	var all int
	if err := row.Scan(&agent.ID, &agent.ThreadID, &agent.Home, &agent.CreatorAgentID, &agent.CreatedAt, &all); err != nil {
		return StandingAgent{}, err
	}
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
// projectIDs unless agent.AllProjects is set: the thread must pass
// requireAssistantThreadTx under its title, as a thread created with a home
// does (PlaceThreadInHome moves an existing one there). The title is the
// assistant's name, checked here, in the insert's transaction, so two
// creations cannot both take one name.
func (s *Store) CreateStandingAgent(agent StandingAgent, projectIDs ...string) (err error) {
	action := fmt.Sprintf("store: create assistant %s on %s", agent.ID, agent.ThreadID)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackAgentWakeTx(tx, action, &err)
	var title string
	if err := tx.QueryRow(`SELECT title FROM threads WHERE id = ?`, agent.ThreadID).Scan(&title); err != nil {
		return fmt.Errorf("%s: read the thread: %w", action, err)
	}
	if err := requireAssistantThreadTx(tx, agent, agent.ThreadID, title); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO agents (`+standingAgentColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		agent.ID, agent.ThreadID, agent.Home, agent.CreatorAgentID, agent.CreatedAt, agent.AllProjects); err != nil {
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

// errAssistantThreadTaken refuses, as an assistant's current thread, a
// thread that is already an assistant's, current or past, or a helper.
var errAssistantThreadTaken = errors.New("the thread is already an assistant's")

// requireAssistantThreadTx refuses threadID as agent's current thread named
// name, the checks creation and a handoff share: the thread is no
// assistant's, current, past or helper (errAssistantThreadTaken), name is
// no other assistant's (checkAssistantName), and the thread is in
// agent.Home outside every project, with no group, worktree or branch
// (ErrAssistantThreadNotInHome). A name refusal is returned as
// checkAssistantName returns it.
func requireAssistantThreadTx(tx *sql.Tx, agent StandingAgent, threadID, name string) error {
	action := fmt.Sprintf("store: thread %s as the current thread of assistant %s", threadID, agent.ID)
	var taken, inHome bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = threads.id)
		OR EXISTS(SELECT 1 FROM agent_past_threads WHERE thread_id = threads.id)
		OR EXISTS(SELECT 1 FROM agent_helpers WHERE thread_id = threads.id),
		project_id IS NULL AND workspace_path = ?2 AND group_id IS NULL
		AND COALESCE(worktree_path, '') = '' AND COALESCE(branch, '') = ''
		FROM threads WHERE id = ?1`, threadID, agent.Home).Scan(&taken, &inHome); err != nil {
		return fmt.Errorf("%s: read the thread: %w", action, err)
	}
	if taken {
		return fmt.Errorf("%s: %w", action, errAssistantThreadTaken)
	}
	if err := checkAssistantName(tx, name, agent.ID); err != nil {
		return err
	}
	if !inHome {
		return fmt.Errorf("%s: %w", action, ErrAssistantThreadNotInHome)
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
	rows, err := s.reader().Query(`SELECT agents.id, agents.thread_id, agents.home,
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
		var all int
		if err := rows.Scan(&listing.Agent.ID, &listing.Agent.ThreadID, &listing.Agent.Home,
			&listing.Agent.CreatorAgentID, &listing.Agent.CreatedAt, &all, &listing.Name, &listing.Archived); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		listing.Agent.AllProjects = all == 1
		out = append(out, listing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// AssistantDependentsError refuses to drop the record of an assistant that
// still has past threads or helpers: they live outside every project, so
// without their assistant no sidebar section would list them. Every writer
// that drops a record checks it (deleteAgentRecordTx), so an assistant's
// delete, marked first, drops its current thread last.
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

// CheckNoAssistantDependents is requireNoAssistantDependents on its own,
// for a delete to run before it removes anything of an assistant's current
// thread: its home and history go only once nothing lives under it.
func (s *Store) CheckNoAssistantDependents(threadID string) error {
	return requireNoAssistantDependents(s.reader(), threadID)
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
