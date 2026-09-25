package store

import (
	"fmt"
	"strconv"
	"strings"
)

// The two thread reads the agent thread tools resolve and list with. Both
// answer in SQL what a caller would otherwise answer by loading every
// thread row and filtering in Go: this computer can hold tens of thousands
// of threads, and `threadColumns` carries correlated subqueries, so a Go
// filter over the whole listing pays for a proposed-plan probe and a turn
// aggregate on every row it then throws away.

// ResolveThreadPrefix returns the threads whose id starts with prefix,
// ordered by id, at most limit rows. Every mode is included, hidden ones
// too, and so are archived threads: a reference names a thread the caller
// already knows about, and deciding which of those a particular caller may
// see belongs to the caller's own visibility rule, not to the lookup.
// Threads this computer gave away are excluded, as `owned_threads`
// excludes them everywhere else.
//
// The match is a half-open range over the id primary key rather than a
// LIKE. `threads.id` collates BINARY and `case_sensitive_like` is off, so
// `id LIKE 'abcd%'` would both scan the table and match case-insensitively,
// which is not what a thread reference means; a range bound is exact,
// case-sensitive, index-driven, and gives `_`, `%` and `\` no special
// meaning, so there is no escaping to get wrong.
//
// An empty prefix matches nothing. A prefix-less listing is
// ListThreadsByActivity, and answering it here would hand back the whole
// table under the name of a lookup. A non-positive limit matches nothing
// either.
func (s *Store) ResolveThreadPrefix(prefix string, limit int) ([]Thread, error) {
	if prefix == "" || limit <= 0 {
		return nil, nil
	}
	conditions := []string{"threads.id >= ?"}
	args := []any{prefix}
	if upper, bounded := prefixUpperBound(prefix); bounded {
		conditions = append(conditions, "threads.id < ?")
		args = append(args, upper)
	}
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY threads.id ASC
		  LIMIT `+strconv.Itoa(limit),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: resolve thread prefix %q: %w", prefix, err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread prefix row: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// prefixUpperBound returns the exclusive upper bound of the byte range a
// prefix owns: the prefix with its last byte below 0xFF incremented.
// bounded is false when every byte is 0xFF, which has no upper bound and
// leaves the range open above.
//
// SQLite compares BINARY text by bytes, so incrementing a byte is the
// correct bound whether or not the result is well-formed UTF-8.
func prefixUpperBound(prefix string) (string, bool) {
	bytes := []byte(prefix)
	for i := len(bytes) - 1; i >= 0; i-- {
		if bytes[i] == 0xFF {
			continue
		}
		bytes[i]++
		return string(bytes[:i+1]), true
	}
	return "", false
}

// ListThreadsByActivity is the query-less half of thread search: this
// computer's threads by last activity, newest first, under the same filter
// SearchThreads applies to its hits. Last activity is the newest completed
// turn, falling back to updated_at, which is the clock the sidebar and the
// tools both call a thread's activity; `updated_at` alone would order a
// thread by the last row written to it, including a write no reader sees.
//
// Ties break on id so LIMIT/OFFSET paging is stable across calls. Kinds and
// SnippetBudget are ignored; every other filter field applies.
func (s *Store) ListThreadsByActivity(filter ThreadSearchFilter) ([]Thread, error) {
	conditions, args := filter.threadRowConditions("threads.")
	if len(filter.ThreadIDs) > 0 {
		clause, clauseArgs := inClause("threads.id", filter.ThreadIDs)
		conditions = append(conditions, clause)
		args = append(args, clauseArgs...)
	}
	limit := filter.Limit
	if limit < 1 {
		limit = 20
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, offset)

	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads
		  WHERE `+strings.Join(conditions, " AND ")+`
		  ORDER BY `+threadLastActivityExpr("threads.")+` DESC, threads.id ASC
		  LIMIT `+strconv.Itoa(limit)+` OFFSET ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads by activity: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread activity row: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// ListOwnedThreadsByID reads the threads this computer owns by id, in one
// statement. The ranked search answers in ITEM rows, and resolving each
// hit's thread on its own would pay the correlated subqueries of
// `threadColumns` once per hit rather than once per page.
//
// An id this computer does not own is simply absent, so the caller matches
// the result by id rather than by position.
func (s *Store) ListOwnedThreadsByID(ids []string) ([]Thread, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	clause, args := inClause("threads.id", ids)
	rows, err := s.reader().Query(
		`SELECT `+threadColumns+` FROM owned_threads AS threads WHERE `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads by id: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan thread by id: %w", err)
		}
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

// ThreadWorktreeWorkspace is one worktree checkout this computer's threads
// run in, with the branch of the most recently touched thread that names
// it.
type ThreadWorktreeWorkspace struct {
	ProjectID string
	Path      string
	Branch    string
}

// ListThreadWorktreeWorkspaces returns the distinct worktree checkouts the
// unarchived threads of this computer run in, newest-touched first. It is
// how the spawn catalog names the workspaces a project offers beyond its
// root: this app records a worktree on the thread that runs in it and has
// no worktree table, and asking git for a listing per project would be a
// subprocess per call on a read that happens whenever a model asks what it
// can start.
//
// `branch` and `project_id` are bare columns beside MAX(updated_at) in the
// group, which SQLite answers from the row holding that maximum; that is
// what makes the reported branch the newest thread's rather than an
// arbitrary one.
func (s *Store) ListThreadWorktreeWorkspaces() ([]ThreadWorktreeWorkspace, error) {
	rows, err := s.reader().Query(
		`SELECT COALESCE(project_id, ''), worktree_path, COALESCE(branch, ''), MAX(updated_at)
		   FROM owned_threads AS threads
		  WHERE archived = 0 AND worktree_path IS NOT NULL AND worktree_path <> ''
		  GROUP BY project_id, worktree_path
		  ORDER BY MAX(updated_at) DESC, worktree_path ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread worktree workspaces: %w", err)
	}
	defer rows.Close()

	var out []ThreadWorktreeWorkspace
	for rows.Next() {
		var row ThreadWorktreeWorkspace
		var updatedAt int64
		if err := rows.Scan(&row.ProjectID, &row.Path, &row.Branch, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan thread worktree workspace: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
