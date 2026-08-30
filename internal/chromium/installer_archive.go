package chromium

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"agent-overflow/internal/headlessshell"
)

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
	return headlessshell.Platform()
}

// binaryPathFor is the canonical post-extraction path under
// versionDir. The Chrome-for-Testing zips for chrome-headless-shell
// always extract to a single subdirectory named after the platform.
func binaryPathFor(versionDir, platform string, artifact Artifact) string {
	if artifact == ArtifactHeadlessShell {
		return headlessshell.BinaryPath(versionDir, platform)
	}
	switch platform {
	case "mac-x64", "mac-arm64":
		return filepath.Join(versionDir, "chrome-"+platform, "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
	case "win64":
		return filepath.Join(versionDir, "chrome-win64", "chrome.exe")
	default:
		return filepath.Join(versionDir, "chrome-"+platform, "chrome")
	}
}

// findHeadlessShell walks versionDir looking for the executable.
// Used as a fallback when the canonical layout shifts (defensive —
// has not been observed in practice as of Chrome 148).
func findBrowserBinary(versionDir, platform string, artifact Artifact) (string, error) {
	target := "chrome"
	if artifact == ArtifactHeadlessShell {
		target = headlessshell.BinaryName(platform)
	} else if platform == "win64" {
		target += ".exe"
	} else if strings.HasPrefix(platform, "mac-") {
		target = "Google Chrome for Testing"
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
	return "", fmt.Errorf("%s binary not found under %s", artifact, versionDir)
}

// errFoundShell is the sentinel filepath.Walk uses to short-circuit.
var errFoundShell = errors.New("found chrome-headless-shell")

// validateVersionSegment rejects a manifest Version that would
// escape the cache directory or step on its parent. Real Chrome
// versions are dotted decimals (e.g. "139.0.7339.207"); a hostile
// or malformed manifest could send "../etc" or "..\\Windows".
func validateVersionSegment(version string) error {
	if version == "" {
		return fmt.Errorf("chromium: manifest version is empty")
	}
	if version == "." || version == ".." {
		return fmt.Errorf("chromium: manifest version is a path placeholder: %q", version)
	}
	if strings.ContainsAny(version, `/\`) {
		return fmt.Errorf("chromium: manifest version contains path separator: %q", version)
	}
	if strings.Contains(version, "..") {
		return fmt.Errorf("chromium: manifest version contains traversal: %q", version)
	}
	// Belt and braces: also confirm filepath.Clean wouldn't change
	// the value (catches NUL bytes, leading dots, etc.).
	if filepath.Clean(version) != version {
		return fmt.Errorf("chromium: manifest version is not a clean path segment: %q", version)
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
		return fmt.Errorf("chromium: parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("chromium: refuse non-https url scheme %q", u.Scheme)
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
