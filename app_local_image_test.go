package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/attachment"
)

func TestGetLocalImageDataReadsSupportedWorkspaceImage(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "diagram.png")
	payload := realPNGBytes(t)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	got, err := (&App{}).GetLocalImageData(path, workspace)
	if err != nil {
		t.Fatalf("GetLocalImageData: %v", err)
	}
	if got.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", got.MimeType)
	}
	if got.Data != base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("data = %q, want encoded payload", got.Data)
	}
}

func TestGetLocalImageDataReadsExistingImageOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(t.TempDir(), "diagram.png")
	payload := realPNGBytes(t)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	if _, err := (&App{}).GetLocalImageData(path, workspace); err != nil {
		t.Fatalf("GetLocalImageData outside workspace: %v", err)
	}
}

func TestGetLocalImageDataRejectsUnsupportedAndOversizedFiles(t *testing.T) {
	workspace := t.TempDir()
	textPath := filepath.Join(workspace, "notes.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}
	if _, err := (&App{}).GetLocalImageData(textPath, workspace); err == nil {
		t.Fatal("GetLocalImageData accepted a non-image file")
	}

	largePath := filepath.Join(workspace, "large.png")
	large := append(
		realPNGBytes(t),
		[]byte(strings.Repeat("x", int(attachment.DefaultMaxSize)))...,
	)
	if err := os.WriteFile(largePath, large, 0o600); err != nil {
		t.Fatalf("write oversized image: %v", err)
	}
	if _, err := (&App{}).GetLocalImageData(largePath, workspace); err == nil {
		t.Fatal("GetLocalImageData accepted an oversized image")
	}
}

func TestGetLocalImageDataRejectsMissingAndDirectoryTargets(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{filepath.Join(workspace, "missing.png"), workspace} {
		if _, err := (&App{}).GetLocalImageData(path, workspace); err == nil {
			t.Fatalf("GetLocalImageData(%q) succeeded", path)
		}
	}
}
