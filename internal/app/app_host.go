package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/dirbrowse"
	"agent-overflow/internal/editor"
	"agent-overflow/internal/externalurl"
)

// Version returns the build-stamped semantic version (e.g. "0.0.1") or
// "dev" for unstamped builds. The frontend's Settings footer reads
// this to display the current release. Read-only, no FS / process /
// settings touch — deliberately an observe scope rather than `host`, so a
// remote --connect client sees the backend's version too.
//
//ao:scope threads:read
//ao:route home
func (a *App) Version() string {
	return a.version
}

// OpenExternalURL opens an absolute HTTP(S) URL in the user's system
// browser. The backend owns this instead of letting the webview handle
// target=_blank / window.open so WSL can deliberately cross into the
// Windows default browser instead of launching a Linux/WSLg browser.
//
//ao:scope host
//ao:route home
func (a *App) OpenExternalURL(rawURL string) error {
	return externalurl.Open(context.Background(), rawURL)
}

// BrowseDirectory lists the contents of path for the project-picker
// UI. The full contract (path normalisation, ordering, .git-marker
// detection, EntryLimit truncation) lives in internal/dirbrowse. This reads
// the selected computer; it opens no native picker and works for paired clients.
//
//ao:scope files:read
//ao:route selected
func (a *App) BrowseDirectory(path string) (dirbrowse.Listing, error) {
	return dirbrowse.Browse(path)
}

// LocalImageData is the validated byte payload for a local image referenced
// by rendered markdown. The frontend turns it into a blob URL rather than
// handing a model-authored file URI to the webview.
type LocalImageData struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	// Width and Height are the declared pixel size when Go could read the
	// header, so the client reserves the box before the bytes decode; zero
	// when unknown (svg, ico, avif).
	Width  int `json:"width"`
	Height int `json:"height"`
}

// GetLocalImageData reads a local markdown image through the same path gate
// used by editor links. It accepts existing regular files only, caps bytes at
// attachment.DisplayImageMaxBytes, and sniffs the content for a format a
// browser displays before returning it. The client pins the call to the
// thread's computer; `selected` is the route for a caller that names none.
//
// Every failure reads `load local image: <reason>: <cause>`. The reason is
// the short phrase the rendered chip shows beside the image's alt text and
// the whole message is its tooltip (StreamdownImageHost.svelte).
//
//ao:scope files:read
//ao:route selected
func (a *App) GetLocalImageData(path, workspacePath string) (LocalImageData, error) {
	resolved, err := editor.ResolvePath(path, workspacePath)
	if err != nil {
		return LocalImageData{}, localImageError(localImageFileReason(err, "path not allowed"), err)
	}

	data, err := readWorkspaceFileBytes(resolved, attachment.DisplayImageMaxBytes)
	if err != nil {
		return LocalImageData{}, localImageError(localImageFileReason(err, "cannot read file"), fmt.Errorf("%s: %w", resolved, err))
	}
	mimeType, err := attachment.DetectDisplayImageMIME(data)
	if err != nil {
		return LocalImageData{}, localImageError("not an image", err)
	}
	width, height, err := attachment.ValidateDisplayImage(data, mimeType)
	if err != nil {
		reason := "cannot decode image"
		if errors.Is(err, attachment.ErrPixelBudget) {
			reason = "too many pixels"
		}
		return LocalImageData{}, localImageError(reason, err)
	}
	return LocalImageData{
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
		Width:    width,
		Height:   height,
	}, nil
}

func localImageError(reason string, cause error) error {
	return fmt.Errorf("load local image: %s: %w", reason, cause)
}

// localImageFileReason names a path-gate or read failure, or answers
// `otherwise` for one it has no phrase for. The os errors are matched by
// sentinel, so the phrase is the same on every platform whatever the OS
// spelled.
func localImageFileReason(err error, otherwise string) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "file not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, errNotRegularFile), errors.Is(err, editor.ErrNotRegularFile):
		return "not a file"
	case errors.Is(err, errFileTooLarge):
		return fmt.Sprintf("larger than %d MiB", attachment.DisplayImageMaxBytes/(1024*1024))
	}
	return otherwise
}
