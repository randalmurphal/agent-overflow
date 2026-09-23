package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/forgeattach"
	"agent-overflow/internal/wsldistro"
)

func TestDownloadFileName(t *testing.T) {
	cases := []struct{ in, mimeType, want string }{
		{"hero.png", "image/png", "hero.png"},
		{"Screen Shot (1).png", "image/png", "Screen Shot (1).png"},
		{"Отчёт.png", "image/png", "Отчёт.png"},
		{"../../etc/passwd", "", "passwd"},
		{`C:\Users\me\clip.mp4`, "video/mp4", "clip.mp4"},
		{"a;rm -rf ~.png", "image/png", "a-rm -rf.png"},
		{"", "", "attachment"},
		{"...", "", "attachment"},
		{".hidden", "", "attachment.hidden"},
		{strings.Repeat("x", 200) + ".png", "image/png", strings.Repeat("x", 80) + ".png"},
		// A GitHub asset id names its bytes with no extension; the
		// classified type supplies one.
		{"4f1c2d7e-9a3b-4c5d-8e6f-0a1b2c3d4e5f", "image/png", "4f1c2d7e-9a3b-4c5d-8e6f-0a1b2c3d4e5f.png"},
		{"diagram", "image/svg+xml", "diagram.svg"},
		{"clip", " Video/MP4 ", "clip.mp4"},
		// An existing extension is the uploader's and is kept, even when the
		// bytes turned out to be another type.
		{"shot.jpeg", "image/png", "shot.jpeg"},
		// A type with no known extension adds nothing.
		{"notes", "application/octet-stream", "notes"},
		{"", "image/png", "attachment.png"},
	}
	for _, tc := range cases {
		if got := downloadFileName(tc.in, tc.mimeType); got != tc.want {
			t.Errorf("downloadFileName(%q, %q) = %q, want %q", tc.in, tc.mimeType, got, tc.want)
		}
	}
}

// TestDownloadExtensionsCoverTheClassifiers ties the extension table to
// what the byte classifiers answer: a media type they can return without
// an entry would save an extensionless file again.
func TestDownloadExtensionsCoverTheClassifiers(t *testing.T) {
	samples := map[string][]byte{
		"png":  realPNGBytes(t),
		"svg":  []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"></svg>`),
		"mp3":  append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, 64)...),
		"flac": append([]byte("fLaC"), make([]byte, 64)...),
		"webm": append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 64)...),
	}
	for name, data := range samples {
		mimeType, kind, err := forgeattach.Classify(data, "")
		if err != nil {
			t.Fatalf("%s: Classify: %v", name, err)
		}
		if kind == forgeattach.KindFile {
			t.Fatalf("%s: classified as a file (%s); the sample no longer exercises a media type", name, mimeType)
		}
		if _, ok := downloadExtensions[mimeType]; !ok {
			t.Errorf("%s: Classify answered %q, which has no download extension", name, mimeType)
		}
	}
}

// homeWithDownloads points HOME at a fresh directory holding a Downloads
// folder and returns that folder.
func homeWithDownloads(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("create Downloads: %v", err)
	}
	t.Setenv("HOME", home)
	return downloads
}

// TestDownloadsDirPrefersTheWindowsDownloadsFolder: under the Windows
// launcher the WSL home's Downloads is not where the user looks, so the
// launcher's export wins over it.
func TestDownloadsDirPrefersTheWindowsDownloadsFolder(t *testing.T) {
	homeWithDownloads(t)
	windowsDownloads := t.TempDir()
	t.Setenv(wsldistro.DownloadsEnv, windowsDownloads)
	app := &App{configDir: t.TempDir()}

	got, err := app.downloadsDir()
	if err != nil {
		t.Fatalf("downloadsDir: %v", err)
	}
	if got != windowsDownloads {
		t.Fatalf("downloadsDir = %q, want the exported Windows folder %q", got, windowsDownloads)
	}
}

// TestDownloadsDirIgnoresAnUnusableWindowsExport: an unset export, one
// that names a file, and one that names nothing all leave the ordinary
// home Downloads folder in charge.
func TestDownloadsDirIgnoresAnUnusableWindowsExport(t *testing.T) {
	homeDownloads := homeWithDownloads(t)
	regular := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for name, value := range map[string]string{
		"unset":   "",
		"file":    regular,
		"missing": filepath.Join(t.TempDir(), "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(wsldistro.DownloadsEnv, value)
			app := &App{configDir: t.TempDir()}
			got, err := app.downloadsDir()
			if err != nil {
				t.Fatalf("downloadsDir: %v", err)
			}
			if got != homeDownloads {
				t.Fatalf("downloadsDir = %q, want the home Downloads %q", got, homeDownloads)
			}
		})
	}
}

// TestDownloadsDirStaysInTheDataDirectoryForAnIsolatedBoot: a mocked boot
// inherits the developer's HOME and the launcher's environment, and must
// not write into either Downloads folder.
func TestDownloadsDirStaysInTheDataDirectoryForAnIsolatedBoot(t *testing.T) {
	homeWithDownloads(t)
	t.Setenv(wsldistro.DownloadsEnv, t.TempDir())
	app := &App{configDir: t.TempDir()}
	ConfigureIsolation(app, IsolationConfig{})

	got, err := app.downloadsDir()
	if err != nil {
		t.Fatalf("downloadsDir: %v", err)
	}
	if want := filepath.Join(app.configDir, "downloads"); got != want {
		t.Fatalf("downloadsDir = %q, want the isolated %q", got, want)
	}
}
