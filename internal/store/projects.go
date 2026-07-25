package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrProjectPathInUse is returned by CreateProject when the supplied path
// already has a project row. Bindings redirect to the existing project
// instead of surfacing an error to the user.
var ErrProjectPathInUse = errors.New("store: project path already in use")

// ErrProjectHasThreads is returned when project deletion is attempted before
// the app layer has explicitly torn down every contained thread.
var ErrProjectHasThreads = errors.New("store: project still has threads")

// ProjectWithCounts is the sidebar-lightweight view: the project row plus
// its thread count and the timestamp of the most recently touched thread.
// LastActive is 0 when the project has no active (non-archived) threads.
type ProjectWithCounts struct {
	Project     Project `json:"project"`
	ThreadCount int     `json:"threadCount"`
	LastActive  int64   `json:"lastActive,omitempty"`
}

// projectColumns omits the dead workflow_queue_paused / workflow_concurrency
// columns (migration v32). The work queue was removed in workflows rev 2; the
// columns survive only because SQLite refuses DROP COLUMN on a CHECK-bearing
// column and rebuilding the FK-parent projects table to delete two unread
// integers is not worth the blast radius. Nothing reads or writes them.
const projectColumns = `id, path, name, slug, color, sort_position, created_at, updated_at, archived`

func scanProject(scanner interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var archived int
	if err := scanner.Scan(
		&p.ID, &p.Path, &p.Name, &p.Slug, &p.Color, &p.SortPosition,
		&p.CreatedAt, &p.UpdatedAt, &archived,
	); err != nil {
		return Project{}, err
	}
	p.Archived = archived != 0
	return p, nil
}

// CreateProject inserts a new project row. The path UNIQUE constraint
// surfaces as ErrProjectPathInUse so callers can decide whether to
// redirect the user to the existing project.
func (s *Store) CreateProject(p Project) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: create project: begin: %w", err)
	}
	p.Slug, err = nextProjectSlug(p.Name, func(candidate string) (bool, error) {
		var exists bool
		if err := tx.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM projects WHERE slug = ?)`, candidate,
		).Scan(&exists); err != nil {
			return false, fmt.Errorf("query project slug %q: %w", candidate, err)
		}
		return exists, nil
	})
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: create project: %w", err)
	}
	_, err = tx.Exec(
		`INSERT INTO projects (id, path, name, slug, color, sort_position, created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Path, p.Name, p.Slug, p.Color, p.SortPosition,
		p.CreatedAt, p.UpdatedAt, boolToInt(p.Archived),
	)
	if err != nil {
		_ = tx.Rollback()
		// modernc.org/sqlite returns the constraint text verbatim; we
		// detect by substring because exposing the full driver error
		// type would couple this package to the driver.
		if isUniqueConstraintError(err, "projects.path") {
			return fmt.Errorf("%w: %s", ErrProjectPathInUse, p.Path)
		}
		return fmt.Errorf("store: create project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create project: commit: %w", err)
	}
	return nil
}

// GetProject returns a single project by id. Returns sql.ErrNoRows when
// the id doesn't exist.
func (s *Store) GetProject(id string) (Project, error) {
	row := s.db.QueryRow(
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id,
	)
	p, err := scanProject(row)
	if err != nil {
		return Project{}, fmt.Errorf("store: get project %s: %w", id, err)
	}
	return p, nil
}

// GetProjectByPath looks up a project by its filesystem path. Used during
// CreateProject to dedupe when the user adds a path that already belongs
// to a project.
func (s *Store) GetProjectByPath(path string) (Project, error) {
	row := s.db.QueryRow(
		`SELECT `+projectColumns+` FROM projects WHERE path = ?`, path,
	)
	p, err := scanProject(row)
	if err != nil {
		return Project{}, fmt.Errorf("store: get project by path %s: %w", path, err)
	}
	return p, nil
}

