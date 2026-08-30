package chromium

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/eventchan"
)

// chromeForTestingManifestURL is the canonical manifest published by
// Chrome-for-Testing. Tests override this via Installer.ManifestURL.
const chromeForTestingManifestURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// installerUserAgent identifies our traffic to googlechromelabs.github.io
// so server-side rate-limit / anomaly logs can attribute requests.
const installerUserAgent = "agent-overflow/chromium-installer"

type Artifact string

const (
	ArtifactChrome        Artifact = "chrome"
	ArtifactHeadlessShell Artifact = "chrome-headless-shell"
)

// Manifest body cap. The real Chrome-for-Testing manifest is ~600 KB;
// a 5 MiB cap leaves room for upstream growth without letting a
// hostile mirror (or an unforeseen JSON expansion) OOM the parser.
const manifestMaxBytes = 5 << 20 // 5 MiB

// Per-archive limits. Chrome-for-Testing's largest individual file
// is well under 500 MiB; the aggregate uncompressed size is ~400 MiB.
// 500 MiB / 1 GiB caps leave enough headroom for upstream packaging
// shifts while bounding zip-bomb damage.
const (
	zipMaxDownloadBytes  int64 = 512 << 20 // 512 MiB compressed archive
	zipMaxFileBytes      int64 = 500 << 20 // 500 MiB per entry
	zipMaxAggregateBytes int64 = 1 << 30   // 1 GiB total
)

// manifestFetchTimeout caps the manifest GET independently of the
// long-tail download timeout — a stuck JSON server shouldn't hold
// the goroutine for the full download budget.
const manifestFetchTimeout = 30 * time.Second

