package store

import (
	"errors"
	"fmt"
)

// threadColumns lists every column in the order scanThread expects. The
// COALESCE-ing of nullable text columns returns "" instead of NULL so the
// Go struct has a clean empty-string value for unset optional fields.
const threadColumns = `id, project_id, title, provider, model,
    workspace_path, COALESCE(worktree_path, ''), COALESCE(branch, ''),
    COALESCE(session_ref, ''), COALESCE(pending_fork_session_ref, ''),
    mode, reasoning_effort, fast_mode, context_window, runtime_mode,
    COALESCE(discussion_id, ''), COALESCE(parent_thread_id, ''),
    COALESCE(forked_from_thread_id, ''), last_token_usage,
    created_at, updated_at, archived`

// -- Validation errors for enum fields. Each binding checks against the
// -- list before hitting SQLite so the caller sees a typed error instead
// -- of a raw CHECK-constraint failure.
var (
	// ErrInvalidEffort is returned when a caller passes a reasoning-effort
	// value outside the five-tier enum (low / medium / high / xhigh / max).
	ErrInvalidEffort = errors.New("store: invalid reasoning effort")
	// ErrInvalidMode is returned for a bad mode value.
	ErrInvalidMode = errors.New("store: invalid thread mode")
	// ErrInvalidContextWindow is returned when the caller requests a
	// context window size the schema's CHECK constraint does not allow.
	ErrInvalidContextWindow = errors.New("store: invalid context window")
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
	"low":    {},
	"medium": {},
	"high":   {},
	"xhigh":  {},
	"max":    {},
}

var legalContextWindows = map[int]struct{}{
	200000:  {},
	1000000: {},
}

var legalProviders = map[string]struct{}{
	"claude": {},
	"codex":  {},
}

func scanThread(scanner interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var archived, fastMode int
	if err := scanner.Scan(
		&t.ID, &t.ProjectID, &t.Title, &t.Provider, &t.Model,
		&t.WorkspacePath, &t.WorktreePath, &t.Branch,
		&t.SessionRef, &t.PendingForkRef,
		&t.Mode, &t.ReasoningEffort, &fastMode, &t.ContextWindow, &t.RuntimeMode,
		&t.DiscussionID, &t.ParentThreadID, &t.ForkedFromThreadID, &t.LastTokenUsage,
		&t.CreatedAt, &t.UpdatedAt, &archived,
	); err != nil {
		return Thread{}, err
	}
	t.FastMode = fastMode != 0
	t.Archived = archived != 0
	return t, nil
}

