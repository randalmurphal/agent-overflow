package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/threadmode"
)

// threadColumns lists every column in the order scanThread expects. The
// COALESCE-ing of nullable text columns returns "" instead of NULL so the
// Go struct has a clean empty-string value for unset optional fields.
// project_id is coalesced because v5 made it nullable: a standalone "home"
// terminal thread has no project, and scanThread reads it into a plain
// string. last_read_at, pinned_at, and pin_group are deliberately NOT coalesced —
// scanThread keeps the NULL / non-NULL distinction via *int64 pointers so
// the frontend can tell "never tracked" / "unpinned" apart from a zero
// timestamp and distinguish migrated front-burner pins from explicit groups.
// The two boolean tail columns are derived sidebar state:
// they are cheap scalar probes over indexed tables, not threads columns.
const threadColumns = `id, COALESCE(project_id, ''),
    COALESCE((SELECT path FROM projects WHERE projects.id = threads.project_id), ''),
    title, provider, model,
    workspace_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    COALESCE(pr_ref, ''),
    COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''),
    pending_fork_resume_at,
    mode, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
    COALESCE(forked_from_thread_id, ''), last_token_usage,
    created_at, updated_at,
    (SELECT MAX(completed_at) FROM turns
      WHERE turns.thread_id = threads.id AND completed_at IS NOT NULL),
    archived, last_read_at, pinned_at, pin_group,
    COALESCE(group_id, ''),
    worktree_setup_state, import_source,
    created_by_device, created_branch, created_remote_url, created_head_commit,
	EXISTS (
      SELECT 1
        FROM proposed_plans
		JOIN timeline_items AS items
          ON items.thread_id = proposed_plans.thread_id
         AND items.id = proposed_plans.item_id
       WHERE proposed_plans.thread_id = threads.id
         AND proposed_plans.version = (
           SELECT MAX(latest.version)
             FROM proposed_plans AS latest
            WHERE latest.thread_id = threads.id
         )
         AND proposed_plans.implemented_at = 0
         AND items.role = 'assistant'
         AND items.status = 'completed'
         AND COALESCE(
           (SELECT local_payload.kind
              FROM payloads AS local_payload
             WHERE local_payload.thread_id = items.thread_id
               AND local_payload.id = items.payload_id),
           (SELECT imported_payload.kind
              FROM thread_import_chunks AS refs
              JOIN import_history_payloads AS imported_payload
                ON imported_payload.chunk_id = refs.chunk_id
               AND imported_payload.id = items.payload_id
             WHERE refs.thread_id = items.thread_id)
         ) = 'proposed_plan'
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
    NOT EXISTS (SELECT 1 FROM timeline_items WHERE timeline_items.thread_id = threads.id)`

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

