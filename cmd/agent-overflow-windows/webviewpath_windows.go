//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// prepareWebviewStorage validates and creates every directory handed to
// WebView2 before Wails constructs the application. A junction is a reparse
// point even when os.Lstat does not report it as a symlink, so both checks are
// required. Empty paths are fatal rather than allowing WebView2 to choose a
// shared default profile.
func prepareWebviewStorage(mode string) error {
	profile := webviewDataDir(mode)
	forensics := renderForensicsDir(mode)
	if strings.TrimSpace(profile) == "" || strings.TrimSpace(forensics) == "" {
		return errors.New("AppData did not resolve to private WebView2 profile and forensics directories")
	}
	for _, path := range []string{profile, forensics} {
		if err := validateWindowsStoragePath(path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := validateWindowsStoragePath(path); err != nil {
			return err
		}
	}
	return nil
}

func validateWindowsStoragePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty WebView2 storage path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve WebView2 storage path %q: %w", path, err)
	}
	current := filepath.Clean(abs)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlinked WebView2 storage component %s", current)
			}
			attrs, attrErr := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
			if attrErr != nil {
				return fmt.Errorf("inspect WebView2 storage component %s: %w", current, attrErr)
			}
			if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("refusing reparse-point WebView2 storage component %s", current)
			}
			parent := filepath.Dir(current)
			if parent == current {
				return nil
			}
			current = parent
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect WebView2 storage component %s: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}
