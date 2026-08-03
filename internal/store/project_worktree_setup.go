package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/worktreesetup"
)

// ProjectWorktreeSetup returns the project's worktree setup recipe and whether
// one is configured at all. An empty column is the unconfigured state; a
// non-empty column that does not decode is an ERROR, never an empty recipe —
// silently running no setup on a worktree whose project asked for one is the
// failure mode this refuses.
//
// Decoding is strict: an unknown field means the blob was written by something
// that does not agree with this build about what a recipe is.
func (s *Store) ProjectWorktreeSetup(projectID string) (worktreesetup.Config, bool, error) {
	var raw string
	if err := s.reader().QueryRow(
		`SELECT worktree_setup FROM projects WHERE id = ?`, projectID,
	).Scan(&raw); err != nil {
		return worktreesetup.Config{}, false, fmt.Errorf("store: get project %s worktree setup: %w", projectID, err)
	}
	if strings.TrimSpace(raw) == "" {
		return worktreesetup.Config{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var config worktreesetup.Config
	if err := decoder.Decode(&config); err != nil {
		return worktreesetup.Config{}, false, fmt.Errorf("store: decode project %s worktree setup: %w", projectID, err)
	}
	return config, true, nil
}

// UpdateProjectWorktreeSetup persists (or, with a nil config, clears) the
// project's worktree setup recipe. A config that asks for nothing clears the
// column too, so "cleared" has one representation however the caller expressed
// it — a later read cannot report a configured-but-empty recipe.
//
// Validation belongs to the caller (worktreesetup.Validate): this package
// persists what it is given, but it will not persist a blob it could not read
// back.
func (s *Store) UpdateProjectWorktreeSetup(projectID string, config *worktreesetup.Config) error {
	stored := ""
	if config != nil && !config.IsZero() {
		data, err := json.Marshal(*config)
		if err != nil {
			return fmt.Errorf("store: encode project %s worktree setup: %w", projectID, err)
		}
		stored = string(data)
	}
	result, err := s.db.Exec(
		`UPDATE projects SET worktree_setup = ?, updated_at = ? WHERE id = ?`,
		stored, nowMillis(), projectID,
	)
	if err != nil {
		return fmt.Errorf("store: update project %s worktree setup: %w", projectID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update project %s worktree setup", projectID))
}
