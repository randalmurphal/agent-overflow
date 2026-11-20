//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
)

// A WKWebView page's own operations (wkwebview_page_darwin.go): find in
// page, the viewport and screenshots. The operations it shares with
// WebKitGTK are nativePageOps (native_page_ops.go).

var _ pageFind = (*wkPage)(nil)

// FindMode is the shared find bar: WKWebView has no find UI on macOS.
func (p *wkPage) FindMode() string { return FindModeBar }

// FindStart searches from the top: WKWebView searches from the selection, so
// the selection is dropped first, from an isolated world
// (webkitClearSelectionScript). WKFindResult has no count.
func (p *wkPage) FindStart(ctx context.Context, query string) (findResult, error) {
	if _, err := p.evalIn(ctx, webkitFindWorld, webkitClearSelectionScript); err != nil {
		return findResult{}, err
	}
	p.mu.Lock()
	p.findQuery = query
	p.mu.Unlock()
	return p.find(ctx, query, false)
}

// FindStep searches again from the selected match.
func (p *wkPage) FindStep(ctx context.Context, backwards bool) (findResult, error) {
	p.mu.Lock()
	query := p.findQuery
	p.mu.Unlock()
	if query == "" {
		return findResult{}, fmt.Errorf("browser: no find is in progress on this page")
	}
	return p.find(ctx, query, backwards)
}

// FindStop drops the selection that marks the match, from an isolated
// world (webkitClearSelectionScript): frames window.frames reaches in the
// call, and every frame told to clear, down from the top, and not waited
// for. If WebKit refuses the frame script (ao_add_find_frame_script), a
// frame the call cannot reach keeps its selection.
func (p *wkPage) FindStop(ctx context.Context) error {
	p.mu.Lock()
	p.findQuery = ""
	p.mu.Unlock()
	_, err := p.evalIn(ctx, webkitFindWorld, webkitClearSelectionScript)
	return err
}

func (p *wkPage) find(ctx context.Context, query string, backwards bool) (findResult, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	found, err := wkFind(opCtx, p.liveView(), query, backwards)
	if err != nil || !found {
		return findResult{}, err
	}
	return findResult{Found: true, Matches: -1}, nil
}

// SetViewport sizes the view's bounds: WebKit has no device-metrics override,
// so the page's viewport IS the view's bounds, and the host keeps those bounds
// whether the page is parked or presented scaled in the pane.
func (p *wkPage) SetViewport(_ context.Context, width, height int) error {
	return wkSetViewSize(p.liveView(), width, height)
}

func (p *wkPage) Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	// A clip is expressed in document coordinates (the CDP driver captures it
	// beyond the viewport), so both the clip and the full-page shapes need the
	// whole document; only the plain case is the visible region.
	pixels, width, height, err := p.capture(opCtx, opts.Clip != nil || opts.FullPage)
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
	}
	frame, err := webkitDecodeSnapshot(pixels, width, height)
	if err != nil {
		return nil, err
	}
	if opts.Clip != nil {
		frame = webkitCrop(frame, int(opts.Clip.X), int(opts.Clip.Y), int(opts.Clip.Width), int(opts.Clip.Height))
	} else if opts.FullPage {
		frame = webkitCrop(frame, 0, 0, maxFullScreenshotWidth, maxFullScreenshotHeight)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, frame, &jpeg.Options{Quality: nativeScreenshotQuality}); err != nil {
		return nil, fmt.Errorf("browser: encode screenshot: %w", err)
	}
	return out.Bytes(), nil
}

// capture takes the snapshot. WKSnapshotConfiguration cannot reach past the
// view's own bounds, so a whole-document capture is a bounded resize, one
// snapshot, and a restore.
// Serialized per page, because two captures would otherwise restore each
// other's size.
func (p *wkPage) capture(ctx context.Context, fullDocument bool) ([]byte, int, int, error) {
	if !fullDocument {
		return wkSnapshot(ctx, p.liveView())
	}
	p.snapshotMu.Lock()
	defer p.snapshotMu.Unlock()
	width, height, err := wkViewSize(p.liveView())
	if err != nil {
		return nil, 0, 0, err
	}
	var document struct{ Width, Height int }
	if err := p.evalInto(ctx, wkDocumentSizeScript, &document); err != nil {
		return nil, 0, 0, err
	}
	target := clampDocumentCapture(document.Width, document.Height, width, height)
	if target.width == width && target.height == height {
		return wkSnapshot(ctx, p.liveView())
	}
	if err := wkSetViewSize(p.liveView(), target.width, target.height); err != nil {
		return nil, 0, 0, err
	}
	pixels, capturedWidth, capturedHeight, err := wkSnapshot(ctx, p.liveView())
	// The restore runs whether or not the capture worked: a page left at the
	// document's size would keep reporting that as its viewport forever.
	if restoreErr := wkSetViewSize(p.liveView(), width, height); restoreErr != nil {
		err = errors.Join(err, fmt.Errorf("browser: restore the view size after a capture: %w", restoreErr))
	}
	return pixels, capturedWidth, capturedHeight, err
}
