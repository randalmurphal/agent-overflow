//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"errors"
	"fmt"
	"sync"

	"agent-overflow/internal/browser/destination"
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

// stageFor names download id's staged file (stagedFile) and answers it
// with its claim, which release gives up (profileOptions.ReleaseStaged).
func stageFor(stage func(downloadID) (string, error), release func(downloadID), id downloadID) destination.Staged {
	path, err := stagedFile(stage, id)
	if err != nil {
		return destination.Staged{Err: err}
	}
	var give func()
	if release != nil {
		give = func() { release(id) }
	}
	return destination.Staged{Path: path, Claim: destination.NewClaim(give)}
}

// stagedFile names the staged file download id is written to
// (profileOptions.StageDownload). It does file system work, so it runs off
// the UI thread.
func stagedFile(stage func(downloadID) (string, error), id downloadID) (string, error) {
	if stage == nil {
		return "", errDownloadsNotStaged
	}
	return stage(id)
}

// downloadHandle is the engine id of the native download numbered n,
// which its start and every report of it name.
func downloadHandle(n uint64) downloadID {
	return downloadID(fmt.Sprintf("dl-%d", n))
}

// nativeEnds are a profile's downloads whose end the UI thread received
// and whose end report has not returned yet: from WebKit's terminal
// callback, through the queued release and report (downloadEvents), to
// that report's return. A download's start answers EndReported from it.
type nativeEnds struct {
	mu  sync.Mutex
	ids map[downloadID]struct{}
}

// received marks id's end received, on the UI thread, before its report
// is queued.
func (e *nativeEnds) received(id downloadID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ids == nil {
		e.ids = make(map[downloadID]struct{})
	}
	e.ids[id] = struct{}{}
}

// reported clears id's mark once its end report returned.
func (e *nativeEnds) reported(id downloadID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.ids, id)
}

// pending is whether id's end was received and its report has not
// returned. On the UI thread, where the mark is made, it is exact: a
// cancel that finds it there has nothing to cancel, and WebKit is not
// asked to cancel a download it finished.
func (e *nativeEnds) pending(id downloadID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.ids[id]
	return ok
}

// endReported is a download start's EndReported for id.
func (e *nativeEnds) endReported(id downloadID) func() bool {
	return func() bool { return e.pending(id) }
}

// nativeCancel is how a native download's cancel on the UI thread ended.
type nativeCancel int

const (
	// nativeCanceled: the download was found and canceled; its terminal
	// report follows.
	nativeCanceled nativeCancel = iota + 1
	// nativeCancelEnded: the download was released before the cancel ran,
	// finished or failed, and its terminal report is on its way.
	nativeCancelEnded
	// nativeCancelWithdrawn: the UI thread did not run the cancel in time,
	// and it never will.
	nativeCancelWithdrawn
)

// err is CancelDownload's answer for c; unavailable is the engine's error
// for a UI thread that did not run it.
func (c nativeCancel) err(unavailable error) error {
	switch c {
	case nativeCanceled:
		return nil
	case nativeCancelEnded:
		return errDownloadEnded
	case nativeCancelWithdrawn:
		return unavailable
	}
	panic("browser: unknown native cancel outcome")
}
