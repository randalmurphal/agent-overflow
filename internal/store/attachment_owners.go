package store

import (
	"database/sql"
	"fmt"
)

// OwnsAttachment reports whether threadID may read an attachment: it owns
// it, or it is a pointer fork that shows a row referencing it, which a
// thread it reads history from owns. Inherited messages keep their
// attachments owned by the thread that sent them, and a fork reads only the
// ones before its cut; a fork that copies such a message takes ownership
// with the copy (ownCopiedAttachmentsTx).
//
// The inherited case reads the fork's inherited rows, prefiltered by the
// id's text, and runs only for an attachment the thread does not own and
// one of its ancestors does. No index keys a row by the attachments its
// meta lists, so that read walks the rows the fork inherits.
func (s *Store) OwnsAttachment(threadID, id string) (bool, error) {
	inherited, inheritedArgs := inheritedTimelineArms(threadID, allLevels, timelineSelection{
		Columns:   func(string, string) string { return "1" },
		Where:     "instr(items.meta, ?) > 0 AND " + attachmentReferencedSQL,
		WhereArgs: []any{id, id, id},
	})
	var owned bool
	err := s.reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM attachment_owners WHERE thread_id=? AND attachment_id=?)
 OR (EXISTS(SELECT 1 FROM thread_fork_lineage l JOIN attachment_owners o ON o.thread_id=l.ancestor_id AND o.attachment_id=?
       WHERE l.thread_id=?)
     AND EXISTS(`+inherited+`))`,
		append([]any{threadID, id, id, threadID}, inheritedArgs...)...).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("store: check attachment ownership: %w", err)
	}
	return owned, nil
}

// attachmentReferencedSQL is true when the `items` row's meta lists the
// attachment id bound twice, in either reference shape.
const attachmentReferencedSQL = `EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(items.meta) THEN items.meta ELSE '{}' END,'$.attachments') ref
 WHERE (ref.type='object' AND json_extract(ref.value,'$.id')=?) OR (ref.type='text' AND ref.value=?))`

// ReleaseAttachment removes an ownership edge and, for the last owner, its
// backing file. Failed file removal rolls back metadata so cleanup can retry.
// removeBytes must not call back into the store while the writer is held.
func (s *Store) ReleaseAttachment(threadID, id string, removeBytes func() error) error {
	if removeBytes == nil {
		return fmt.Errorf("store: attachment removal requires file cleanup")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM attachment_owners WHERE thread_id=? AND attachment_id=?`, threadID, id)
	if err != nil {
		return fmt.Errorf("store: release attachment: %w", err)
	}
	if err := requireRowsAffected(result, "store: release attachment"); err != nil {
		return err
	}
	var retained bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM attachments WHERE id=?)`, id).Scan(&retained); err != nil {
		return err
	}
	if !retained {
		if err := removeBytes(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ownCopiedAttachmentsTx makes threadID an owner of the attachments its
// newly copied rows reference and the thread it copied each row from owns,
// so the files outlive that thread. A reference to another thread's
// attachment grants nothing, as it did not to the row's owner.
func ownCopiedAttachmentsTx(tx *sql.Tx, threadID string, rows []inheritedRow) error {
	byOwner := make(map[string][]string)
	var owners []string
	for _, row := range rows {
		if _, seen := byOwner[row.owner]; !seen {
			owners = append(owners, row.owner)
		}
		byOwner[row.owner] = append(byOwner[row.owner], row.id)
	}
	for _, owner := range owners {
		ids := byOwner[owner]
		for start := 0; start < len(ids); start += forkCopyBatch {
			clause, args := inClause("i.id", ids[start:min(start+forkCopyBatch, len(ids))])
			if _, err := tx.Exec(`INSERT OR IGNORE INTO attachment_owners(thread_id,attachment_id)
 SELECT ?,o.attachment_id FROM items i,
 json_each(CASE WHEN json_valid(i.meta) THEN i.meta ELSE '{}' END,'$.attachments') ref
 JOIN attachment_owners o ON o.thread_id=? AND o.attachment_id=CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') WHEN ref.type='text' THEN ref.value END
 WHERE i.thread_id=? AND `+clause, append([]any{threadID, owner, threadID}, args...)...); err != nil {
				return fmt.Errorf("store: own copied attachments in %s: %w", threadID, err)
			}
		}
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
