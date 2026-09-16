package app

import (
	"errors"
	"time"

	"agent-overflow/internal/triage"
)

// importCodexGeneratedImage is the app half of the generated-image seam
// (internal/triage/codex_generated_image.go): it resolves the ONE directory a
// Codex-generated image may come from and hands the path to the attachment
// store, which owns canonicalization, containment, the size bound, the
// signature check and the atomic publish.
//
// Resolving the directory here rather than in triage is the provider-home
// rule (app_provider_home.go): an isolated boot pins its own home, and a
// helper that re-derived one would read the developer's real `~/.codex`.
func (a *App) importCodexGeneratedImage(threadID, savedPath string) (triage.GeneratedImage, error) {
	if a.attachments == nil {
		return triage.GeneratedImage{}, errors.New("attachment store is not available")
	}
	dir, err := a.codexGeneratedImagesDir()
	if err != nil {
		return triage.GeneratedImage{}, err
	}
	record, err := a.attachments.ImportImageFromDir(threadID, dir, savedPath, time.Now().UnixMilli())
	if err != nil {
		return triage.GeneratedImage{}, err
	}
	return triage.GeneratedImage{
		AttachmentID: record.ID,
		Filename:     record.Filename,
		MimeType:     record.MimeType,
		Size:         record.Size,
	}, nil
}
