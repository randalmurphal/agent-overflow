package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

const discussionProjectIDExpr = "COALESCE(project_id, '')"

// errDiscussionDefinitionDecode marks a row whose `definition` blob does
// not decode. It is a per-row content failure — the read itself worked —
// which is why the list read can skip past it while the single-row
// getters, whose caller asked for that exact row, still fail.
var errDiscussionDefinitionDecode = errors.New("store: decode discussion definition")

// discussionDefinitionPayload contains only the fields that belong in the
// serialized JSON blob. The remaining fields (id, name, description, scope,
// projectId, timestamps) are stored in their own columns and should not be
// duplicated into the blob.
type discussionDefinitionPayload struct {
	Participants []DiscussionParticipant `json:"participants"`
	Settings     DiscussionSettings      `json:"settings"`
}

func (s *Store) CreateDiscussionDef(def DiscussionDefinition) error {
	definition, err := json.Marshal(discussionDefinitionPayload{
		Participants: def.Participants,
		Settings:     def.Settings,
	})
	if err != nil {
		return fmt.Errorf("store: marshal discussion definition %s: %w", def.Name, err)
	}

	_, err = s.db.Exec(
		`INSERT INTO discussion_definitions (
			id, name, description, scope, project_id, definition, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		def.ID, def.Name, def.Description, def.Scope, def.ProjectID, string(definition), def.CreatedAt, def.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create discussion definition %s: %w", def.Name, err)
	}
	return nil
}

func (s *Store) GetDiscussionDef(name, scope, projectID string) (DiscussionDefinition, error) {
	row := s.reader().QueryRow(
		fmt.Sprintf(`SELECT id, name, description, scope, %s, definition, created_at, updated_at
			FROM discussion_definitions
			WHERE name = ? AND scope = ? AND %s = ?`, discussionProjectIDExpr, discussionProjectIDExpr),
		name, scope, projectID,
	)
	return scanDiscussionDefinition(row)
}

func (s *Store) GetDiscussionDefByID(id string) (DiscussionDefinition, error) {
	row := s.reader().QueryRow(
		fmt.Sprintf(`SELECT id, name, description, scope, %s, definition, created_at, updated_at
			FROM discussion_definitions
			WHERE id = ?`, discussionProjectIDExpr),
		id,
	)
	return scanDiscussionDefinition(row)
}

// ListDiscussionDefs returns the definitions matching the scope/project
// filter. A row whose `definition` blob is undecodable is dropped from the
// result with a log line naming it, rather than failing the read: one
// corrupt blob taking the whole discussion picker away is a worse outcome
// than the picker missing the entry that is already unusable. Errors are
// reserved for failures of the read itself (query, scan, iteration), which
// no amount of skipping recovers from. Callers wanting that one row —
// GetDiscussionDef / GetDiscussionDefByID — still get the decode error.
func (s *Store) ListDiscussionDefs(scope, projectID string) ([]DiscussionDefinition, error) {
	query := fmt.Sprintf(`SELECT id, name, description, scope, %s, definition, created_at, updated_at
		FROM discussion_definitions`, discussionProjectIDExpr)
	var args []any
	switch {
	case scope != "" && projectID != "":
		query += fmt.Sprintf(" WHERE scope = ? AND %s = ?", discussionProjectIDExpr)
		args = append(args, scope, projectID)
	case scope != "":
		query += " WHERE scope = ?"
		args = append(args, scope)
	case projectID != "":
		query += fmt.Sprintf(" WHERE %s = ?", discussionProjectIDExpr)
		args = append(args, projectID)
	}
	query += " ORDER BY updated_at DESC, created_at DESC, name ASC"

	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list discussion definitions: %w", err)
	}
	defer rows.Close()

	var defs []DiscussionDefinition
	for rows.Next() {
		def, err := scanDiscussionDefinition(rows)
		if err != nil {
			if errors.Is(err, errDiscussionDefinitionDecode) {
				log.Printf("store: skipping unreadable discussion definition: %v", err)
				continue
			}
			return nil, err
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate discussion definitions: %w", err)
	}
	return defs, nil
}

func (s *Store) UpdateDiscussionDef(previousName, previousScope, previousProjectID string, def DiscussionDefinition) error {
	definition, err := json.Marshal(discussionDefinitionPayload{
		Participants: def.Participants,
		Settings:     def.Settings,
	})
	if err != nil {
		return fmt.Errorf("store: marshal discussion definition %s: %w", def.Name, err)
	}

	result, err := s.db.Exec(
		fmt.Sprintf(`UPDATE discussion_definitions
			SET id = ?, name = ?, description = ?, scope = ?, project_id = ?, definition = ?, updated_at = ?
			WHERE name = ? AND scope = ? AND %s = ?`, discussionProjectIDExpr),
		def.ID, def.Name, def.Description, def.Scope, def.ProjectID, string(definition), def.UpdatedAt,
		previousName, previousScope, previousProjectID,
	)
	if err != nil {
		return fmt.Errorf("store: update discussion definition %s: %w", previousName, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update discussion definition %s", previousName))
}

func (s *Store) DeleteDiscussionDef(name, scope, projectID string) error {
	result, err := s.db.Exec(
		fmt.Sprintf(`DELETE FROM discussion_definitions
			WHERE name = ? AND scope = ? AND %s = ?`, discussionProjectIDExpr),
		name, scope, projectID,
	)
	if err != nil {
		return fmt.Errorf("store: delete discussion definition %s: %w", name, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete discussion definition %s", name))
}

type discussionDefinitionScanner interface {
	Scan(dest ...any) error
}

func scanDiscussionDefinition(scanner discussionDefinitionScanner) (DiscussionDefinition, error) {
	var (
		defID          string
		name           string
		description    string
		scope          string
		projectID      string
		createdAt      int64
		updatedAt      int64
		definitionJSON string
	)

	if err := scanner.Scan(
		&defID,
		&name,
		&description,
		&scope,
		&projectID,
		&definitionJSON,
		&createdAt,
		&updatedAt,
	); err != nil {
		return DiscussionDefinition{}, fmt.Errorf("store: scan discussion definition: %w", err)
	}

	var def DiscussionDefinition
	if definitionJSON != "" {
		if err := json.Unmarshal([]byte(definitionJSON), &def); err != nil {
			return DiscussionDefinition{}, fmt.Errorf("%w %s (id %s): %w", errDiscussionDefinitionDecode, name, defID, err)
		}
	}

	def.ID = defID
	def.Name = name
	def.Description = description
	def.Scope = scope
	def.ProjectID = projectID
	def.CreatedAt = createdAt
	def.UpdatedAt = updatedAt
	return def, nil
}
