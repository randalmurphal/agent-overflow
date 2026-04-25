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

// ProjectWithCounts is the sidebar-lightweight view: the project row plus
// its thread count and the timestamp of the most recently touched thread.
// LastActive is 0 when the project has no active (non-archived) threads.
type ProjectWithCounts struct {
	Project     Project `json:"project"`
	ThreadCount int     `json:"threadCount"`
	LastActive  int64   `json:"lastActive,omitempty"`
}

const projectColumns = `id, path, name, color, sort_position, created_at, updated_at, archived`

func scanProject(scanner interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var archived int
	if err := scanner.Scan(
		&p.ID, &p.Path, &p.Name, &p.Color, &p.SortPosition,
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
	_, err := s.db.Exec(
		`INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Path, p.Name, p.Color, p.SortPosition,
		p.CreatedAt, p.UpdatedAt, boolToInt(p.Archived),
	)
	if err != nil {
		// modernc.org/sqlite returns the constraint text verbatim; we
		// detect by substring because exposing the full driver error
		// type would couple this package to the driver.
		if isUniqueConstraintError(err, "projects.path") {
			return fmt.Errorf("%w: %s", ErrProjectPathInUse, p.Path)
		}
		return fmt.Errorf("store: create project: %w", err)
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
// keeps projects with zero threads in the result.
func (s *Store) ListProjectsWithThreadCounts() ([]ProjectWithCounts, error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.path, p.name, p.color, p.sort_position,
		        p.created_at, p.updated_at, p.archived,
		        COALESCE(COUNT(t.id), 0) AS thread_count,
		        COALESCE(MAX(t.updated_at), 0) AS last_active
		 FROM projects p
		 LEFT JOIN threads t ON t.project_id = p.id AND t.archived = 0
		 WHERE p.archived = 0
		 GROUP BY p.id
		 ORDER BY p.name ASC`,
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
			&pwc.Project.ID, &pwc.Project.Path, &pwc.Project.Name, &pwc.Project.Color,
			&pwc.Project.SortPosition, &pwc.Project.CreatedAt, &pwc.Project.UpdatedAt, &archived,
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

// DeleteProject removes the project row. Threads cascade via the
// ON DELETE CASCADE on threads.project_id.
func (s *Store) DeleteProject(id string) error {
	result, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete project %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete project %s", id))
}

// ListThreadIDsForProject returns the ids of every thread that would be
// cascaded away by DeleteProject. The binding layer uses this to build
// the list of deleted thread ids it hands to the frontend so pane state
// can prune.
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
