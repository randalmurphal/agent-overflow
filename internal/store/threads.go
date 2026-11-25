package store

import (
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/threadmode"
)

// proposedPlanItemSQL is the row behind a thread's pending-plan flag: the
// latest plan's completed assistant row, whose payload (local overlay
// first, as timeline_payloads resolves it) is a proposed plan. Both the
// row and its imported payload are found by id, so a sidebar row costs
// the same however many chunks its thread references.
func proposedPlanItemSQL() string {
	sql, _ := correlatedTimelineArms(timelineSelection{
		Columns:  func(string, string) string { return "1" },
		Thread:   "proposed_plans.thread_id",
		KeyFirst: true,
		Where: `items.id = proposed_plans.item_id
         AND items.role = 'assistant'
         AND items.status = 'completed'
         AND COALESCE(
           (SELECT local_payload.kind
              FROM payloads AS local_payload
             WHERE local_payload.thread_id = proposed_plans.thread_id
               AND local_payload.id = items.payload_id),
           (SELECT imported_payload.kind
              FROM import_history_payloads AS imported_payload
             CROSS JOIN thread_import_chunks AS payload_refs
                ON payload_refs.chunk_id = imported_payload.chunk_id
             WHERE imported_payload.id = items.payload_id
               AND payload_refs.thread_id = proposed_plans.thread_id)
         ) = 'proposed_plan'`,
	})
	return sql
}

// threadColumns lists every column in the order scanThread expects. The
// COALESCE-ing of nullable text columns returns "" instead of NULL so the
// Go struct has a clean empty-string value for unset optional fields.
// project_id is coalesced because v5 made it nullable: a standalone "home"
// terminal thread has no project, and scanThread reads it into a plain
// string. last_read_at, pinned_at, and pin_group are deliberately NOT coalesced —
// scanThread keeps the NULL / non-NULL distinction via *int64 pointers so
// the frontend can tell "never tracked" / "unpinned" apart from a zero
// timestamp and distinguish migrated front-burner pins from explicit groups.
// The boolean tail columns are derived sidebar state: keyed probes whose
// cost does not grow with the thread's rows. hasFailedTurn's turn-error
// half reads the write-time pair thread_turn_error_aggregate.go maintains,
// and the pending-plan flag finds its row and payload by id
// (proposedPlanItemSQL). The agent columns read the assistant records and
// the local spawn requests (idx_thread_requests_spawn_target for the
// spawner). An assistant's current thread reads its record once, as one
// JSON object (threadAgentColumn) that also carries the projects it serves,
// and any other thread reads NULL there, so only an assistant's row pays
// for them. AskCopy probes scratch_threads by key only for a scratch
// thread. The last column is the thread's ticket link
// (threadTicketLinkColumn); a read of the whole catalog takes it from
// threadListSelect instead.
var threadColumns = threadRowColumns + threadTicketLinkColumn

// threadRowColumns are threadColumns but the last, the link.
var threadRowColumns = `id, COALESCE(project_id, ''),
    COALESCE((SELECT path FROM projects WHERE projects.id = threads.project_id), ''),
    title, provider, model,
    workspace_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''),
    pending_fork_resume_at, fork_preparing,
    CASE WHEN deleting = 1 THEN 2 WHEN delete_claim <> '' THEN 1 ELSE 0 END,
    mode, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    COALESCE(forked_from_thread_id, ''), last_token_usage,
    created_at, updated_at,
    (SELECT MAX(completed_at) FROM turns
      WHERE turns.thread_id = threads.id AND completed_at IS NOT NULL),
    archived, last_read_at, pinned_at, pin_group,
    COALESCE(group_id, ''), placement,
    worktree_setup_state, import_source,
    created_by_device, created_branch, created_head_commit,
	EXISTS (
      SELECT 1
        FROM proposed_plans
       WHERE proposed_plans.thread_id = threads.id
         AND proposed_plans.version = (
           SELECT MAX(latest.version)
             FROM proposed_plans AS latest
            WHERE latest.thread_id = threads.id
         )
         AND proposed_plans.implemented_at = 0
         AND EXISTS (` + proposedPlanItemSQL() + `)
    ),
    ` + newestTurnStateSQL() + `,
    COALESCE((
      SELECT turns.stop_reason = 'error'
         AND (threads.last_read_at IS NULL OR threads.last_read_at < turns.completed_at)
        FROM turns
       WHERE turns.thread_id = threads.id
       ORDER BY turns.turn_index DESC
       LIMIT 1
    ), 0) OR (
      threads.newest_turn_error_at IS NOT NULL
      AND (threads.last_read_at IS NULL OR threads.last_read_at < threads.newest_turn_error_at)
    ),
    NOT EXISTS (SELECT 1 FROM timeline_items WHERE timeline_items.thread_id = threads.id),
    (SELECT COALESCE(MAX(ownership_epoch),0) FROM thread_transfers
      WHERE thread_id = threads.id AND direction = 'incoming' AND phase = 'complete'),
    (SELECT json_object(
        'id', agents.id,
        'allProjects', agents.all_projects,
        'projectIds', json((SELECT json_group_array(project_id) FROM (
          SELECT project_id FROM agent_projects WHERE agent_projects.agent_id = agents.id ORDER BY project_id))))
       FROM agents WHERE agents.thread_id = threads.id),
    COALESCE((SELECT agent_id FROM agent_past_threads WHERE agent_past_threads.thread_id = threads.id), ''),
    (SELECT superseded_at FROM agent_past_threads WHERE agent_past_threads.thread_id = threads.id),
    COALESCE((SELECT agent_id FROM agent_helpers WHERE agent_helpers.thread_id = threads.id), ''),
    COALESCE(` + spawnerOfSQL("threads.id") + `, ''), ` + spawnedByComputerSQL("threads.id") + `,
    attention_kind, attention_reason, attention_at,
    spawn_instructions, spawn_level,
    ` + askCopySQL + `,
    `

// threadListSelect is SELECT threadColumns FROM from (naming the thread
// threads) for a read of the whole catalog (ListThreads,
// ListThreadsWithItems): the links CTE computes each link on the computer
// once (threadTicketLinksSQL), not once per thread
// (threadTicketLinkColumn), and each thread probes it by key. A read of
// part of the catalog takes threadColumns, whose link work is its rows'.
// The CTE is referenced once.
func threadListSelect(from string) string {
	return `WITH links AS MATERIALIZED (` + threadTicketLinksSQL + `) SELECT ` + threadRowColumns + `links.link FROM ` + from +
		` LEFT JOIN links ON links.thread_id = threads.id`
}

// askCopySQL is true for the row of threads that is an ask's hidden copy
// (Thread.AskCopy).
const askCopySQL = `threads.mode = 'scratch' AND EXISTS (SELECT 1 FROM scratch_threads
      WHERE scratch_threads.thread_id = threads.id AND scratch_threads.request_token IS NOT NULL)`

// threadAgentColumn is the record column threadColumns reads for an
// assistant's current thread.
type threadAgentColumn struct {
	ID          string   `json:"id"`
	AllProjects int      `json:"allProjects"`
	ProjectIDs  []string `json:"projectIds"`
}

// -- Validation errors for enum fields. Each binding checks against the
// -- list before hitting SQLite so the caller sees a typed error instead
// -- of a raw CHECK-constraint failure.
var (
	// ErrInvalidEffort is returned when a caller passes a reasoning-effort
	// value outside the provider effort enum.
	ErrInvalidEffort = errors.New("store: invalid reasoning effort")
	// ErrInvalidMode is returned for a bad mode value.
	ErrInvalidMode = errors.New("store: invalid thread mode")
	// ErrInvalidContextWindow is returned when the caller requests a
	// context window size the schema's CHECK constraint does not allow.
	ErrInvalidContextWindow = errors.New("store: invalid context window")
	// ErrInvalidAutoCompactPercent is returned when a caller passes an
	// auto-compact percent outside 0..90. Zero means provider default.
	ErrInvalidAutoCompactPercent = errors.New("store: invalid auto-compact percent")
	// ErrInvalidProvider is returned for a bad provider value.
	ErrInvalidProvider = errors.New("store: invalid provider")
	// ErrThreadProviderLocked is returned when a caller tries to switch a
	// thread's provider after timeline items have been persisted.
	ErrThreadProviderLocked = errors.New("store: thread provider locked")
	// ErrInvalidImportSource is returned for an import provenance outside
	// the migration v50 enum ("", "claude", "codex").
	ErrInvalidImportSource = errors.New("store: invalid import source")
	// ErrInvalidPinGroup is returned for a pin group outside the exact
	// front/back pair. The database repeats the constraint for direct writes.
	ErrInvalidPinGroup = errors.New("store: invalid pin group")
)

const (
	PinGroupFront = 0
	PinGroupBack  = 1
)

