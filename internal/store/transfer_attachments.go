package store

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"

	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

// ThreadTransferAttachments includes attachments inherited through a local
// fork. Those timeline references still name their original owner, so a plain
// ListAttachments(threadID) would silently leave their bytes behind. A pointer
// fork's inherited messages reference attachments a thread it reads owns
// (OwnsAttachment), which travel with the fork.
func (s *Store) ThreadTransferAttachments(ctx context.Context, threadID string) ([]Attachment, error) {
	rows, err := s.reader().QueryContext(ctx, `WITH refs(thread_id, id) AS (
 SELECT CASE WHEN ref.type = 'object' THEN COALESCE(NULLIF(json_extract(ref.value,'$.threadId'),''),?1) ELSE ?1 END,
        CASE WHEN ref.type = 'object' THEN json_extract(ref.value,'$.id') WHEN ref.type = 'text' THEN ref.value END
 FROM timeline_items AS items, json_each(CASE WHEN json_valid(items.meta) THEN items.meta ELSE '{}' END,'$.attachments') AS ref
 WHERE items.thread_id = ?1 AND items.kind = 'user_text'
)
SELECT `+attachmentColumns+` FROM attachments
WHERE id IN (SELECT attachment_id FROM attachment_owners WHERE thread_id=?1)
   OR (thread_id,id) IN (SELECT thread_id, id FROM refs)
   OR id IN (SELECT refs.id FROM refs
              JOIN thread_fork_lineage l ON l.thread_id = ?1
              JOIN attachment_owners o ON o.thread_id = l.ancestor_id AND o.attachment_id = refs.id)
ORDER BY id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attachments []Attachment
	for rows.Next() {
		if len(attachments) >= transferfiles.MaxFiles {
			return nil, errors.New("transfer: too many attachments")
		}
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		if !transferfiles.ValidName(a.RelativePath) {
			return nil, errors.New("transfer: attachment path is not portable")
		}
		attachments = append(attachments, a)
	}
	return attachments, rows.Err()
}

// TransferredAttachment is the deterministic destination metadata used by BOTH
// file installation and history import. A copy may be retried without minting
// another identity. A move keeps an attachment already owned by that thread.
func TransferredAttachment(source Attachment, targetID string) (Attachment, error) {
	if !transferfiles.ValidName(targetID) || strings.Contains(targetID, "/") || !transferfiles.ValidName(source.RelativePath) || source.ID == "" {
		return Attachment{}, errors.New("transfer: invalid attachment destination")
	}
	if source.ThreadID == targetID {
		return source, nil
	}
	source.ID = transferContentID(targetID, "attachment", source.ID)
	source.ThreadID = targetID
	name := path.Base(source.RelativePath)
	source.RelativePath = path.Join(targetID, source.ID+path.Ext(name))
	if source.Kind == AttachmentKindFile {
		source.RelativePath = path.Join(targetID, source.ID, name)
	}
	return source, nil
}

func transferContentID(threadID, kind, id string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("agent-overflow/transfer/"+threadID+"/"+kind+"/"+id)).String()
}

func importTransferAttachment(tx *sql.Tx, a Attachment) error {
	existing, err := scanAttachment(tx.QueryRow(`SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, a.ID))
	if err == nil {
		if existing != a {
			return errors.New("transfer: attachment identity conflicts with an existing upload")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(`INSERT INTO attachments (`+attachmentColumns+`) VALUES (?,?,?,?,?,?,?,?)`, a.ID, a.ThreadID, a.Filename, a.MimeType, a.Size, a.RelativePath, a.CreatedAt, a.Kind)
	return err
}
