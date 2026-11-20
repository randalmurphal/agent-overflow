//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
)

// A WebKitGTK page's own operations (webkit_page_linux.go): find in page,
// the viewport and screenshots. The operations it shares with WKWebView are
// nativePageOps (native_page_ops.go).

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
	if err := jpeg.Encode(&out, frame, &jpeg.Options{Quality: nativeScreenshotQuality}); err != nil {
		return nil, fmt.Errorf("browser: encode screenshot: %w", err)
	}
	return out.Bytes(), nil
}