var legalEfforts = map[string]struct{}{
	"none":    {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
	"ultra":   {},
}

func legalEffortForProvider(providerName, effort string) bool {
	switch providerName {
	case "codex":
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
			return true
		default:
			return false
		}
	case "claude", "claude-tui":
		// claude-tui drives the same claude binary, so it shares claude's
		// reasoning-effort set. Kept in lockstep with the provider/effort
		// coupling CHECK on threads + chat_model_profiles (migrate.go).
		switch effort {
		case "low", "medium", "high", "xhigh", "max":
			return true
		default:
			return false
		}
	case "agy":
		switch effort {
		case "low", "medium", "high", "xhigh":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

var legalProviders = map[string]struct{}{
	"claude":     {},
	"codex":      {},
	"claude-tui": {},
	"agy":        {},
}

func validContextWindow(tokens int) bool {
	return tokens > 0
}

// validImportSource validates thread import provenance.
func validImportSource(source string) bool {
	switch source {
	case "", "claude", "codex", "agy":
		return true
	default:
		return false
	}
}

func validAutoCompactPercent(percent int) bool {
	return percent >= 0 && percent <= 90
}

func scanThread(scanner interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var archived, fastMode, hasActionableProposedPlan, hasFailedTurn, isDraft, askCopy int
	var newestTurn newestTurnState
	var agentRecord sql.NullString
	var latestTurnCompletedAt, lastReadAt, pinnedAt, pinGroup, agentSupersededAt, attentionAt sql.NullInt64
	var attentionKind, attentionReason string
	var ticketLink sql.NullString
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.ProjectPath, &t.Title, &t.Provider, &t.Model,
		&t.WorkspacePath, &t.WorktreePath, &t.Branch,
		&t.SessionRef, &t.PendingForkRef,
		&t.PendingForkResumeAt, &t.ForkPreparing, &t.Delete,
		&t.Mode, &t.ReasoningEffort, &fastMode, &t.ContextWindow,
		&t.AutoCompactStandardPercent, &t.AutoCompactExtendedPercent, &t.RuntimeMode,
		&t.ForkedFromThreadID, &t.LastTokenUsage,
		&t.CreatedAt, &t.UpdatedAt, &latestTurnCompletedAt, &archived, &lastReadAt, &pinnedAt, &pinGroup,
		&t.GroupID, &t.Placement,
		&t.WorktreeSetupState, &t.ImportSource,
		&t.CreatedByDevice, &t.Origin.Branch, &t.Origin.HeadCommit,
		&hasActionableProposedPlan, &newestTurn, &hasFailedTurn, &isDraft, &t.OwnershipEpoch,
		&agentRecord, &t.PastAgentID, &agentSupersededAt, &t.HelperOfAgentID, &t.SpawnedByThreadID, &t.SpawnedByComputerID,
		&attentionKind, &attentionReason, &attentionAt,
		&t.SpawnInstructions, &t.SpawnLevel, &askCopy, &ticketLink,
	); err != nil {
		return Thread{}, err
	}
	t.FastMode = fastMode != 0
	t.AskCopy = askCopy != 0
	t.Archived = archived != 0
	t.HasActionableProposedPlan = hasActionableProposedPlan != 0
	t.HasIncompleteTurn = newestTurn == newestTurnInterruptedUnread
	t.HasOpenTurn = newestTurn == newestTurnOpen
	t.HasFailedTurn = hasFailedTurn != 0
	t.IsDraft = isDraft != 0
	if latestTurnCompletedAt.Valid {
		v := latestTurnCompletedAt.Int64
		t.LatestTurnCompletedAt = &v
	}
	if lastReadAt.Valid {
		v := lastReadAt.Int64
		t.LastReadAt = &v
	}
	if pinnedAt.Valid {
		v := pinnedAt.Int64
		t.PinnedAt = &v
	}
	if pinGroup.Valid {
		v := int(pinGroup.Int64)
		t.PinGroup = &v
	}
	if agentSupersededAt.Valid {
		v := agentSupersededAt.Int64
		t.AgentSupersededAt = &v
	}
	if attentionAt.Valid && attentionKind != "" {
		t.Attention = &ThreadAttention{Kind: attentionKind, Reason: attentionReason, At: attentionAt.Int64}
	}
	if agentRecord.Valid {
		var agent threadAgentColumn
		if err := json.Unmarshal([]byte(agentRecord.String), &agent); err != nil {
			return Thread{}, fmt.Errorf("read the assistant of thread %s: %w", t.ID, err)
		}
		t.AgentID, t.AgentAllProjects = agent.ID, agent.AllProjects != 0
		if len(agent.ProjectIDs) > 0 {
			t.AgentProjectIDs = agent.ProjectIDs
		}
	}
	if ticketLink.Valid {
		t.TicketLink = new(ThreadTicketLink)
		// json/v2 decodes it in about a quarter of v1's time; the list
		// read decodes one per linked thread (BenchmarkListThreadsLinked).
		if err := jsonv2.Unmarshal([]byte(ticketLink.String), t.TicketLink); err != nil {
			return Thread{}, fmt.Errorf("read the ticket link of thread %s: %w", t.ID, err)
		}
	}
	return t, nil
}

func (s *Store) CreateThread(t Thread) error {
	_, err := s.createThread(t, false)
	return err
}

// CreateThreadAndRead is CreateThread returning the created row, read in
// the insert's transaction: a read that fails rolls the insert back, so a
// caller never holds a thread it could not read.
func (s *Store) CreateThreadAndRead(t Thread) (Thread, error) {
	return s.createThread(t, true)
}

func (s *Store) createThread(t Thread, readBack bool) (Thread, error) {
	prepared, lastReadAtArg, err := prepareThreadForCreate(t)
	if err != nil {
		return Thread{}, err
	}
	// One transaction because the row and its title's index entry are one
	// fact: a thread that exists and is unsearchable would stay that way,
	// the background build being a one-time pass over what already existed.
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, fmt.Errorf("store: begin create thread: %w", err)
	}
	defer tx.Rollback()
	if err := insertThread(tx, prepared, lastReadAtArg); err != nil {
		return Thread{}, fmt.Errorf("store: create thread: %w", err)
	}
	if err := recordAgentHelperTx(tx, prepared); err != nil {
		return Thread{}, fmt.Errorf("store: create thread: %w", err)
	}
	var created Thread
	if readBack {
		if created, err = getThreadTx(tx, prepared.ID); err != nil {
			return Thread{}, fmt.Errorf("store: create thread: read %s back: %w", prepared.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, fmt.Errorf("store: commit create thread: %w", err)
	}
	return created, nil
}

func prepareThreadForCreate(t Thread) (Thread, any, error) {
	t.Mode = normalizeMode(t.Mode)
	if !threadmode.IsLegal(t.Mode) {
		return Thread{}, nil, fmt.Errorf("%w: %q", ErrInvalidMode, t.Mode)
	}
	t.RuntimeMode = normalizeRuntimeMode(t.RuntimeMode)
	t.ReasoningEffort = normalizeEffort(t.ReasoningEffort)
	if !legalEffortForProvider(t.Provider, t.ReasoningEffort) {
		return Thread{}, nil, fmt.Errorf("%w: %s/%s", ErrInvalidEffort, t.Provider, t.ReasoningEffort)
	}
	if t.ContextWindow == 0 {
		t.ContextWindow = 1000000
	}
	if !validContextWindow(t.ContextWindow) {
		return Thread{}, nil, fmt.Errorf("%w: %d", ErrInvalidContextWindow, t.ContextWindow)
	}
	if !validAutoCompactPercent(t.AutoCompactStandardPercent) {
		return Thread{}, nil, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactStandardPercent)
	}
	if !validAutoCompactPercent(t.AutoCompactExtendedPercent) {
		return Thread{}, nil, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactExtendedPercent)
	}
	if !validImportSource(t.ImportSource) {
		return Thread{}, nil, fmt.Errorf("%w: %q", ErrInvalidImportSource, t.ImportSource)
	}
	lastReadAt := t.LastReadAt
	if lastReadAt == nil {
		lastReadAt = &t.CreatedAt
	}
	return t, nullableInt64(lastReadAt), nil
}

const threadInsertColumns = `id, project_id, title, provider, model,
		    workspace_path, worktree_path, branch, session_ref, pending_fork_session_ref,
		    pending_fork_resume_at, fork_preparing,
		    mode, reasoning_effort, fast_mode, context_window,
		    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
		    forked_from_thread_id, last_token_usage,
		    created_at, updated_at, archived, last_read_at, import_source,
		    created_by_device, created_branch, created_remote_url, created_head_commit,
		    group_id, placement, spawn_instructions, spawn_level`

const threadInsertSQL = `INSERT INTO threads (` + threadInsertColumns + `)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE ? = '' OR EXISTS (SELECT 1 FROM thread_groups WHERE id = ? AND project_id = ?)`

// ErrHelperInProject refuses a helper in a project: a helper lives under
// its assistant, outside every project.
var ErrHelperInProject = errors.New("store: a helper lives outside every project")

// ErrHelperOfNoAssistant refuses a helper of an assistant that is gone or
// whose delete committed (CommitDeleteClaim).
var ErrHelperOfNoAssistant = errors.New("store: the assistant is gone or being deleted")

// recordAgentHelperTx records a new thread as a helper of the assistant
// t.HelperOfAgentID names, if any. A created thread and a fork take it; a
// thread another computer sent arrives as no assistant's. The thread must
// be outside every project, and the assistant here with no delete committed
// on its current thread. A spawn admitted before a delete claimed the
// assistant records its helper, which the delete claims when it reads the
// family again after draining that admission.
func recordAgentHelperTx(tx *sql.Tx, t Thread) error {
	if t.HelperOfAgentID == "" {
		return nil
	}
	action := fmt.Sprintf("record %s as a helper of assistant %s", t.ID, t.HelperOfAgentID)
	result, err := tx.Exec(`INSERT INTO agent_helpers (thread_id, agent_id)
		SELECT threads.id, agents.id FROM threads, agents JOIN threads AS current ON current.id = agents.thread_id
		 WHERE threads.id = ?1 AND threads.project_id IS NULL AND agents.id = ?2 AND current.deleting = 0`,
		t.ID, t.HelperOfAgentID)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if inserted == 1 {
		return nil
	}
	var inProject bool
	if err := tx.QueryRow(`SELECT project_id IS NOT NULL FROM threads WHERE id = ?`, t.ID).Scan(&inProject); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if inProject {
		return fmt.Errorf("%s: %w", action, ErrHelperInProject)
	}
	return fmt.Errorf("%s: %w", action, ErrHelperOfNoAssistant)
}

func insertThread(tx *sql.Tx, t Thread, lastReadAtArg any) error {
	return writeThread(tx, t, lastReadAtArg, "")
}

// Validate group ownership in the insert so deletion cannot race a prior read.
func writeThread(tx *sql.Tx, t Thread, lastReadAtArg any, conflict string) error {
	result, err := tx.Exec(
		threadInsertSQL+conflict,
		t.ID, nilIfEmpty(t.ProjectID), t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.PendingForkResumeAt, boolToInt(t.ForkPreparing),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived), lastReadAtArg, t.ImportSource,
		// The write-once creation facts. They appear here and in
		// threadColumns, and deliberately NOT in updateThreadSetSQL: a
		// whole-row UpdateThread carrying a stale copy must not be able to
		// blank a thread's provenance or its git origin.
		t.CreatedByDevice, t.Origin.Branch, "", t.Origin.HeadCommit,
		nilIfEmpty(t.GroupID), insertedPlacement(t.GroupID),
		t.SpawnInstructions, t.SpawnLevel,
		t.GroupID, t.GroupID, t.ProjectID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: insert thread rows affected: %w", err)
	}
	if affected == 0 {
		return ErrThreadGroupGone
	}
	// Titles are searchable, so every path that writes a thread row writes
	// its title index entry with it: creation, the transfer import, and the
	// activation upsert that replaces a received thread.
	return indexThreadTitleTx(tx, t.ID, t.Title)
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.reader().QueryRow(
		`SELECT `+threadColumns+` FROM threads WHERE id = ?`, id,
	)
	t, err := scanThread(row)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	return t, nil
}

