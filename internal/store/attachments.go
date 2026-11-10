package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Attachment kinds. The kind decides what an attachment IS to every reader:
// an image is delivered to the provider as bytes or a `localImage` path and
// is the only kind whose bytes are ever served back to a client; a file is
// delivered as a path reference in the prompt and never decoded.
//
// The set is closed. InsertAttachment refuses anything else, so a reader can
// treat "not image" as "file" without a third branch to get wrong.
const (
	AttachmentKindImage = "image"
	AttachmentKindFile  = "file"
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
	// Kind is AttachmentKindImage or AttachmentKindFile (migration v75).
	// Rows written before the column existed default to `image`, which is
	// what every attachment was.
	Kind string `json:"kind"`
	// Shell is set on a `file` attachment whose bytes are the output of a
	// command the person ran from the composer (migration v147). The zero
	// value means an ordinary file.
	Shell AttachmentShell `json:"shell,omitzero"`
}

// AttachmentShell describes the composer shell command whose combined
// stdout and stderr a `file` attachment holds. A value type so Attachment
// stays comparable; Command is never empty on a shell attachment.
type AttachmentShell struct {
	Command string `json:"command"`
	// ExitCode is the command's own, or -1 when a signal or the timeout
	// ended it.
	ExitCode   int   `json:"exitCode"`
	DurationMs int64 `json:"durationMs"`
	// Truncated reports that the file holds the head and tail of a longer
	// stream around an omission marker.
	Truncated bool `json:"truncated,omitempty"`
	TimedOut  bool `json:"timedOut,omitempty"`
	// Preview is the first lines of the output, bounded, so a timeline row
	// paints without reading the file.
	Preview string `json:"preview,omitempty"`
}

// IsZero reports an attachment that is not a shell command's output.
func (s AttachmentShell) IsZero() bool { return s.Command == "" }

const attachmentColumns = `id, thread_id, filename, mime_type, size, relative_path, created_at, kind, shell`

func scanAttachment(scanner interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	var shell sql.NullString
	if err := scanner.Scan(
		&a.ID, &a.ThreadID, &a.Filename, &a.MimeType, &a.Size, &a.RelativePath, &a.CreatedAt, &a.Kind, &shell,
	); err != nil {
		return Attachment{}, err
	}
	if err := a.Shell.scan(shell); err != nil {
		return Attachment{}, fmt.Errorf("attachment %s: %w", a.ID, err)
	}
	return a, nil
}

func (s *AttachmentShell) scan(column sql.NullString) error {
	if !column.Valid || column.String == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(column.String), s); err != nil {
		return fmt.Errorf("decode shell metadata: %w", err)
	}
	return nil
}

// column is the stored form: SQL NULL for an ordinary attachment.
func (s AttachmentShell) column() (any, error) {
	if s.IsZero() {
		return nil, nil
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode shell metadata: %w", err)
	}
	return string(encoded), nil
}

// validateAttachment is the closed-set rule every writer of the table applies.
func validateAttachment(a Attachment) error {
	if a.Kind != AttachmentKindImage && a.Kind != AttachmentKindFile {
		return fmt.Errorf("unknown kind %q", a.Kind)
	}
	if !a.Shell.IsZero() && a.Kind != AttachmentKindFile {
		return fmt.Errorf("shell output must be a file attachment, not %q", a.Kind)
	}
	return nil
}

// InsertAttachment persists attachment metadata. The on-disk file must already
// exist at RelativePath (resolved against the attachment root).
func (s *Store) InsertAttachment(a Attachment) error {
	if err := validateAttachment(a); err != nil {
		return fmt.Errorf("store: insert attachment %s: %w", a.ID, err)
	}
	shell, err := a.Shell.column()
	if err != nil {
		return fmt.Errorf("store: insert attachment %s: %w", a.ID, err)
	}
	_, err = s.db.Exec(
		`INSERT INTO attachments (`+attachmentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ThreadID, a.Filename, a.MimeType, a.Size, a.RelativePath, a.CreatedAt, a.Kind, shell,
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
		`SELECT `+attachmentColumns+` FROM attachments WHERE id IN (SELECT attachment_id FROM attachment_owners WHERE thread_id = ?) ORDER BY created_at ASC, id ASC`,
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
		a.ThreadID = threadID
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
	var thumbMime, shell sql.NullString
	if err := row.Scan(
		&out.ID, &out.ThreadID, &out.Filename, &out.MimeType, &out.Size, &out.RelativePath, &out.CreatedAt, &out.Kind, &shell,
		&out.ThumbnailData, &thumbMime,
	); err != nil {
		if err == sql.ErrNoRows {
			return AttachmentWithThumbnail{}, false, nil
		}
		return AttachmentWithThumbnail{}, false, fmt.Errorf("store: get attachment+thumbnail %s: %w", id, err)
	}
	if err := out.Shell.scan(shell); err != nil {
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
