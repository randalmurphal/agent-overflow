package chromium

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

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/headlessshell"
)

// fakeArchive builds an in-memory zip mirroring Chrome-for-Testing's
// layout: one top-level directory named after the platform,
// containing a fake `chrome-headless-shell` (or .exe on Windows).
// The fake binary is a tiny shell script / batch file we never
// actually exec — it only has to exist with the executable bit set
// for headlessshell.Executable() to accept it.
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

func fakeChromeArchive(t *testing.T, platform string) []byte {
	t.Helper()
	path := filepath.ToSlash(strings.TrimPrefix(binaryPathFor("root", platform, ArtifactChrome), "root"+string(filepath.Separator)))
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: path, Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestChromeBinaryPathsMatchPublishedArchiveLayouts(t *testing.T) {
	root := "/cache/version"
	cases := map[string]string{
		"linux64":   filepath.Join(root, "chrome-linux64", "chrome"),
		"win64":     filepath.Join(root, "chrome-win64", "chrome.exe"),
		"mac-x64":   filepath.Join(root, "chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		"mac-arm64": filepath.Join(root, "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
	}
	for platform, want := range cases {
		if got := binaryPathFor(root, platform, ArtifactChrome); got != want {
			t.Errorf("%s path = %q, want %q", platform, got, want)
		}
	}
}

func TestInstallerUsesSuppliedExecutableWithoutNetwork(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installer := NewInstaller("", ArtifactChrome, eventchan.BrowserInstallProgress, nil)
	installer.BinaryPath = binary
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.BinaryPath != binary || result.Version != "supplied" {
		t.Fatalf("result = %#v", result)
	}
}

func TestInstallerInstallsFullChromeArtifact(t *testing.T) {
	platform, err := currentPlatform()
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	archive := fakeChromeArchive(t, platform)
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/chrome.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(chromeForTestingManifest{Channels: map[string]chromeForTestingChannel{
			"Stable": {Version: "999.1.2.3", Downloads: map[string][]chromeForTestingDownload{
				"chrome": {{Platform: platform, URL: server.URL + "/chrome.zip"}},
			}},
		}})
	})
	installer := NewInstaller(t.TempDir(), ArtifactChrome, "", nil)
	installer.ManifestURL = server.URL + "/manifest"
	installer.AllowInsecureScheme = true
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !isExecutable(result.BinaryPath) || !strings.Contains(result.BinaryPath, string(filepath.Separator)+"chrome"+string(filepath.Separator)) {
		t.Fatalf("full Chrome binary = %q", result.BinaryPath)
	}
}