// GetProjectBySlug resolves the stable user-facing identifier accepted by
// workflow tooling.
func (s *Store) GetProjectBySlug(slug string) (Project, error) {
	row := s.db.QueryRow(
		`SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug,
	)
	p, err := scanProject(row)
	if err != nil {
		return Project{}, fmt.Errorf("store: get project by slug %s: %w", slug, err)
	}
	return p, nil
}

// ListProjects returns all non-archived projects ordered by name ASC.
// The frontend re-sorts client-side when the user picks a different
// ordering.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT ` + projectColumns + ` FROM projects WHERE archived = 0 ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan project row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProjectsWithThreadCounts returns every non-archived project plus
// its thread count and the most-recent thread timestamp. The LEFT JOIN
// keeps projects with zero threads in the result. Draft threads (no
// items persisted) are excluded from the last_active MAX so creating or
// configuring an unsent thread does not move the project to the top of
// the sidebar — only real activity (first message send and onward,
// gated by MarkThreadActivity) counts.
func (s *Store) ListProjectsWithThreadCounts() ([]ProjectWithCounts, error) {
	hiddenClause, hiddenArgs := hiddenThreadModesClause("t.mode")
	rows, err := s.db.Query(
		`SELECT p.id, p.path, p.name, p.slug, p.color, p.sort_position,
		        p.created_at, p.updated_at, p.archived,
		        COALESCE(COUNT(t.id), 0) AS thread_count,
		        COALESCE(
		          MAX(CASE
		            WHEN EXISTS (SELECT 1 FROM items i WHERE i.thread_id = t.id)
		            THEN t.updated_at
		          END),
		          0
		        ) AS last_active
		 FROM projects p
		 LEFT JOIN threads t ON t.project_id = p.id AND t.archived = 0 AND `+hiddenClause+`
		 WHERE p.archived = 0
		 GROUP BY p.id
		 ORDER BY p.name ASC`, hiddenArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list projects with counts: %w", err)
	}
	defer rows.Close()

	var out []ProjectWithCounts
	for rows.Next() {
		var (
			pwc      ProjectWithCounts
			archived int
		)
		if err := rows.Scan(
			&pwc.Project.ID, &pwc.Project.Path, &pwc.Project.Name, &pwc.Project.Slug, &pwc.Project.Color,
			&pwc.Project.SortPosition,
			&pwc.Project.CreatedAt, &pwc.Project.UpdatedAt, &archived,
			&pwc.ThreadCount, &pwc.LastActive,
		); err != nil {
			return nil, fmt.Errorf("store: scan project-with-counts row: %w", err)
		}
		pwc.Project.Archived = archived != 0
		out = append(out, pwc)
	}
	return out, rows.Err()
}

// UpdateProjectName overwrites the display name. Path is immutable after
// creation; renaming semantically means "new project + move threads", a
// flow not supported in v1.
func (s *Store) UpdateProjectName(id, name string) error {
	result, err := s.db.Exec(
		`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`,
		name, nowMillis(), id,
	)
	if err != nil {
		return fmt.Errorf("store: update project name %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update project name %s", id))
}

// UpdateProjectSortPositions assigns a fresh sort_position to each id in
// the order supplied. The sidebar uses this when the user drag-reorders
// the project list under the "manual" sort mode. Positions are dense
// 0..N-1 — the bulk update normalises any gaps left by archive / delete.
//
// Runs as a single transaction so a partial write doesn't leave the
// list half-reordered if SQLite errors mid-batch. Ids not present in
// the supplied slice keep their existing positions.
func (s *Store) UpdateProjectSortPositions(orderedIDs []string) error {
	if len(orderedIDs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: update project sort positions: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE projects SET sort_position = ?, updated_at = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("store: update project sort positions: prepare: %w", err)
	}
	defer stmt.Close()

	now := nowMillis()
	for index, id := range orderedIDs {
		if _, err := stmt.Exec(index, now, id); err != nil {
			return fmt.Errorf("store: update project sort position %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update project sort positions: commit: %w", err)
	}
	return nil
}

// ArchiveProject hides the project from default listings. Threads remain
// intact; UnarchiveProject reverses it.
func (s *Store) ArchiveProject(id string) error {
	result, err := s.db.Exec(
		`UPDATE projects SET archived = 1, updated_at = ? WHERE id = ?`,
		nowMillis(), id,
	)
	if err != nil {
		return fmt.Errorf("store: archive project %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: archive project %s", id))
}

// UnarchiveProject reverses ArchiveProject and bumps updated_at so the
// project resurfaces at the top of any "recently touched" ordering.
func (s *Store) UnarchiveProject(id string) error {
	result, err := s.db.Exec(
		`UPDATE projects SET archived = 0, updated_at = ? WHERE id = ?`,
		nowMillis(), id,
	)
	if err != nil {
		return fmt.Errorf("store: unarchive project %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: unarchive project %s", id))
}

// DeleteProject removes the project row only when it contains no threads. The
// app layer must tear threads down explicitly so provider processes and
// per-thread resources are cleaned before their ownership rows disappear.
// Keeping the emptiness predicate in the DELETE closes the race with a thread
// inserted immediately before this statement.
func (s *Store) DeleteProject(id string) error {
	result, err := s.db.Exec(
		`DELETE FROM projects
		  WHERE id = ?
		    AND NOT EXISTS (SELECT 1 FROM threads WHERE project_id = ?)`,
		id, id,
	)
	if err != nil {
		return fmt.Errorf("store: delete project %s: %w", id, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete project %s rows affected: %w", id, err)
	}
	if deleted == 0 {
		var exists int
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE id = ?)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("store: check project %s after guarded delete: %w", id, err)
		}
		if exists != 0 {
			return fmt.Errorf("%w: %s", ErrProjectHasThreads, id)
		}
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete project %s", id))
}

// ListThreadIDsForProject returns every thread owned by a project. The app
// layer uses the list to tear each thread down and to tell the frontend which
// pane state to prune after project deletion.
func (s *Store) ListThreadIDsForProject(projectID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM threads WHERE project_id = ?`, projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list thread ids for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan thread id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// isUniqueConstraintError checks whether err was raised by SQLite as a
// UNIQUE constraint failure on the given target column. modernc.org/sqlite
// formats these errors as "UNIQUE constraint failed: table.column"; we
// match by substring because the driver does not expose typed errors.
func isUniqueConstraintError(err error, column string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, column)
}

// compile-time check: DeleteProject relies on FK cascade. If this ever
// evolves to require driver-level row reports, switch to the Go API's
// sql.Result and adapt accordingly.
var _ = sql.ErrNoRows
