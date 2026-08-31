package app

import (
	"context"
	"encoding/base64"
	"fmt"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/dirbrowse"
	"agent-overflow/internal/editor"
	"agent-overflow/internal/externalurl"
)

// Version returns the build-stamped semantic version (e.g. "0.0.1") or
// "dev" for unstamped builds. The frontend's Settings footer reads
// this to display the current release. Read-only, no FS / process /
// settings touch — intentionally NOT in LocalOnlyMethods so a
// remote --connect client sees the backend's version too.
func (a *App) Version() string {
	return a.version
}

// OpenExternalURL opens an absolute HTTP(S) URL in the user's system
// browser. The backend owns this instead of letting the webview handle
// target=_blank / window.open so WSL can deliberately cross into the
// Windows default browser instead of launching a Linux/WSLg browser.
func (a *App) OpenExternalURL(rawURL string) error {
	return externalurl.Open(context.Background(), rawURL)
}

// BrowseDirectory lists the contents of path for the project-picker
// UI. The full contract (path normalisation, ordering, .git-marker
// detection, EntryLimit truncation) lives in internal/dirbrowse.
func (a *App) BrowseDirectory(path string) (dirbrowse.Listing, error) {
	return dirbrowse.Browse(path)
}

// LocalImageData is the validated byte payload for a local image referenced
// by rendered markdown. The frontend turns it into a blob URL rather than
// handing a model-authored file URI to the webview.
type LocalImageData struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// GetLocalImageData reads a local markdown image through the same path gate
// used by editor links. It accepts existing regular files only, caps bytes at
// the attachment limit, and validates the content signature before returning
// it. This method is LocalOnly because its arguments select a host file.
func (a *App) GetLocalImageData(path, workspacePath string) (LocalImageData, error) {
	resolved, err := editor.ResolvePath(path, workspacePath)
	if err != nil {
		return LocalImageData{}, fmt.Errorf("load local image: %w", err)
	}

	data, err := readWorkspaceFileBytes(resolved, attachment.DefaultMaxSize)
	if err != nil {
		return LocalImageData{}, fmt.Errorf("load local image: read %q: %w", resolved, err)
	}
	mimeType, err := attachment.DetectImageMIME(data)
	if err != nil {
		return LocalImageData{}, fmt.Errorf("load local image: %w", err)
	}
	if err := attachment.ValidateImageDimensions(data); err != nil {
		return LocalImageData{}, fmt.Errorf("load local image: %w", err)
	}
	return LocalImageData{
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}, nil
}