// validImportSource mirrors the migration v50 CHECK. Only the two
// importable providers are legal: claude-tui shares claude's binary but has
// no session files of its own to import from.
func validImportSource(source string) bool {
	switch source {
	case "", "claude", "codex":
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
	var archived, fastMode, hasActionableProposedPlan, hasIncompleteTurn, isDraft int
	var latestTurnCompletedAt, lastReadAt, pinnedAt, pinGroup sql.NullInt64
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.ProjectPath, &t.Title, &t.Provider, &t.Model,
		&t.WorkspacePath, &t.WorktreePath, &t.Branch, &t.PRRef,
		&t.SessionRef, &t.PendingForkRef,
		&t.PendingForkResumeAt,
		&t.Mode, &t.ReasoningEffort, &fastMode, &t.ContextWindow,
		&t.AutoCompactStandardPercent, &t.AutoCompactExtendedPercent, &t.RuntimeMode,
		&t.DiscussionID, &t.ParentThreadID, &t.ForkedFromThreadID, &t.LastTokenUsage,
		&t.CreatedAt, &t.UpdatedAt, &latestTurnCompletedAt, &archived, &lastReadAt, &pinnedAt, &pinGroup,
		&t.GroupID,
		&t.WorktreeSetupState, &t.ImportSource,
		&t.CreatedByDevice, &t.Origin.Branch, &t.Origin.RemoteURL, &t.Origin.HeadCommit,
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
	if pinGroup.Valid {
		v := int(pinGroup.Int64)
		t.PinGroup = &v
	}
	return t, nil
}

func (s *Store) CreateThread(t Thread) error {
	prepared, lastReadAtArg, err := prepareThreadForCreate(t)
	if err != nil {
		return err
	}
	if err := insertThread(s.db, prepared, lastReadAtArg); err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

type threadExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
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

func insertThread(execer threadExecer, t Thread, lastReadAtArg any) error {
	_, err := execer.Exec(
		`INSERT INTO threads (id, project_id, title, provider, model,
		    workspace_path, worktree_path, branch, pr_ref, session_ref, pending_fork_session_ref,
		    pending_fork_resume_at,
		    mode, reasoning_effort, fast_mode, context_window,
		    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
		    discussion_id, parent_thread_id, forked_from_thread_id, last_token_usage,
		    created_at, updated_at, archived, last_read_at, import_source,
		    created_by_device, created_branch, created_remote_url, created_head_commit,
		    group_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, nilIfEmpty(t.ProjectID), t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		t.PRRef,
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.PendingForkResumeAt,
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived), lastReadAtArg, t.ImportSource,
		// The write-once creation facts. They appear here and in
		// threadColumns, and deliberately NOT in updateThreadSetSQL: a
		// whole-row UpdateThread carrying a stale copy must not be able to
		// blank a thread's provenance or its git origin.
		t.CreatedByDevice, t.Origin.Branch, t.Origin.RemoteURL, t.Origin.HeadCommit,
		nilIfEmpty(t.GroupID),
	)
	return err
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

// GetThreadTitle reads only the title column. It is the narrow read for
// callers that need a thread's LABEL and nothing else — the OS-notification
// mapping, which is allowed to say a thread's title and nothing more about
// it — and it exists for the same reason GetThreadProviderWorkspace does:
// GetThread's projection computes four derived sidebar-state subqueries per
// call that such a caller would compute and throw away.
//
// A thread that is gone answers "" with no error. The caller is reacting to
// an event about a thread that may since have been deleted, and a deleted
// thread is not a failure to report — it is a notification with a fallback
// label, or none at all.
func (s *Store) GetThreadTitle(id string) (string, error) {
	var title string
	err := s.reader().QueryRow(`SELECT title FROM threads WHERE id = ?`, id).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get thread title %s: %w", id, err)
	}
	return title, nil
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
	rows, err := s.reader().Query(
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
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM threads
		 WHERE archived = 0 AND `+hiddenClause+`
		   AND (
		       threads.mode = 'terminal'
		    OR EXISTS (SELECT 1 FROM timeline_items WHERE timeline_items.thread_id = threads.id)
		    OR EXISTS (
		         SELECT 1 FROM thread_drafts
		          WHERE thread_drafts.thread_id = threads.id
		            AND thread_drafts.has_content = 1
		       )
		   )
		 ORDER BY updated_at DESC`, hiddenArgs...,
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
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM threads
		 WHERE archived = 1 AND `+hiddenClause+` ORDER BY updated_at DESC`, hiddenArgs...,
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
	hiddenClause, hiddenArgs := hiddenThreadModesClause("mode")
	args := append([]any{projectID}, hiddenArgs...)
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM threads
		 WHERE project_id = ? AND archived = 0 AND `+hiddenClause+`
		 ORDER BY updated_at DESC`,
		args...,
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

func hiddenThreadModesClause(column string) (string, []any) {
	modes := threadmode.HiddenModes()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(modes)), ",")
	args := make([]any, len(modes))
	for index, mode := range modes {
		args[index] = mode
	}
	return column + " NOT IN (" + placeholders + ")", args
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
	rows, err := s.reader().Query(
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
	err := s.reader().QueryRow(
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
	rows, err := s.reader().Query(
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

const blockedThreadWorkspaceRefsSQL = `SELECT t.id, t.workspace_path, COALESCE(t.worktree_path, '')
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
	rows, err := s.reader().Query(blockedThreadWorkspaceRefsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: list blocked thread workspace refs: %w", err)
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

// updateThreadSetSQL is the whole-row thread write every rename, model
// change, workspace switch, and provider switch issues. It is a hand-kept
// column list, and what it OMITS is load-bearing: a caller that only meant to
// rename a thread hands `UpdateThread` a `Thread` struct it read some time
// ago, so every column in this list is one a stale read can roll back.
// Columns owned by a narrow writer, by a trigger, or by the row's own
// lifecycle are therefore absent — see
// threadColumnsNotWrittenByUpdateThread (threads_test.go), which names each
// one and why, and whose TestUpdateThreadColumnGate forces every column into
// exactly one of the two lists.
const updateThreadSetSQL = `UPDATE threads SET project_id=?, title=?, provider=?, model=?,
    workspace_path=?, worktree_path=?, branch=?, pr_ref=?, session_ref=?,
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
		nilIfEmpty(t.SessionRef),
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

// ItemMetaUpdate names one item's replacement meta blob for
// UpdateSessionRefAndRemapProviderIDs.
type ItemMetaUpdate struct {
	ItemID string
	Meta   string
}

// MessageAnchorProviderIDsUpdate names one message anchor's replacement
// provider ids for UpdateSessionRefAndRemapProviderIDs. Empty strings
// preserve the stored value (same contract as
// UpdateMessageAnchorProviderIDs).
type MessageAnchorProviderIDsUpdate struct {
	UserItemID            string
	ProviderUserMessageID string
	ProviderParentUUID    string
}

// UpdateSessionRefAndRemapProviderIDs is the targeted counterpart to
// separate session-ref and provider-id writes. Materializing or rolling back
// a Claude branch remints transcript UUIDs, so the new session reference and
// every surviving SQLite correlation id must commit together. Only
// session_ref and pending-fork state change on the thread row; concurrent
// title, workspace, model, and activity updates cannot be overwritten by a
// stale whole-row snapshot.
func (s *Store) UpdateSessionRefAndRemapProviderIDs(
	threadID, ref string,
	items []ItemMetaUpdate,
	anchors []MessageAnchorProviderIDsUpdate,
) (changed bool, err error) {
	if threadID == "" {
		return false, fmt.Errorf("store: update session ref with provider id remap: thread id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin update session ref with provider id remap for %s: %w", threadID, err)
	}
	defer tx.Rollback()

	var prev sql.NullString
	if err := tx.QueryRow(`SELECT session_ref FROM threads WHERE id = ?`, threadID).Scan(&prev); err != nil {
		return false, fmt.Errorf("store: read session ref for provider id remap %s: %w", threadID, err)
	}
	result, err := tx.Exec(
		`UPDATE threads SET session_ref = ?, pending_fork_session_ref = NULL, pending_fork_resume_at = '' WHERE id = ?`,
		ref, threadID,
	)
	if err != nil {
		return false, fmt.Errorf("store: update session ref for provider id remap %s: %w", threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: update session ref for provider id remap %s", threadID)); err != nil {
		return false, err
	}
	if err := remapProviderIDsTx(tx, threadID, items, anchors); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit session ref with provider id remap for %s: %w", threadID, err)
	}
	return prev.String != ref, nil
}

func remapProviderIDsTx(
	tx *sql.Tx,
	threadID string,
	items []ItemMetaUpdate,
	anchors []MessageAnchorProviderIDsUpdate,
) error {
	for _, item := range items {
		label := fmt.Sprintf("store: remap item meta %s/%s", threadID, item.ItemID)
		if err := requireMutableItemTx(tx, threadID, item.ItemID, label); err != nil {
			return err
		}
		result, err := tx.Exec(
			`UPDATE items SET meta = ? WHERE thread_id = ? AND id = ?`,
			item.Meta, threadID, item.ItemID,
		)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if err := requireRowsAffected(result, label); err != nil {
			return err
		}
	}
	for _, anchor := range anchors {
		if _, err := tx.Exec(
			`UPDATE message_anchors
			    SET provider_user_message_id = CASE WHEN ? != '' THEN ? ELSE provider_user_message_id END,
			        provider_parent_uuid = CASE WHEN ? != '' THEN ? ELSE provider_parent_uuid END
			  WHERE thread_id = ? AND user_item_id = ?`,
			anchor.ProviderUserMessageID, anchor.ProviderUserMessageID,
			anchor.ProviderParentUUID, anchor.ProviderParentUUID,
			threadID, anchor.UserItemID,
		); err != nil {
			return fmt.Errorf("store: remap message anchor provider ids %s/%s: %w", threadID, anchor.UserItemID, err)
		}
	}
	return nil
}

func (s *Store) UpdateThreadIfProviderSwitchAllowed(t Thread, previousProvider string) error {
	t, err := normalizeThreadForUpdate(t)
	if err != nil {
		return err
	}
	args := append(updateThreadArgs(t), t.ID, previousProvider, t.ID)
	// A provider switch discards the previous provider's resume wiring, and
	// the lazy-fork pin is deliberately absent from updateThreadSetSQL (see
	// SetThreadForkResume), so the clear rides this UPDATE itself: a second
	// statement would leave a crash window where the switch committed and a
	// stale pin into the old provider's session files survived it.
	result, err := s.db.Exec(
		updateThreadSetSQL+`, pending_fork_session_ref=NULL, pending_fork_resume_at=''
		 WHERE id=?
		 AND provider = ?
		 AND NOT EXISTS (SELECT 1 FROM timeline_items WHERE thread_id = ? LIMIT 1)`,
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

// deleteThreadItemChunk bounds how many items a single DELETE statement
// (and therefore a single write transaction) removes while a thread is
// being deleted. Each item delete fires the two payload-GC triggers, so
// a 38k-item thread deleted through the FK cascade alone is one ~6s
// write transaction — longer than the 5s busy_timeout, meaning any
// concurrent writer errors SQLITE_BUSY instead of briefly waiting.
// 500-item chunks keep every write transaction in the tens of
// milliseconds.
const deleteThreadItemChunk = 500

// DeleteThread removes a thread row and everything that cascades from
// it. The thread's items are drained first in bounded chunks, each its
// own implicit transaction, so no single write transaction ever spans a
// large thread's whole item set. Draining items before the thread row
// is safe under the app layer's idempotent-retry model: the thread row
// is the resumability anchor, and a crash mid-drain leaves a thread a
// retried delete completes.
func (s *Store) DeleteThread(id string) error {
	for {
		n, err := s.deleteThreadItemsChunk(id)
		if err != nil {
			return err
		}
		if n < deleteThreadItemChunk {
			break
		}
	}
	result, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete thread %s", id))
}

// deleteThreadItemsChunk removes one bounded slice while aggregating the
// history stamps the per-item delete trigger would otherwise write one at a
// time. The flag, deletes, exact rev/epoch advance, and flag reset share one
// transaction: readers either see the prior chunk or the shortened history
// with its new stamp, and any failure rolls the flag back with the rows.
func (s *Store) deleteThreadItemsChunk(id string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin delete thread %s item chunk: %w", id, err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE threads SET history_bulk_load = 1
		  WHERE id = ? AND history_bulk_load = 0`, id,
	)
	if err != nil {
		return 0, fmt.Errorf("store: begin delete thread %s item history: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: begin delete thread %s item history", id)); err != nil {
		return 0, err
	}

	result, err = tx.Exec(
		`DELETE FROM items
		  WHERE rowid IN (SELECT rowid FROM items WHERE thread_id = ? LIMIT ?)`,
		id, deleteThreadItemChunk,
	)
	if err != nil {
		return 0, fmt.Errorf("store: delete thread %s items: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete thread %s items count: %w", id, err)
	}

	result, err = tx.Exec(
		`UPDATE threads
		    SET history_rev = history_rev + ?,
		        history_epoch = history_epoch + ?,
		        history_bulk_load = 0
		  WHERE id = ? AND history_bulk_load = 1`,
		n, n, id,
	)
	if err != nil {
		return 0, fmt.Errorf("store: finish delete thread %s item history: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: finish delete thread %s item history", id)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit delete thread %s item chunk: %w", id, err)
	}
	return n, nil
}

// ArchiveThread flips the archived column to 1 and bumps updated_at so the
// thread leaves the active sidebar. Returns the archived row plus whether the
// write moved anything: re-archiving an already-archived thread changes
// nothing, so it must not bump updated_at and must not broadcast. A missing
// id is still sql.ErrNoRows.
func (s *Store) ArchiveThread(id string) (Thread, bool, error) {
	return s.applyThreadRowWrite(rowWrite{
		Action:  fmt.Sprintf("store: archive thread %s", id),
		ID:      id,
		Set:     "archived = 1, updated_at = ?",
		SetArgs: []any{nowMillis()},
		Change:  "archived IS NOT 1",
	})
}

// UnarchiveThread flips the archived column back to 0 for a thread and bumps
// updated_at so the sidebar reshuffles it toward the top of the active list.
// Returns the restored row plus whether the write moved anything; a thread
// that was already active is a no-op, not a reshuffle. Returns sql.ErrNoRows
// if no row matches the id.
func (s *Store) UnarchiveThread(id string) (Thread, bool, error) {
	return s.applyThreadRowWrite(rowWrite{
		Action:  fmt.Sprintf("store: unarchive thread %s", id),
		ID:      id,
		Set:     "archived = 0, updated_at = ?",
		SetArgs: []any{nowMillis()},
		Change:  "archived IS NOT 0",
	})
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
//
// The context is required rather than optional. This is a write, so it
// runs on the single writer connection, and database/sql makes a caller
// wait for that connection to come free — behind a retention sweep's
// delete batch, a streaming flush, or a checkpoint. A context-less
// Begin waits for that with no ceiling; callers of a bookkeeping write
// nobody is watching need one.
//
// Returns the stamped row plus whether the stamp moved. Opening a thread
// whose read marker already covers its newest turn is the common case and
// changes nothing, so it hands back (zero row, false, nil) and the caller
// broadcasts nothing.
func (s *Store) MarkThreadReadNow(ctx context.Context, id string) (Thread, bool, error) {
	now := nowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: begin mark thread read %s: %w", id, err)
	}
	defer tx.Rollback()

	var latestTurnCompletedAt, latestIncompleteStartedAt, lastReadAt sql.NullInt64
	err = tx.QueryRowContext(
		ctx,
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
			return Thread{}, false, fmt.Errorf("store: mark thread read %s: %w", id, sql.ErrNoRows)
		}
		return Thread{}, false, fmt.Errorf("store: read thread read-state %s: %w", id, err)
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
				return Thread{}, false, fmt.Errorf("store: commit mark thread read no-op %s: %w", id, err)
			}
			return Thread{}, false, nil
		}
	}

	readAt := now
	if hasReadTarget && readTarget > readAt {
		readAt = readTarget
	}
	if lastReadAt.Valid && lastReadAt.Int64 >= readAt {
		if err := tx.Commit(); err != nil {
			return Thread{}, false, fmt.Errorf("store: commit mark thread read no-op %s: %w", id, err)
		}
		return Thread{}, false, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE threads SET last_read_at = ? WHERE id = ?`, readAt, id)
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: mark thread read %s: %w", id, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: mark thread read %s", id)); err != nil {
		return Thread{}, false, err
	}
	// Read back inside the write's own transaction, like every other
	// thread-row mutation: the caller broadcasts this row on
	// `thread:updated`, and the two no-op returns above are what keeps a
	// re-open of an already-read thread silent.
	rows, err := listThreadsByIDTx(tx, []string{id})
	if err != nil {
		return Thread{}, false, fmt.Errorf("store: read back mark thread read %s: %w", id, err)
	}
	if len(rows) != 1 {
		return Thread{}, false, fmt.Errorf("store: read back mark thread read %s: %d rows, want 1", id, len(rows))
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, false, fmt.Errorf("store: commit mark thread read %s: %w", id, err)
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
	if group != PinGroupFront && group != PinGroupBack {
		return Thread{}, false, fmt.Errorf("%w: %d", ErrInvalidPinGroup, group)
	}
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: update pin_group for pinned thread %s", id),
		ID:         id,
		Set:        "pin_group = ?",
		SetArgs:    []any{group},
		Match:      "pinned_at IS NOT NULL",
		Change:     "pin_group IS NOT ?",
		ChangeArgs: []any{group},
	})
}

// setThreadPinnedAt is the shared pin/unpin primitive. We deliberately do NOT
// touch updated_at here: pinning is a sidebar-presentation tweak, not
// thread activity, and bumping updated_at would shuffle the project's
// `lastActivity` ordering.
func (s *Store) setThreadPinnedAt(id string, ts *int64) (Thread, bool, error) {
	write := rowWrite{
		Action: fmt.Sprintf("store: update pin state for %s", id),
		ID:     id,
	}
	if ts == nil {
		write.Set = "pinned_at = NULL, pin_group = NULL"
		write.Change = "pinned_at IS NOT NULL"
	} else {
		write.Set = "pinned_at = ?, pin_group = ?"
		write.SetArgs = []any{*ts, PinGroupFront}
		// A grouped row holds no pin of its own (the v76 CHECK). Making
		// that the write's eligibility predicate keeps the refusal from
		// surfacing as a raw constraint failure; a grouped row misses the
		// same predicate in the miss probe, and threadIsGrouped names it.
		write.Match = "group_id IS NULL"
	}
	row, changed, err := s.applyThreadRowWrite(write)
	if ts != nil && errors.Is(err, sql.ErrNoRows) && s.threadIsGrouped(id) {
		return Thread{}, false, fmt.Errorf("store: pin %s: %w", id, ErrThreadGrouped)
	}
	return row, changed, err
}

// threadIsGrouped is the failure-path probe behind ErrThreadGrouped. A
// missing row reads as ungrouped so the caller's sql.ErrNoRows stands.
func (s *Store) threadIsGrouped(id string) bool {
	var grouped bool
	if err := s.db.QueryRow(
		`SELECT group_id IS NOT NULL FROM threads WHERE id = ?`, id,
	).Scan(&grouped); err != nil {
		return false
	}
	return grouped
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

// CompareAndSwapModelProfile replaces the model-related fields only when they
// still match before. Import refresh uses this to restore provider-recorded
// settings without overwriting a model selection the user made after the
// refresh plan was built.
func (s *Store) CompareAndSwapModelProfile(before, after Thread) (bool, error) {
	if before.ID == "" || after.ID != before.ID {
		return false, fmt.Errorf("store: compare-and-swap model profile requires one matching thread id")
	}
	normalized, err := normalizeThreadForUpdate(after)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(
		`UPDATE threads
		    SET model = ?, reasoning_effort = ?, fast_mode = ?, context_window = ?
		  WHERE id = ? AND model = ? AND reasoning_effort = ?
		    AND fast_mode = ? AND context_window = ?`,
		normalized.Model, normalized.ReasoningEffort, boolToInt(normalized.FastMode), normalized.ContextWindow,
		before.ID, before.Model, before.ReasoningEffort, boolToInt(before.FastMode), before.ContextWindow,
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

// UpdateMode overwrites the thread's mode (chat, plan, discussion,
// terminal, or a workflow-owned mode). Empty strings are normalized to "chat" to
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
	var providerName string
	if err := s.reader().QueryRow(`SELECT provider FROM threads WHERE id = ?`, threadID).Scan(&providerName); err != nil {
		return Thread{}, false, fmt.Errorf("store: load provider for effort update %s: %w", threadID, err)
	}
	if !legalEffortForProvider(providerName, normalized) {
		return Thread{}, false, fmt.Errorf("%w: %s/%s", ErrInvalidEffort, providerName, normalized)
	}
	return s.applyThreadRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: update reasoning effort for %s", threadID),
		ID:         threadID,
		Set:        "reasoning_effort = ?",
		SetArgs:    []any{normalized},
		Change:     "reasoning_effort IS NOT ?",
		ChangeArgs: []any{normalized},
	})
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
