package store

import (
	"agent-overflow/internal/repoidentity"
	"database/sql"
	"errors"
)

const repositoryIdentityV141SQL = `ALTER TABLE projects ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN identity_source TEXT NOT NULL DEFAULT '';
UPDATE projects SET identity_error = 'Repository identity has not been verified yet.' WHERE identity_error = '' AND (remote_url <> '' OR root_commit <> '');
UPDATE projects SET remote_url = '', root_commit = '';
UPDATE threads SET created_remote_url = '';`

// scrubRepositoryCoordinates changes only derived coordinates and credential
// material. Project IDs, thread placement, activity, and history stay intact.
func scrubRepositoryCoordinates(tx *sql.Tx) error {
	after := ""
	for {
		rows, err := tx.Query("SELECT id, identity_error FROM projects WHERE id > ? ORDER BY id LIMIT 256", after)
		if err != nil {
			return err
		}
		type row struct{ id, value string }
		batch := make([]row, 0, 256)
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.value); err != nil {
				return errors.Join(err, rows.Close())
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, r := range batch {
			if safe := repoidentity.RedactText(r.value); safe != r.value {
				if _, err := tx.Exec("UPDATE projects SET identity_error = ? WHERE id = ?", safe, r.id); err != nil {
					return err
				}
			}
			after = r.id
		}
	}
	return nil
}
