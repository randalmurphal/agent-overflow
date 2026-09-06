package app

import (
	"context"
	"errors"

	"agent-overflow/internal/filepreview"
	"agent-overflow/internal/transport"
)

// MintFilePreviewURL opens an HTML file and its directory assets on a separate
// origin. The calling frontend explicitly targets the file's owning computer.
// The caller's authenticated session determines local versus remote access;
// neither a path nor a frontend argument can choose a cleartext network bind.
//
//ao:scope preview:open
//ao:route selected
func (a *App) MintFilePreviewURL(ctx context.Context, path, workspacePath string) (string, error) {
	srv := a.transportServer.Load()
	if srv == nil || a.shuttingDown.Load() {
		return "", errors.New("this computer is not serving previews")
	}
	a.preview.mu.Lock()
	if a.shuttingDown.Load() {
		a.preview.mu.Unlock()
		return "", errors.New("this computer is shutting down")
	}
	if a.preview.files == nil {
		a.preview.files = filepreview.New(transport.PreviewGatewayConfig{
			Sources:     a.previewSources(srv),
			SessionLive: srv.SessionLive,
		})
	}
	files := a.preview.files
	a.preview.mu.Unlock()
	principal := transport.SessionFromContext(ctx)
	local := principal == "" || transport.CallerProofFromContext(ctx).HostPresent
	return files.Open(path, workspacePath, principal, local)
}
