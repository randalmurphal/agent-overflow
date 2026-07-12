package project

import (
	"fmt"
	"os"
	"path/filepath"
)

// ConfigDir returns the app-config directory reserved for a project slug.
func ConfigDir(configRoot, slug string) string {
	return filepath.Join(configRoot, "projects", slug)
}

// EnsureConfigDir creates and returns the app-config directory for a project.
func EnsureConfigDir(configRoot, slug string) (string, error) {
	dir := ConfigDir(configRoot, slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create project config directory %s: %w", dir, err)
	}
	return dir, nil
}