// GetOwnedThread is the public metadata read. A retained transfer cache is
// readable internally, but must not teach a reconnecting client that this
// computer still owns the conversation. Keep the ownership test in the row
// query so a concurrent retirement cannot land between a check and its read.
func (s *Store) GetOwnedThread(id string) (Thread, error) {
	row, err := scanThread(s.reader().QueryRow(`SELECT `+threadColumns+` FROM owned_threads AS threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		if ownershipErr := s.CheckThreadTransferAccess(id); ownershipErr != nil {
			return Thread{}, ownershipErr
		}
	}
	return row, err
}

// GetThreadProviderWorkspace reads only the provider and workspace_path
// columns. It is the narrow read for per-provider-event triage paths
// (tool completions, file-change results, inline diffs) that need one of
// these two effectively session-immutable scalars — GetThread's
// threadColumns projection computes four derived sidebar-state subqueries
// per call, which those hot paths would compute and throw away.
func (s *Store) GetThreadProviderWorkspace(id string) (provider, workspacePath string, err error) {
	err = s.reader().QueryRow(
		`SELECT provider, workspace_path FROM threads WHERE id = ?`, id,
	).Scan(&provider, &workspacePath)
	if err != nil {
		return "", "", fmt.Errorf("store: get thread provider/workspace %s: %w", id, err)
	}
	return provider, workspacePath, nil
}

// ThreadNotificationFacts is what an OS notification may learn about a
// thread: its label, and whether the sidebar lists it (Mode, judged by
// threadmode.IsHidden). Exists is false for a thread that is gone.
type ThreadNotificationFacts struct {
	Title string
	Mode  string
	// Attention is the kind of the thread's attention flag while it is
	// pending (Thread.AttentionPending), empty otherwise: one of a few
	// fixed values, never the agent's reason. AttentionAt is when that
	// flag was raised.
	Attention   string
	AttentionAt int64
	// Owned is a thread the thread that spawned it owns
	// (PlacementOwned): it has no sidebar row, and its answer goes to that
	// thread rather than to the user.
	Owned  bool
	Exists bool
}

// GetThreadNotificationFacts reads only the title, mode, placement and
// pending attention flag. It is the narrow read for the OS-notification mapping,
// which is allowed to say a thread's title and to know whether the thread
// is on the sidebar, and nothing more about it — and it exists for the same reason
// GetThreadProviderWorkspace does: GetThread's projection computes four
// derived sidebar-state subqueries per call that such a caller would compute
// and throw away.
//
// A thread that is gone answers Exists=false with no error. The caller is
// reacting to an event about a thread that may since have been deleted, and
// a deleted thread is not a failure to report — it is a thread the sidebar
// cannot show, which the notification gate treats like any other hidden one.
func (s *Store) GetThreadNotificationFacts(id string) (ThreadNotificationFacts, error) {
	var (
		facts       ThreadNotificationFacts
		kind        string
		attentionAt sql.NullInt64
		lastReadAt  sql.NullInt64
	)
	err := s.reader().QueryRow(`SELECT title, mode, placement = '`+PlacementOwned+`', attention_kind, attention_at, last_read_at
	  FROM threads WHERE id = ?`, id).Scan(&facts.Title, &facts.Mode, &facts.Owned, &kind, &attentionAt, &lastReadAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadNotificationFacts{}, nil
	}
	if err != nil {
		return ThreadNotificationFacts{}, fmt.Errorf("store: get thread notification facts %s: %w", id, err)
	}
	var marker *int64
	if lastReadAt.Valid {
		marker = &lastReadAt.Int64
	}
	if attentionAt.Valid && attentionPending(attentionAt.Int64, marker) {
		facts.Attention, facts.AttentionAt = kind, attentionAt.Int64
	}
	facts.Exists = true
	return facts, nil
}

// ThreadExists reports whether a thread row is still present. It is the narrow
// probe for the callers that hold a thread id from a table with no threads
// foreign key (workflow run records) and need to know whether the pointer is
// still live without materializing the row.
func (s *Store) ThreadExists(id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var one int
	err := s.reader().QueryRow(`SELECT 1 FROM threads WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: thread exists %s: %w", id, err)
	}
	return true, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.reader().Query(listThreadsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// ListThreadsWithItems returns every non-archived thread that has at least
// one persisted item. Local placeholder panes do not create backend rows; once
// a draft has real persisted content (typed text, attachments, terminal chips,
// or a pending plan implementation) it is visible so the sidebar reflects work
// the user would otherwise lose track of. Terminal-mode threads are always
// included: they are first-class sidebar fixtures that never carry items or
// drafts, so the item/draft gates would otherwise hide them. A hidden-mode
// or owned thread is never listed (catalogThreadClause).
func (s *Store) ListThreadsWithItems() ([]Thread, error) {
	query, args := listThreadsWithItemsQuery()
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads with items: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// ListArchivedThreads returns every archived thread the sidebar would list
// unarchived (catalogThreadClause), newest-touched first. Used by the
// settings panel to surface threads that have been hidden from the sidebar
// so the user can unarchive or permanently delete them; an archived owned
// thread is its owner's to show.
func (s *Store) ListArchivedThreads() ([]Thread, error) {
	query, args := listArchivedThreadsQuery()
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list archived threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan archived thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// ListThreadsByProject returns all non-archived threads belonging to a
// project, newest-touched first. Used by the sidebar to render the threads
// nested under a project row.
func (s *Store) ListThreadsByProject(projectID string) ([]Thread, error) {
	query, args := listProjectThreadsQuery(projectID)
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan project thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// catalogThreadClause is the one rule for which threads the sidebar's
// catalog reads list (ListThreadsWithItems, the project thread counts and
// ListArchivedThreads), over the threads alias: not a hidden mode, and not
// owned by the thread that spawned it (thread_placement.go). Search and the
// thread tools read owned threads like any other and apply
// hiddenThreadModesClause alone.
func catalogThreadClause(alias string) (string, []any) {
	hidden, args := hiddenThreadModesClause(alias + ".mode")
	return hidden + " AND " + alias + ".placement <> '" + PlacementOwned + "'", args
}

func hiddenThreadModesClause(column string) (string, []any) {
	modes := threadmode.HiddenModes()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(modes)), ",")
	args := make([]any, len(modes))
	for index, mode := range modes {
		args[index] = mode
	}
	return column + " NOT IN (" + placeholders + ")", args
}

// ListThreadWorkspaceRefs returns workspace/worktree pointers for all thread
// rows, including archived ones. Worktree removal uses this to avoid deleting a
// checkout that an archived thread would reference if restored.
func (s *Store) ListThreadWorkspaceRefs() ([]ThreadWorkspaceRef, error) {
	refs, err := s.queryThreadWorkspaceRefs(`SELECT ` + threadWorkspaceRefColumns + ` FROM threads t`)
	if err != nil {
		return nil, fmt.Errorf("store: list thread workspace refs: %w", err)
	}
	return refs, nil
}

// ListThreadWorkspaceRefsByProject is ListThreadWorkspaceRefs for the rows of
// one project, archived and hidden rows included: a checkout that vanished
// under an archived thread must be reattached before the thread is restored.
func (s *Store) ListThreadWorkspaceRefsByProject(projectID string) ([]ThreadWorkspaceRef, error) {
	refs, err := s.queryThreadWorkspaceRefs(`SELECT `+threadWorkspaceRefColumns+` FROM threads t WHERE t.project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list thread workspace refs for project %s: %w", projectID, err)
	}
	return refs, nil
}

// threadWorkspaceRefColumns is a ThreadWorkspaceRef's columns of the
// threads row aliased t, in queryThreadWorkspaceRefs' scan order.
const threadWorkspaceRefColumns = `t.id, t.workspace_path, COALESCE(t.worktree_path, '')`

// queryThreadWorkspaceRefs runs a query selecting threadWorkspaceRefColumns
// and scans its rows.
func (s *Store) queryThreadWorkspaceRefs(query string, args ...any) (refs []ThreadWorkspaceRef, err error) {
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var ref ThreadWorkspaceRef
		if err := rows.Scan(&ref.ID, &ref.WorkspacePath, &ref.WorktreePath); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ListThreadWorkspacePaths returns every distinct workspace_path spelling a
// thread row holds, archived rows included. It exists for one caller,
// `threadapp.UpdateBranch`, which resolves each spelling against the
// directory it is about to re-branch so `UpdateBranchForWorkspace` can stay
// an exact match. Distinct spellings number the workspaces, not the threads.
func (s *Store) ListThreadWorkspacePaths() ([]string, error) {
	rows, err := s.reader().Query(`SELECT DISTINCT workspace_path FROM threads WHERE workspace_path != ''`)
	if err != nil {
		return nil, fmt.Errorf("store: list thread workspace paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("store: scan thread workspace path: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// threadBusyPredicateSQL is the persisted "this thread is doing work in its
// checkout right now" test, correlated on `t` (the `threads` row) so any query
// over threads can drop it straight into a WHERE clause. Three signals: an
// open turn, a live background launch, or a live Codex subagent whose child
// thread is still running.
//
// It is deliberately the same shape App.threadActivityBlockReason evaluates
// per thread while holding that thread's lock before removing a worktree. The
// affordance that greys the destructive action out and the refusal that
// rejects it must be computed from ONE predicate; two spellings of "busy" are
// two things to keep in sync, and the direction they drift in is a button
// that stays enabled over a running agent.
const threadBusyPredicateSQL = `(
     COALESCE((
       SELECT latest.completed_at IS NULL
         FROM turns latest
        WHERE latest.thread_id = t.id
        ORDER BY latest.turn_index DESC
        LIMIT 1
     ), 0)
     OR EXISTS(
       SELECT 1
         FROM items launch INDEXED BY idx_items_live_background
        WHERE launch.thread_id = t.id
          AND launch.kind = 'tool_call'
          AND launch.status = 'running'
          AND launch.is_background = 1
          AND launch.parent_id = ''
          AND COALESCE(json_extract(launch.meta, '$.live_background_active'), 1) != 0
          AND NOT EXISTS(
            SELECT 1 FROM items completion INDEXED BY idx_items_completion_of
             WHERE completion.thread_id = launch.thread_id
               AND completion.completion_of = launch.id
               AND completion.completion_of <> ''
               AND completion.status <> 'parked'
          )
     )
   )`

const blockedThreadWorkspaceRefsSQL = `SELECT ` + threadWorkspaceRefColumns + `
  FROM threads t
 WHERE ` + threadBusyPredicateSQL

// ListBlockedThreadWorkspaceRefs returns workspace pointers for every thread —
// any project, archived included — whose persisted activity currently blocks
// moving or deleting the checkout it points at.
//
// Unscoped on purpose. The removal gate it feeds (App.threadsReferencingWorkspace
// → removeProjectWorktree) matches paths across every project, because a
// directory does not stop being in use when a second project row also names
// it; a project-scoped answer would leave the affordance enabled over work the
// backend then refuses. The scan is affordable precisely because the predicate
// is selective: it is three index probes per thread row and the result set is
// the handful of threads that are busy right now, not the history.
func (s *Store) ListBlockedThreadWorkspaceRefs() ([]ThreadWorkspaceRef, error) {
	refs, err := s.queryThreadWorkspaceRefs(blockedThreadWorkspaceRefsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: list blocked thread workspace refs: %w", err)
	}
	return refs, nil
}

// updateThreadSetSQL is UpdateThread's whole-row thread write. No production
// writer uses it: each change has a narrow writer (UpdateTitle,
// UpdateThreadModelSettings, UpdateThreadCheckout, ArchiveThread, ...),
// because every column in this list is one a stale read can roll back. It
// is a hand-kept column list, and what it OMITS is load-bearing.
// Columns owned by a narrow writer, by a trigger, or by the row's own
// lifecycle are therefore absent — see
// threadColumnsNotWrittenByUpdateThread (threads_test.go), which names each
// one and why, and whose TestUpdateThreadColumnGate forces every column into
// exactly one of the two lists.
const updateThreadSetSQL = `UPDATE threads SET project_id=?, title=?, provider=?, model=?,
    workspace_path=?, worktree_path=?, branch=?, session_ref=?,
    mode=?, reasoning_effort=?, fast_mode=?, context_window=?,
    auto_compact_standard_percent=?, auto_compact_extended_percent=?, runtime_mode=?,
    forked_from_thread_id=?, last_token_usage=?`

func normalizeThreadForUpdate(t Thread) (Thread, error) {
	t.Mode = normalizeMode(t.Mode)
	t.RuntimeMode = normalizeRuntimeMode(t.RuntimeMode)
	t.ReasoningEffort = normalizeEffort(t.ReasoningEffort)
	if !legalEffortForProvider(t.Provider, t.ReasoningEffort) {
		return Thread{}, fmt.Errorf("%w: %s/%s", ErrInvalidEffort, t.Provider, t.ReasoningEffort)
	}
	if t.ContextWindow == 0 {
		t.ContextWindow = 1000000
	}
	if !validContextWindow(t.ContextWindow) {
		return Thread{}, fmt.Errorf("%w: %d", ErrInvalidContextWindow, t.ContextWindow)
	}
	if !validAutoCompactPercent(t.AutoCompactStandardPercent) {
		return Thread{}, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactStandardPercent)
	}
	if !validAutoCompactPercent(t.AutoCompactExtendedPercent) {
		return Thread{}, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactExtendedPercent)
	}
	return t, nil
}

func updateThreadArgs(t Thread) []any {
	return []any{
		nilIfEmpty(t.ProjectID), t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
	}
}

// UpdateThread writes the columns updateThreadSetSQL lists from t. It keeps
// an assistant's invariants as the narrow writers do: the title of its
// current thread is its name (checkAssistantTitleTx), and that thread stays
// in its home (ErrAssistantThreadNotInHome).
func (s *Store) UpdateThread(t Thread) error {
	t, err := normalizeThreadForUpdate(t)
	if err != nil {
		return err
	}
	args := append(updateThreadArgs(t), t.ID)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update thread %s: %w", t.ID, err)
	}
	defer tx.Rollback()
	if err := checkAssistantTitleTx(tx, t.ID, t.Title); err != nil {
		return err
	}
	result, err := tx.Exec(
		updateThreadSetSQL+` WHERE id=?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update thread %s", t.ID)); err != nil {
		return err
	}
	var outOfHome bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents JOIN threads ON threads.id = agents.thread_id
		WHERE agents.thread_id = ? AND NOT (threads.project_id IS NULL AND threads.workspace_path = agents.home
		  AND COALESCE(threads.worktree_path, '') = '' AND COALESCE(threads.branch, '') = ''))`, t.ID).Scan(&outOfHome); err != nil {
		return fmt.Errorf("store: update thread %s: read its assistant's home: %w", t.ID, err)
	}
	if outOfHome {
		return fmt.Errorf("store: update thread %s: %w", t.ID, ErrAssistantThreadNotInHome)
	}
	// The whole-row update carries the title, so it can rename a thread.
	if err := indexThreadTitleTx(tx, t.ID, t.Title); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update thread %s: %w", t.ID, err)
	}
	return nil
}

// ThreadModelSettings is a thread's model configuration: what a model or
// provider choice and the sanitation of stored settings write
// (UpdateThreadModelSettings), and nothing a stale read could use to roll
// back a rename, a move or an archive.
type ThreadModelSettings struct {
	Provider                   string
	Model                      string
	ReasoningEffort            string
	FastMode                   bool
	ContextWindow              int
	AutoCompactStandardPercent int
	AutoCompactExtendedPercent int
	RuntimeMode                string
}

// ModelSettings is the thread's model configuration.
func (t Thread) ModelSettings() ThreadModelSettings {
	return ThreadModelSettings{
		Provider: t.Provider, Model: t.Model, ReasoningEffort: t.ReasoningEffort, FastMode: t.FastMode,
		ContextWindow: t.ContextWindow, AutoCompactStandardPercent: t.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: t.AutoCompactExtendedPercent, RuntimeMode: t.RuntimeMode,
	}
}

// UpdateThreadModelSettings writes a thread's model configuration, provided
// its provider is still previousProvider. A change of provider is allowed
// only while the thread has no history (ErrThreadProviderLocked), and
// discards the previous provider's resume wiring, the session ref and the
// lazy-fork pin, in the same UPDATE: a second statement would leave a crash
// window where the switch committed and a pin into the old provider's
// session files survived it.
func (s *Store) UpdateThreadModelSettings(threadID, previousProvider string, settings ThreadModelSettings) error {
	normalized, err := normalizeThreadForUpdate(Thread{
		Provider: settings.Provider, Model: settings.Model, ReasoningEffort: settings.ReasoningEffort,
		FastMode: settings.FastMode, ContextWindow: settings.ContextWindow,
		AutoCompactStandardPercent: settings.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: settings.AutoCompactExtendedPercent, RuntimeMode: settings.RuntimeMode,
	})
	if err != nil {
		return err
	}
	settings = normalized.ModelSettings()
	query := `UPDATE threads SET provider = ?, model = ?, reasoning_effort = ?, fast_mode = ?, context_window = ?,
	    auto_compact_standard_percent = ?, auto_compact_extended_percent = ?, runtime_mode = ?`
	guard := ` WHERE id = ? AND provider = ?`
	if settings.Provider != previousProvider {
		query += `, session_ref = NULL, pending_fork_session_ref = NULL, pending_fork_resume_at = ''`
		guard += ` AND NOT EXISTS (SELECT 1 FROM timeline_items WHERE thread_id = threads.id LIMIT 1)`
	}
	result, err := s.db.Exec(query+guard,
		settings.Provider, settings.Model, settings.ReasoningEffort, boolToInt(settings.FastMode), settings.ContextWindow,
		settings.AutoCompactStandardPercent, settings.AutoCompactExtendedPercent, settings.RuntimeMode,
		threadID, previousProvider)
	if err != nil {
		return fmt.Errorf("store: update the model settings of thread %s: %w", threadID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update the model settings of thread %s: rows affected: %w", threadID, err)
	}
	if rows != 0 {
		return nil
	}
	return s.explainProviderSwitchNoRows(threadID, previousProvider)
}

func (s *Store) explainProviderSwitchNoRows(threadID, previousProvider string) error {
	var currentProvider string
	var hasItems int
	err := s.reader().QueryRow(
		`SELECT provider,
		        EXISTS(SELECT 1 FROM timeline_items WHERE thread_id = threads.id LIMIT 1)
		   FROM threads
		  WHERE id = ?`,
		threadID,
	).Scan(&currentProvider, &hasItems)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: guarded provider switch for thread %s: %w", threadID, sql.ErrNoRows)
		}
		return fmt.Errorf("store: inspect guarded provider switch for thread %s: %w", threadID, err)
	}
	if hasItems != 0 {
		return fmt.Errorf("%w: %s", ErrThreadProviderLocked, currentProvider)
	}
	return fmt.Errorf("store: guarded provider switch for thread %s: %w", threadID, sql.ErrNoRows)
}

// ArchiveThread flips the archived column to 1 and bumps updated_at so the
// thread leaves the active sidebar. Returns the archived row plus whether the
// write moved anything: re-archiving an already-archived thread changes
// nothing, so it must not bump updated_at and must not broadcast, though
// its archive is marked unfinished again (setThreadArchivedTx). A missing
// id is still sql.ErrNoRows.
func (s *Store) ArchiveThread(id string) (Thread, bool, error) {
	return s.setThreadArchived(id, true, workerFence{})
}

// UnarchiveThread flips the archived column back to 0 for a thread and bumps
// updated_at so the sidebar reshuffles it toward the top of the active list.
// Returns the restored row plus whether the write moved anything; a thread
// that was already active is a no-op, not a reshuffle. Returns sql.ErrNoRows
// if no row matches the id.
func (s *Store) UnarchiveThread(id string) (Thread, bool, error) {
	return s.setThreadArchived(id, false, workerFence{})
}

// MarkThreadActivity bumps threads.updated_at to `at`. Sidebar sort and
// the per-row "last activity" display both key off this column, so the
// helper is the explicit one-stop point for "a meaningful interaction
// just happened on this thread." Triage calls it on user_text persist,
// turn settlement, and approval / user-input request creation; nothing
// else should touch updated_at on a live thread.
//
// Per-row writes (item upserts, streaming summary appends) deliberately
// do NOT bump activity — that used to make the sidebar reshuffle on
// every assistant token. Passive bookkeeping (token-usage cache, model
// rename, branch swap) also leaves activity alone.
//
// Monotonic guard: only the largest seen timestamp wins. Late-arriving
// stale events (e.g. a synthesized turn-complete that races a real
// one) cannot pull activity backward.
func (s *Store) MarkThreadActivity(threadID string, at int64) error {
	if threadID == "" {
		return fmt.Errorf("store: mark thread activity: thread id is required")
	}
	_, err := s.db.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ? AND updated_at < ?`,
		at, threadID, at,
	)
	if err != nil {
		return fmt.Errorf("store: mark thread activity %s: %w", threadID, err)
	}
	return nil
}

// threadReadTargetColumns are the four timestamps a read stamp clamps to,
// over a `threads` row: the newest completed turn, an interrupted newest
// turn's start, the newest turn error that lights the Failed pill, and the
// attention flag its agent raised. The error stamp is the column
// hasFailedTurn compares and the flag shows while the marker is before its
// time, so a pill and the read that clears it cannot disagree.
const threadReadTargetColumns = `
    (SELECT MAX(completed_at) FROM turns
      WHERE thread_id = threads.id AND completed_at IS NOT NULL),
    (SELECT CASE WHEN completed_at IS NULL THEN started_at END
       FROM turns
      WHERE thread_id = threads.id
      ORDER BY turns.turn_index DESC
      LIMIT 1),
    newest_turn_error_at,
    attention_at`

// threadReadStateQuery is the per-row read markThreadsReadSQL makes, as a
// single-row probe the plan and page-count guards measure.
const threadReadStateQuery = `SELECT ` + threadReadTargetColumns + `,
    last_read_at
   FROM threads
  WHERE id = ?`

// ErrThreadReadRefused is a read stamp on one thread that is gone or that
// this computer may not change (MarkThreadsReadNow's refused list). It
// wraps sql.ErrNoRows.
var ErrThreadReadRefused = fmt.Errorf("thread is gone or not changeable on this computer: %w", sql.ErrNoRows)

// MarkThreadsReadLimit caps one MarkThreadsReadNow call. A caller with more
// threads splits them into calls of at most this many.
const MarkThreadsReadLimit = 500

// markThreadsReadSQL stamps every named thread that is still owed a read,
// in one statement, and returns the ids it stamped. ?1 is the JSON array of
// ids, ?2 the current unix-ms. Each stamp is the later of now and the
// thread's newest read target (threadReadTargetColumns): provider
// timestamps can run ahead of the wall clock, and a stamp short of the
// target would leave the pill on. A thread is owed a read unless its marker
// already covers that target, or, with no target, covers now. Only a thread
// this computer may change is stamped: not a holder, not mid fork
// preparation, and not refused by its transfer state
// (threadTransferReadableExpr), the same rows CheckThreadExecutionAccess
// passes but for the provider session's native transfer, which governs
// execution, not read state.
var markThreadsReadSQL = `UPDATE threads SET last_read_at = MAX(?2, marks.target)
  FROM (WITH targets(id, marker, completed, started, errored, flagged) AS (
          SELECT id, last_read_at,` + threadReadTargetColumns + `
            FROM threads
           WHERE id IN (SELECT value FROM json_each(?1)) AND ` + threadReadMutableSQL + `
        )
        SELECT id, marker, MAX(COALESCE(completed, -1), COALESCE(started, -1), COALESCE(errored, -1), COALESCE(flagged, -1)) AS target
          FROM targets) AS marks
 WHERE threads.id = marks.id
   AND (marks.marker IS NULL OR (marks.target >= 0 AND marks.marker < marks.target) OR (marks.target < 0 AND marks.marker < ?2))
RETURNING id`

// threadReadMutableSQL is markThreadsReadSQL's predicate for a `threads`
// row this computer may stamp read.
var threadReadMutableSQL = `mode <> ` + sqlTextLiteral(threadmode.ModeHolder) + ` AND fork_preparing = 0
     AND ` + threadTransferReadableExpr("threads.")

// markThreadsReadRefusedSQL lists the ids of ?1 markThreadsReadSQL could
// not stamp: gone, or failing threadReadMutableSQL.
var markThreadsReadRefusedSQL = `SELECT value FROM json_each(?1)
 WHERE value NOT IN (SELECT id FROM threads
                      WHERE id IN (SELECT value FROM json_each(?1)) AND ` + threadReadMutableSQL + `)
 ORDER BY value`

// threadRowsByJSONIDsSQL reads the thread rows whose ids are in the JSON
// array ?1, one statement text for any number of ids.
var threadRowsByJSONIDsSQL = `SELECT ` + threadColumns + ` FROM threads WHERE id IN (SELECT value FROM json_each(?1)) ORDER BY id`

// MarkThreadReadNow stamps last_read_at with the current unix-ms, clamped to
// the latest sidebar read target (markThreadsReadSQL). Completed turns key
// off completed_at; an interrupted newest turn keys off started_at so
// opening it clears the durable Interrupted pill just like opening a
// completed thread clears Completed.
//
// Does NOT bump updated_at: read-state is UI bookkeeping, not a thread
// mutation, and bumping would thrash the sidebar ordering.
//
// The context is required rather than optional. This is a write, so it
// runs on the single writer connection, and database/sql makes a caller
// wait for that connection to come free: behind a retention sweep's
// delete batch, a streaming flush, or a checkpoint. A context-less
// Begin waits for that with no ceiling; callers of a bookkeeping write
// nobody is watching need one.
//
// Returns the stamped row plus whether the stamp moved. Opening a thread
// whose read marker already covers its newest turn is the common case and
// changes nothing, so it hands back (zero row, false, nil) and the caller
// broadcasts nothing. A thread that is gone, or that this computer may not
// change, is ErrThreadReadRefused.
func (s *Store) MarkThreadReadNow(ctx context.Context, id string) (Thread, bool, error) {
	rows, refused, err := s.MarkThreadsReadNow(ctx, []string{id})
	if err != nil {
		return Thread{}, false, err
	}
	if len(refused) > 0 {
		return Thread{}, false, fmt.Errorf("store: mark thread read %s: %w", id, ErrThreadReadRefused)
	}
	if len(rows) == 0 {
		return Thread{}, false, nil
	}
	return rows[0], true, nil
}

// MarkThreadsReadNow is MarkThreadReadNow for up to MarkThreadsReadLimit
// threads in one transaction of a fixed number of statements: the stamp
// (markThreadsReadSQL), the changed rows read back, and the ids it could not
// stamp. Repeated ids count once. It returns the rows it changed, in id
// order, and refused: each named thread that is gone or that this computer
// may not change. A thread whose marker was already current is in neither.
func (s *Store) MarkThreadsReadNow(ctx context.Context, ids []string) (changed []Thread, refused []string, err error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) > MarkThreadsReadLimit {
		return nil, nil, fmt.Errorf("store: mark %d threads read: at most %d in one call", len(ids), MarkThreadsReadLimit)
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, nil, fmt.Errorf("store: mark %d threads read: encode ids: %w", len(ids), err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("store: begin mark %d threads read: %w", len(ids), err)
	}
	defer func() { _ = tx.Rollback() }()
	stamped, err := queryStringsTx(tx, markThreadsReadSQL, string(encoded), nowMillis())
	if err != nil {
		return nil, nil, fmt.Errorf("store: mark %d threads read: %w", len(ids), err)
	}
	if len(stamped) > 0 {
		stampedJSON, err := json.Marshal(stamped)
		if err != nil {
			return nil, nil, fmt.Errorf("store: mark %d threads read: encode stamped ids: %w", len(ids), err)
		}
		if changed, err = scanThreadQuery(tx, threadRowsByJSONIDsSQL, string(stampedJSON)); err != nil {
			return nil, nil, fmt.Errorf("store: read back %d threads marked read: %w", len(stamped), err)
		}
		if len(changed) != len(stamped) {
			return nil, nil, fmt.Errorf("store: read back %d threads marked read: %d rows", len(stamped), len(changed))
		}
	}
	if refused, err = queryStringsTx(tx, markThreadsReadRefusedSQL, string(encoded)); err != nil {
		return nil, nil, fmt.Errorf("store: mark %d threads read: list refused: %w", len(ids), err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("store: commit mark %d threads read: %w", len(ids), err)
	}
	return changed, refused, nil
}

// MarkThreadReadThrough stamps last_read_at up to at, a turn's completion,
// and never backward: the read mark a spawned thread gets when its turn
// answers its spawner (docs/specs/standing-agents.md ruling 30). Unlike
// MarkThreadReadNow it reads nothing later than at, so a turn the user
// caused after the one that answered still shows as unread. A pinned
// thread keeps its dot, so a pin that lands after the caller decided to
// mark still wins, and so does a pending attention flag: the thread's
// agent asked for the user, which the answer reaching its spawner does not
// settle. A row whose marker already covers at (NULL, "never tracked",
// included: that reads as read), that is pinned, flagged or gone changes
// nothing and hands back (zero row, false, nil). The context bounds the
// wait for the writer connection, as MarkThreadReadNow's does.
func (s *Store) MarkThreadReadThrough(ctx context.Context, id string, at int64) (Thread, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: begin mark thread read through %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx,
		`UPDATE threads SET last_read_at = ? WHERE id = ? AND last_read_at IS NOT NULL AND last_read_at < ? AND pinned_at IS NULL
		   AND (attention_at IS NULL OR attention_at <= last_read_at)`, at, id, at)
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: mark thread read through %s: %w", id, err)
	}
	changed, err := rowsChanged(result, fmt.Sprintf("store: mark thread read through %s", id))
	if err != nil {
		return Thread{}, false, err
	}
	if !changed {
		if err := tx.Commit(); err != nil {
			return Thread{}, false, fmt.Errorf("store: commit mark thread read through no-op %s: %w", id, err)
		}
		return Thread{}, false, nil
	}
	rows, err := listThreadsByIDTx(tx, []string{id})
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: read back mark thread read through %s: %w", id, err)
	}
	if len(rows) != 1 {
		return Thread{}, false, fmt.Errorf("store: read back mark thread read through %s: %d rows, want 1", id, len(rows))
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, false, fmt.Errorf("store: commit mark thread read through %s: %w", id, err)
	}
	return rows[0], true, nil
}

