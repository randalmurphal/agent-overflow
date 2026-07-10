package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// threadColumns lists every column in the order scanThread expects. The
// COALESCE-ing of nullable text columns returns "" instead of NULL so the
// Go struct has a clean empty-string value for unset optional fields.
// project_id is coalesced because v5 made it nullable: a standalone "home"
// terminal thread has no project, and scanThread reads it into a plain
// string. last_read_at and pinned_at are deliberately NOT coalesced —
// scanThread keeps the NULL / non-NULL distinction via *int64 pointers so
// the frontend can tell "never tracked" / "unpinned" apart from a zero
// timestamp. The two boolean tail columns are derived sidebar state:
// they are cheap scalar probes over indexed tables, not threads columns.
const threadColumns = `id, COALESCE(project_id, ''),
    COALESCE((SELECT path FROM projects WHERE projects.id = threads.project_id), ''),
    title, provider, model,
    workspace_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    COALESCE(pr_ref, ''),
    COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''),
    mode, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
    COALESCE(forked_from_thread_id, ''), last_token_usage,
    created_at, updated_at,
    (SELECT MAX(completed_at) FROM turns
      WHERE turns.thread_id = threads.id AND completed_at IS NOT NULL),
    archived, last_read_at, pinned_at, disabled_mcp_servers,
    EXISTS (
      SELECT 1
        FROM proposed_plans
        JOIN items
          ON items.thread_id = proposed_plans.thread_id
         AND items.id = proposed_plans.item_id
        JOIN payloads ON payloads.id = items.payload_id
       WHERE proposed_plans.thread_id = threads.id
         AND proposed_plans.version = (
           SELECT MAX(latest.version)
             FROM proposed_plans AS latest
            WHERE latest.thread_id = threads.id
         )
         AND proposed_plans.implemented_at = 0
         AND items.role = 'assistant'
         AND items.status = 'completed'
         AND payloads.kind = 'proposed_plan'
    ),
    COALESCE((
      SELECT CASE
          WHEN turns.completed_at IS NULL
            THEN (threads.last_read_at IS NULL OR threads.last_read_at < turns.started_at)
          WHEN turns.stop_reason = 'interrupted'
            THEN (threads.last_read_at IS NULL OR threads.last_read_at < turns.completed_at)
          ELSE 0
        END
        FROM turns
       WHERE turns.thread_id = threads.id
       ORDER BY turns.turn_index DESC
       LIMIT 1
    ), 0),
    NOT EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id)`

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
)

// legalModes maps every valid mode value to struct{}{} so membership
// checks are constant-time. Kept in sync with the CHECK constraint on
// threads.mode (see migrate.go rebuildThreadsV5SQL).
var legalModes = map[string]struct{}{
	"chat":       {},
	"plan":       {},
	"design":     {},
	"discussion": {},
	"terminal":   {},
}

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
	default:
		return false
	}
}

var legalProviders = map[string]struct{}{
	"claude":     {},
	"codex":      {},
	"claude-tui": {},
}

func validContextWindow(tokens int) bool {
	return tokens > 0
}

func validAutoCompactPercent(percent int) bool {
	return percent >= 0 && percent <= 90
}

func scanThread(scanner interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var archived, fastMode, hasActionableProposedPlan, hasIncompleteTurn, isDraft int
	var latestTurnCompletedAt, lastReadAt, pinnedAt sql.NullInt64
	var disabledMcpServersJSON sql.NullString
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.ProjectPath, &t.Title, &t.Provider, &t.Model,
		&t.WorkspacePath, &t.WorktreePath, &t.Branch, &t.PRRef,
		&t.SessionRef, &t.PendingForkRef,
		&t.Mode, &t.ReasoningEffort, &fastMode, &t.ContextWindow,
		&t.AutoCompactStandardPercent, &t.AutoCompactExtendedPercent, &t.RuntimeMode,
		&t.DiscussionID, &t.ParentThreadID, &t.ForkedFromThreadID, &t.LastTokenUsage,
		&t.CreatedAt, &t.UpdatedAt, &latestTurnCompletedAt, &archived, &lastReadAt, &pinnedAt,
		&disabledMcpServersJSON,
		&hasActionableProposedPlan, &hasIncompleteTurn, &isDraft,
	); err != nil {
		return Thread{}, err
	}
	t.FastMode = fastMode != 0
	t.Archived = archived != 0
	t.HasActionableProposedPlan = hasActionableProposedPlan != 0
	t.HasIncompleteTurn = hasIncompleteTurn != 0
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
	if disabledMcpServersJSON.Valid {
		var names []string
		if err := json.Unmarshal([]byte(disabledMcpServersJSON.String), &names); err == nil {
			t.DisabledMcpServers = &names
		}
	}
	return t, nil
}

