package project

import (
	"path/filepath"
)

// ConfigDir returns the app-config directory reserved for a project slug.
func ConfigDir(configRoot, slug string) string {
	return filepath.Join(configRoot, "projects", slug)
}