// MarkThreadUnread stamps last_read_at to zero. NULL is reserved for
// "never tracked" and is treated as read by the frontend so old rows do not
// light up on first launch; an explicit unread action needs a concrete value
// that is older than every real thread update.
//
// Returns the stamped row plus whether the write moved anything: a thread
// already marked unread stays as it is and broadcasts nothing.
func (s *Store) MarkThreadUnread(id string) (Thread, bool, error) {
	var zero int64
	return s.setThreadLastRead(id, &zero)
}

// setThreadLastRead is the shared primitive. Kept unexported — callers
// should use the named MarkThreadReadNow / MarkThreadUnread wrappers so
// the intent is visible at the call site.
func (s *Store) setThreadLastRead(id string, ts *int64) (Thread, bool, error) {
	var arg any
	if ts != nil {
		arg = *ts
	}
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: update last_read_at for %s", id),
		ID:         id,
		Set:        "last_read_at = ?",
		SetArgs:    []any{arg},
		Change:     "last_read_at IS NOT ?",
		ChangeArgs: []any{arg},
	})
}

// PinThread places the thread on the front burner and preserves the existing
// API contract of stamping pinned_at on every call. The timestamp remains
// metadata only; it no longer controls ordering within a pin group.
//
// Returns the pinned row; the changed flag is true whenever the row exists,
// because the restamp always moves pinned_at.
func (s *Store) PinThread(id string) (Thread, bool, error) {
	now := nowMillis()
	return s.setThreadPinnedAt(id, &now)
}