func (s *Store) CreateThread(t Thread) error {
	t.Mode = normalizeMode(t.Mode)
	t.RuntimeMode = normalizeRuntimeMode(t.RuntimeMode)
	t.ReasoningEffort = normalizeEffort(t.ReasoningEffort)
	if !legalEffortForProvider(t.Provider, t.ReasoningEffort) {
		return fmt.Errorf("%w: %s/%s", ErrInvalidEffort, t.Provider, t.ReasoningEffort)
	}
	if t.ContextWindow == 0 {
		t.ContextWindow = 1000000
	}
	if !validContextWindow(t.ContextWindow) {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, t.ContextWindow)
	}
	if !validAutoCompactPercent(t.AutoCompactStandardPercent) {
		return fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactStandardPercent)
	}
	if !validAutoCompactPercent(t.AutoCompactExtendedPercent) {
		return fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, t.AutoCompactExtendedPercent)
	}
	lastReadAt := t.LastReadAt
	if lastReadAt == nil {
		lastReadAt = &t.CreatedAt
	}
	lastReadAtArg := nullableInt64(lastReadAt)
	_, err := s.db.Exec(
		`INSERT INTO threads (id, project_id, title, provider, model,
		    workspace_path, worktree_path, branch, pr_ref, session_ref, pending_fork_session_ref,
		    mode, reasoning_effort, fast_mode, context_window,
		    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
		    discussion_id, parent_thread_id, forked_from_thread_id, last_token_usage,
		    created_at, updated_at, archived, last_read_at, disabled_mcp_servers)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, nilIfEmpty(t.ProjectID), t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		t.PRRef,
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived), lastReadAtArg, marshalDisabledMcpServers(t.DisabledMcpServers),
	)
	if err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT `+threadColumns+` FROM threads WHERE id = ?`, id,
	)
	t, err := scanThread(row)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	return t, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads WHERE archived = 0 ORDER BY updated_at DESC`,
	)
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
// drafts, so the item/draft gates would otherwise hide them.
func (s *Store) ListThreadsWithItems() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads
		 WHERE archived = 0
		   AND (
		       threads.mode = 'terminal'
		    OR EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id)
		    OR EXISTS (
		         SELECT 1 FROM thread_drafts
		          WHERE thread_drafts.thread_id = threads.id
		            AND thread_drafts.has_content = 1
		       )
		   )
		 ORDER BY updated_at DESC`,
	)
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

