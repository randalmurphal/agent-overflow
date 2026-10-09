package app

import (
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/attachment"
)

func TestGetLocalImageDataReadsSupportedWorkspaceImage(t *testing.T) {
	t.Parallel()
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
	// realPNGBytes encodes an 8x8 image.
	if got.Width != 8 || got.Height != 8 {
		t.Fatalf("size = %dx%d, want 8x8", got.Width, got.Height)
	}
}

func TestGetLocalImageDataReportsUnknownSizeForSVG(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "diagram.svg")
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"></svg>`)
	if err := os.WriteFile(path, svg, 0o600); err != nil {
		t.Fatalf("write svg: %v", err)
	}
	got, err := (&App{}).GetLocalImageData(path, workspace)
	if err != nil {
		t.Fatalf("GetLocalImageData: %v", err)
	}
	if got.MimeType != "image/svg+xml" || got.Width != 0 || got.Height != 0 {
		t.Fatalf("got %q %dx%d, want image/svg+xml with an unknown size", got.MimeType, got.Width, got.Height)
	}
}

// Every refusal names a short reason between the method prefix and the
// cause, which is what the rendered chip shows beside the alt text.
func TestGetLocalImageDataNamesTheReason(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	textPath := filepath.Join(workspace, "notes.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}
	largePath := filepath.Join(workspace, "large.png")
	large := append(
		realPNGBytes(t),
		[]byte(strings.Repeat("x", int(attachment.DisplayImageMaxBytes)))...,
	)
	if err := os.WriteFile(largePath, large, 0o600); err != nil {
		t.Fatalf("write oversized image: %v", err)
	}
	bombPath := filepath.Join(workspace, "bomb.png")
	// A well-formed signature and IHDR declaring 60000x60000 pixels, with
	// the CRC the decoder verifies before it reads the size.
	ihdr := append([]byte("IHDR"), 0, 0, 0xEA, 0x60, 0, 0, 0xEA, 0x60, 8, 6, 0, 0, 0)
	bomb := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 13}, ihdr...)
	bomb = binary.BigEndian.AppendUint32(bomb, crc32.ChecksumIEEE(ihdr))
	if err := os.WriteFile(bombPath, bomb, 0o600); err != nil {
		t.Fatalf("write bomb: %v", err)
	}
	cases := []struct {
		name   string
		path   string
		reason string
	}{
		{"missing", filepath.Join(workspace, "missing.png"), "load local image: file not found: "},
		{"missing outside the workspace", filepath.Join(t.TempDir(), "gone.png"), "load local image: file not found: "},
		{"directory", workspace, "load local image: not a file: "},
		{"text", textPath, "load local image: not an image: "},
		{"oversized", largePath, "load local image: larger than 25 MiB: "},
		{"pixel bomb", bombPath, "load local image: too many pixels: "},
		{"traversal", filepath.Join(workspace, "a") + "/../b.png", "load local image: path not allowed: "},
		{"network share", `\\server\share\b.png`, "load local image: path not allowed: "},
	}
	for _, tc := range cases {
		_, err := (&App{}).GetLocalImageData(tc.path, workspace)
		if err == nil {
			t.Fatalf("%s: GetLocalImageData(%q) succeeded", tc.name, tc.path)
		}
		if !strings.HasPrefix(err.Error(), tc.reason) {
			t.Fatalf("%s: error = %q, want prefix %q", tc.name, err.Error(), tc.reason)
		}
	}
}

func TestGetLocalImageDataReadsExistingImageOutsideWorkspace(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		[]byte(strings.Repeat("x", int(attachment.DisplayImageMaxBytes)))...,
	)
	if err := os.WriteFile(largePath, large, 0o600); err != nil {
		t.Fatalf("write oversized image: %v", err)
	}
	if _, err := (&App{}).GetLocalImageData(largePath, workspace); err == nil {
		t.Fatal("GetLocalImageData accepted an oversized image")
	}
}

func TestGetLocalImageDataRejectsMissingAndDirectoryTargets(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	for _, path := range []string{filepath.Join(workspace, "missing.png"), workspace} {
		if _, err := (&App{}).GetLocalImageData(path, workspace); err == nil {
			t.Fatalf("GetLocalImageData(%q) succeeded", path)
		}
	}
}
