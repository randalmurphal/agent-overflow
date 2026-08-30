package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
)

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.Join(copyErr, closeErr)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileDigest(path string) (string, error) { return hashFile(path) }
func hashBytes(data []byte) string           { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func digestIfPresent(root string, names ...string) (string, error) {
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read %s: %w", filepath.Join(root, name), err)
		}
	}
	return "", nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	ar, _ := filepath.EvalSymlinks(aa)
	br, _ := filepath.EvalSymlinks(bb)
	if ar != "" {
		aa = ar
	}
	if br != "" {
		bb = br
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func pathWithin(path, root string) bool {
	path = canonicalForCompare(path)
	root = canonicalForCompare(root)
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalForCompare(path string) string {
	resolved, err := instanceinfo.CanonicalPath(path)
	if err == nil {
		return resolved
	}
	return instanceinfo.NormalizeSystemPath(path)
}

func samePathOrAncestor(path, root string) bool {
	if samePath(path, root) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func refuseSymlinkPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve source %s: %w", path, err)
	}
	abs = instanceinfo.NormalizeSystemPath(abs)
	for current := abs; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("compare prepare refuses symlinked source path %s", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func safeRelative(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// validateRunPaths validates every path Run may create before it loads the
// capsule or creates the first disposable leg. Missing directories are
// allowed, but their existing ancestor and every existing component must be
// a real directory, not a link.
func validateRunPaths(capsule, base, report string) (string, string, error) {
	capsule, err := filepath.Abs(capsule)
	if err != nil {
		return "", "", fmt.Errorf("resolve capsule path: %w", err)
	}
	capsuleDir := filepath.Dir(capsule)
	if err := refuseSymlinkPath(capsule); err != nil {
		return "", "", err
	}
	base, err = resolvedRunDirectory(base)
	if err != nil {
		return "", "", fmt.Errorf("compare base directory: %w", err)
	}
	if err := refuseRealComparePath(base, "base directory"); err != nil {
		return "", "", err
	}
	if samePathOrAncestor(base, capsuleDir) {
		return "", "", fmt.Errorf("compare base directory %s is inside immutable capsule %s", base, capsuleDir)
	}
	if report == "" {
		return base, "", nil
	}
	report, err = filepath.Abs(report)
	if err != nil {
		return "", "", fmt.Errorf("resolve compare report: %w", err)
	}
	if err := validateRunFilePath(report); err != nil {
		return "", "", fmt.Errorf("compare report: %w", err)
	}
	if err := refuseRealComparePath(report, "report"); err != nil {
		return "", "", err
	}
	if samePathOrAncestor(report, capsuleDir) {
		return "", "", fmt.Errorf("compare report %s is inside immutable capsule %s", report, capsuleDir)
	}
	return base, report, nil
}

func resolvedRunDirectory(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = os.TempDir()
	}
	path, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	path = instanceinfo.NormalizeSystemPath(path)
	if err := validateRunDirectory(path); err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func validateRunDirectory(path string) error {
	if err := refuseSymlinkPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("path is not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	for {
		info, statErr := os.Lstat(parent)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("missing path has no safe directory ancestor")
			}
			return nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		next := filepath.Dir(parent)
		if next == parent {
			return errors.New("path has no existing directory ancestor")
		}
		parent = next
	}
}

func validateRunFilePath(path string) error {
	if err := refuseSymlinkPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("path is not a regular file")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return validateRunDirectory(filepath.Dir(path))
}

func refuseRealComparePath(path, label string) error {
	root, err := appdirs.Root()
	if err != nil {
		return fmt.Errorf("resolve real app data root before compare %s: %w", label, err)
	}
	configRoot := filepath.Dir(root)
	if pathWithin(path, root) || pathWithin(root, path) || pathWithin(path, configRoot) {
		return fmt.Errorf("compare %s %s overlaps the real app data root %s", label, path, root)
	}
	return nil
}
