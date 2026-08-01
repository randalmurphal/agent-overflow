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
	row := s.reader().QueryRow(
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
	rows, err := s.reader().Query(
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

// AttachmentWithThumbnail bundles attachment metadata with any cached
// thumbnail bytes so a single SELECT covers both the authz check and the
// cache-hit fast path. ThumbnailData is nil and ThumbnailMime is "" when
// no thumbnail has been generated yet.
type AttachmentWithThumbnail struct {
	Attachment
	ThumbnailData []byte
	ThumbnailMime string
}

// GetAttachmentWithThumbnail returns metadata + any cached thumbnail in one
// indexed point lookup. Used by the thumbnail generator's hot path so the
// cache hit is one SELECT, not two.
func (s *Store) GetAttachmentWithThumbnail(id string) (AttachmentWithThumbnail, bool, error) {
	row := s.reader().QueryRow(
		`SELECT `+attachmentColumns+`, thumbnail_data, thumbnail_mime FROM attachments WHERE id = ?`, id,
	)
	var out AttachmentWithThumbnail
	var thumbMime sql.NullString
	if err := row.Scan(
		&out.ID, &out.ThreadID, &out.Filename, &out.MimeType, &out.Size, &out.RelativePath, &out.CreatedAt,
		&out.ThumbnailData, &thumbMime,
	); err != nil {
		if err == sql.ErrNoRows {
			return AttachmentWithThumbnail{}, false, nil
		}
		return AttachmentWithThumbnail{}, false, fmt.Errorf("store: get attachment+thumbnail %s: %w", id, err)
	}
	if out.ThumbnailData != nil && thumbMime.Valid {
		out.ThumbnailMime = thumbMime.String
	} else {
		out.ThumbnailData = nil
		out.ThumbnailMime = ""
	}
	return out, true, nil
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
