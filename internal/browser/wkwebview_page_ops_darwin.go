//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"strings"
	"time"
)

// A WKWebView page's operations on its content (wkwebview_page_darwin.go):
// locators and the elements they find, input, find in page, the viewport,
// screenshots and assets, each a body built in webkitjs.go or a WebKit
// call.

func (p *wkPage) ResolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error) {
	raw, err := p.evalBody(ctx, webkitLocatorResolveScript(locator, attribute))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("locator returned no result")
	}
	if len(raw) > maxLocatorResultBytes {
		return nil, fmt.Errorf("locator result exceeds %d bytes", maxLocatorResultBytes)
	}
	var matches []LocatorMatch
	if err := json.Unmarshal(raw, &matches); err != nil {
		return nil, fmt.Errorf("decode matches: %w", err)
	}
	for i := range matches {
		matches[i].FrameDepth = len(locator.Frames)
	}
	return matches, nil
}

func (p *wkPage) ReadNode(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error) {
	fn, err := nodeReadFunction(kind, argument)
	if err != nil {
		return nil, err
	}
	return p.callElement(ctx, locator.Frames, match.Selector, fn)
}

func (p *wkPage) ActOnNode(ctx context.Context, match LocatorMatch, locator Locator, act nodeAction) error {
	script, err := webkitNodeActScript(locator.Frames, match.Selector, act)
	if err != nil {
		return err
	}
	_, err = p.evalBody(ctx, script)
	return err
}

func (p *wkPage) ScrollNode(ctx context.Context, ref nodeReference, x, y float64) error {
	_, err := p.callElement(ctx, ref.Frames, ref.Selector, nodeScrollFunction(x, y))
	return err
}

// callElement runs one shared element function against a locator's element and
// bounds the result the way the CDP driver bounds an element call.
func (p *wkPage) callElement(ctx context.Context, frames []string, selector, fn string) (any, error) {
	raw, err := p.evalBody(ctx, webkitElementCallScript(frames, selector, fn))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxEvaluateBytes {
		return nil, fmt.Errorf("browser: element result exceeds %d bytes", maxEvaluateBytes)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (p *wkPage) Click(ctx context.Context, selector string) error {
	_, err := p.evalBody(ctx, webkitSelectorClickScript(selector))
	return err
}

func (p *wkPage) Type(ctx context.Context, selector, text string, clear bool) error {
	_, err := p.evalBody(ctx, webkitSelectorTypeScript(selector, text, clear))
	return err
}

func (p *wkPage) Press(ctx context.Context, raw string) error {
	if !chordHasKey(raw) {
		return fmt.Errorf("browser: key is required")
	}
	if _, err := p.evalBody(ctx, webkitFocusedPressScript(raw)); err != nil {
		return fmt.Errorf("browser: press key: %w", err)
	}
	return nil
}

func (p *wkPage) TypeText(ctx context.Context, text string) error {
	_, err := p.evalBody(ctx, webkitTypeTextScript(text))
	return err
}

func (p *wkPage) SelectionText(ctx context.Context) string {
	var selected string
	_ = p.evalInto(ctx, webkitSelectionTextScript, &selected)
	return selected
}

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

func (p *wkPage) Scroll(ctx context.Context, selector string, x, y float64) error {
	_, err := p.evalBody(ctx, webkitScrollScript(selector, x, y))
	return err
}

func (p *wkPage) Pointer(ctx context.Context, opts PointerOptions) error {
	script, err := webkitPointerScript(opts)
	if err != nil {
		return err
	}
	_, err = p.evalBody(ctx, script)
	return err
}

// WaitVisible polls, because WebKit has no wait primitive. The Manager owns the
// deadline; this only samples until it is reached.
func (p *wkPage) WaitVisible(ctx context.Context, selector string) error {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	script := webkitVisibleScript(selector)
	ticker := time.NewTicker(wkPollInterval)
	defer ticker.Stop()
	for {
		var visible bool
		if err := p.evalInto(opCtx, script, &visible); err == nil && visible {
			return nil
		}
		select {
		case <-opCtx.Done():
			return opCtx.Err()
		case <-ticker.C:
		}
	}
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
	if err := jpeg.Encode(&out, frame, &jpeg.Options{Quality: wkScreenshotQuality}); err != nil {
		return nil, fmt.Errorf("browser: encode screenshot: %w", err)
	}
	return out.Bytes(), nil
}

// capture takes the snapshot. WKSnapshotConfiguration cannot reach past the
// view's own bounds — unlike WebKitGTK's FULL_DOCUMENT region — so a
// whole-document capture is a bounded resize, one snapshot, and a restore.
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
	if restoreErr := wkSetViewSize(p.liveView(), width, height); err == nil {
		err = restoreErr
	}
	return pixels, capturedWidth, capturedHeight, err
}

func (p *wkPage) AssetInventory(ctx context.Context) (pageAssets, error) {
	var raw pageAssets
	if err := p.evalInto(ctx, "return "+assetInventoryExpression()+";", &raw); err != nil {
		return pageAssets{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	return raw, nil
}

// AssetFetcher reads assets THROUGH the page, which is what makes the page's own
// cookies and origin apply. WebKit has no out-of-band resource stream, so each
// asset crosses as base64 in one bounded read rather than as a stream.
func (p *wkPage) AssetFetcher(ctx context.Context) (assetFetcher, error) {
	return func(target string) (assetStream, error) {
		var payload struct {
			ContentType string `json:"contentType"`
			Base64      string `json:"base64"`
		}
		if err := p.evalInto(ctx, webkitAssetFetchScript(target, wkMaxInPageAssetBytes), &payload); err != nil {
			return assetStream{}, err
		}
		data, err := base64.StdEncoding.DecodeString(payload.Base64)
		if err != nil {
			return assetStream{}, fmt.Errorf("decode asset: %w", err)
		}
		return assetStream{
			ContentType: strings.TrimSpace(payload.ContentType),
			Copy: func(out io.Writer, perFile, remaining int64) (int64, error) {
				if int64(len(data)) > perFile || int64(len(data)) > remaining {
					return 0, fmt.Errorf("browser: asset exceeds bundle size limit")
				}
				written, err := out.Write(data)
				return int64(written), err
			},
			Close: func() { data = nil },
		}, nil
	}, nil
}
