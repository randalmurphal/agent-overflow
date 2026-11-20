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

// releaseLocked moves the held popup handle to a new receipt and answers
// it; the caller runs the close of its view and settles the receipt
// (settle). The caller holds h.mu.
func (h *heldPopups) releaseLocked(handle ids.PageHandle) *popupRelease {
	delete(h.held, handle)
	receipt := &popupRelease{done: make(chan struct{})}
	h.releasing[handle] = receipt
	return receipt
}

// settle records how the close of handle's view ended, err, on its
// receipt, which every discard waiting on it then answers.
func (h *heldPopups) settle(handle ids.PageHandle, receipt *popupRelease, err error) {
	receipt.err = err
	h.mu.Lock()
	delete(h.releasing, handle)
	h.mu.Unlock()
	close(receipt.done)
}

// await answers receipt's outcome, or within ctx that handle's close is
// still pending.
func (h *heldPopups) await(ctx context.Context, handle ids.PageHandle, receipt *popupRelease) error {
	select {
	case <-receipt.done:
		return receipt.err
	case <-ctx.Done():
		return fmt.Errorf("browser: discard popup %s: %w: %w", handle, errNativeReleasePending, ctx.Err())
	}
}

// discard closes the held popup handle with release, which answers once
// the native close has run, and waits for it within ctx: past ctx it
// answers errNativeReleasePending, and the close still runs, owned by its
// receipt. A discard of a popup whose close is under way waits for that
// one, an engine's stop's included (closeAll); one of a popup neither held
// nor being closed has nothing to close.
func (h *heldPopups) discard(ctx context.Context, handle ids.PageHandle, release func(unsafe.Pointer) error) error {
	h.mu.Lock()
	receipt := h.releasing[handle]
	if receipt == nil {
		held, ok := h.held[handle]
		if !ok {
			h.mu.Unlock()
			return nil
		}
		receipt = h.releaseLocked(handle)
		go func() { h.settle(handle, receipt, release(held.view)) }()
	}
	h.mu.Unlock()
	return h.await(ctx, handle, receipt)
}

// closeAll closes every popup still held, for an engine's stop, each
// through the receipt a discard of it joins, all moved there in one hold,
// so a discard made meanwhile waits for the same close and never answers
// before it. On the UI thread (here) the closes run in the caller, one
// after another, inline; elsewhere each runs as a discard's does and is
// waited for within ctx. It also waits within ctx for the closes discards
// began, and answers every close's error joined.
func (h *heldPopups) closeAll(ctx context.Context, release func(unsafe.Pointer) error, here bool) error {
	type closing struct {
		handle  ids.PageHandle
		view    unsafe.Pointer
		receipt *popupRelease
	}
	h.mu.Lock()
	began := make(map[ids.PageHandle]*popupRelease, len(h.releasing))
	for handle, receipt := range h.releasing {
		began[handle] = receipt
	}
	closes := make([]closing, 0, len(h.held))
	for handle, held := range h.held {
		closes = append(closes, closing{handle, held.view, h.releaseLocked(handle)})
	}
	h.mu.Unlock()
	var errs []error
	for _, c := range closes {
		if here {
			h.settle(c.handle, c.receipt, release(c.view))
		} else {
			go func() { h.settle(c.handle, c.receipt, release(c.view)) }()
		}
	}
	for _, c := range closes {
		errs = append(errs, h.await(ctx, c.handle, c.receipt))
	}
	if !here {
		for handle, receipt := range began {
			errs = append(errs, h.await(ctx, handle, receipt))
		}
	}
	return errors.Join(errs...)
}
