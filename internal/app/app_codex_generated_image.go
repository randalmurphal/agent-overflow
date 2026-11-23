package app

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
)

// importCodexGeneratedImage is the app half of the generated-image seam
// (internal/triage/codex_generated_image.go): it resolves the allowed
// directories a provider-generated image may come from and hands the path to
// the attachment store, which owns canonicalization, containment, the size
// bound, the signature check and the atomic publish.
//
// Resolving the directory here rather than in triage is the provider-home
// rule (app_provider_home.go): an isolated boot pins its own home, and a
// helper that re-derived one would read the developer's real provider home.
func (a *App) importCodexGeneratedImage(threadID, savedPath string) (triage.GeneratedImage, error) {
	if a.attachments == nil {
		return triage.GeneratedImage{}, errors.New("attachment store is not available")
	}
	now := time.Now().UnixMilli()
	if a.store != nil {
		if providerName, workspacePath, err := a.store.GetThreadProviderWorkspace(threadID); err == nil && providerName == string(provider.Agy) {
			var allowedDirs []string
			if appDataDir := strings.TrimSpace(a.agyAppDataDir()); appDataDir != "" {
				allowedDirs = append(allowedDirs, filepath.Join(appDataDir, "brain"))
			}
			if ws := strings.TrimSpace(workspacePath); ws != "" {
				allowedDirs = append(allowedDirs, ws)
			}
			if len(allowedDirs) == 0 {
				return triage.GeneratedImage{}, errors.New("antigravity generated-image root is not available")
			}
			var lastErr error
			for _, dir := range allowedDirs {
				record, importErr := a.attachments.ImportImageFromDir(threadID, dir, savedPath, now)
				if importErr == nil {
					return triage.GeneratedImage{
						AttachmentID: record.ID,
						Filename:     record.Filename,
						MimeType:     record.MimeType,
						Size:         record.Size,
					}, nil
				}
				lastErr = importErr
			}
			return triage.GeneratedImage{}, lastErr
		}
	}
	dir, err := a.codexGeneratedImagesDir()
	if err != nil {
		return triage.GeneratedImage{}, err
	}
	record, err := a.attachments.ImportImageFromDir(threadID, dir, savedPath, now)
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