// ListArchivedThreads returns every archived thread, newest-touched first.
// Used by the settings panel to surface threads that have been hidden from
// the sidebar so the user can unarchive or permanently delete them.
func (s *Store) ListArchivedThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads WHERE archived = 1 ORDER BY updated_at DESC`,
	)
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
	rows, err := s.db.Query(
		`SELECT `+threadColumns+` FROM threads
		 WHERE project_id = ? AND archived = 0
		 ORDER BY updated_at DESC`,
		projectID,
	)
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

// ListChildThreads returns a parent's child threads ordered by
// creation. Discussion participant threads are all created with the
// same CreatedAt millisecond (see BuildParticipantPlans, which stamps
// every plan with one shared nowMillis), so `ORDER BY created_at ASC`
// alone is not a deterministic tiebreak under SQL semantics — ties can
// come back in any order. `rowid ASC` breaks the tie by insert order
// (these are rowid tables), which matches definition order and keeps
// the deliberation roster's round-robin sequence stable across reads.
func (s *Store) ListChildThreads(parentID string) ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT `+threadColumns+` FROM threads WHERE parent_thread_id = ? ORDER BY created_at ASC, rowid ASC`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list child threads for %s: %w", parentID, err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan child thread row: %w", err)
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) HasChildThreads(parentID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM threads WHERE parent_thread_id = ? LIMIT 1)`,
		parentID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: check child threads for %s: %w", parentID, err)
	}
	return exists != 0, nil
}

// ListThreadWorkspaceRefs returns workspace/worktree pointers for all thread
// rows, including archived ones. Worktree removal uses this to avoid deleting a
// checkout that an archived thread would reference if restored.
func (s *Store) ListThreadWorkspaceRefs() ([]ThreadWorkspaceRef, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_path, COALESCE(worktree_path, '') FROM threads`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread workspace refs: %w", err)
	}
	defer rows.Close()

	var refs []ThreadWorkspaceRef
	for rows.Next() {
		var ref ThreadWorkspaceRef
		if err := rows.Scan(&ref.ID, &ref.WorkspacePath, &ref.WorktreePath); err != nil {
			return nil, fmt.Errorf("store: scan thread workspace ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ListThreadWorkspaceRefsForProject is the project-scoped counterpart used by
// app-layer transient activity overlays. It intentionally reads only thread
// identity and paths, never item history.
func (s *Store) ListThreadWorkspaceRefsForProject(projectID string) ([]ThreadWorkspaceRef, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_path, COALESCE(worktree_path, '')
		   FROM threads
		  WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread workspace refs for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var refs []ThreadWorkspaceRef
	for rows.Next() {
		var ref ThreadWorkspaceRef
		if err := rows.Scan(&ref.ID, &ref.WorkspacePath, &ref.WorktreePath); err != nil {
			return nil, fmt.Errorf("store: scan project thread workspace ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

const blockedThreadWorkspaceRefsForProjectSQL = `SELECT t.id, t.workspace_path, COALESCE(t.worktree_path, '')
  FROM threads t
 WHERE t.project_id = ?
   AND (
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
          )
     )
     OR EXISTS(
       SELECT 1
         FROM items subagent INDEXED BY idx_items_live_codex_subagent
        WHERE subagent.thread_id = t.id
          AND t.provider = 'codex'
          AND subagent.kind = 'tool_call'
          AND subagent.status = 'completed'
          AND subagent.tool_name = 'collab_agent'
          AND subagent.is_background = 1
          AND COALESCE(json_extract(subagent.meta, '$.live_background_active'), 1) != 0
          AND json_extract(subagent.meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')
          AND NOT EXISTS(
            SELECT 1 FROM items completion INDEXED BY idx_items_completion_of
             WHERE completion.thread_id = subagent.thread_id
               AND completion.completion_of = subagent.id
               AND completion.completion_of <> ''
          )
     )
   )`

// ListBlockedThreadWorkspaceRefsForProject returns workspace pointers only for
// threads whose persisted activity currently blocks worktree removal. It is
// scoped to one project so opening a picker never scans unrelated history.
func (s *Store) ListBlockedThreadWorkspaceRefsForProject(projectID string) ([]ThreadWorkspaceRef, error) {
	rows, err := s.db.Query(blockedThreadWorkspaceRefsForProjectSQL, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list blocked thread workspace refs for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var refs []ThreadWorkspaceRef
	for rows.Next() {
		var ref ThreadWorkspaceRef
		if err := rows.Scan(&ref.ID, &ref.WorkspacePath, &ref.WorktreePath); err != nil {
			return nil, fmt.Errorf("store: scan blocked thread workspace ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

const updateThreadSetSQL = `UPDATE threads SET project_id=?, title=?, provider=?, model=?,
    workspace_path=?, worktree_path=?, branch=?, pr_ref=?, session_ref=?, pending_fork_session_ref=?,
    mode=?, reasoning_effort=?, fast_mode=?, context_window=?,
    auto_compact_standard_percent=?, auto_compact_extended_percent=?, runtime_mode=?,
    discussion_id=?, parent_thread_id=?, forked_from_thread_id=?, last_token_usage=?,
    archived=?`

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
		t.PRRef,
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		boolToInt(t.Archived),
	}
}

func (s *Store) UpdateThread(t Thread) error {
	t, err := normalizeThreadForUpdate(t)
	if err != nil {
		return err
	}
	args := append(updateThreadArgs(t), t.ID)
	result, err := s.db.Exec(
		updateThreadSetSQL+` WHERE id=?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update thread %s", t.ID))
}

func (s *Store) UpdateThreadAndDeleteCheckpoints(t Thread) ([]CheckpointRef, error) {
	t, err := normalizeThreadForUpdate(t)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin update thread %s and delete checkpoints: %w", t.ID, err)
	}
	defer tx.Rollback()

	refs, err := checkpointRefsForThread(tx, t.ID)
	if err != nil {
		return nil, err
	}

	args := append(updateThreadArgs(t), t.ID)
	result, err := tx.Exec(updateThreadSetSQL+` WHERE id=?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update thread %s", t.ID)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM thread_checkpoints WHERE thread_id = ?`, t.ID); err != nil {
		return nil, fmt.Errorf("store: delete checkpoints for %s: %w", t.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit update thread %s and delete checkpoints: %w", t.ID, err)
	}
	return refs, nil
}

func (s *Store) UpdateThreadIfProviderSwitchAllowed(t Thread, previousProvider string) error {
	t, err := normalizeThreadForUpdate(t)
	if err != nil {
		return err
	}
	args := append(updateThreadArgs(t), t.ID, previousProvider, t.ID)
	result, err := s.db.Exec(
		updateThreadSetSQL+` WHERE id=?
		 AND provider = ?
		 AND NOT EXISTS (SELECT 1 FROM items WHERE thread_id = ? LIMIT 1)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("store: guarded provider switch for thread %s: %w", t.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: guarded provider switch for thread %s: rows affected: %w", t.ID, err)
	}
	if rows != 0 {
		return nil
	}
	return s.explainProviderSwitchNoRows(t.ID, previousProvider)
}

func (s *Store) explainProviderSwitchNoRows(threadID, previousProvider string) error {
	var currentProvider string
	var hasItems int
	err := s.db.QueryRow(
		`SELECT provider,
		        EXISTS(SELECT 1 FROM items WHERE thread_id = threads.id LIMIT 1)
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

func (s *Store) DeleteThread(id string) error {
	result, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete thread %s", id))
}

func (s *Store) ArchiveThread(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET archived = 1, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: archive thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: archive thread %s", id))
}

// UnarchiveThread flips the archived column back to 0 for a thread and bumps
// updated_at so the sidebar reshuffles it toward the top of the active list.
// Returns an error if no row matches the id.
func (s *Store) UnarchiveThread(id string) error {
	result, err := s.db.Exec(`UPDATE threads SET archived = 0, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: unarchive thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: unarchive thread %s", id))
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

// MarkThreadReadNow stamps last_read_at with the current unix-ms, clamped to
// the latest sidebar read target. Completed turns key off completed_at; an
// interrupted newest turn keys off started_at so opening it clears the
// durable Interrupted pill just like opening a completed thread clears
// Completed. The clamp is load-bearing because provider timestamps can
// occasionally be ahead of wall-clock now when the frontend asks to mark the
// thread read.
//
// Does NOT bump updated_at: read-state is UI bookkeeping, not a thread
// mutation, and bumping would thrash the sidebar ordering.
func (s *Store) MarkThreadReadNow(id string) error {
	now := nowMillis()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin mark thread read %s: %w", id, err)
	}
	defer tx.Rollback()

	var latestTurnCompletedAt, latestIncompleteStartedAt, lastReadAt sql.NullInt64
	err = tx.QueryRow(
		`SELECT
		    (SELECT MAX(completed_at) FROM turns
		      WHERE thread_id = threads.id AND completed_at IS NOT NULL),
		    (SELECT CASE WHEN completed_at IS NULL THEN started_at END
		       FROM turns
		      WHERE thread_id = threads.id
		      ORDER BY turn_index DESC
		      LIMIT 1),
		    last_read_at
		   FROM threads
		  WHERE id = ?`,
		id,
	).Scan(&latestTurnCompletedAt, &latestIncompleteStartedAt, &lastReadAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: mark thread read %s: %w", id, sql.ErrNoRows)
		}
		return fmt.Errorf("store: read thread read-state %s: %w", id, err)
	}

	readTarget := int64(0)
	hasReadTarget := false
	if latestTurnCompletedAt.Valid {
		hasReadTarget = true
		readTarget = latestTurnCompletedAt.Int64
	}
	if latestIncompleteStartedAt.Valid {
		hasReadTarget = true
		if latestIncompleteStartedAt.Int64 > readTarget {
			readTarget = latestIncompleteStartedAt.Int64
		}
	}

	if lastReadAt.Valid {
		if hasReadTarget && lastReadAt.Int64 >= readTarget {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("store: commit mark thread read no-op %s: %w", id, err)
			}
			return nil
		}
	}

	readAt := now
	if hasReadTarget && readTarget > readAt {
		readAt = readTarget
	}
	if lastReadAt.Valid && lastReadAt.Int64 >= readAt {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit mark thread read no-op %s: %w", id, err)
		}
		return nil
	}
	result, err := tx.Exec(`UPDATE threads SET last_read_at = ? WHERE id = ?`, readAt, id)
	if err != nil {
		return fmt.Errorf("store: mark thread read %s: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: mark thread read %s", id)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit mark thread read %s: %w", id, err)
	}
	return nil
}

// MarkThreadUnread stamps last_read_at to zero. NULL is reserved for
// "never tracked" and is treated as read by the frontend so old rows do not
// light up on first launch; an explicit unread action needs a concrete value
// that is older than every real thread update.
func (s *Store) MarkThreadUnread(id string) error {
	var zero int64
	return s.setThreadLastRead(id, &zero)
}

// setThreadLastRead is the shared primitive. Kept unexported — callers
// should use the named MarkThreadReadNow / MarkThreadUnread wrappers so
// the intent is visible at the call site.
func (s *Store) setThreadLastRead(id string, ts *int64) error {
	var arg any
	if ts != nil {
		arg = *ts
	}
	result, err := s.db.Exec(
		`UPDATE threads SET last_read_at = ? WHERE id = ?`,
		arg, id,
	)
	if err != nil {
		return fmt.Errorf("store: update last_read_at for %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update last_read_at for %s", id))
}

// PinThread stamps pinned_at with the current unix-ms. Idempotent — the
// caller can re-pin to bump the position within the pinned tier without
// special-casing.
func (s *Store) PinThread(id string) error {
	now := nowMillis()
	return s.setThreadPinnedAt(id, &now)
}

// UnpinThread clears pinned_at, returning the thread to the regular
// status-aware sort order.
func (s *Store) UnpinThread(id string) error {
	return s.setThreadPinnedAt(id, nil)
}

// setThreadPinnedAt is the shared primitive. We deliberately do NOT
// touch updated_at here: pinning is a sidebar-presentation tweak, not
// thread activity, and bumping updated_at would shuffle the project's
// `lastActivity` ordering.
func (s *Store) setThreadPinnedAt(id string, ts *int64) error {
	var arg any
	if ts != nil {
		arg = *ts
	}
	result, err := s.db.Exec(
		`UPDATE threads SET pinned_at = ? WHERE id = ?`,
		arg, id,
	)
	if err != nil {
		return fmt.Errorf("store: update pinned_at for %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update pinned_at for %s", id))
}

// UpdateSessionRef records the provider resume cursor without touching
// updated_at. Provider init can happen during sidebar-driven auto-resume, and
// opening a thread must not count as new thread activity.
func (s *Store) UpdateSessionRef(threadID, ref string) error {
	result, err := s.db.Exec(
		`UPDATE threads
		 SET session_ref = ?, pending_fork_session_ref = NULL
		 WHERE id = ?`,
		ref, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update session ref for %s", threadID))
}

// In-thread setters below intentionally do NOT bump updated_at. Title
// renames, model / mode / effort / fast-mode / context-window edits,
// branch / workspace-path swaps, token-usage refreshes, and runtime-mode
// flips are in-place mutations that should not move the sidebar.
// Sidebar activity is owned by Store.MarkThreadActivity, called from
// triage at user_text persist, turn settle, and approval / user-input
// request creation.

func (s *Store) UpdateTitle(threadID, title string) error {
	result, err := s.db.Exec(`UPDATE threads SET title = ? WHERE id = ?`,
		title, threadID)
	if err != nil {
		return fmt.Errorf("store: update title for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update title for %s", threadID))
}

func (s *Store) UpdateTitleIfCurrent(threadID, currentTitle, newTitle string) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE threads SET title = ? WHERE id = ? AND title = ?`,
		newTitle, threadID, currentTitle,
	)
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title for %s: %w", threadID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: compare-and-swap title rows affected for %s: %w", threadID, err)
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

// UpdateProvider overwrites the provider ('claude' / 'codex' / 'claude-tui').
// Invalid values surface ErrInvalidProvider before hitting SQLite so the
// binding can translate to a user-facing error.
func (s *Store) UpdateProvider(threadID, prov string) error {
	if _, ok := legalProviders[prov]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidProvider, prov)
	}
	result, err := s.db.Exec(`UPDATE threads SET provider = ? WHERE id = ?`,
		prov, threadID)
	if err != nil {
		return fmt.Errorf("store: update provider for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update provider for %s", threadID))
}

// UpdateMode overwrites the thread's mode (chat, plan, design,
// discussion, or terminal). Empty strings are normalized to "chat" to
// match CreateThread/UpdateThread. This is the permissive store
// primitive; user-driven toggles route through threadmode.ValidateSet,
// which restricts the reachable set to chat/plan.
func (s *Store) UpdateMode(threadID, mode string) error {
	mode = normalizeMode(mode)
	if _, ok := legalModes[mode]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
	result, err := s.db.Exec(`UPDATE threads SET mode = ? WHERE id = ?`,
		mode, threadID)
	if err != nil {
		return fmt.Errorf("store: update mode for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update mode for %s", threadID))
}

// UpdateReasoningEffort overwrites the effort tier. See legalEfforts for
// the enumerated values.
func (s *Store) UpdateReasoningEffort(threadID, effort string) error {
	normalized := normalizeEffort(effort)
	if _, ok := legalEfforts[normalized]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidEffort, effort)
	}
	var providerName string
	if err := s.db.QueryRow(`SELECT provider FROM threads WHERE id = ?`, threadID).Scan(&providerName); err != nil {
		return fmt.Errorf("store: load provider for effort update %s: %w", threadID, err)
	}
	if !legalEffortForProvider(providerName, normalized) {
		return fmt.Errorf("%w: %s/%s", ErrInvalidEffort, providerName, normalized)
	}
	result, err := s.db.Exec(`UPDATE threads SET reasoning_effort = ? WHERE id = ?`,
		normalized, threadID)
	if err != nil {
		return fmt.Errorf("store: update reasoning effort for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update reasoning effort for %s", threadID))
}

// UpdateFastMode flips the fast-mode boolean.
func (s *Store) UpdateFastMode(threadID string, on bool) error {
	result, err := s.db.Exec(`UPDATE threads SET fast_mode = ? WHERE id = ?`,
		boolToInt(on), threadID)
	if err != nil {
		return fmt.Errorf("store: update fast mode for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update fast mode for %s", threadID))
}

// UpdateContextWindow overwrites the context_window column.
func (s *Store) UpdateContextWindow(threadID string, tokens int) error {
	if !validContextWindow(tokens) {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	result, err := s.db.Exec(`UPDATE threads SET context_window = ? WHERE id = ?`,
		tokens, threadID)
	if err != nil {
		return fmt.Errorf("store: update context window for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update context window for %s", threadID))
}

func (s *Store) GetThreadContextSettings(threadID string) (ThreadContextSettings, error) {
	var settings ThreadContextSettings
	err := s.db.QueryRow(
		`SELECT provider, model, project_id, context_window,
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
func (s *Store) UpdateContextSettings(threadID string, tokens, standardPercent, extendedPercent int) error {
	if !validContextWindow(tokens) {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	if !validAutoCompactPercent(standardPercent) {
		return fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, standardPercent)
	}
	if !validAutoCompactPercent(extendedPercent) {
		return fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, extendedPercent)
	}
	result, err := s.db.Exec(
		`UPDATE threads
		    SET context_window = ?,
		        auto_compact_standard_percent = ?,
		        auto_compact_extended_percent = ?
		  WHERE id = ?`,
		tokens, standardPercent, extendedPercent, threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update context settings for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update context settings for %s", threadID))
}

// UpdateBranch persists a new branch string without touching the git
// working tree. Callers that want to actually switch branches should
// wrap this with the git checkout side effect.
func (s *Store) UpdateBranch(threadID, branch string) error {
	result, err := s.db.Exec(`UPDATE threads SET branch = ? WHERE id = ?`,
		nilIfEmpty(branch), threadID)
	if err != nil {
		return fmt.Errorf("store: update branch for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update branch for %s", threadID))
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

// UpdateRuntimeMode overwrites the thread's runtime mode (approval-required,
// auto-accept-edits, or full-access). Unknown values are coerced to the
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

// normalizeMode coerces empty strings to the schema default "chat" and
// returns anything else verbatim. Callers that need strict validation
// run the legalModes check separately.
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
// internal/store stays provider-free (import cycle avoidance).
func normalizeRuntimeMode(mode string) string {
	switch mode {
	case "approval-required", "auto-accept-edits", "full-access":
		return mode
	default:
		return "full-access"
	}
}

func marshalDisabledMcpServers(names *[]string) any {
	if names == nil {
		return nil
	}
	data, _ := json.Marshal(*names)
	return string(data)
}

// GetDisabledMcpServers returns the per-thread MCP disabled set.
// snapshotted=false when the column is NULL (pre-feature thread).
func (s *Store) GetDisabledMcpServers(threadID string) (names []string, snapshotted bool, err error) {
	var raw sql.NullString
	err = s.db.QueryRow(
		`SELECT disabled_mcp_servers FROM threads WHERE id = ?`, threadID,
	).Scan(&raw)
	if err != nil {
		return nil, false, fmt.Errorf("store: get disabled mcp servers for %s: %w", threadID, err)
	}
	if !raw.Valid {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw.String), &names); err != nil {
		return nil, false, fmt.Errorf("store: decode disabled mcp servers for %s: %w", threadID, err)
	}
	return names, true, nil
}

// SetDisabledMcpServers persists the per-thread MCP disabled set. Always
// produces a non-NULL value (empty slice serializes to "[]"). Does NOT
// bump updated_at — MCP preferences are UI bookkeeping, same convention
// as UpdateBranch, UpdateModel, and other in-thread setters.
func (s *Store) SetDisabledMcpServers(threadID string, names []string) error {
	if names == nil {
		names = []string{}
	}
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("store: encode disabled mcp servers for %s: %w", threadID, err)
	}
	result, err := s.db.Exec(
		`UPDATE threads SET disabled_mcp_servers = ? WHERE id = ?`,
		string(data), threadID,
	)
	if err != nil {
		return fmt.Errorf("store: set disabled mcp servers for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set disabled mcp servers for %s", threadID))
}
