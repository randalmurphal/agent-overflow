package screenshot

import (
	"context"
	"fmt"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// MaxCaptureHeightPx caps the y-extent of the captured rectangle.
// Without this, an accidental infinite-scroll or adversarial page
// would let chromedp.FullScreenshot return a multi-megabyte PNG that
// decodes to gigabytes of RGBA in image/png. The cap is sized at
// DefaultMaxTiles × DefaultTileHeight so a normal capture is never
// truncated by the cap; oversized pages get the same trailing
// "clipped" treatment they'd get from the slicer's MaxTiles ceiling.
const MaxCaptureHeightPx = DefaultMaxTiles * DefaultTileHeight

// runCapture executes the capture pipeline against an already-bound
// chromedp.Context. The sequence is:
//
//  1. Emulation.setDeviceMetricsOverride to the requested viewport.
//     This pins the rendering width so the captured screenshot is
//     the natural width × document-height rectangle — instead of
//     whatever the headless browser's default viewport happens to
//     be (1280×720 on most builds, but we don't trust that across
//     Chrome rolls).
//
//  2. Page.navigate + WaitReady("body") — chromedp handles
//     loadEventFired internally; WaitReady is the doc.body sentinel
//     so we don't proceed before the DOM is at least minimally
//     present.
//
//  3. settleDocument: race document.fonts.ready against a 4 s soft
//     cap, scroll once to the bottom + back to settle
//     IntersectionObserver-driven content (lazy images, late-loaded
//     chart libraries, etc), then a 2× rAF for paint to land. The
//     cap exists because a cold-cache fetch of variable fonts can
//     hang fonts.ready longer than the agent's per-tool timeout;
//     FOUT in the screenshot is preferable to a canceled tool call.
//
//  4. captureWithHeightCap reads the document height, clamps it to
//     MaxCaptureHeightPx, and issues Page.captureScreenshot with an
//     explicit clip. The clip is the height-bound around an
//     adversarial / accidental infinite-scroll page; without it the
//     resulting PNG decodes to gigabytes of RGBA on the slicer's
//     image/png pass.
//
// PNG-then-JPEG (the slicer converts to JPEG) keeps fidelity higher
// than asking Chrome for JPEG directly at the cost of a few hundred
// KB between Chrome and us.
func runCapture(ctx context.Context, opts CaptureOptions) ([]byte, error) {
	var pngBytes []byte
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(opts.ViewportWidth), int64(opts.ViewportHeight), opts.DeviceScaleFactor, false),
		chromedp.Navigate(opts.URL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		settleDocument(),
		captureWithHeightCap(&pngBytes, opts.ViewportWidth),
	)
	if err != nil {
		return nil, fmt.Errorf("screenshot: capture %s: %w", opts.URL, err)
	}
	if len(pngBytes) == 0 {
		return nil, fmt.Errorf("screenshot: capture %s returned 0 bytes", opts.URL)
	}
	return pngBytes, nil
}

// captureWithHeightCap measures the rendered document height in
// css px, clamps it to MaxCaptureHeightPx, and captures a clipped
// PNG. The clamp is the load-bearing defense against unbounded
// captures — a page with body{height:100000px} would otherwise
// decode to a multi-gigabyte RGBA image inside the slicer.
func captureWithHeightCap(out *[]byte, viewportWidth int) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var height int64
		if err := chromedp.Run(ctx,
			chromedp.Evaluate(
				`Math.max(document.documentElement.scrollHeight || 0, document.body ? document.body.scrollHeight : 0)`,
				&height,
			),
		); err != nil {
			return fmt.Errorf("measure height: %w", err)
		}
		if height <= 0 {
			height = int64(DefaultTileHeight)
		}
		if height > MaxCaptureHeightPx {
			height = MaxCaptureHeightPx
		}
		buf, err := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(true).
			WithFromSurface(true).
			WithClip(&page.Viewport{
				X:      0,
				Y:      0,
				Width:  float64(viewportWidth),
				Height: float64(height),
				Scale:  1,
			}).
			Do(ctx)
		if err != nil {
			return err
		}
		*out = buf
		return nil
	})
}

// settleDocument races document.fonts.ready against a soft cap,
// scrolls the document once end-to-end to fire IntersectionObserver-
// driven loaders, then rAF×2 to land paints. Wrapped in a try/catch
// so capture proceeds even if a page lacks document.fonts (some
// sandboxed iframes do).
//
// Why the soft cap on fonts.ready: a cold-cache fetch of variable
// fonts (e.g. Fraunces with all four axes — ~50 KB of font binary,
// or any Google Fonts CSS that needs a separate woff2 round-trip)
// can take many seconds. document.fonts.ready hangs until every
// declared font finishes loading, with no surfaced timeout. The
// agent's MCP client has its own per-call timeout — typically much
// shorter than that — so an unbounded fonts wait gets the entire
// capture canceled. We'd rather render the page with whatever fonts
// have loaded by the cap (FOUT visible if a font is still in
// flight) than ship the agent a tool error.
func settleDocument() chromedp.Action {
	// fontsReadyMaxWaitMs caps the document.fonts.ready race. Mirrored
	// in the JS expression below; bump both if you change one. 4 s is
	// long enough for Google Fonts on a normal connection, short
	// enough to fit comfortably under typical per-tool agent
	// timeouts.
	const expr = `(async () => {
  const FONTS_READY_TIMEOUT_MS = 4000;
  try {
    if (document.fonts && typeof document.fonts.ready?.then === 'function') {
      await Promise.race([
        document.fonts.ready,
        new Promise((r) => setTimeout(r, FONTS_READY_TIMEOUT_MS)),
      ]);
    }
  } catch (_) {}
  // Scroll-to-bottom + back. Triggers IntersectionObserver-driven
  // lazy content; the back-scroll keeps the post-screenshot page
  // state visually unchanged in case the user opens DevTools.
  try {
    window.scrollTo(0, document.documentElement.scrollHeight || 0);
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    window.scrollTo(0, 0);
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  } catch (_) {}
  return true;
})()`
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, exception, err := runtime.Evaluate(expr).WithAwaitPromise(true).Do(ctx)
		if err != nil {
			return fmt.Errorf("settle document: %w", err)
		}
		if exception != nil {
			return fmt.Errorf("settle document: %w", exception)
		}
		return nil
	})
}
