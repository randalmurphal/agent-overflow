package browser

import (
	"context"
	"sync"
	"time"

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
// A start whose frame resolves to no popup and that the Manager refuses may
// be a popup's whose creation is not handled yet: with a lifetime, it is
// offered again, on a goroutine of its own, once popup names its frame,
// for up to ownerWait, and refused after. Its progress is held meanwhile.
//
// Both states are bounded by orderSlack; the oldest GUID is dropped first.
// A dropped started download's later progress is held as if early, which
// the bound then drops in turn. The Manager's own page cancellation still
// settles such a download.
type downloadTracker struct {
	events engineEvents
	cancel func(id string)
	// popup answers, for a download's frame, the popup the frame is the
	// main frame of, as downloadStart's Popup, Openers and Profile, or an
	// empty start. Nil names none.
	popup func(frame string) downloadStart
	// lifetime is the connection's, which ends a start's wait for its
	// popup; nil waits for none. ownerWait bounds the wait, requestOwnerWait
	// when zero.
	lifetime  context.Context
	ownerWait time.Duration

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
	start := t.startLocked(event)
	admitted := t.events.DownloadStarted(start)
	if !admitted && start.Popup == "" && t.lifetime != nil {
		go t.awaitPopup(event)
		return
	}
	t.beganLocked(event.GUID, admitted)
}

// startLocked is the start event names. The caller holds t.mu.
func (t *downloadTracker) startLocked(event cdpbrowser.EventDownloadWillBegin) downloadStart {
	var start downloadStart
	if t.popup != nil {
		start = t.popup(string(event.FrameID))
	}
	start.Frame, start.ID, start.URL, start.SuggestedName = string(event.FrameID), event.GUID, event.URL, event.SuggestedFilename
	return start
}

// beganLocked settles a start the Manager answered: a refused one is
// cancelled, and the progress held for it is forwarded. The caller holds
// t.mu.
func (t *downloadTracker) beganLocked(guid string, admitted bool) {
	if !admitted {
		go t.cancel(guid)
	}
	lifecycle, _ := t.downloads.take(guid)
	if held := lifecycle.held; held != nil {
		t.events.DownloadProgress(*held)
		if downloadSettled(held.State) {
			return
		}
	}
	t.downloads.put(guid, downloadLifecycle{started: true})
}

// awaitPopup offers a refused start again once popup names its frame, and
// refuses it when ownerWait passes first or the connection ends.
func (t *downloadTracker) awaitPopup(event cdpbrowser.EventDownloadWillBegin) {
	wait := t.ownerWait
	if wait <= 0 {
		wait = requestOwnerWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(requestOwnerPoll)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.mu.Lock()
			t.beganLocked(event.GUID, false)
			t.mu.Unlock()
			return
		case <-t.lifetime.Done():
			return
		}
		t.mu.Lock()
		if start := t.startLocked(event); start.Popup != "" {
			t.beganLocked(event.GUID, t.events.DownloadStarted(start))
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
	}
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
