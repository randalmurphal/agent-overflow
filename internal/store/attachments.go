package store

import (
	"database/sql"
	"fmt"
)

// Attachment is the persisted metadata for a file attached to a thread.
type Attachment struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	RelativePath string `json:"relativePath"`
	CreatedAt    int64  `json:"createdAt"`
}

const attachmentColumns = `id, thread_id, filename, mime_type, size, relative_path, created_at`

func scanAttachment(scanner interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	if err := scanner.Scan(
		&a.ID, &a.ThreadID, &a.Filename, &a.MimeType, &a.Size, &a.RelativePath, &a.CreatedAt,
	); err != nil {
		return Attachment{}, err
	}
	return a, nil
}

// InsertAttachment persists attachment metadata. The on-disk file must already
// exist at RelativePath (resolved against the attachment root).
func (s *Store) InsertAttachment(a Attachment) error {
	_, err := s.db.Exec(
		`INSERT INTO attachments (id, thread_id, filename, mime_type, size, relative_path, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ThreadID, a.Filename, a.MimeType, a.Size, a.RelativePath, a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert attachment %s: %w", a.ID, err)
	}
	return nil
}

// GetAttachment returns metadata for a single attachment. Second return value
// is false when no row matches; callers should treat that as not-found.
func (s *Store) GetAttachment(id string) (Attachment, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, id,
	)
	a, err := scanAttachment(row)
	if err == sql.ErrNoRows {
		return Attachment{}, false, nil
	}
	if err != nil {
		return Attachment{}, false, fmt.Errorf("store: get attachment %s: %w", id, err)
	}
	return a, true, nil
}

// ListAttachments returns every attachment for a thread in creation order.
func (s *Store) ListAttachments(threadID string) ([]Attachment, error) {
	rows, err := s.db.Query(
		`SELECT `+attachmentColumns+` FROM attachments WHERE thread_id = ? ORDER BY created_at ASC, id ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list attachments for %s: %w", threadID, err)
	}
	defer rows.Close()

	var attachments []Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan attachment row: %w", err)
		}
		attachments = append(attachments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate attachments for %s: %w", threadID, err)
	}
	return attachments, nil
}

// GetAttachmentThumbnail returns the cached thumbnail bytes + mime type for an
// attachment. The third return value is false when no thumbnail has been
// generated yet (NULL columns); callers should generate-and-cache via
// SetAttachmentThumbnail in that case. Kept off the standard attachmentColumns
// SELECT so list reads don't pull large blobs they won't use.
func (s *Store) GetAttachmentThumbnail(id string) ([]byte, string, bool, error) {
	row := s.db.QueryRow(
		`SELECT thumbnail_data, thumbnail_mime FROM attachments WHERE id = ?`, id,
	)
	var data []byte
	var mime sql.NullString
	if err := row.Scan(&data, &mime); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("store: get attachment thumbnail %s: %w", id, err)
	}
	if data == nil || !mime.Valid {
		return nil, "", false, nil
	}
	return data, mime.String, true, nil
}

// SetAttachmentThumbnail caches the generated thumbnail bytes + mime type on
// the attachments row. Idempotent: repeated calls overwrite. The two columns
// are written together so a partial state never appears.
func (s *Store) SetAttachmentThumbnail(id string, data []byte, mime string) error {
	if len(data) == 0 {
		return fmt.Errorf("store: set attachment thumbnail %s: empty data", id)
	}
	if mime == "" {
		return fmt.Errorf("store: set attachment thumbnail %s: empty mime", id)
	}
	result, err := s.db.Exec(
		`UPDATE attachments SET thumbnail_data = ?, thumbnail_mime = ? WHERE id = ?`,
		data, mime, id,
	)
	if err != nil {
		return fmt.Errorf("store: set attachment thumbnail %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set attachment thumbnail %s", id))
}

// DeleteAttachment removes a single attachment row. The caller is responsible
// for deleting the on-disk file.
func (s *Store) DeleteAttachment(id string) error {
	result, err := s.db.Exec(`DELETE FROM attachments WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete attachment %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete attachment %s", id))
}