// UnpinThread clears both pin fields, returning the thread to the regular
// status-aware sort order. An unpinned row never retains a latent group.
// Unpinning an already-unpinned thread changes nothing and reports so.
func (s *Store) UnpinThread(id string) (Thread, bool, error) {
	return s.setThreadPinnedAt(id, nil)
}

// SetThreadPinGroup moves an already-pinned thread between the exact two
// manual attention groups. The WHERE clause makes assigning a group to an
// unpinned row impossible even for a future caller that skips prevalidation.
// An unpinned row is still refused with sql.ErrNoRows; a pinned row already
// in the requested group is a no-op that changes nothing.
func (s *Store) SetThreadPinGroup(id string, group int) (Thread, bool, error) {
	write, err := threadPinGroupWrite(id, group)
	if err != nil {
		return Thread{}, false, err
	}
	return s.applyThreadRowWrite(write)
}

// setThreadPinGroupTx is SetThreadPinGroup inside the caller's transaction.
func setThreadPinGroupTx(tx *sql.Tx, id string, group int) (Thread, bool, error) {
	write, err := threadPinGroupWrite(id, group)
	if err != nil {
		return Thread{}, false, err
	}
	return applyThreadRowWriteTx(tx, write)
}

func threadPinGroupWrite(id string, group int) (rowWrite, error) {
	if group != PinGroupFront && group != PinGroupBack {
		return rowWrite{}, fmt.Errorf("%w: %d", ErrInvalidPinGroup, group)
	}
	return rowWrite{
		Action:     fmt.Sprintf("store: update pin_group for pinned thread %s", id),
		ID:         id,
		Set:        "pin_group = ?",
		SetArgs:    []any{group},
		Match:      "pinned_at IS NOT NULL",
		Change:     "pin_group IS NOT ?",
		ChangeArgs: []any{group},
	}, nil
}

