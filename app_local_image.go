package main

import (
	"encoding/base64"
	"fmt"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/editor"
)

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
