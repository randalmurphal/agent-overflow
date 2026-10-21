package store

import (
	"encoding/json"
	"errors"
	"fmt"
)

// PRListProject is one repository's last project read on the Pull
// requests page (migration v146): its key (git.RepoReference.Key), the
// published ProjectPRList as JSON, and when the read landed (unix
// milliseconds).
type PRListProject struct {
	RepoKey     string
	ProjectJSON []byte
	ReadAt      int64
}

// maxPRListProjectBytes bounds one stored project read: 1000 rows of a
// few KiB each.
const maxPRListProjectBytes = 16 << 20

// PutPRListProject stores a repository's project read, replacing the one
// before.
func (s *Store) PutPRListProject(project PRListProject) error {
	switch {
	case project.RepoKey == "" || len(project.RepoKey) > MaxDiffReviewSourceKeyBytes || hasControlCharacter(project.RepoKey):
		return fmt.Errorf("store: PR list repository key %q is invalid", project.RepoKey)
	case project.ReadAt <= 0:
		return fmt.Errorf("store: PR list read of %s needs the time it landed", project.RepoKey)
	case !json.Valid(project.ProjectJSON):
		return fmt.Errorf("store: PR list read of %s is not JSON", project.RepoKey)
	case len(project.ProjectJSON) > maxPRListProjectBytes:
		return fmt.Errorf("store: PR list read of %s is %d bytes, over %d", project.RepoKey, len(project.ProjectJSON), maxPRListProjectBytes)
	}
	if _, err := s.db.Exec(`INSERT INTO pr_list_projects(repo_key, project_json, read_at) VALUES (?, ?, ?)
		ON CONFLICT(repo_key) DO UPDATE SET project_json = excluded.project_json, read_at = excluded.read_at`,
		project.RepoKey, project.ProjectJSON, project.ReadAt); err != nil {
		return fmt.Errorf("store: record PR list read of %s: %w", project.RepoKey, err)
	}
	return nil
}

// ListPRListProjects reads every stored project read, by repository key.
// There is one per forge repository the projects name.
func (s *Store) ListPRListProjects() (map[string]PRListProject, error) {
	rows, err := s.reader().Query(`SELECT repo_key, project_json, read_at FROM pr_list_projects`)
	if err != nil {
		return nil, fmt.Errorf("store: list PR list reads: %w", err)
	}
	out := map[string]PRListProject{}
	for rows.Next() {
		var project PRListProject
		if err := rows.Scan(&project.RepoKey, &project.ProjectJSON, &project.ReadAt); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan PR list read: %w", err), rows.Close())
		}
		out[project.RepoKey] = project
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list PR list reads: %w", err)
	}
	return out, nil
}

// NewestPRListReadAt is the latest read_at of the stored project reads,
// 0 with none.
func (s *Store) NewestPRListReadAt() (int64, error) {
	var newest int64
	if err := s.reader().QueryRow(`SELECT COALESCE(MAX(read_at), 0) FROM pr_list_projects`).Scan(&newest); err != nil {
		return 0, fmt.Errorf("store: newest PR list read: %w", err)
	}
	return newest, nil
}

// DeletePRListProjectsExcept deletes the stored reads of every repository
// keep does not name, and reports how many it deleted.
func (s *Store) DeletePRListProjectsExcept(keep []string) (int64, error) {
	// uniqueNonEmptyStrings answers [] for none, never null: json_each(null)
	// is one NULL row, and NOT IN (NULL) would delete nothing.
	encoded, err := json.Marshal(uniqueNonEmptyStrings(keep))
	if err != nil {
		return 0, fmt.Errorf("store: prune PR list reads: %w", err)
	}
	result, err := s.db.Exec(`DELETE FROM pr_list_projects WHERE repo_key NOT IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return 0, fmt.Errorf("store: prune PR list reads: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune PR list reads: %w", err)
	}
	return n, nil
}