// setThreadPinnedAt is the shared pin/unpin primitive. We deliberately do NOT
// touch updated_at here: pinning is a sidebar-presentation tweak, not
// thread activity, and bumping updated_at would shuffle the project's
// `lastActivity` ordering.
func (s *Store) setThreadPinnedAt(id string, ts *int64) (Thread, bool, error) {
	return s.applyThreadRowWrite(threadPinnedAtWrite(id, ts))
}

// setThreadPinnedAtTx is setThreadPinnedAt inside the caller's transaction.
func setThreadPinnedAtTx(tx *sql.Tx, id string, ts *int64) (Thread, bool, error) {
	return applyThreadRowWriteTx(tx, threadPinnedAtWrite(id, ts))
}

func threadPinnedAtWrite(id string, ts *int64) rowWrite {
	write := rowWrite{
		Action: fmt.Sprintf("store: update pin state for %s", id),
		ID:     id,
	}
	if ts == nil {
		write.Set = "pinned_at = NULL, pin_group = NULL"
		write.Change = "pinned_at IS NOT NULL"
		return write
	}
	write.Set = "pinned_at = ?, pin_group = ?"
	write.SetArgs = []any{*ts, PinGroupFront}
	return write
}

// UpdateSessionRef records the provider resume cursor without touching
// updated_at. Provider init can happen during sidebar-driven auto-resume, and
// opening a thread must not count as new thread activity.
//
// The returned changed flag reports whether session_ref actually moved —
// callers gate the thread:updated push on it, so a resumed session restating
// the same ref on every init doesn't spam the frontend. The read-then-write
// is race-free in practice: all writers go through the single writer
// connection, and both callers (triage handleInit, the pre-send context
// repair) are serialized per thread.
func (s *Store) UpdateSessionRef(threadID, ref string) (changed bool, err error) {
	var prev sql.NullString
	if err := s.db.QueryRow(
		`SELECT session_ref FROM threads WHERE id = ?`, threadID,
	).Scan(&prev); err != nil {
		return false, fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	result, err := s.db.Exec(
		`UPDATE threads
		 SET session_ref = ?, pending_fork_session_ref = NULL, pending_fork_resume_at = ''
		 WHERE id = ?`,
		ref, threadID,
	)
	if err != nil {
		return false, fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update session ref for %s", threadID)); err != nil {
		return false, err
	}
	return prev.String != ref, nil
}

// SetThreadForkResume writes a new fork's provider resume wiring in one
// narrow UPDATE: the fork's own session ref (a Claude slice or a Codex
// thread/fork child) plus the one-shot lazy-fork pin
// (pending_fork_session_ref + pending_fork_resume_at, which the fork's first
// session start consumes together). Empty clears; the three are always
// written as a set, because a ref without its pin — or a pin without its ref
// — is not a resume state any reader can act on.
//
// It exists because the pin must not ride a whole-row write. The two columns
// are one-shot state the session-ref writers CONSUME, while fifteen callers
// hand UpdateThread a Thread struct they read some time ago in order to
// change one field; such a snapshot could resurrect a pin a session start had
// already cleared, or overwrite one a concurrent fork had just set. They are
// therefore absent from updateThreadSetSQL, like worktree_setup_state and
// live_todo, and this is their only writer outside CreateThread and those two
// clears.
//
// Like UpdateSessionRef it leaves updated_at alone: wiring resume state is
// system work and the sidebar sorts by updated_at.
func (s *Store) SetThreadForkResume(threadID, sessionRef, pendingForkRef, pinnedResumeAt string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("store: set thread fork resume: empty thread id")
	}
	result, err := s.db.Exec(
		`UPDATE threads
		 SET session_ref = ?, pending_fork_session_ref = ?, pending_fork_resume_at = ?
		 WHERE id = ?`,
		nilIfEmpty(sessionRef), nilIfEmpty(pendingForkRef), pinnedResumeAt, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: set fork resume for %s: %w", threadID, err)
	}
	// A fork row deleted underneath the saga is the one case where silence
	// would leave an unresumable fork looking wired, so it is named.
	return requireRowsAffected(result, fmt.Sprintf("store: set fork resume for %s", threadID))
}

// In-thread setters below intentionally do NOT bump updated_at. Title
// renames, model / mode / effort / fast-mode / context-window edits,
// branch / workspace-path swaps, token-usage refreshes, and runtime-mode
// flips are in-place mutations that should not move the sidebar.
// Sidebar activity is owned by Store.MarkThreadActivity, called from
// triage at user_text persist, turn settle, and approval / user-input
// request creation.

func (s *Store) UpdateTitle(threadID, title string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update title for %s: %w", threadID, err)
	}
	defer tx.Rollback()
	if err := updateTitleTx(tx, threadID, title); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update title for %s: %w", threadID, err)
	}
	return nil
}

// updateTitleTx writes the display name and its search index entry inside
// the caller's transaction. The two always move together.
func updateTitleTx(tx *sql.Tx, threadID, title string) error {
	// An assistant's current thread's title is its name (docs/specs/
	// standing-agents.md, "Standing agent"), unique on this computer.
	if err := checkAssistantTitleTx(tx, threadID, title); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE threads SET title = ? WHERE id = ?`,
		title, threadID)
	if err != nil {
		return fmt.Errorf("store: update title for %s: %w", threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update title for %s", threadID)); err != nil {
		return err
	}
	return indexThreadTitleTx(tx, threadID, title)
}

// UpdateProviderTitle writes a title a provider reported for its thread,
// and reports whether it did: a provider never renames an assistant.
func (s *Store) UpdateProviderTitle(threadID, title string) (_ bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin provider title for %s: %w", threadID, err)
	}
	defer rollbackAgentWakeTx(tx, "store: provider title for "+threadID, &err)
	var assistant bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agents WHERE thread_id = ?)`, threadID).Scan(&assistant); err != nil {
		return false, fmt.Errorf("store: read the assistant of %s: %w", threadID, err)
	}
	if assistant {
		return false, nil
	}
	if err := updateTitleTx(tx, threadID, title); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit provider title for %s: %w", threadID, err)
	}
	return true, nil
}

func (s *Store) UpdateTitleIfCurrent(threadID, currentTitle, newTitle string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkThreadTransferAccess(tx, threadID); err != nil {
		var transfer *ThreadTransferError
		if errors.As(err, &transfer) {
			return false, nil
		}
		return false, err
	}
	result, err := tx.Exec(
		// A generated title never renames an assistant.
		`UPDATE threads SET title = ? WHERE id = ? AND title = ?
		   AND NOT EXISTS (SELECT 1 FROM agents WHERE agents.thread_id = threads.id)`,
		newTitle, threadID, currentTitle,
	)
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title for %s: %w", threadID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title rows affected for %s: %w", threadID, err)
	}
	if rows > 0 {
		if err := indexThreadTitleTx(tx, threadID, newTitle); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) UpdateModel(threadID, model string) error {
	result, err := s.db.Exec(`UPDATE threads SET model = ? WHERE id = ?`,
		model, threadID)
	if err != nil {
		return fmt.Errorf("store: update model for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update model for %s", threadID))
}

// CompareAndSwapModelProfile replaces the model-related fields only when they
// still match before. Import refresh uses this to restore provider-recorded
// settings without overwriting a model selection the user made after the
// refresh plan was built.
func (s *Store) CompareAndSwapModelProfile(before, after Thread) (bool, error) {
	if before.ID == "" || after.ID != before.ID {
		return false, fmt.Errorf("store: compare-and-swap model profile requires one matching thread id")
	}
	if before.Provider != "" && after.Provider != "" && before.Provider != after.Provider {
		return false, fmt.Errorf("store: compare-and-swap model profile cannot change provider from %q to %q", before.Provider, after.Provider)
	}
	normalized, err := normalizeThreadForUpdate(after)
	if err != nil {
		return false, err
	}
	expectedProvider := before.Provider
	if expectedProvider == "" {
		expectedProvider = normalized.Provider
	}
	result, err := s.db.Exec(
		`UPDATE threads
		    SET model = ?, reasoning_effort = ?, fast_mode = ?, context_window = ?
		  WHERE id = ? AND provider = ? AND model = ? AND reasoning_effort = ?
		    AND fast_mode = ? AND context_window = ?`,
		normalized.Model, normalized.ReasoningEffort, boolToInt(normalized.FastMode), normalized.ContextWindow,
		before.ID, expectedProvider, before.Model, before.ReasoningEffort, boolToInt(before.FastMode), before.ContextWindow,
	)
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap model profile for %s: %w", before.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap model profile rows affected for %s: %w", before.ID, err)
	}
	return rows > 0, nil
}

