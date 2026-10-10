package app

import (
	"errors"
	"path/filepath"

	"agent-overflow/internal/localimage"
	"agent-overflow/internal/transport"
)

// LocalImage is what GetLocalImage answers for an image rendered markdown
// references by path: a single-use URL for its bytes and the sizes a client
// needs to reserve its box and choose a tier.
type LocalImage struct {
	// URL is the relative, single-use, ticketed URL the client fetches the
	// bytes from (internal/transport/localimageroutes.go).
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	// Width and Height are the served pixel size; 0 when unknown.
	Width  int `json:"width"`
	Height int `json:"height"`
	// OriginalWidth and OriginalHeight are the file's pixel size; 0 when
	// Go cannot read its header (svg, ico, avif).
	OriginalWidth  int   `json:"originalWidth"`
	OriginalHeight int   `json:"originalHeight"`
	OriginalBytes  int64 `json:"originalBytes"`
	// Derived is false when the served bytes are the file itself.
	Derived bool `json:"derived"`
}

// GetLocalImage resolves a local markdown image through the editor-link
// path gate, validates it as an image a browser displays, and mints the
// ticket that serves it. maxWidth is the display width in device pixels the
// client wants it for: a positive value is served as a derivative at the
// next ladder width (attachment.DeriveWidths) when that is smaller than the
// file, and 0 serves the file itself. The client pins the call to the
// thread's computer; `selected` is the route for a caller that names none.
//
// Every failure reads `load local image: <reason>: <cause>` (package
// localimage); the rendered chip shows the reason beside the alt text.
//
//ao:scope files:read
//ao:route selected
func (a *App) GetLocalImage(path, workspacePath string, maxWidth int) (LocalImage, error) {
	resolved, err := a.localImages().Resolve(path, workspacePath, maxWidth)
	if err != nil {
		return LocalImage{}, err
	}
	server := a.transportServer.Load()
	if server == nil {
		return LocalImage{}, errors.New("load local image: transport is not serving")
	}
	url, err := server.MintLocalImageTicket(resolved.ContentID)
	if err != nil {
		return LocalImage{}, err
	}
	return LocalImage{
		URL:            url,
		MimeType:       resolved.MimeType,
		Width:          resolved.Width,
		Height:         resolved.Height,
		OriginalWidth:  resolved.OriginalWidth,
		OriginalHeight: resolved.OriginalHeight,
		OriginalBytes:  resolved.OriginalBytes,
		Derived:        resolved.Derived,
	}, nil
}

// SaveLocalImage copies the original file a local markdown image names into
// the Downloads directory on this computer, never overwriting a file already
// there, and returns the path it wrote. The same gate and image checks as
// GetLocalImage apply, so it copies nothing a chat could not display.
//
//ao:scope files:read
//ao:route selected
func (a *App) SaveLocalImage(path, workspacePath string) (string, error) {
	original, err := localimage.ReadOriginal(path, workspacePath)
	if err != nil {
		return "", err
	}
	return a.saveDownload(filepath.Base(original.Path), original.MimeType, original.Data)
}

// localImages is the lazily constructed service behind both methods and the
// byte route. Its derivatives live in <data dir>/cache/images, under the
// same root as the attachment store, so an isolated boot keeps them in its
// own data directory. An App with no data directory (a fixture) passes no
// directory, and the service holds derivatives in memory.
func (a *App) localImages() *localimage.Service {
	a.localImageOnce.Do(func() {
		dir := ""
		if a.configDir != "" {
			dir = filepath.Join(a.configDir, "cache", "images")
		}
		a.localImageService = localimage.New(dir)
	})
	return a.localImageService
}

// OpenLocalImage satisfies transport.AttachmentTransfer.
func (t attachmentTransfer) OpenLocalImage(contentID string) (transport.LocalImageContent, error) {
	content, err := t.app.localImages().Open(contentID)
	if err != nil {
		return transport.LocalImageContent{}, err
	}
	return transport.LocalImageContent{
		MimeType: content.MimeType,
		ModTime:  content.ModTime,
		Content:  content.Content,
	}, nil
}
