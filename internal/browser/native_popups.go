//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"agent-overflow/internal/browser/ids"
)

// heldPopupView is a popup view a WebKit engine holds until the Manager
// adopts or discards it, with the handle of the page that opened it and
// the page number its adoption gives it.
type heldPopupView struct {
	view   unsafe.Pointer
	opener ids.PageHandle
	n      uint64
}

// heldPopups is a WebKit engine's popup views from their creation until
// the Manager adopts or discards them, and each discarded one until its
// native close has run (a popupRelease, the discard's receipt). Both WebKit
// engines keep theirs here, so a discard answers within its caller's
// context and a later discard of the same popup joins the first, never
// answering done while the close is still queued on the UI thread. Its
// lock covers map bookkeeping only: nothing under it calls the UI thread.
type heldPopups struct {
	mu        sync.Mutex
	held      map[ids.PageHandle]heldPopupView
	releasing map[ids.PageHandle]*popupRelease
}

// popupRelease is the receipt of one popup's discard: done closes once its
// native close has run, and err is then how it ended.
type popupRelease struct {
	done chan struct{}
	err  error
}

// errNativeReleasePending is DiscardPage's answer when its caller's context
// ended before the popup's native close ran on the UI thread; the engine
// still runs it, and a later discard waits for the same close.
var errNativeReleasePending = errors.New("the popup's close is still queued on the UI thread, which runs it")

func newHeldPopups() *heldPopups {
	return &heldPopups{held: make(map[ids.PageHandle]heldPopupView), releasing: make(map[ids.PageHandle]*popupRelease)}
}

// hold keeps view under handle.
func (h *heldPopups) hold(handle ids.PageHandle, view heldPopupView) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[handle] = view
}

// take takes the held popup handle, for its attach; the zero view when it
// is not held (adopted, discarded or never held).
func (h *heldPopups) take(handle ids.PageHandle) heldPopupView {
	h.mu.Lock()
	defer h.mu.Unlock()
	held := h.held[handle]
	delete(h.held, handle)
	return held
}

// opener answers the opener of the held popup handle, "" for none.
func (h *heldPopups) opener(handle ids.PageHandle) ids.PageHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[handle].opener
}

// drain takes every held view, for an engine's stop to close.
func (h *heldPopups) drain() []unsafe.Pointer {
	h.mu.Lock()
	defer h.mu.Unlock()
	views := make([]unsafe.Pointer, 0, len(h.held))
	for handle, held := range h.held {
		views = append(views, held.view)
		delete(h.held, handle)
	}
	return views
}

// discard closes the held popup handle with release, which answers once
// the native close has run, and waits for it within ctx: past ctx it
// answers errNativeReleasePending, and the close still runs, owned by its
// receipt. A discard of a popup whose close is under way waits for that
// one; one of a popup neither held nor being closed has nothing to close.
func (h *heldPopups) discard(ctx context.Context, handle ids.PageHandle, release func(unsafe.Pointer) error) error {
	h.mu.Lock()
	receipt := h.releasing[handle]
	if receipt == nil {
		held, ok := h.held[handle]
		if !ok {
			h.mu.Unlock()
			return nil
		}
		delete(h.held, handle)
		receipt = &popupRelease{done: make(chan struct{})}
		h.releasing[handle] = receipt
		go func() {
			receipt.err = release(held.view)
			h.mu.Lock()
			delete(h.releasing, handle)
			h.mu.Unlock()
			close(receipt.done)
		}()
	}
	h.mu.Unlock()
	select {
	case <-receipt.done:
		return receipt.err
	case <-ctx.Done():
		return fmt.Errorf("browser: discard popup %s: %w: %w", handle, errNativeReleasePending, ctx.Err())
	}
}
