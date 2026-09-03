package app

import (
	"encoding/base64"
	"fmt"
	"time"

	"agent-overflow/internal/store"
)

// UploadAttachment decodes base64 bytes, validates + persists them on disk,
// and returns the new attachment metadata.
func (a *App) UploadAttachment(threadID, filename, mimeType, dataB64 string) (store.Attachment, error) {
	if a.attachments == nil {
		return store.Attachment{}, fmt.Errorf("attachment store not initialized")
	}
	return a.attachments.Upload(threadID, filename, mimeType, dataB64, time.Now().UnixMilli())
}

// claudeAdditionalDirs is what every Claude spawn (headless and
// claude-tui) passes as `--add-dir`: the attachments root, so a session
// can Read a file the user attached without raising a permission prompt
// for a path outside its workspace (docs/specs/file-attachments.md).
//
// The path comes from the store rather than being re-derived from the
// config dir, so there is one answer to "where do attachments live". A
// boot with no attachment store adds no directory rather than guessing
// one — and then no file could have been attached either.
func (a *App) claudeAdditionalDirs() []string {
	if a.attachments == nil {
		return nil
	}
	return []string{a.attachments.Root()}
}

// ListAttachments returns every attachment metadata row for a thread.
func (a *App) ListAttachments(threadID string) ([]store.Attachment, error) {
	if a.attachments == nil {
		return nil, fmt.Errorf("attachment store not initialized")
	}
	list, err := a.attachments.List(threadID)
	if err != nil {
		return nil, err
	}
	if list == nil {
		return []store.Attachment{}, nil
	}
	return list, nil
}

// DeleteAttachment removes both the metadata row and the disk file.
//
// The thread id is not decoration: it is the ownership boundary, checked
// against the row before anything is removed, so a stale id from a closed
// composer or a foreign one from any client cannot delete another thread's
// attachment. Every other thread-scoped accessor takes it for the same
// reason.
func (a *App) DeleteAttachment(threadID, attachmentID string) error {
	if a.attachments == nil {
		return fmt.Errorf("attachment store not initialized")
	}
	return a.attachments.Delete(threadID, attachmentID)
}

// GetAttachmentData returns the base64-encoded raw bytes for rendering a
// thread-owned attachment inline in the UI. Keeping this behind RPC avoids
// exposing a static file server.
//
// This is the FULL-SIZE path used by the lightbox modal. Inline grid
// rendering should call GetAttachmentThumbnail instead so we don't ship
// 5 MB of pixels for a 128 px tile (especially over the remote
// `--connect` transport).
func (a *App) GetAttachmentData(threadID, attachmentID string) (string, error) {
	if a.attachments == nil {
		return "", fmt.Errorf("attachment store not initialized")
	}
	_, data, err := a.attachments.ReadThreadBytes(threadID, attachmentID)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// AttachmentThumbnail is the wire shape returned by GetAttachmentThumbnail.
// Carries the encoded thumbnail bytes (base64) plus the actual output mime
// type so the frontend can build a Blob without trusting the original
// attachment's mime — the thumbnailer may downconvert (e.g. WEBP source →
// JPEG output) and the consumer needs to know.
type AttachmentThumbnail struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// GetAttachmentThumbnail returns a small, inline-grid-sized version of the
// attachment, generating it lazily on first call and caching the result on
// the attachments row. ~10–30 KB per thumb vs ~1–10 MB for the original;
// the savings matter even locally (base64 + JSON encode cost) and become
// the difference between "instant grid" and "watch them load" over the
// remote `--connect` transport.
func (a *App) GetAttachmentThumbnail(threadID, attachmentID string) (AttachmentThumbnail, error) {
	if a.attachments == nil {
		return AttachmentThumbnail{}, fmt.Errorf("attachment store not initialized")
	}
	data, mime, err := a.attachments.Thumbnail(threadID, attachmentID)
	if err != nil {
		return AttachmentThumbnail{}, err
	}
	return AttachmentThumbnail{
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mime,
	}, nil
}
