package screenshot

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
)

// chromeForTestingManifestURL is the canonical manifest published by
// Chrome-for-Testing. The Stable channel's `chrome-headless-shell`
// download is what Puppeteer / Playwright / Cypress all use. Tests
// override this via Installer.ManifestURL.
const chromeForTestingManifestURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// installerUserAgent identifies our traffic to googlechromelabs.github.io
// so server-side rate-limit / anomaly logs can attribute requests.
const installerUserAgent = "agent-overflow/headless-shell-installer"

// Manifest body cap. The real Chrome-for-Testing manifest is ~600 KB;
// a 5 MiB cap leaves room for upstream growth without letting a
// hostile mirror (or an unforeseen JSON expansion) OOM the parser.
const manifestMaxBytes = 5 << 20 // 5 MiB

// Per-archive limits. chrome-headless-shell's largest individual file
// is well under 500 MiB; the aggregate uncompressed size is ~400 MiB.
// 500 MiB / 1 GiB caps leave enough headroom for upstream packaging
// shifts while bounding zip-bomb damage.
const (
	zipMaxFileBytes      int64 = 500 << 20 // 500 MiB per entry
	zipMaxAggregateBytes int64 = 1 << 30   // 1 GiB total
)

// manifestFetchTimeout caps the manifest GET independently of the
// long-tail download timeout — a stuck JSON server shouldn't hold
// the goroutine for the full download budget.
const manifestFetchTimeout = 30 * time.Second

// InstallEventName is the channel name the install progress events
// flow over. No frontend listener is wired today; events are useful
// for diagnostic logs and a future "Downloading rendering engine…"
// banner.
const InstallEventName = eventchan.ScreenshotInstallProgress

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

