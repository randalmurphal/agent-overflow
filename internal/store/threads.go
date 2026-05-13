package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// threadColumns lists every column in the order scanThread expects. The
// COALESCE-ing of nullable text columns returns "" instead of NULL so the
// Go struct has a clean empty-string value for unset optional fields.
// last_read_at and pinned_at are deliberately NOT coalesced — scanThread
// keeps the NULL / non-NULL distinction via *int64 pointers so the
// frontend can tell "never tracked" / "unpinned" apart from a zero
// timestamp. The two boolean tail columns are derived sidebar state:
// they are cheap scalar probes over indexed tables, not threads columns.
const threadColumns = `id, project_id,
    COALESCE((SELECT path FROM projects WHERE projects.id = threads.project_id), ''),
    title, provider, model,
    workspace_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''),
    mode, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
    COALESCE(forked_from_thread_id, ''), last_token_usage,
    created_at, updated_at,
    (SELECT MAX(completed_at) FROM turns
      WHERE turns.thread_id = threads.id AND completed_at IS NOT NULL),
    archived, last_read_at, pinned_at,
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
      SELECT turns.completed_at IS NULL
         AND (threads.last_read_at IS NULL OR threads.last_read_at < turns.started_at)
        FROM turns
       WHERE turns.thread_id = threads.id
       ORDER BY turns.turn_index DESC
       LIMIT 1
    ), 0)`

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
)

// legalModes maps every valid mode value to struct{}{} so membership
// checks are constant-time. Kept in sync with the CHECK constraint on
// threads.mode (see migrate.go::v13SQL).
var legalModes = map[string]struct{}{
	"chat":       {},
	"plan":       {},
	"design":     {},
	"discussion": {},
}

var legalEfforts = map[string]struct{}{
	"none":    {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
}

func legalEffortForProvider(providerName, effort string) bool {
	switch providerName {
	case "codex":
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
			return true
		default:
			return false
		}
	case "claude":
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
	"claude": {},
	"codex":  {},
}

func validContextWindow(tokens int) bool {
	return tokens > 0
}

func validAutoCompactPercent(percent int) bool {
	return percent >= 0 && percent <= 90
}

func scanThread(scanner interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var archived, fastMode, hasActionableProposedPlan, hasIncompleteTurn int
	var latestTurnCompletedAt, lastReadAt, pinnedAt sql.NullInt64
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.ProjectPath, &t.Title, &t.Provider, &t.Model,
		&t.WorkspacePath, &t.WorktreePath, &t.Branch,
		&t.SessionRef, &t.PendingForkRef,
		&t.Mode, &t.ReasoningEffort, &fastMode, &t.ContextWindow,
		&t.AutoCompactStandardPercent, &t.AutoCompactExtendedPercent, &t.RuntimeMode,
		&t.DiscussionID, &t.ParentThreadID, &t.ForkedFromThreadID, &t.LastTokenUsage,
		&t.CreatedAt, &t.UpdatedAt, &latestTurnCompletedAt, &archived, &lastReadAt, &pinnedAt,
		&hasActionableProposedPlan, &hasIncompleteTurn,
	); err != nil {
		return Thread{}, err
	}
	t.FastMode = fastMode != 0
	t.Archived = archived != 0
	t.HasActionableProposedPlan = hasActionableProposedPlan != 0
	t.HasIncompleteTurn = hasIncompleteTurn != 0
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
	_, err := s.db.Exec(
		`INSERT INTO threads (id, project_id, title, provider, model,
		    workspace_path, worktree_path, branch, session_ref, pending_fork_session_ref,
		    mode, reasoning_effort, fast_mode, context_window,
		    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
		    discussion_id, parent_thread_id, forked_from_thread_id, last_token_usage,
		    created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived),
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
// the user would otherwise lose track of.
func (s *Store) ListThreadsWithItems() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads
		 WHERE archived = 0
		   AND (
		       EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id)
		    OR EXISTS (
		         SELECT 1 FROM thread_drafts
		          WHERE thread_drafts.thread_id = threads.id
		            AND (
		              TRIM(thread_drafts.content) <> ''
		              OR COALESCE(thread_drafts.attachments, '[]') NOT IN ('', '[]', 'null')
		              OR COALESCE(thread_drafts.terminal_chips, '[]') NOT IN ('', '[]', 'null')
		              OR thread_drafts.pending_plan_implementation IS NOT NULL
		            )
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

func (s *Store) ListChildThreads(parentID string) ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT `+threadColumns+` FROM threads WHERE parent_thread_id = ? ORDER BY created_at ASC`,
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

func (s *Store) UpdateThread(t Thread) error {
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
	result, err := s.db.Exec(
		`UPDATE threads SET project_id=?, title=?, provider=?, model=?,
		    workspace_path=?, worktree_path=?, branch=?, session_ref=?, pending_fork_session_ref=?,
		    mode=?, reasoning_effort=?, fast_mode=?, context_window=?,
		    auto_compact_standard_percent=?, auto_compact_extended_percent=?, runtime_mode=?,
		    discussion_id=?, parent_thread_id=?, forked_from_thread_id=?, last_token_usage=?,
		    updated_at=?, archived=?
		 WHERE id=?`,
		t.ProjectID, t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow,
		t.AutoCompactStandardPercent, t.AutoCompactExtendedPercent, t.RuntimeMode,
		nilIfEmpty(t.DiscussionID), nilIfEmpty(t.ParentThreadID), nilIfEmpty(t.ForkedFromThreadID), t.LastTokenUsage,
		t.UpdatedAt, boolToInt(t.Archived), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update thread %s", t.ID))
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
		if !hasReadTarget || lastReadAt.Int64 >= readTarget {
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

// UpdateProvider overwrites the provider ('claude' / 'codex'). Invalid
// values surface ErrInvalidProvider before hitting SQLite so the binding
// can translate to a user-facing error.
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

// UpdateMode overwrites the thread's mode (chat, plan, design, or
// discussion). Empty strings are normalized to "chat" to match
// CreateThread/UpdateThread.
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
		`SELECT provider, model, context_window,
		        auto_compact_standard_percent, auto_compact_extended_percent
		   FROM threads
		  WHERE id = ?`,
		threadID,
	).Scan(
		&settings.Provider,
		&settings.Model,
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
