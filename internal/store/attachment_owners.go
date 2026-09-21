package store

import (
	"database/sql"
	"fmt"
)

func (s *Store) OwnsAttachment(threadID, id string) (bool, error) {
	var owned bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM attachment_owners WHERE thread_id=? AND attachment_id=?)`, threadID, id).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("store: check attachment ownership: %w", err)
	}
	return owned, nil
}

// ReleaseAttachment removes one ownership edge. It reports whether the
// last owner released the asset, so the file owner can remove its bytes.
func (s *Store) ReleaseAttachment(threadID, id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM attachment_owners WHERE thread_id=? AND attachment_id=?`, threadID, id)
	if err != nil {
		return false, fmt.Errorf("store: release attachment: %w", err)
	}
	if err := requireRowsAffected(result, "store: release attachment"); err != nil {
		return false, err
	}
	var retained bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM attachments WHERE id=?)`, id).Scan(&retained); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return !retained, nil
}

func cloneAttachmentOwnersTx(tx *sql.Tx, source, target string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO attachment_owners(thread_id,attachment_id)
 SELECT ?,owner.attachment_id FROM timeline_items i,
 json_each(CASE WHEN json_valid(i.meta) THEN i.meta ELSE '{}' END,'$.attachments') ref
 JOIN attachment_owners owner ON owner.thread_id=? AND owner.attachment_id=
 CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') WHEN ref.type='text' THEN ref.value END
 WHERE i.thread_id=?`, target, source, target)
	if err != nil {
		return fmt.Errorf("store: retain fork attachments: %w", err)
	}
	return nil
}

// RetainedAttachmentPaths names files inside an original thread's directory
// that another branch still owns. Paths retain their native-provider meaning.
func (s *Store) RetainedAttachmentPaths(threadID string) ([]string, error) {
	rows, err := s.reader().Query(`SELECT relative_path FROM attachments a WHERE a.thread_id=? AND EXISTS(
 SELECT 1 FROM attachment_owners o WHERE o.attachment_id=a.id AND o.thread_id<>?)`, threadID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}
