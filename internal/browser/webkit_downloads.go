//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"errors"
	"sync"
)

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

// runOrWithdraw runs fn on the UI thread through do (gtkDo, wkDo) and
// answers whether it ran. Unlike do's, a false answer means fn never runs:
// a dispatch that times out before fn starts withdraws it, and one that
// times out while fn runs waits for fn to end and answers true. It is for
// a download action whose answer the caller reports (a cancel, a sample),
// so an error is never followed by the action, and fn's results are read
// only after it ran.
func runOrWithdraw(do func(func()) bool, fn func()) bool {
	var mu sync.Mutex
	started, withdrawn := false, false
	finished := make(chan struct{})
	if do(func() {
		mu.Lock()
		if withdrawn {
			mu.Unlock()
			return
		}
		started = true
		mu.Unlock()
		defer close(finished)
		fn()
	}) {
		return true
	}
	mu.Lock()
	if !started {
		withdrawn = true
		mu.Unlock()
		return false
	}
	mu.Unlock()
	<-finished
	return true
}
