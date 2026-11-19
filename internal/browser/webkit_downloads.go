//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import "errors"

// A WebKit download's destination is decided in two halves: the engine's
// UI thread hands it over and leaves the decision open, staging names its
// file off that thread (stagedFile), and the UI thread completes the
// decision and reports the start.

var (
	errStagedProfileClosed     = errors.New("browser: workspace profile is closed")
	errDownloadFailedUndecided = errors.New("browser: the download failed before its file was chosen")
	errDownloadsNotStaged      = errors.New("browser: this profile stages no downloads")
)

// stagedFile names the staged file download id is written to
// (profileOptions.StageDownload). It does file system work, so it runs off
// the UI thread.
func stagedFile(stage func(string) (string, error), id string) (string, error) {
	if stage == nil {
		return "", errDownloadsNotStaged
	}
	return stage(id)
}