func (s *Store) UpdateModelAndContextWindow(threadID, model string, tokens int) error {
	if !validContextWindow(tokens) {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	result, err := s.db.Exec(
		`UPDATE threads SET model = ?, context_window = ? WHERE id = ?`,
		model, tokens, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update model/context for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update model/context for %s", threadID))
}

func (s *Store) UpdateLastTokenUsage(threadID, usage string) error {
	result, err := s.db.Exec(
		`UPDATE threads SET last_token_usage = ? WHERE id = ?`,
		usage, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update last token usage for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update last token usage for %s", threadID))
}

func (s *Store) ClearLastTokenUsage(threadID string) error {
	return s.UpdateLastTokenUsage(threadID, "")
}

// UpdateMode overwrites the thread's mode (chat, plan, terminal, or a
// workflow-owned mode). Empty strings are normalized to "chat" to
// match CreateThread/UpdateThread. This is the permissive store
// primitive; user-driven toggles route through threadmode.ValidateSet,
// which restricts the reachable set to chat/plan.
func (s *Store) UpdateMode(threadID, mode string) error {
	mode = normalizeMode(mode)
	if !threadmode.IsLegal(mode) {
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
	result, err := s.db.Exec(`UPDATE threads SET mode = ? WHERE id = ?`,
		mode, threadID)
	if err != nil {
		return fmt.Errorf("store: update mode for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update mode for %s", threadID))
}

// UpdateReasoningEffort overwrites the effort tier and hands back the row it
// wrote. See legalEfforts for the enumerated values. Re-selecting the tier
// the thread already carries changes nothing and reports so.
func (s *Store) UpdateReasoningEffort(threadID, effort string) (Thread, bool, error) {
	normalized := normalizeEffort(effort)
	if _, ok := legalEfforts[normalized]; !ok {
		return Thread{}, false, fmt.Errorf("%w: %q", ErrInvalidEffort, effort)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: begin reasoning effort update %s: %w", threadID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var providerName, currentEffort string
	if err := tx.QueryRow(`SELECT provider, reasoning_effort FROM threads WHERE id = ?`, threadID).Scan(&providerName, &currentEffort); err != nil {
		return Thread{}, false, fmt.Errorf("store: load provider for effort update %s: %w", threadID, err)
	}
	if !legalEffortForProvider(providerName, normalized) {
		return Thread{}, false, fmt.Errorf("%w: %s/%s", ErrInvalidEffort, providerName, normalized)
	}
	changed := currentEffort != normalized
	if changed {
		if _, err := tx.Exec(`UPDATE threads SET reasoning_effort = ? WHERE id = ?`, normalized, threadID); err != nil {
			return Thread{}, false, fmt.Errorf("store: update reasoning effort for %s: %w", threadID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, false, fmt.Errorf("store: commit reasoning effort update %s: %w", threadID, err)
	}
	thread, err := s.GetThread(threadID)
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: reload thread after effort update %s: %w", threadID, err)
	}
	return thread, changed, nil
}

// UpdateFastMode flips the fast-mode boolean and hands back the row it wrote.
// Setting the value the thread already carries changes nothing.
func (s *Store) UpdateFastMode(threadID string, on bool) (Thread, bool, error) {
	value := boolToInt(on)
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: update fast mode for %s", threadID),
		ID:         threadID,
		Set:        "fast_mode = ?",
		SetArgs:    []any{value},
		Change:     "fast_mode IS NOT ?",
		ChangeArgs: []any{value},
	})
}

func (s *Store) GetThreadContextSettings(threadID string) (ThreadContextSettings, error) {
	var settings ThreadContextSettings
	err := s.reader().QueryRow(
		`SELECT provider, model, COALESCE(project_id, ''), context_window,
		        auto_compact_standard_percent, auto_compact_extended_percent
		   FROM threads
		  WHERE id = ?`,
		threadID,
	).Scan(
		&settings.Provider,
		&settings.Model,
		&settings.ProjectID,
		&settings.ContextWindow,
		&settings.AutoCompactStandardPercent,
		&settings.AutoCompactExtendedPercent,
	)
	if err != nil {
		return ThreadContextSettings{}, err
	}
	return settings, nil
}

// UpdateContextSettings overwrites the context window and both compaction
// threshold overrides. Percent zero means provider default/inherit.
// Hands back the row it wrote; a write that restates all three current
// values changes nothing and reports so.
func (s *Store) UpdateContextSettings(threadID string, tokens, standardPercent, extendedPercent int) (Thread, bool, error) {
	if !validContextWindow(tokens) {
		return Thread{}, false, fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	if !validAutoCompactPercent(standardPercent) {
		return Thread{}, false, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, standardPercent)
	}
	if !validAutoCompactPercent(extendedPercent) {
		return Thread{}, false, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, extendedPercent)
	}
	return s.applyThreadRowWrite(rowWrite{
		Action: fmt.Sprintf("store: update context settings for %s", threadID),
		ID:     threadID,
		Set: `context_window = ?,
		        auto_compact_standard_percent = ?,
		        auto_compact_extended_percent = ?`,
		SetArgs: []any{tokens, standardPercent, extendedPercent},
		Change: `(context_window IS NOT ?
		       OR auto_compact_standard_percent IS NOT ?
		       OR auto_compact_extended_percent IS NOT ?)`,
		ChangeArgs: []any{tokens, standardPercent, extendedPercent},
	})
}

// UpdateBranchForWorkspace persists a branch observed in a workspace onto
// every thread row currently sitting in that workspace, and returns those
// rows as they stand afterwards. The workspace arrives as every SPELLING of
// its directory the caller knows to be stored (see below).
//
// The branch is a fact about the CHECKOUT, not about one thread: several
// threads share a worktree routinely (project-root threads default to it,
// and "implement this plan in a new thread" inherits the source worktree),
// so a per-thread write leaves the others claiming a branch the working
// tree left behind. The workspace is the entity, so the workspace is what
// the write is keyed on.
//
// That keying is also the compare-and-swap the caller needs. The caller is
// the frontend's asynchronous branch-persist queue, which reads a branch off
// a gitwatch status for one workspace and writes it back a moment later,
// holding no lock — while a worktree switch (which takes threadLocks and
// rewrites workspace_path AND branch together) can land in between. Scoping
// the UPDATE to the workspace path means a thread that has since moved is
// simply not matched, so the stale observation cannot follow it.
//
// The match is an EXACT one against each spelling in `spellings`, never a
// prefix or a re-resolution per row: that is what keeps this a
// compare-and-swap on a directory rather than a scan. Thread rows store
// whichever spelling of a directory was current when they were created (a
// worktree cut through a symlinked path keeps that path; on macOS a row
// can hold `/var/...` where git answers `/private/var/...`), and the caller
// knows only its own, so the caller (`threadapp.UpdateBranch`) first asks
// `ListThreadWorkspacePaths` which stored spellings resolve to the same
// directory and hands ALL of them here. Two fixed spellings (the observed
// one and its canonical form) were not enough: a row under a third
// spelling kept a branch the working tree had left behind (2026-09-03,
// found on macOS). Duplicates and empties in `spellings` are dropped.
//
// Matching zero rows is a normal outcome, not an error: every thread may
// have left the workspace (or the last one was deleted) between the
// observation and the write.
//
// Returning zero rows is the COMMON outcome, and deliberately so. The caller
// writes on every attach — a pane mount, a thread switch, a reconnect — and
// the branch it observed almost always equals the one already cached, so the
// UPDATE excludes rows that would not change (`branch IS NOT ?`, null-safe,
// which is what makes the empty-string/NULL spelling work) and nothing is
// read back for them. An unconditional write plus a full workspace listing
// meant every attach paid the threadColumns projection — two correlated
// subqueries per row — to hand back rows nobody had changed.
//
// The read runs inside the write's transaction and is anchored on the ids
// the UPDATE returned, so the rows handed back are exactly the rows written:
// neither a concurrent writer nor a thread that already sat on this branch
// can widen the answer.
func (s *Store) UpdateBranchForWorkspace(spellings []string, branch string) ([]Thread, error) {
	keys := make([]string, 0, len(spellings))
	for _, spelling := range spellings {
		if spelling != "" && !slices.Contains(keys, spelling) {
			keys = append(keys, spelling)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("store: update branch: workspace path is required")
	}
	workspacePath := keys[0]

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin branch write for workspace %s: %w", workspacePath, err)
	}
	defer tx.Rollback()

	value := nilIfEmpty(branch)
	args := make([]any, 0, len(keys)+2)
	args = append(args, value)
	for _, key := range keys {
		args = append(args, key)
	}
	args = append(args, value)
	updated, err := tx.Query(
		`UPDATE threads SET branch = ?
		   WHERE workspace_path IN (`+placeholders(len(keys))+`) AND branch IS NOT ?
		 RETURNING id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: update branch for workspace %s: %w", workspacePath, err)
	}
	var ids []string
	for updated.Next() {
		var id string
		if err := updated.Scan(&id); err != nil {
			updated.Close()
			return nil, fmt.Errorf("store: scan updated branch row in %s: %w", workspacePath, err)
		}
		ids = append(ids, id)
	}
	if err := updated.Err(); err != nil {
		updated.Close()
		return nil, fmt.Errorf("store: iterate updated branch rows in %s: %w", workspacePath, err)
	}
	updated.Close()

	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit branch write for workspace %s: %w", workspacePath, err)
		}
		return nil, nil
	}

	threads, err := listThreadsByIDTx(tx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read back branch write for workspace %s: %w", workspacePath, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit branch write for workspace %s: %w", workspacePath, err)
	}
	return threads, nil
}

// listThreadsByIDTx reads the full thread projection for an explicit id set,
// inside the caller's transaction. Only used by writers that have to hand
// back exactly the rows they touched.
// scanThreadQuery runs a threadColumns query in tx and scans its rows.
func scanThreadQuery(tx *sql.Tx, query string, args ...any) ([]Thread, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		threads = append(threads, thread)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return threads, nil
}

func listThreadsByIDTx(tx *sql.Tx, ids []string) ([]Thread, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := tx.Query(
		`SELECT `+threadColumns+` FROM threads WHERE id IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threads := make([]Thread, 0, len(ids))
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// CheckoutWrite is what UpdateThreadCheckout moved.
type CheckoutWrite struct {
	// Row is the thread after the write.
	Row Thread
	// Changed reports that the workspace, worktree or branch moved.
	Changed bool
	// Listing reports whether binding or unbinding a worktree moved the
	// thread across the sidebar listing rule.
	Listing ListingChange
}

// UpdateThreadCheckout moves a thread to another checkout: it writes the
// workspace, worktree and branch and no other column, so a write to any other
// column that lands between the caller's read and this one survives.
func (s *Store) UpdateThreadCheckout(threadID, workspacePath, worktreePath, branch string) (CheckoutWrite, error) {
	action := fmt.Sprintf("store: update checkout for %s", threadID)
	args := []any{workspacePath, nilIfEmpty(worktreePath), nilIfEmpty(branch)}
	tx, err := s.db.Begin()
	if err != nil {
		return CheckoutWrite{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	before, err := threadListed(tx, threadID)
	if err != nil {
		return CheckoutWrite{}, err
	}
	row, changed, err := applyThreadRowWriteTx(tx, rowWrite{
		Action:     action,
		ID:         threadID,
		Set:        "workspace_path = ?, worktree_path = ?, branch = ?",
		SetArgs:    args,
		Change:     "(workspace_path IS NOT ? OR worktree_path IS NOT ? OR branch IS NOT ?)",
		ChangeArgs: args,
	})
	if err != nil {
		return CheckoutWrite{}, err
	}
	write := CheckoutWrite{Row: row, Changed: changed}
	if changed {
		after, err := threadListed(tx, threadID)
		if err != nil {
			return CheckoutWrite{}, err
		}
		write.Listing = listingChange(before, after)
	}
	if err := tx.Commit(); err != nil {
		return CheckoutWrite{}, fmt.Errorf("%s: commit: %w", action, err)
	}
	return write, nil
}

// RenameThreadBranch sets the thread's branch to `to` while it is still
// `from`, and writes no other column. A thread that moved to another branch
// since the caller read it is left alone and reports no change.
func (s *Store) RenameThreadBranch(threadID, from, to string) (Thread, bool, error) {
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: rename branch for %s", threadID),
		ID:         threadID,
		Set:        "branch = ?",
		SetArgs:    []any{nilIfEmpty(to)},
		Change:     "branch IS ? AND branch IS NOT ?",
		ChangeArgs: []any{nilIfEmpty(from), nilIfEmpty(to)},
	})
}

// UpdateWorkspacePath overwrites workspace_path. Used by the env/worktree
// picker when a thread switches between the project root and a worktree.
func (s *Store) UpdateWorkspacePath(threadID, path string) error {
	result, err := s.db.Exec(`UPDATE threads SET workspace_path = ? WHERE id = ?`,
		path, threadID)
	if err != nil {
		return fmt.Errorf("store: update workspace path for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update workspace path for %s", threadID))
}

// UpdateRuntimeMode overwrites the thread's runtime mode (read-only,
// approval-required, auto-accept-edits, auto, or full-access). Unknown values are coerced to the
// default via normalizeRuntimeMode — the CHECK constraint on the column
// would otherwise reject the write, so we prefer falling back silently over
// breaking a session restart for an old client that sent a stale string.
func (s *Store) UpdateRuntimeMode(threadID, mode string) error {
	mode = normalizeRuntimeMode(mode)
	result, err := s.db.Exec(`UPDATE threads SET runtime_mode = ? WHERE id = ?`,
		mode, threadID)
	if err != nil {
		return fmt.Errorf("store: update runtime mode for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update runtime mode for %s", threadID))
}

// SwapRuntimeMode sets the thread's runtime mode to next only while it is
// still from, and reports whether it did. A writer that follows the provider
// uses it so a mode the user picked meanwhile is never overwritten. A missing
// thread is an error; a row holding another mode is (false, nil).
func (s *Store) SwapRuntimeMode(threadID, from, next string) (bool, error) {
	from, next = normalizeRuntimeMode(from), normalizeRuntimeMode(next)
	result, err := s.db.Exec(`UPDATE threads SET runtime_mode = ?1 WHERE id = ?2 AND runtime_mode = ?3 AND runtime_mode IS NOT ?1`,
		next, threadID, from)
	if err != nil {
		return false, fmt.Errorf("store: swap runtime mode for %s: %w", threadID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: swap runtime mode for %s: %w", threadID, err)
	}
	if changed > 0 {
		return true, nil
	}
	if _, err := s.GetThread(threadID); err != nil {
		return false, fmt.Errorf("store: swap runtime mode for %s: %w", threadID, err)
	}
	return false, nil
}

// normalizeMode coerces empty strings to the schema default "chat" and
// returns anything else verbatim. Callers that need strict validation
// run the threadmode.IsLegal check separately.
func normalizeMode(mode string) string {
	if mode == "" {
		return "chat"
	}
	return mode
}

// normalizeEffort coerces empty strings to the schema default "high".
func normalizeEffort(effort string) string {
	if effort == "" {
		return "high"
	}
	return effort
}

// normalizeRuntimeMode coerces empty or unknown strings to the default
// runtime mode. Duplicated from provider.NormalizeRuntimeMode so that
// internal/store stays provider-free (import cycle avoidance). The literal
// value set is asserted against provider.AllRuntimeModes by
// TestRuntimeModeCheckMatchesProvider so the copy cannot drift.
func normalizeRuntimeMode(mode string) string {
	switch mode {
	case "read-only", "approval-required", "auto-accept-edits", "auto", "full-access":
		return mode
	default:
		return "full-access"
	}
}

// listArchivedThreadsQuery is the ListArchivedThreads statement and
// listProjectThreadsQuery the ListThreadsByProject statement. Each returns
// a part of the catalog, so each reads its rows' links per row
// (threadColumns), not every link on the computer (threadListSelect).
func listArchivedThreadsQuery() (string, []any) {
	hiddenClause, hiddenArgs := catalogThreadClause("threads")
	return `SELECT ` + threadColumns + ` FROM owned_threads AS threads
		 WHERE archived = 1 AND ` + hiddenClause + ` ORDER BY updated_at DESC`, hiddenArgs
}

func listProjectThreadsQuery(projectID string) (string, []any) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	return `SELECT ` + threadColumns + ` FROM owned_threads AS threads
		 WHERE project_id = ? AND archived = 0 AND ` + hiddenClause + `
		 ORDER BY updated_at DESC`, append([]any{projectID}, hiddenArgs...)
}

// listThreadsQuery is the ListThreads statement. It is a package-level
// var rather than inline text for the same reason listThreadsWithItemsQuery
// is a function: the plan tripwire (TestTimelineItemsViewJoinPushesDown)
// pins the production statement instead of a copy that is free to drift
// from it. threadColumns reaches `timeline_items` through its
// proposed-plan probe, which is the shape that tripwire exists for.
var listThreadsQuery = threadListSelect(`owned_threads AS threads`) + `
		 WHERE archived = 0 ORDER BY updated_at DESC`

// listThreadsWithItemsQuery is the ListThreadsWithItems statement. It is a
// function rather than inline text so the plan tripwire
// (TestTimelineItemsViewJoinPushesDown) pins the production statement:
// its EXISTS probes and threadColumns' proposed-plan probe reach
// `timeline_items` through correlated terms from OUTSIDE the view's UNION
// ALL, the one shape where SQLite can decline to push the term into the
// arms and scan `items` once per sidebar row (see importedItemRevExpr).
func listThreadsWithItemsQuery() (string, []any) {
	listed, args := sidebarListedClause()
	return threadListSelect(`owned_threads AS threads`) + `
		 WHERE ` + listed + `
		 ORDER BY updated_at DESC`, args
}

// The sidebar lists a thread that is real and visible (sidebarListedClause;
// product rule in docs/decisions.md, "Sidebar, threads, drafts"). A thread is
// real once it holds something a person would lose with it: an item, a draft
// with content, a draft recovery, a bound worktree (the thread is how the
// person reaches the checkout), a fork being prepared, a terminal, or an
// assistant's record naming it as its current thread, which a new assistant
// or a handoff's successor holds before its opening message lands (its
// delete goes through deleteAgentRecordTx), or a spawn relation: a thread
// an agent spawned is real from the write that names it as the spawn's
// thread (its local spawn link, or the receipt of a paired computer's
// spawn), and a thread that spawned or owns others is real for as long as
// a link names it the spawner (agentSpawnLinkSQL). A never-sent thread
// with none of them is not listed, and the empty-draft cleanup may delete
// it (emptyDraftThreadSQL). Draft, checkout, spawn naming, group and
// placement writes report a move across the rule as a ListingChange from
// inside their transaction; the app announces it, and checks the rows it
// posts itself (App.writeAppRow).
var threadRealClause = `(
		       threads.fork_preparing = 1
		    OR threads.mode = 'terminal'
		    OR COALESCE(threads.worktree_path, '') <> ''
		    OR EXISTS (SELECT 1 FROM agents WHERE agents.thread_id = threads.id)
		    OR EXISTS (SELECT 1 FROM timeline_items WHERE timeline_items.thread_id = threads.id)
		    OR EXISTS (SELECT 1 FROM thread_draft_recoveries WHERE thread_id = threads.id)
		    OR EXISTS (
		         SELECT 1 FROM thread_drafts
		          WHERE thread_drafts.thread_id = threads.id
		            AND thread_drafts.has_content = 1
		       )
		    OR EXISTS (SELECT 1 FROM thread_requests AS link
		                WHERE ` + agentSpawnLinkSQL("link") + ` AND link.target_thread_id = threads.id)
		    OR EXISTS (SELECT 1 FROM thread_requests AS link
		                WHERE ` + agentSpawnLinkSQL("link") + ` AND link.caller_thread_id = threads.id)
		    OR EXISTS (SELECT 1 FROM thread_request_receipts AS receipt
		                WHERE receipt.target_thread_id = threads.id AND receipt.kind = 'spawn')
		   )`

// sidebarListedClause is the condition, on a row of owned_threads aliased
// `threads`, for the thread to be listed in the sidebar
// (ListThreadsWithItems): visible (not archived, not a hidden mode, not an
// owned worker; catalogThreadClause) and real.
func sidebarListedClause() (string, []any) {
	hiddenClause, hiddenArgs := catalogThreadClause("threads")
	return `threads.archived = 0 AND ` + hiddenClause + `
		   AND ` + threadRealClause, hiddenArgs
}

// ListingChange is how a write moved its thread across the sidebar listing
// rule (sidebarListedClause).
type ListingChange int

const (
	// ListingUnchanged: listed before and after, or neither.
	ListingUnchanged ListingChange = iota
	// ListingListed: the write put the thread on the list.
	ListingListed
	// ListingUnlisted: the write took it off the list; the row remains.
	ListingUnlisted
)

func listingChange(before, after bool) ListingChange {
	switch {
	case !before && after:
		return ListingListed
	case before && !after:
		return ListingUnlisted
	default:
		return ListingUnchanged
	}
}

// ThreadListed reports whether ListThreadsWithItems lists the thread.
func (s *Store) ThreadListed(threadID string) (bool, error) {
	return threadListed(s.reader(), threadID)
}

// threadListed reports whether ListThreadsWithItems lists the thread.
func threadListed(q sqlQueryer, threadID string) (bool, error) {
	listed, args := sidebarListedClause()
	var yes bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM owned_threads AS threads WHERE threads.id = ? AND `+listed+`)`,
		append([]any{threadID}, args...)...).Scan(&yes)
	if err != nil {
		return false, fmt.Errorf("store: thread %s listed: %w", threadID, err)
	}
	return yes, nil
}
