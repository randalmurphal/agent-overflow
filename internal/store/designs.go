package store

import "fmt"

func (s *Store) InsertDesignArtifact(artifact DesignArtifact) error {
	_, err := s.db.Exec(
		`INSERT INTO design_artifacts (
			id, thread_id, title, description, kind, html_path, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID,
		artifact.ThreadID,
		artifact.Title,
		artifact.Description,
		artifact.Kind,
		artifact.HTMLPath,
		artifact.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert design artifact %s: %w", artifact.ID, err)
	}
	return nil
}

func (s *Store) GetDesignArtifact(threadID, artifactID string) (DesignArtifact, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, title, description, kind, html_path, created_at
		 FROM design_artifacts
		 WHERE thread_id = ? AND id = ?`,
		threadID,
		artifactID,
	)
	return scanDesignArtifact(row, artifactID)
}

func (s *Store) ListDesignArtifacts(threadID, kind string) ([]DesignArtifact, error) {
	query := `SELECT id, thread_id, title, description, kind, html_path, created_at
		FROM design_artifacts
		WHERE thread_id = ?`
	args := []any{threadID}
	if kind != "" {
		query += " AND kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list design artifacts for %s: %w", threadID, err)
	}
	defer rows.Close()

	var artifacts []DesignArtifact
	for rows.Next() {
		artifact, err := scanDesignArtifact(rows, "")
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate design artifacts for %s: %w", threadID, err)
	}
	return artifacts, nil
}

type designArtifactScanner interface {
	Scan(dest ...any) error
}

func scanDesignArtifact(scanner designArtifactScanner, artifactID string) (DesignArtifact, error) {
	var artifact DesignArtifact
	if err := scanner.Scan(
		&artifact.ID,
		&artifact.ThreadID,
		&artifact.Title,
		&artifact.Description,
		&artifact.Kind,
		&artifact.HTMLPath,
		&artifact.CreatedAt,
	); err != nil {
		if artifactID == "" {
			return DesignArtifact{}, fmt.Errorf("store: scan design artifact: %w", err)
		}
		return DesignArtifact{}, fmt.Errorf("store: get design artifact %s: %w", artifactID, err)
	}
	return artifact, nil
}