func (s *Store) CreateThread(t Thread) error {
	t.Mode = normalizeMode(t.Mode)
	t.RuntimeMode = normalizeRuntimeMode(t.RuntimeMode)
	t.ReasoningEffort = normalizeEffort(t.ReasoningEffort)
	if t.ContextWindow == 0 {
		t.ContextWindow = 1000000
	}
	_, err := s.db.Exec(
		`INSERT INTO threads (id, project_id, title, provider, model,
		    workspace_path, worktree_path, branch, session_ref, pending_fork_session_ref,
		    mode, reasoning_effort, fast_mode, context_window, runtime_mode,
		    discussion_id, parent_thread_id, forked_from_thread_id, last_token_usage,
		    created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectID, t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow, t.RuntimeMode,
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
// one persisted item. "Draft" threads (created by the user clicking "New
// Thread" but never sent) deliberately don't appear here — they only
// surface in the sidebar once the first message lands. Discussion threads
// are still included even before their first user turn because
// StartDiscussion inserts the assistant's opening plan as an item, so the
// EXISTS probe already considers them non-empty.
func (s *Store) ListThreadsWithItems() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT ` + threadColumns + ` FROM threads
		 WHERE archived = 0
		   AND EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id)
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

func (s *Store) UpdateThread(t Thread) error {
	t.Mode = normalizeMode(t.Mode)
	t.RuntimeMode = normalizeRuntimeMode(t.RuntimeMode)
	t.ReasoningEffort = normalizeEffort(t.ReasoningEffort)
	if t.ContextWindow == 0 {
		t.ContextWindow = 1000000
	}
	result, err := s.db.Exec(
		`UPDATE threads SET project_id=?, title=?, provider=?, model=?,
		    workspace_path=?, worktree_path=?, branch=?, session_ref=?, pending_fork_session_ref=?,
		    mode=?, reasoning_effort=?, fast_mode=?, context_window=?, runtime_mode=?,
		    discussion_id=?, parent_thread_id=?, forked_from_thread_id=?, last_token_usage=?,
		    updated_at=?, archived=?
		 WHERE id=?`,
		t.ProjectID, t.Title, t.Provider, t.Model,
		t.WorkspacePath, nilIfEmpty(t.WorktreePath), nilIfEmpty(t.Branch),
		nilIfEmpty(t.SessionRef), nilIfEmpty(t.PendingForkRef),
		t.Mode, t.ReasoningEffort, boolToInt(t.FastMode), t.ContextWindow, t.RuntimeMode,
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

func (s *Store) UpdateSessionRef(threadID, ref string) error {
	result, err := s.db.Exec(
		`UPDATE threads
		 SET session_ref = ?, pending_fork_session_ref = NULL, updated_at = ?
		 WHERE id = ?`,
		ref, nowMillis(), threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update session ref for %s", threadID))
}

func (s *Store) UpdateTitle(threadID, title string) error {
	result, err := s.db.Exec(`UPDATE threads SET title = ?, updated_at = ? WHERE id = ?`,
		title, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update title for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update title for %s", threadID))
}

func (s *Store) UpdateTitleIfCurrent(threadID, currentTitle, newTitle string) (bool, error) {
	result, err := s.db.Exec(
		`UPDATE threads SET title = ?, updated_at = ? WHERE id = ? AND title = ?`,
		newTitle, nowMillis(), threadID, currentTitle,
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
	result, err := s.db.Exec(`UPDATE threads SET model = ?, updated_at = ? WHERE id = ?`,
		model, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update model for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update model for %s", threadID))
}

func (s *Store) UpdateModelAndContextWindow(threadID, model string, tokens int) error {
	if _, ok := legalContextWindows[tokens]; !ok {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	result, err := s.db.Exec(
		`UPDATE threads SET model = ?, context_window = ?, updated_at = ? WHERE id = ?`,
		model, tokens, nowMillis(), threadID,
	)
	if err != nil {
		return fmt.Errorf("store: update model/context for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update model/context for %s", threadID))
}

func (s *Store) UpdateLastTokenUsage(threadID, usage string) error {
	result, err := s.db.Exec(
		`UPDATE threads SET last_token_usage = ?, updated_at = ? WHERE id = ?`,
		usage, nowMillis(), threadID,
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
	result, err := s.db.Exec(`UPDATE threads SET provider = ?, updated_at = ? WHERE id = ?`,
		prov, nowMillis(), threadID)
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
	result, err := s.db.Exec(`UPDATE threads SET mode = ?, updated_at = ? WHERE id = ?`,
		mode, nowMillis(), threadID)
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
	result, err := s.db.Exec(`UPDATE threads SET reasoning_effort = ?, updated_at = ? WHERE id = ?`,
		normalized, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update reasoning effort for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update reasoning effort for %s", threadID))
}

// UpdateFastMode flips the fast-mode boolean.
func (s *Store) UpdateFastMode(threadID string, on bool) error {
	result, err := s.db.Exec(`UPDATE threads SET fast_mode = ?, updated_at = ? WHERE id = ?`,
		boolToInt(on), nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update fast mode for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update fast mode for %s", threadID))
}

// UpdateContextWindow overwrites the context_window column. The schema
// CHECK constraint restricts values to {200000, 1000000}; we mirror that
// here so the binding surfaces ErrInvalidContextWindow instead of a raw
// SQLite error.
func (s *Store) UpdateContextWindow(threadID string, tokens int) error {
	if _, ok := legalContextWindows[tokens]; !ok {
		return fmt.Errorf("%w: %d", ErrInvalidContextWindow, tokens)
	}
	result, err := s.db.Exec(`UPDATE threads SET context_window = ?, updated_at = ? WHERE id = ?`,
		tokens, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update context window for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update context window for %s", threadID))
}

// UpdateBranch persists a new branch string without touching the git
// working tree. Callers that want to actually switch branches should
// wrap this with the git checkout side effect.
func (s *Store) UpdateBranch(threadID, branch string) error {
	result, err := s.db.Exec(`UPDATE threads SET branch = ?, updated_at = ? WHERE id = ?`,
		nilIfEmpty(branch), nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update branch for %s: %w", threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update branch for %s", threadID))
}

// UpdateWorkspacePath overwrites workspace_path. Used by the env/worktree
// picker when a thread switches between the project root and a worktree.
func (s *Store) UpdateWorkspacePath(threadID, path string) error {
	result, err := s.db.Exec(`UPDATE threads SET workspace_path = ?, updated_at = ? WHERE id = ?`,
		path, nowMillis(), threadID)
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
	result, err := s.db.Exec(`UPDATE threads SET runtime_mode = ?, updated_at = ? WHERE id = ?`,
		mode, nowMillis(), threadID)
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