// InstallProgress is the per-tick event the installer emits.
//
// Phase values:
//   - "resolving"   — fetching the version manifest
//   - "downloading" — streaming the zip; Downloaded / Total are bytes
//   - "extracting"  — unpacking the zip
//   - "ready"       — binary is on disk and executable; Version set
//   - "error"       — failed; Error carries the reason
//
// When Total is 0 (server didn't send Content-Length), the bar should
// render as indeterminate.
type InstallProgress struct {
	Phase      string `json:"phase"`
	Downloaded int64  `json:"downloaded,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Version    string `json:"version,omitempty"`
	Error      string `json:"error,omitempty"`
}

// InstallResult is the post-install handle the rest of the package
// uses. BinaryPath is an absolute path to the executable, ready to
// be passed to chromedp's `ExecPath` allocator option.
type InstallResult struct {
	Version    string
	BinaryPath string
}

// Installer resolves and downloads one managed Chrome artifact. Reuse a
// single instance per app process — it has no real state of its own
// but it's the documented entry point.
type Installer struct {
	Artifact     Artifact
	EventChannel eventchan.Channel
	// ConfigDir is the parent of the artifact cache. Defaults
	// to the App's configDir; tests pass a t.TempDir().
	ConfigDir string

	// ManifestURL overrides ChromeForTestingManifestURL. Tests inject
	// their own httptest URL here.
	ManifestURL string

	// HTTPClient overrides the default. Tests inject one with a
	// shorter timeout or a recorded round-tripper.
	HTTPClient *http.Client

	// Emit receives InstallProgress events. nil is allowed — events
	// are dropped silently.
	Emit func(channel eventchan.Channel, data any)

	// AllowInsecureScheme bypasses the https-only check on
	// ManifestURL and the resolved zip URL. ONLY for tests serving
	// the manifest from httptest.NewServer (plain HTTP loopback);
	// never enable in production.
	AllowInsecureScheme bool
}

// NewInstaller wires the supplied dependencies and applies defaults.
func NewInstaller(configDir string, artifact Artifact, channel eventchan.Channel, emit func(eventchan.Channel, any)) *Installer {
	return &Installer{
		Artifact:     artifact,
		EventChannel: channel,
		ConfigDir:    configDir,
		ManifestURL:  chromeForTestingManifestURL,
		HTTPClient: &http.Client{
			// The zip is ~150 MB. A slow connection can take a while;
			// the timeout exists only to bound a server that stops
			// sending bytes mid-stream.
			Timeout: 30 * time.Minute,
		},
		Emit: emit,
	}
}

// Install resolves the latest stable version, downloads the platform-
// specific artifact zip if it isn't already cached,
// extracts it, and returns the absolute path to the executable.
// Repeated calls with the same cached version are cheap — the
// manifest is still re-fetched (cheap JSON request) so we pick up
// upstream rolls, but the byte download is skipped when the version
// directory already exists.
func (i *Installer) Install(ctx context.Context) (InstallResult, error) {
	if i.ConfigDir == "" {
		return InstallResult{}, fmt.Errorf("chromium: installer ConfigDir required")
	}
	if i.Artifact != ArtifactChrome && i.Artifact != ArtifactHeadlessShell {
		return InstallResult{}, fmt.Errorf("chromium: unsupported artifact %q", i.Artifact)
	}
	if i.ManifestURL == "" {
		i.ManifestURL = chromeForTestingManifestURL
	}
	if i.HTTPClient == nil {
		i.HTTPClient = http.DefaultClient
	}

	platform, err := currentPlatform()
	if err != nil {
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, err
	}
	if !i.AllowInsecureScheme {
		if err := assertHTTPS(i.ManifestURL); err != nil {
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
	}

	i.emit(InstallProgress{Phase: "resolving"})
	manifest, err := i.fetchManifest(ctx)
	if err != nil {
		if cached, ok := i.cachedInstall(platform); ok {
			log.Printf("chromium: manifest unavailable; using cached %s %s: %v", i.Artifact, cached.Version, err)
			i.emit(InstallProgress{Phase: "ready", Version: cached.Version})
			return cached, nil
		}
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("chromium: resolve manifest: %w", err)
	}

	stable, ok := manifest.Channels["Stable"]
	if !ok || stable.Version == "" {
		err := fmt.Errorf("chromium: manifest missing Stable channel")
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, err
	}

	// Validate Version before interpolating it into a path. The
	// manifest comes over HTTPS from googlechromelabs.github.io, but
	// ManifestURL is overrideable (air-gapped mirror) so we can't
	// assume a trusted producer. A version segment must be a single
	// non-empty path component without separators or "." / ".." so
	// later filepath.Join calls can't escape cacheDir.
	if err := validateVersionSegment(stable.Version); err != nil {
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, err
	}

	zipURL, ok := stable.artifactURL(i.Artifact, platform)
	if !ok {
		err := fmt.Errorf("chromium: no %s download for platform %s", i.Artifact, platform)
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, err
	}
	if !i.AllowInsecureScheme {
		if err := assertHTTPS(zipURL); err != nil {
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
	}

	cacheDir := filepath.Join(i.ConfigDir, i.cacheName())
	versionDir := filepath.Join(cacheDir, stable.Version)
	binaryPath := binaryPathFor(versionDir, platform, i.Artifact)

	// Already installed?
	if isExecutable(binaryPath) {
		i.emit(InstallProgress{Phase: "ready", Version: stable.Version})
		i.pruneOldVersions(cacheDir, stable.Version)
		return InstallResult{Version: stable.Version, BinaryPath: binaryPath}, nil
	}

	// Fresh install — make sure prior partial state is cleared.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("chromium: mkdir cache: %w", err)
	}
	_ = os.RemoveAll(versionDir)

	zipPath := filepath.Join(cacheDir, stable.Version+".zip.partial")
	if err := i.download(ctx, zipURL, zipPath, stable.Version); err != nil {
		_ = os.Remove(zipPath)
		if cached, ok := i.cachedInstall(platform); ok {
			log.Printf("chromium: download unavailable; using cached %s %s: %v", i.Artifact, cached.Version, err)
			i.emit(InstallProgress{Phase: "ready", Version: cached.Version})
			return cached, nil
		}
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("chromium: download: %w", err)
	}

	i.emit(InstallProgress{Phase: "extracting", Version: stable.Version})
	if err := unzip(zipPath, versionDir); err != nil {
		_ = os.RemoveAll(versionDir)
		_ = os.Remove(zipPath)
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("chromium: extract: %w", err)
	}
	_ = os.Remove(zipPath)

	if !isExecutable(binaryPath) {
		// Either the zip layout shifted upstream or chmod failed.
		// Walk the version dir and try to recover the binary path.
		recovered, walkErr := findBrowserBinary(versionDir, platform, i.Artifact)
		if walkErr != nil || recovered == "" {
			err := fmt.Errorf("chromium: extracted bundle missing binary at %s", binaryPath)
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
		binaryPath = recovered
	}

	i.emit(InstallProgress{Phase: "ready", Version: stable.Version})
	i.pruneOldVersions(cacheDir, stable.Version)
	return InstallResult{Version: stable.Version, BinaryPath: binaryPath}, nil
}

func (i *Installer) cacheName() string {
	if i.Artifact == ArtifactHeadlessShell {
		return "headless-shell"
	}
	return string(i.Artifact)
}

func (i *Installer) cachedInstall(platform string) (InstallResult, bool) {
	cacheDir := filepath.Join(i.ConfigDir, i.cacheName())
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return InstallResult{}, false
	}
	var best InstallResult
	var bestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() || validateVersionSegment(entry.Name()) != nil {
			continue
		}
		binaryPath := binaryPathFor(filepath.Join(cacheDir, entry.Name()), platform, i.Artifact)
		if !isExecutable(binaryPath) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if best.BinaryPath == "" || info.ModTime().After(bestMod) {
			best = InstallResult{Version: entry.Name(), BinaryPath: binaryPath}
			bestMod = info.ModTime()
		}
	}
	return best, best.BinaryPath != ""
}

// pruneOldVersions removes sibling version directories under cacheDir
// that do not match keepVersion. Each candidate segment is re-validated
// via validateVersionSegment before any RemoveAll, so a hand-edited
// cacheDir with an arbitrary subdir name (or a partially-extracted
// .zip.partial leftover) is left alone rather than recursively removed.
//
// Soft-fail per entry: a stale dir we can't unlink (Windows file lock
// on a peer process's chrome-headless-shell.exe surfaces as
// ERROR_SHARING_VIOLATION; a permission error on a manually-chmod'd
// path) is logged but does not abort the caller's Install. Next launch
// retries.
func (i *Installer) pruneOldVersions(cacheDir, keepVersion string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		// cacheDir may not exist yet on a true first install; nothing
		// to prune in that case.
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			// .zip.partial leftovers are files; the success path
			// removes them, but a crashed download can leave one
			// behind. Skip — RemoveAll on a file would still work,
			// but keeping pruneOldVersions strictly directory-scoped
			// makes the contract easier to reason about.
			continue
		}
		if name == keepVersion {
			continue
		}
		if err := validateVersionSegment(name); err != nil {
			// Not a version dir we created — leave it alone. This is
			// the load-bearing safety check; trusting filesystem
			// names blindly would let an attacker who could write
			// into cacheDir gain a recursive-remove primitive on any
			// path that filepath.Clean accepts.
			continue
		}
		target := filepath.Join(cacheDir, name)
		if err := os.RemoveAll(target); err != nil {
			log.Printf("chromium: prune %q: %v", target, err)
		}
	}
}

func (i *Installer) emit(p InstallProgress) {
	if i.Emit == nil || i.EventChannel == "" {
		return
	}
	i.Emit(i.EventChannel, p)
}

// chromeForTestingManifest is the subset of the Chrome-for-Testing
// JSON schema we care about. We tolerate unknown fields so future
// schema additions don't break parsing.
