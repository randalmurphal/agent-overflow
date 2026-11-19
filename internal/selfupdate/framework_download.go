package selfupdate

import (
	"path/filepath"
	"strings"
)

// FrameworkDownloadPrefix names the folder the Wails updater downloads a
// release into: os.MkdirTemp("", "wails-update-*"). The framework removes
// it only in its own Restart and before its next download, so a path that
// installs the release another way removes it itself.
const FrameworkDownloadPrefix = "wails-update-"

// FrameworkDownloadFolder is the ancestor of downloaded directly under
// tempDir, when the framework named it (FrameworkDownloadPrefix). Any other
// location is not the framework's to remove.
func FrameworkDownloadFolder(downloaded, tempDir string) (string, bool) {
	tempDir = filepath.Clean(tempDir)
	for dir := filepath.Dir(filepath.Clean(downloaded)); ; {
		parent := filepath.Dir(dir)
		if parent == tempDir {
			return dir, strings.HasPrefix(filepath.Base(dir), FrameworkDownloadPrefix)
		}
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
