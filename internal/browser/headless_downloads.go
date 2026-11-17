package browser

import (
	"context"
	"sync"

	cdpbrowser "github.com/chromedp/cdproto/browser"
)

// downloadTracker translates the two browser-level download events of one
// browser connection into the seam's vocabulary. A download the Manager
// refuses is cancelled through cancel, on its own goroutine because the
// cancel is a CDP round trip.
//
// Headless Chromium's alone: the hosted engine's downloads are the
// launcher's (internal/webview2host/download.go).
//
// downloadWillBegin and downloadProgress arrive on two subscriptions, and a
// small download can be complete before its start is handled. The Manager
// ignores progress for a download it has not seen start, so the tracker
// keeps each GUID in one of two states: started (progress is forwarded),
// or progress held (the newest progress before the start, forwarded right
// after it). A terminal progress after the start ends the lifecycle and
// nothing is kept. The start and the forward of held progress happen under
// one lock with the progress handler, so no progress slips between them.
//
// Both states are bounded by orderSlack; the oldest GUID is dropped first.
// A dropped started download's later progress is held as if early, which
// the bound then drops in turn. The Manager's own page cancellation still
// settles such a download.
type downloadTracker struct {
	events engineEvents
	cancel func(id string)

	mu        sync.Mutex
	downloads *boundedMap[string, downloadLifecycle]
}

type downloadLifecycle struct {
	started bool
	held    *downloadProgress
}

func newDownloadTracker(events engineEvents, cancel func(id string)) *downloadTracker {
	return &downloadTracker{events: events, cancel: cancel, downloads: newBoundedMap[string, downloadLifecycle](orderSlack)}
}

// refusedDownloadCanceler is the tracker's cancel for a download the Manager
// refused: no caller waits on it, so a failure is logged.
func refusedDownloadCanceler(cancel func(id string) error, logf func(string, ...any)) func(id string) {
	return func(id string) {
		if err := cancel(id); err != nil {
			logf("browser: cancel refused download %s: %v", id, err)
		}
	}
}

func (t *downloadTracker) willBegin(event cdpbrowser.EventDownloadWillBegin) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lifecycle, _ := t.downloads.take(event.GUID)
	if !t.events.DownloadStarted(downloadStart{
		Frame: string(event.FrameID), ID: event.GUID,
		URL: event.URL, SuggestedName: event.SuggestedFilename,
	}) {
		go t.cancel(event.GUID)
	}
	if held := lifecycle.held; held != nil {
		t.events.DownloadProgress(*held)
		if downloadSettled(held.State) {
			return
		}
	}
	t.downloads.put(event.GUID, downloadLifecycle{started: true})
}

func (t *downloadTracker) progress(event cdpbrowser.EventDownloadProgress) {
	progress := cdpDownloadProgress(event)
	t.mu.Lock()
	defer t.mu.Unlock()
	lifecycle, _ := t.downloads.get(event.GUID)
	if !lifecycle.started {
		t.downloads.put(event.GUID, downloadLifecycle{held: &progress})
		return
	}
	t.events.DownloadProgress(progress)
	if downloadSettled(progress.State) {
		t.downloads.delete(event.GUID)
	}
}

// retained reports how many downloads the tracker keeps.
func (t *downloadTracker) retained() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.downloads.len()
}

func downloadSettled(state string) bool {
	return state == downloadCompleted || state == downloadCanceled
}

func cdpDownloadProgress(event cdpbrowser.EventDownloadProgress) downloadProgress {
	state := downloadInProgress
	switch event.State {
	case cdpbrowser.DownloadProgressStateCompleted:
		state = downloadCompleted
	case cdpbrowser.DownloadProgressStateCanceled:
		state = downloadCanceled
	}
	return downloadProgress{ID: event.GUID, Received: event.ReceivedBytes, Total: event.TotalBytes, State: state, FilePath: event.FilePath}
}

// subscribe connects the tracker to the download events of the browser of
// ctx.
func (t *downloadTracker) subscribe(ctx context.Context, logf func(string, ...any)) error {
	if err := onBrowserEvent(ctx, cdpbrowser.DownloadWillBegin, logf, t.willBegin); err != nil {
		return err
	}
	return onBrowserEvent(ctx, cdpbrowser.DownloadProgress, logf, t.progress)
}