// Installer resolves and downloads chrome-headless-shell. Reuse a
// single instance per app process — it has no real state of its own
// but it's the documented entry point.
type Installer struct {
	// ConfigDir is the parent of the headless-shell cache. Defaults
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

// NewInstaller wires the supplied dependencies and applies defaults
// for the rest. ConfigDir is required; the cache lives under
// configDir/headless-shell/.
func NewInstaller(configDir string, emit func(eventchan.Channel, any)) *Installer {
	return &Installer{
		ConfigDir:   configDir,
		ManifestURL: chromeForTestingManifestURL,
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
// specific chrome-headless-shell zip if it isn't already cached,
// extracts it, and returns the absolute path to the executable.
// Repeated calls with the same cached version are cheap — the
// manifest is still re-fetched (cheap JSON request) so we pick up
// upstream rolls, but the byte download is skipped when the version
// directory already exists.
func (i *Installer) Install(ctx context.Context) (InstallResult, error) {
	if i.ConfigDir == "" {
		return InstallResult{}, fmt.Errorf("screenshot: installer ConfigDir required")
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

	i.emit(InstallProgress{Phase: "resolving"})
	manifest, err := i.fetchManifest(ctx)
	if err != nil {
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("screenshot: resolve manifest: %w", err)
	}

	stable, ok := manifest.Channels["Stable"]
	if !ok || stable.Version == "" {
		err := fmt.Errorf("screenshot: manifest missing Stable channel")
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

	zipURL, ok := stable.headlessShellURL(platform)
	if !ok {
		err := fmt.Errorf("screenshot: no chrome-headless-shell download for platform %s", platform)
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, err
	}
	if !i.AllowInsecureScheme {
		if err := assertHTTPS(i.ManifestURL); err != nil {
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
		if err := assertHTTPS(zipURL); err != nil {
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
	}

	cacheDir := filepath.Join(i.ConfigDir, "headless-shell")
	versionDir := filepath.Join(cacheDir, stable.Version)
	binaryPath := binaryPathFor(versionDir, platform)

	// Already installed?
	if isExecutable(binaryPath) {
		i.emit(InstallProgress{Phase: "ready", Version: stable.Version})
		i.pruneOldVersions(cacheDir, stable.Version)
		return InstallResult{Version: stable.Version, BinaryPath: binaryPath}, nil
	}

	// Fresh install — make sure prior partial state is cleared.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("screenshot: mkdir cache: %w", err)
	}
	_ = os.RemoveAll(versionDir)

	zipPath := filepath.Join(cacheDir, stable.Version+".zip.partial")
	if err := i.download(ctx, zipURL, zipPath, stable.Version); err != nil {
		_ = os.Remove(zipPath)
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("screenshot: download: %w", err)
	}

	i.emit(InstallProgress{Phase: "extracting", Version: stable.Version})
	if err := unzip(zipPath, versionDir); err != nil {
		_ = os.RemoveAll(versionDir)
		_ = os.Remove(zipPath)
		i.emit(InstallProgress{Phase: "error", Error: err.Error()})
		return InstallResult{}, fmt.Errorf("screenshot: extract: %w", err)
	}
	_ = os.Remove(zipPath)

	if !isExecutable(binaryPath) {
		// Either the zip layout shifted upstream or chmod failed.
		// Walk the version dir and try to recover the binary path.
		recovered, walkErr := findHeadlessShell(versionDir, platform)
		if walkErr != nil || recovered == "" {
			err := fmt.Errorf("screenshot: extracted bundle missing binary at %s", binaryPath)
			i.emit(InstallProgress{Phase: "error", Error: err.Error()})
			return InstallResult{}, err
		}
		binaryPath = recovered
	}

	i.emit(InstallProgress{Phase: "ready", Version: stable.Version})
	i.pruneOldVersions(cacheDir, stable.Version)
	return InstallResult{Version: stable.Version, BinaryPath: binaryPath}, nil
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
			log.Printf("screenshot: prune %q: %v", target, err)
		}
	}
}

func (i *Installer) emit(p InstallProgress) {
	if i.Emit == nil {
		return
	}
	i.Emit(InstallEventName, p)
}

// chromeForTestingManifest is the subset of the Chrome-for-Testing
// JSON schema we care about. We tolerate unknown fields so future
// schema additions don't break parsing.
type chromeForTestingManifest struct {
	Channels map[string]chromeForTestingChannel `json:"channels"`
}

type chromeForTestingChannel struct {
	Version   string                                `json:"version"`
	Downloads map[string][]chromeForTestingDownload `json:"downloads"`
}

type chromeForTestingDownload struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

func (c chromeForTestingChannel) headlessShellURL(platform string) (string, bool) {
	for _, d := range c.Downloads["chrome-headless-shell"] {
		if d.Platform == platform {
			return d.URL, true
		}
	}
	return "", false
}

func (i *Installer) fetchManifest(ctx context.Context) (*chromeForTestingManifest, error) {
	// Tighten the manifest deadline regardless of HTTPClient.Timeout —
	// a stuck JSON server shouldn't hold the goroutine for the long
	// download budget.
	fetchCtx, cancel := context.WithTimeout(ctx, manifestFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, i.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", installerUserAgent)
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest status %d", resp.StatusCode)
	}
	// Bound the body before parse: a hostile mirror could otherwise
	// stream gigabytes of JSON and OOM the process.
	body := io.LimitReader(resp.Body, manifestMaxBytes+1)
	var m chromeForTestingManifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	return &m, nil
}

// download streams the zip to dst with periodic progress events.
//
// We don't currently support HTTP Range resumption: a one-time ~150
// MB download from Google's CDN is reliable in practice, and a
// retry-on-failure UX is simpler to reason about than a partial
// resumption that can land mid-byte if the server's content shifted.
// The .partial suffix on dst exists so the caller can distinguish a
// completed file from an in-flight one for cleanup.
func (i *Installer) download(ctx context.Context, url, dst, version string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", installerUserAgent)
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	total := resp.ContentLength

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	// Throttled progress emission — once per 256 KB, to avoid flooding
	// the event ring on a fast download.
	const reportEvery = 256 * 1024
	var written, lastReport int64
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written-lastReport >= reportEvery || readErr == io.EOF {
				lastReport = written
				i.emit(InstallProgress{
					Phase:      "downloading",
					Downloaded: written,
					Total:      total,
					Version:    version,
				})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	if total > 0 && written != total {
		return fmt.Errorf("downloaded %d bytes, expected %d", written, total)
	}
	return nil
}

// unzip extracts src to dst with three defenses:
//
//   - Slip-zip: every extracted path must stay rooted under dst.
//   - Zip-bomb: per-entry and aggregate decompressed-size caps; an
//     archive that exceeds either is rejected before consuming the
//     overflowing bytes (the LimitReader is +1 to detect overflow).
//   - Symlink: zip entries with the symlink mode bit are skipped;
//     extracting a symlink would let later operations (open, chmod,
//     remove) follow a link out of dst even with the path-prefix
//     guard satisfied at extraction time.
//
// File modes from the zip have non-permission bits (setuid/setgid/
// sticky) masked off; we never need to preserve them and they invite
// privilege-escalation surface a malicious archive could exploit.
func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	var aggregate int64
	for _, file := range zr.File {
		// Reject path-traversal even before resolving the join — a
		// .. or absolute-path entry shouldn't reach the OS at all.
		if filepath.IsAbs(file.Name) || strings.Contains(file.Name, "..") {
			return fmt.Errorf("zip slip: %s", file.Name)
		}
		fp := filepath.Join(dstAbs, file.Name)
		if !strings.HasPrefix(fp, dstAbs+string(os.PathSeparator)) && fp != dstAbs {
			return fmt.Errorf("zip slip: %s", file.Name)
		}
		// Skip symlink entries. A future change that switches to
		// extracting them would break the path-traversal guarantee
		// — chrome-for-testing's archives don't use symlinks, so
		// rejection is the safe default.
		if file.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(fp, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			return err
		}
		written, err := extractZipFile(file, fp, zipMaxFileBytes)
		if err != nil {
			return err
		}
		aggregate += written
		if aggregate > zipMaxAggregateBytes {
			return fmt.Errorf("zip aggregate size exceeds %d bytes", zipMaxAggregateBytes)
		}
	}
	return nil
}

// extractZipFile writes one entry to dst, capped at maxBytes
// uncompressed. Returns the number of bytes written so the caller
// can track aggregate size.
func extractZipFile(file *zip.File, dst string, maxBytes int64) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	// Mask non-permission bits (setuid, setgid, sticky) and clamp to
	// 0o755 — chrome-for-testing zips contain only regular files
	// with the executable bit, and we don't need to preserve the
	// rest.
	mode := file.Mode().Perm() & 0o755
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	// LimitReader at maxBytes+1 so we can detect overflow rather
	// than silently truncate.
	written, copyErr := io.Copy(out, io.LimitReader(rc, maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	if written > maxBytes {
		return written, fmt.Errorf("zip entry %q exceeds %d bytes", file.Name, maxBytes)
	}
	if mode&0o111 != 0 && runtime.GOOS != "windows" {
		_ = os.Chmod(dst, mode|0o100)
	}
	return written, nil
}

// currentPlatform returns the Chrome-for-Testing platform string for
// the running OS+arch.
func currentPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "darwin/arm64":
		return "mac-arm64", nil
	case "windows/amd64":
		return "win64", nil
	default:
		return "", fmt.Errorf("unsupported platform %s/%s — Chrome-for-Testing has no chrome-headless-shell build", runtime.GOOS, runtime.GOARCH)
	}
}

// binaryPathFor is the canonical post-extraction path under
// versionDir. The Chrome-for-Testing zips for chrome-headless-shell
// always extract to a single subdirectory named after the platform.
func binaryPathFor(versionDir, platform string) string {
	bin := "chrome-headless-shell"
	if platform == "win64" {
		bin = "chrome-headless-shell.exe"
	}
	return filepath.Join(versionDir, "chrome-headless-shell-"+platform, bin)
}

// findHeadlessShell walks versionDir looking for the executable.
// Used as a fallback when the canonical layout shifts (defensive —
// has not been observed in practice as of Chrome 148).
func findHeadlessShell(versionDir, platform string) (string, error) {
	target := "chrome-headless-shell"
	if platform == "win64" {
		target = "chrome-headless-shell.exe"
	}
	var found string
	err := filepath.Walk(versionDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() == target {
			found = path
			return errFoundShell
		}
		return nil
	})
	if errors.Is(err, errFoundShell) {
		return found, nil
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("chrome-headless-shell binary not found under %s", versionDir)
}

// errFoundShell is the sentinel filepath.Walk uses to short-circuit.
var errFoundShell = errors.New("found chrome-headless-shell")

// validateVersionSegment rejects a manifest Version that would
// escape the cache directory or step on its parent. Real Chrome
// versions are dotted decimals (e.g. "139.0.7339.207"); a hostile
// or malformed manifest could send "../etc" or "..\\Windows".
func validateVersionSegment(version string) error {
	if version == "" {
		return fmt.Errorf("screenshot: manifest version is empty")
	}
	if version == "." || version == ".." {
		return fmt.Errorf("screenshot: manifest version is a path placeholder: %q", version)
	}
	if strings.ContainsAny(version, `/\`) {
		return fmt.Errorf("screenshot: manifest version contains path separator: %q", version)
	}
	if strings.Contains(version, "..") {
		return fmt.Errorf("screenshot: manifest version contains traversal: %q", version)
	}
	// Belt and braces: also confirm filepath.Clean wouldn't change
	// the value (catches NUL bytes, leading dots, etc.).
	if filepath.Clean(version) != version {
		return fmt.Errorf("screenshot: manifest version is not a clean path segment: %q", version)
	}
	return nil
}

// assertHTTPS rejects manifest / zip URLs whose scheme isn't HTTPS.
// TLS to googlechromelabs.github.io is the only integrity guarantee
// we have for the headless-shell binary; a downgrade to plain HTTP
// would let a network attacker substitute the archive contents.
func assertHTTPS(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("screenshot: parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("screenshot: refuse non-https url scheme %q", u.Scheme)
	}
	return nil
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		// Trust filename + existence; Windows uses extension-based
		// dispatch and the zip preserves the .exe.
		return strings.HasSuffix(strings.ToLower(path), ".exe")
	}
	return info.Mode()&0o111 != 0
}