// fakeManifestServer serves the manifest + zip from a single
// httptest server. Both URLs are absolute against the server's
// own address so the installer can navigate manifest → zip without
// any external network access.
func fakeManifestServer(t *testing.T, version string) (*httptest.Server, *int32) {
	t.Helper()
	platform, err := headlessshell.Platform()
	if err != nil {
		t.Skipf("headlessshell.Platform unsupported: %v", err)
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
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()
	srv, downloads := fakeManifestServer(t, "999.0.1.0")

	var events []InstallProgress
	emit := func(name eventchan.Channel, data any) {
		if name != eventchan.ScreenshotInstallProgress {
			return
		}
		if p, ok := data.(InstallProgress); ok {
			events = append(events, p)
		}
	}

	inst := NewInstaller(cacheDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, emit)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true // httptest.NewServer is plain HTTP

	res, err := inst.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Version != "999.0.1.0" {
		t.Errorf("Version = %q, want 999.0.1.0", res.Version)
	}
	if !headlessshell.Executable(res.BinaryPath) {
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
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()
	srv, downloads := fakeManifestServer(t, "999.0.1.0")

	inst := NewInstaller(cacheDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
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
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	cacheDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	inst := NewInstaller(cacheDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err == nil {
		t.Fatal("Install() = nil err, want error on 500 manifest")
	}
}

func TestInstallerDownloadRefusesOversizedArchiveFromHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", zipMaxDownloadBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	installer := NewInstaller(t.TempDir(), ArtifactChrome, "", nil)
	err := installer.download(context.Background(), server.URL, filepath.Join(t.TempDir(), "chrome.zip.partial"), "test")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized download error = %v", err)
	}
}

func TestInstallerRejectsInsecureManifestBeforeNetworkRequest(t *testing.T) {
	var requested bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	t.Cleanup(server.Close)
	installer := NewInstaller(t.TempDir(), ArtifactChrome, "", nil)
	installer.ManifestURL = server.URL + "/manifest"
	if _, err := installer.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("insecure manifest error = %v", err)
	}
	if requested {
		t.Fatal("insecure manifest was requested before validation")
	}
}

func TestInstallerUsesCachedArtifactWhenManifestIsOffline(t *testing.T) {
	platform, err := currentPlatform()
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	configDir := t.TempDir()
	version := "999.0.1.0"
	binaryPath := binaryPathFor(filepath.Join(configDir, "headless-shell", version), platform, ArtifactHeadlessShell)
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	installer := NewInstaller(configDir, ArtifactHeadlessShell, "", nil)
	installer.ManifestURL = "http://127.0.0.1:1/offline"
	installer.AllowInsecureScheme = true
	installer.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond}
	result, err := installer.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.BinaryPath != binaryPath || result.Version != version {
		t.Fatalf("cached result = %#v", result)
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

	inst := NewInstaller(cacheDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
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
	platform, err := headlessshell.Platform()
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

	inst := NewInstaller(cacheDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	res, err := inst.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.HasSuffix(res.BinaryPath, "chrome-headless-shell") {
		t.Errorf("recovered BinaryPath = %q, want suffix chrome-headless-shell", res.BinaryPath)
	}
	if !headlessshell.Executable(res.BinaryPath) {
		t.Errorf("recovered BinaryPath %q not executable", res.BinaryPath)
	}
}

// TestInstaller_PrunesOldVersionsOnFreshInstall pins the disk-space
// contract: a fresh install of vN removes any sibling vM directories
// left behind by previous Chrome rolls, so the on-disk cache size is
// O(1) in the number of upstream releases rather than unbounded.
func TestInstaller_PrunesOldVersionsOnFreshInstall(t *testing.T) {
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	configDir := t.TempDir()
	cacheDir := filepath.Join(configDir, "headless-shell")
	if err := os.MkdirAll(filepath.Join(cacheDir, "888.0.1.0"), 0o755); err != nil {
		t.Fatalf("seed stale 888: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "777.0.1.0"), 0o755); err != nil {
		t.Fatalf("seed stale 777: %v", err)
	}
	srv, _ := fakeManifestServer(t, "999.0.1.0")

	inst := NewInstaller(configDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "999.0.1.0")); err != nil {
		t.Errorf("current version dir missing after install: %v", err)
	}
	for _, stale := range []string{"888.0.1.0", "777.0.1.0"} {
		if _, err := os.Stat(filepath.Join(cacheDir, stale)); !os.IsNotExist(err) {
			t.Errorf("stale dir %q still exists after install (err=%v)", stale, err)
		}
	}
}

// TestInstaller_PrunesOldVersionsOnWarmCache covers the warm-cache
// path: a stale dir that arrived between two installs of the same
// version (e.g. user upgraded the app, downgraded, then upgraded
// again) is removed on the second invocation even though no fresh
// download happened.
func TestInstaller_PrunesOldVersionsOnWarmCache(t *testing.T) {
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	configDir := t.TempDir()
	cacheDir := filepath.Join(configDir, "headless-shell")
	srv, _ := fakeManifestServer(t, "999.0.1.0")

	inst := NewInstaller(configDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// Drop a stale version dir AFTER the first install creates the
	// cache layout, so the second Install hits the warm-cache path
	// (binary already executable) but still has to clean up.
	if err := os.MkdirAll(filepath.Join(cacheDir, "888.0.1.0"), 0o755); err != nil {
		t.Fatalf("seed stale 888 between installs: %v", err)
	}

	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "888.0.1.0")); !os.IsNotExist(err) {
		t.Errorf("warm-cache install did not prune stale dir (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "999.0.1.0")); err != nil {
		t.Errorf("current version dir disappeared: %v", err)
	}
}

// TestInstaller_PruneIgnoresInvalidSegments pins the safety guarantee:
// a directory under headless-shell/ whose name doesn't pass
// validateVersionSegment is left alone. Without this check, a hand-
// edited cacheDir could turn the prune step into a recursive-remove
// primitive on arbitrary sibling paths.
func TestInstaller_PruneIgnoresInvalidSegments(t *testing.T) {
	if _, err := headlessshell.Platform(); err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	configDir := t.TempDir()
	cacheDir := filepath.Join(configDir, "headless-shell")
	// Both names trip the contains("..") guard in validateVersionSegment
	// — they're shapes a hand-edited cacheDir might plausibly contain
	// (a half-typed traversal attempt, a manual-rename gone wrong) but
	// nothing the installer itself would ever produce.
	invalidNames := []string{"..bad", "ver..0.1.0"}
	for _, name := range invalidNames {
		if err := os.MkdirAll(filepath.Join(cacheDir, name), 0o755); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}
	srv, _ := fakeManifestServer(t, "999.0.1.0")

	inst := NewInstaller(configDir, ArtifactHeadlessShell, eventchan.ScreenshotInstallProgress, nil)
	inst.ManifestURL = srv.URL + "/manifest"
	inst.AllowInsecureScheme = true

	if _, err := inst.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, name := range invalidNames {
		if _, err := os.Stat(filepath.Join(cacheDir, name)); err != nil {
			t.Errorf("invalid-segment dir %q was removed (err=%v); pruner must leave non-version dirs alone", name, err)
		}
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
