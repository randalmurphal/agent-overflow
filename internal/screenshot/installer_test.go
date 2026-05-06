package screenshot

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeArchive builds an in-memory zip mirroring Chrome-for-Testing's
// layout: one top-level directory named after the platform,
// containing a fake `chrome-headless-shell` (or .exe on Windows).
// The fake binary is a tiny shell script / batch file we never
// actually exec — it only has to exist with the executable bit set
// for isExecutable() to accept it.
func fakeArchive(t *testing.T, platform string) []byte {
	t.Helper()
	binName := "chrome-headless-shell"
	if platform == "win64" {
		binName = "chrome-headless-shell.exe"
	}
	innerDir := "chrome-headless-shell-" + platform

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Top-level folder entry — some unzippers care, ours doesn't,
	// but match the real layout.
	if _, err := zw.Create(innerDir + "/"); err != nil {
		t.Fatalf("zip create dir: %v", err)
	}

	// The headless-shell binary with executable mode set.
	hdr := &zip.FileHeader{Name: innerDir + "/" + binName, Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("zip create file: %v", err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatalf("zip write: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// fakeManifestServer serves the manifest + zip from a single
// httptest server. Both URLs are absolute against the server's
// own address so the installer can navigate manifest → zip without
// any external network access.
func fakeManifestServer(t *testing.T, version string) (*httptest.Server, *int32) {
	t.Helper()
	platform, err := currentPlatform()
	if err != nil {
		t.Skipf("currentPlatform unsupported: %v", err)
	}
	zipBytes := fakeArchive(t, platform)

	var downloads int32
	var mu sync.Mutex

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/zip", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		downloads++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
		_, _ = w.Write(zipBytes)
	})

	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		zipURL := srv.URL + "/zip"
		body := chromeForTestingManifest{
			Channels: map[string]chromeForTestingChannel{
				"Stable": {
					Version: version,
					Downloads: map[string][]chromeForTestingDownload{
						"chrome-headless-shell": {{Platform: platform, URL: zipURL}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	return srv, &downloads
}

func TestInstaller_Install_Fresh(t *testing.T) {
	if _, err := currentPlatform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()
	srv, downloads := fakeManifestServer(t, "999.0.1.0")

	var events []InstallProgress
	emit := func(name string, data any) {
		if name != InstallEventName {
			return
		}
		if p, ok := data.(InstallProgress); ok {
			events = append(events, p)
		}
	}

	inst := NewInstaller(cacheDir, emit)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true // httptest.NewServer is plain HTTP

	res, err := inst.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Version != "999.0.1.0" {
		t.Errorf("Version = %q, want 999.0.1.0", res.Version)
	}
	if !isExecutable(res.BinaryPath) {
		t.Errorf("BinaryPath %q not executable", res.BinaryPath)
	}
	if *downloads != 1 {
		t.Errorf("download count = %d, want 1", *downloads)
	}

	// Phases must include resolving → downloading → extracting → ready.
	phases := []string{}
	for _, e := range events {
		phases = append(phases, e.Phase)
	}
	for _, want := range []string{"resolving", "downloading", "extracting", "ready"} {
		found := false
		for _, p := range phases {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("phase %q missing from event stream %v", want, phases)
		}
	}
}

func TestInstaller_Install_Cached(t *testing.T) {
	if _, err := currentPlatform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()
	srv, downloads := fakeManifestServer(t, "999.0.1.0")

	inst := NewInstaller(cacheDir, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// Second call must NOT re-download. Manifest is still re-fetched
	// (cheap, picks up upstream rolls) but the zip is skipped.
	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if *downloads != 1 {
		t.Errorf("second Install re-downloaded; download count = %d, want 1", *downloads)
	}
}

func TestInstaller_Install_BadStatus(t *testing.T) {
	if _, err := currentPlatform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	inst := NewInstaller(cacheDir, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err == nil {
		t.Fatal("Install() = nil err, want error on 500 manifest")
	}
}

func TestInstaller_Install_BadPlatform(t *testing.T) {
	cacheDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := chromeForTestingManifest{
			Channels: map[string]chromeForTestingChannel{
				"Stable": {
					Version: "999.0.1.0",
					Downloads: map[string][]chromeForTestingDownload{
						"chrome-headless-shell": {{Platform: "alien-arch", URL: "ignored"}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	inst := NewInstaller(cacheDir, nil)
	inst.ManifestURL = srv.URL
	inst.AllowInsecureScheme = true

	_, err := inst.Install(context.Background())
	if err == nil {
		t.Fatal("Install with no matching platform = nil, want error")
	}
	if !strings.Contains(err.Error(), "platform") {
		t.Errorf("err = %v, want platform mismatch message", err)
	}
}

func TestInstaller_Install_RecoversIfBinaryAtUnexpectedPath(t *testing.T) {
	platform, err := currentPlatform()
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	if platform == "win64" {
		t.Skip("symlink-based recovery test skipped on windows")
	}

	cacheDir := t.TempDir()

	// Build a zip whose binary is at an unexpected nested path —
	// simulates an upstream layout shift. findHeadlessShell should
	// recover by walking the version dir.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "weird-layout/nested/chrome-headless-shell", Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	_, _ = w.Write([]byte("#!/bin/sh\nexit 0\n"))
	_ = zw.Close()
	zipBytes := buf.Bytes()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	})
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		body := chromeForTestingManifest{
			Channels: map[string]chromeForTestingChannel{
				"Stable": {
					Version: "999.0.1.0",
					Downloads: map[string][]chromeForTestingDownload{
						"chrome-headless-shell": {{Platform: platform, URL: srv.URL + "/zip"}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	inst := NewInstaller(cacheDir, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	res, err := inst.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.HasSuffix(res.BinaryPath, "chrome-headless-shell") {
		t.Errorf("recovered BinaryPath = %q, want suffix chrome-headless-shell", res.BinaryPath)
	}
	if !isExecutable(res.BinaryPath) {
		t.Errorf("recovered BinaryPath %q not executable", res.BinaryPath)
	}
}

// TestUnzip_RejectsZipSlip pins the path-traversal guard. A zip
// entry like "../evil" must not write outside the destination dir.
func TestUnzip_RejectsZipSlip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path separator handling differs on windows; covered by Linux/Mac CI")
	}
	tmpRoot := t.TempDir()
	dstDir := filepath.Join(tmpRoot, "dst")
	srcZip := filepath.Join(tmpRoot, "evil.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escaped.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write([]byte("escape"))
	_ = zw.Close()

	if err := os.WriteFile(srcZip, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := unzip(srcZip, dstDir); err == nil {
		t.Fatal("unzip(zip-slip) = nil err, want rejection")
	}
}
