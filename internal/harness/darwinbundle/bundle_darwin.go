//go:build darwin

// Package darwinbundle makes the isolated macOS harness executable a distinct
// application bundle. WKWebView keys its default data store by bundle id, so
// the ordinary com.agentoverflow.app bundle must never host an isolated run.
package darwinbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
)

const bundlePrefix = "com.agentoverflow.harness."

var runCodesign = func(appPath string) error {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		return nil // ad-hoc signing is optional on developer machines
	}
	cmd := exec.Command(codesign, "--force", "--deep", "--sign", "-", appPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codesign %s: %w (%s)", appPath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func BundleID(dataRoot string, runID ...string) string {
	canonical, err := instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		canonical = filepath.Clean(dataRoot)
	}
	identity := canonical
	if len(runID) > 0 {
		identity += "\x00" + runID[0]
	}
	digest := sha256.Sum256([]byte(identity))
	return bundlePrefix + hex.EncodeToString(digest[:8])
}

func Create(binary, dataRoot, plistTemplate string, runID ...string) (string, error) {
	if strings.TrimSpace(binary) == "" || strings.TrimSpace(dataRoot) == "" {
		return "", errors.New("darwin bundle: binary and data root are required")
	}
	info, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("darwin bundle: stat binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("darwin bundle: binary is not a regular file: %s", binary)
	}
	if plistTemplate == "" {
		return "", errors.New("darwin bundle: Info.plist template is required")
	}
	plist, err := os.ReadFile(plistTemplate)
	if err != nil {
		return "", fmt.Errorf("darwin bundle: read Info.plist: %w", err)
	}
	id := BundleID(dataRoot, runID...)
	plist, err = patchPlist(plist, id)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(dataRoot)
	if err != nil {
		return "", fmt.Errorf("darwin bundle: resolve data root: %w", err)
	}
	if err := rejectSymlink(root); err != nil {
		return "", err
	}
	root, err = instanceinfo.CanonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("darwin bundle: canonicalize data root: %w", err)
	}
	if configRoot, configErr := os.UserConfigDir(); configErr == nil {
		for _, forbidden := range []string{configRoot, filepath.Join(configRoot, appdirs.DirName)} {
			canonicalForbidden, canonicalErr := instanceinfo.CanonicalPath(forbidden)
			if canonicalErr == nil && root == canonicalForbidden {
				return "", fmt.Errorf("darwin bundle: data root %s is the real app data tree", dataRoot)
			}
		}
	}
	if err := ensureOwnedRoot(root); err != nil {
		return "", err
	}
	appPath := filepath.Join(root, ".ao-webview", id+".app")
	contents := filepath.Join(appPath, "Contents")
	macOSDir := filepath.Join(contents, "MacOS")
	for _, path := range []string{filepath.Dir(appPath), appPath, contents, macOSDir} {
		if err := rejectSymlink(path); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(macOSDir, 0o700); err != nil {
		return "", fmt.Errorf("darwin bundle: create app: %w", err)
	}
	plistPath := filepath.Join(contents, "Info.plist")
	if err := rejectSymlink(plistPath); err != nil {
		return "", err
	}
	if err := os.WriteFile(plistPath, plist, 0o600); err != nil {
		return "", fmt.Errorf("darwin bundle: write Info.plist: %w", err)
	}
	executable := filepath.Join(macOSDir, "agent-overflow")
	if err := copyExecutable(binary, executable, info.Mode().Perm()); err != nil {
		return "", err
	}
	if err := runCodesign(appPath); err != nil {
		return "", err
	}
	return appPath, nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("darwin bundle: inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("darwin bundle: refusing symlinked bundle path %s", path)
	}
	return nil
}

func ensureOwnedRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("darwin bundle: create data root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("darwin bundle: inspect data root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("darwin bundle: data root %s is a symlink", root)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || uint32(stat.Uid) != uint32(os.Getuid()) {
		return fmt.Errorf("darwin bundle: data root %s is not owned by this user", root)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("darwin bundle: data root %s is group- or world-writable", root)
	}
	return nil
}

func copyExecutable(source, destination string, mode fs.FileMode) error {
	if err := rejectSymlink(destination); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("darwin bundle: read executable: %w", err)
	}
	if err := os.WriteFile(destination, data, mode|0o700); err != nil {
		return fmt.Errorf("darwin bundle: write executable: %w", err)
	}
	return nil
}

func patchPlist(data []byte, id string) ([]byte, error) {
	text := string(data)
	key := "<key>CFBundleIdentifier</key>"
	start := strings.Index(text, key)
	if start < 0 {
		return nil, errors.New("darwin bundle: Info.plist has no CFBundleIdentifier")
	}
	valueStart := strings.Index(text[start+len(key):], "<string>")
	if valueStart < 0 {
		return nil, errors.New("darwin bundle: CFBundleIdentifier has no string value")
	}
	valueStart += start + len(key) + len("<string>")
	valueEnd := strings.Index(text[valueStart:], "</string>")
	if valueEnd < 0 {
		return nil, errors.New("darwin bundle: CFBundleIdentifier string is unterminated")
	}
	valueEnd += valueStart
	text = text[:valueStart] + id + text[valueEnd:]
	return []byte(text), nil
}

func Verify(executable, dataRoot, expected string, runID ...string) error {
	if expected == "" {
		return errors.New("darwin bundle: expected bundle id is missing")
	}
	want := BundleID(dataRoot, runID...)
	if expected != want || !strings.HasPrefix(expected, bundlePrefix) {
		return fmt.Errorf("darwin bundle: expected id %q is not the harness id %q", expected, want)
	}
	clean := filepath.Clean(executable)
	marker := string(filepath.Separator) + "Contents" + string(filepath.Separator) + "MacOS" + string(filepath.Separator)
	idx := strings.LastIndex(clean, marker)
	if idx < 0 || !strings.HasSuffix(clean[:idx], ".app") {
		return fmt.Errorf("darwin bundle: executable %q is outside an .app bundle", executable)
	}
	// idx points at the separator before Contents. The .app component is
	// already entirely in clean[:idx]. Adding len(".app") would append the
	// first four bytes of "/Contents" and make every valid bundle unreadable.
	appPath := clean[:idx]
	plist, err := os.ReadFile(filepath.Join(appPath, "Contents", "Info.plist"))
	if err != nil {
		return fmt.Errorf("darwin bundle: read running Info.plist: %w", err)
	}
	actual, err := plistIdentifier(plist)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("darwin bundle: running bundle id %q does not match expected %q", actual, expected)
	}
	if actual == "com.agentoverflow.app" {
		return errors.New("darwin bundle: refusing the production bundle id for an isolated run")
	}
	return nil
}

func plistIdentifier(data []byte) (string, error) {
	text := string(data)
	key := "<key>CFBundleIdentifier</key>"
	start := strings.Index(text, key)
	if start < 0 {
		return "", errors.New("darwin bundle: running Info.plist has no CFBundleIdentifier")
	}
	valueStart := strings.Index(text[start+len(key):], "<string>")
	if valueStart < 0 {
		return "", errors.New("darwin bundle: running bundle id is malformed")
	}
	valueStart += start + len(key) + len("<string>")
	valueEnd := strings.Index(text[valueStart:], "</string>")
	if valueEnd < 0 {
		return "", errors.New("darwin bundle: running bundle id is unterminated")
	}
	return text[valueStart : valueStart+valueEnd], nil
}
