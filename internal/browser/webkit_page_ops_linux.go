//go:build linux && cgo && !gtk3 && !android && !server && !nogui

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
)

// A WebKitGTK page's operations on its content (webkit_page_linux.go):
// locators and the elements they find, input, find in page, the viewport,
// screenshots and assets, each a body built in webkitjs.go or a WebKit
// call.

func (p *webkitPage) ResolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error) {
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

func (p *webkitPage) ReadNode(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error) {
	fn, err := nodeReadFunction(kind, argument)
	if err != nil {
		return nil, err
	}
	return p.callElement(ctx, locator.Frames, match.Selector, fn)
}

func (p *webkitPage) ActOnNode(ctx context.Context, match LocatorMatch, locator Locator, act nodeAction) error {
	script, err := webkitNodeActScript(locator.Frames, match.Selector, act)
	if err != nil {
		return err
	}
	_, err = p.evalBody(ctx, script)
	return err
}

func (p *webkitPage) ScrollNode(ctx context.Context, ref nodeReference, x, y float64) error {
	_, err := p.callElement(ctx, ref.Frames, ref.Selector, nodeScrollFunction(x, y))
	return err
}

// callElement runs one shared element function against a locator's element and
// bounds the result the way the CDP driver bounds an element call.
func (p *webkitPage) callElement(ctx context.Context, frames []string, selector, fn string) (any, error) {
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

func (p *webkitPage) Click(ctx context.Context, selector string) error {
	_, err := p.evalBody(ctx, webkitSelectorClickScript(selector))
	return err
}

func (p *webkitPage) Type(ctx context.Context, selector, text string, clear bool) error {
	_, err := p.evalBody(ctx, webkitSelectorTypeScript(selector, text, clear))
	return err
}

func (p *webkitPage) Press(ctx context.Context, raw string) error {
	if !chordHasKey(raw) {
		return fmt.Errorf("browser: key is required")
	}
	if _, err := p.evalBody(ctx, webkitFocusedPressScript(raw)); err != nil {
		return fmt.Errorf("browser: press key: %w", err)
	}
	return nil
}

func (p *webkitPage) TypeText(ctx context.Context, text string) error {
	_, err := p.evalBody(ctx, webkitTypeTextScript(text))
	return err
}

func (p *webkitPage) SelectionText(ctx context.Context) string {
	var selected string
	_ = p.evalInto(ctx, webkitSelectionTextScript, &selected)
	return selected
}

// webkitFindMaxMatches is the most matches a search counts and highlights;
// past it the count is reported as capped.
const webkitFindMaxMatches = 1000

var _ pageFind = (*webkitPage)(nil)

// FindMode is the shared find bar: WebKitFindController has no UI of its own.
func (p *webkitPage) FindMode() string { return FindModeBar }

func (p *webkitPage) FindStart(ctx context.Context, query string) (findResult, error) {
	return p.find(ctx, webkitFindSearch, query)
}

func (p *webkitPage) FindStep(ctx context.Context, backwards bool) (findResult, error) {
	op := webkitFindNext
	if backwards {
		op = webkitFindPrevious
	}
	return p.find(ctx, op, "")
}

// FindStop ends the search and drops the selection that marks its match,
// from an isolated world (webkitClearSelectionScript): search_finish clears
// only the highlights. Frames window.frames reaches are cleared in the
// call; every frame is also told to clear, down from the top, and not
// waited for.
func (p *webkitPage) FindStop(ctx context.Context) error {
	if err := gtkFindFinish(p.liveView()); err != nil {
		return err
	}
	_, err := p.evalIn(ctx, webkitFindWorld, webkitClearSelectionScript)
	return err
}

func (p *webkitPage) find(ctx context.Context, op int, query string) (findResult, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	answer, err := gtkFind(opCtx, p.liveView(), op, query, webkitFindMaxMatches)
	if err != nil {
		return findResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case answer.found == 0:
		p.findCount = findResult{}
	case op != webkitFindSearch:
		// A step's found-text carries no total, so the search's count stands.
		if !p.findCount.Found {
			return findResult{Found: true, Matches: -1}, nil
		}
	case answer.matches == webkitFindUnbounded || answer.matches > webkitFindMaxMatches:
		p.findCount = findResult{Found: true, Matches: webkitFindMaxMatches, Capped: true}
	default:
		p.findCount = findResult{Found: true, Matches: int(answer.matches)}
	}
	return p.findCount, nil
}

func (p *webkitPage) Scroll(ctx context.Context, selector string, x, y float64) error {
	_, err := p.evalBody(ctx, webkitScrollScript(selector, x, y))
	return err
}

func (p *webkitPage) Pointer(ctx context.Context, opts PointerOptions) error {
	script, err := webkitPointerScript(opts)
	if err != nil {
		return err
	}
	_, err = p.evalBody(ctx, script)
	return err
}

// SetViewport sizes the view itself: WebKit has no device-metrics override,
// so the page's viewport IS the widget's allocation, and the host keeps that
// allocation whether the page is parked or presented scaled in the pane.
func (p *webkitPage) SetViewport(_ context.Context, width, height int) error {
	return webkitSetViewSize(p.liveView(), width, height)
}

func (p *webkitPage) Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	// A clip is expressed in document coordinates (the CDP driver captures it
	// beyond the viewport), so both the clip and the full-page shapes need the
	// whole document; only the plain case is the visible region.
	pixels, width, height, err := webkitSnapshot(opCtx, p.liveView(), opts.Clip != nil || opts.FullPage)
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
	if err := jpeg.Encode(&out, frame, &jpeg.Options{Quality: webkitScreenshotQuality}); err != nil {
		return nil, fmt.Errorf("browser: encode screenshot: %w", err)
	}
	return out.Bytes(), nil
}

func (p *webkitPage) AssetInventory(ctx context.Context) (pageAssets, error) {
	var raw pageAssets
	if err := p.evalInto(ctx, "return "+assetInventoryExpression()+";", &raw); err != nil {
		return pageAssets{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	return raw, nil
}

// AssetFetcher reads assets THROUGH the page, which is what makes the page's
// own cookies and origin apply. WebKit has no out-of-band resource stream, so
// each asset crosses as base64 in one bounded read rather than as a stream.
func (p *webkitPage) AssetFetcher(ctx context.Context) (assetFetcher, error) {
	return func(url string) (assetStream, error) {
		var payload struct {
			ContentType string `json:"contentType"`
			Base64      string `json:"base64"`
		}
		if err := p.evalInto(ctx, webkitAssetFetchScript(url, webkitMaxInPageAssetBytes), &payload); err != nil {
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
