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
//
// Returns the project row and whether the write moved it. The recipe itself is
// not on that row — `worktree_setup` is deliberately outside projectColumns,
// since the sidebar reads the project list far more often than anyone opens
// the recipe editor — so the row carries only the bumped updated_at. That is
// still what the `project:updated` broadcast needs: the announcement is "this
// project moved", and the recipe has its own read (ProjectWorktreeSetup).
func (s *Store) UpdateProjectWorktreeSetup(projectID string, config *worktreesetup.Config) (Project, bool, error) {
	stored := ""
	if config != nil && !config.IsZero() {
		data, err := json.Marshal(*config)
		if err != nil {
			return Project{}, false, fmt.Errorf("store: encode project %s worktree setup: %w", projectID, err)
		}
		stored = string(data)
	}
	return s.applyProjectRowWrite(rowWrite{
		Action:     fmt.Sprintf("store: update project %s worktree setup", projectID),
		ID:         projectID,
		Set:        "worktree_setup = ?, updated_at = ?",
		SetArgs:    []any{stored, nowMillis()},
		Change:     "worktree_setup IS NOT ?",
		ChangeArgs: []any{stored},
	})
}
