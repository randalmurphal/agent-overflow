//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"errors"

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
func stageFor(stage func(string) (string, error), release func(string), id string) destination.Staged {
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
func stagedFile(stage func(string) (string, error), id string) (string, error) {
	if stage == nil {
		return "", errDownloadsNotStaged
	}
	return stage(id)
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
