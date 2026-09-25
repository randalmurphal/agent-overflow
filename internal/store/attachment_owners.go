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
// with the copy (ownRowAttachmentsTx), and a holder owns the ones its rows
// reference (ownHeldAttachmentsTx).
//
// The inherited case runs only for an attachment the thread does not own
// and one of its ancestors does. It reads the inherited rows whose meta
// lists attachments at all (attachmentBearingSQL), prefiltered by the id's
// text, so it costs the ancestors' attachment-bearing rows below the cut,
// not every row the fork inherits.
func (s *Store) OwnsAttachment(threadID, id string) (bool, error) {
	query, args := ownsAttachmentQuery(threadID, id)
	var owned bool
	if err := s.reader().QueryRow(query, args...).Scan(&owned); err != nil {
		return false, fmt.Errorf("store: check attachment ownership: %w", err)
	}
	return owned, nil
}

func ownsAttachmentQuery(threadID, id string) (string, []any) {
	inherited, inheritedArgs := inheritedTimelineArms(threadID, allLevels, timelineSelection{
		Columns:   func(string, string) string { return "1" },
		Where:     attachmentBearingSQL + " AND instr(items.meta, ?) > 0 AND " + attachmentReferencedSQL,
		WhereArgs: []any{id, id, id},
	})
	return `SELECT EXISTS(SELECT 1 FROM attachment_owners WHERE thread_id=? AND attachment_id=?)
 OR (EXISTS(SELECT 1 FROM thread_fork_lineage l JOIN attachment_owners o ON o.thread_id=l.ancestor_id AND o.attachment_id=?
       WHERE l.thread_id=?)
     AND EXISTS(` + inherited + `))`, append([]any{threadID, id, id, threadID}, inheritedArgs...)
}

// attachmentBearingSQL is the predicate of the partial indexes
// idx_items_attachment_refs and idx_import_history_items_attachment_refs
// (migration v120). SQLite uses a partial index only for a query that
// states the index's predicate, so this must stay the same expression.
// Neither encoding/json nor SQLite's JSON functions escape a key's
// letters, and a transfer re-encodes the meta whose attachments it
// rewrites, so every row whose meta has an attachments key matches it.
const attachmentBearingSQL = `instr(items.meta, '"attachments"') > 0`

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

// ownRowAttachmentsTx makes threadID an owner of the attachments its
// newly copied rows reference and the thread it copied each row from owns,
// so the files outlive that thread. A reference to another thread's
// attachment grants nothing, as it did not to the row's owner.
func ownRowAttachmentsTx(tx *sql.Tx, threadID string, rows []inheritedRow) error {
	byOwner := make(map[string][]string)
	var owners []string
	for _, row := range rows {
		if _, seen := byOwner[row.owner]; !seen {
			owners = append(owners, row.owner)
		}
		byOwner[row.owner] = append(byOwner[row.owner], row.id)
	}
	for _, owner := range owners {
		list, err := jsonList(byOwner[owner])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO attachment_owners(thread_id,attachment_id)
 SELECT ?,o.attachment_id FROM items i,
 json_each(CASE WHEN json_valid(i.meta) THEN i.meta ELSE '{}' END,'$.attachments') ref
 JOIN attachment_owners o ON o.thread_id=? AND o.attachment_id=CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') WHEN ref.type='text' THEN ref.value END
 WHERE i.thread_id=? AND i.id IN (SELECT value FROM json_each(?))`, threadID, owner, threadID, list); err != nil {
			return fmt.Errorf("store: own copied attachments in %s: %w", threadID, err)
		}
	}
	return nil
}

// ownHeldAttachmentsTx makes holder an owner of the attachments its rows,
// local and imported, reference and owner owns: the rows a split gave it.
func ownHeldAttachmentsTx(tx *sql.Tx, holder, owner string) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO attachment_owners(thread_id,attachment_id)
 SELECT ?1,o.attachment_id FROM (
   SELECT items.meta AS meta FROM items WHERE items.thread_id=?1 AND `+attachmentBearingSQL+`
   UNION ALL
   SELECT items.meta FROM thread_import_chunks refs CROSS JOIN import_history_items items ON items.chunk_id=refs.chunk_id
    WHERE refs.thread_id=?1 AND `+attachmentBearingSQL+` AND `+importedNotOverridden+`
 ) held,
 json_each(CASE WHEN json_valid(held.meta) THEN held.meta ELSE '{}' END,'$.attachments') ref
 JOIN attachment_owners o ON o.thread_id=?2 AND o.attachment_id=CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') WHEN ref.type='text' THEN ref.value END`,
		holder, owner); err != nil {
		return fmt.Errorf("store: own the attachments holder %s holds: %w", holder, err)
	}
	return nil
}

// ReleasableAttachments lists the attachments threadID owns that no pointer
// fork of it shows: the ones its delete releases. An attachment a row of its
// own history before its forks' last cut references stays owned, since the
// thread keeps that row for them (retireToHolderTx).
func (s *Store) ReleasableAttachments(threadID string) ([]Attachment, error) {
	owned, err := s.ListAttachments(threadID)
	if err != nil || len(owned) == 0 {
		return owned, err
	}
	cut, read, err := maxReaderCutTx(s.reader(), threadID)
	if err != nil || !read {
		return owned, err
	}
	kept, err := queryIDs(s.reader(), `SELECT DISTINCT CASE WHEN ref.type='object' THEN json_extract(ref.value,'$.id') ELSE ref.value END
 FROM (
   SELECT items.meta AS meta FROM items
    WHERE items.thread_id=?1 AND `+attachmentBearingSQL+` AND (items.turn_index, items.item_index) < (?2, ?3)
   UNION ALL
   SELECT items.meta FROM thread_import_chunks refs CROSS JOIN import_history_items items ON items.chunk_id=refs.chunk_id
    WHERE refs.thread_id=?1 AND `+attachmentBearingSQL+` AND `+importedNotOverridden+`
      AND (items.turn_index, items.item_index) < (?2, ?3)
 ) shown,
 json_each(CASE WHEN json_valid(shown.meta) THEN shown.meta ELSE '{}' END,'$.attachments') ref
 WHERE ref.type IN ('object','text')`, threadID, cut.turn, cut.item)
	if err != nil {
		return nil, fmt.Errorf("store: list the attachments %s's forks show: %w", threadID, err)
	}
	keep := make(map[string]bool, len(kept))
	for _, id := range kept {
		keep[id] = true
	}
	releasable := owned[:0]
	for _, a := range owned {
		if !keep[a.ID] {
			releasable = append(releasable, a)
		}
	}
	return releasable, nil
}

// RetainedAttachmentPaths names files inside an original thread's directory
// that a thread still owns: another branch, or the thread itself as the
// holder of messages its forks show (ReleasableAttachments). Paths retain
// their native-provider meaning.
func (s *Store) RetainedAttachmentPaths(threadID string) ([]string, error) {
	rows, err := s.reader().Query(`SELECT relative_path FROM attachments a WHERE a.thread_id=? AND EXISTS(
 SELECT 1 FROM attachment_owners o WHERE o.attachment_id=a.id)`, threadID)
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
