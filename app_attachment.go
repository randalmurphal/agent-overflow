package main

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
func (a *App) DeleteAttachment(attachmentID string) error {
	if a.attachments == nil {
		return fmt.Errorf("attachment store not initialized")
	}
	return a.attachments.Delete(attachmentID)
}

// GetAttachmentData returns the base64-encoded raw bytes for rendering the
// attachment inline in the UI. Keeping this behind RPC avoids exposing a
// static file server.
func (a *App) GetAttachmentData(attachmentID string) (string, error) {
	if a.attachments == nil {
		return "", fmt.Errorf("attachment store not initialized")
	}
	_, data, err := a.attachments.ReadBytes(attachmentID)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
