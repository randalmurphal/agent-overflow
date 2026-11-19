package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func (p *cdpPage) Info(ctx context.Context) (string, string, error) {
	location, err := chromedp.Run(ctx, chromedp.Location())
	if err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	title, err := chromedp.Run(ctx, chromedp.Title())
	if err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	return location, title, nil
}

func (p *cdpPage) HistoryState(ctx context.Context) (bool, bool, error) {
	history, err := chromedp.Call(ctx, page.GetNavigationHistory, cdp.Empty{})
	if err != nil {
		return false, false, fmt.Errorf("browser: read history state: %w", err)
	}
	return history.CurrentIndex > 0, int(history.CurrentIndex)+1 < len(history.Entries), nil
}

func (p *cdpPage) Navigate(ctx context.Context, url string) error {
	if err := chromedp.Do(ctx, chromedp.Navigate(url)); err != nil {
		return cdpNavigateError(err)
	}
	return nil
}

// cdpNavigateError marks the load error Chromium answers for a navigation a
// stop abandoned (errNavigationAborted).
func cdpNavigateError(err error) error {
	if loadErr := (*chromedp.LoadError)(nil); errors.As(err, &loadErr) && loadErr.ErrorText == "net::ERR_ABORTED" {
		return fmt.Errorf("browser: navigate: %w: %w", errNavigationAborted, err)
	}
	return fmt.Errorf("browser: navigate: %w", err)
}

func (p *cdpPage) History(ctx context.Context, action string) error {
	var runErr error
	switch action {
	case "back":
		history, err := chromedp.Call(ctx, page.GetNavigationHistory, cdp.Empty{})
		if err != nil {
			return fmt.Errorf("browser: history back: %w", err)
		}
		if history.CurrentIndex <= 0 {
			return fmt.Errorf("browser: no previous history entry")
		}
		_, runErr = chromedp.Call(ctx, page.NavigateToHistoryEntry, page.NavigateToHistoryEntryParams{EntryID: history.Entries[history.CurrentIndex-1].ID})
	case "forward":
		history, err := chromedp.Call(ctx, page.GetNavigationHistory, cdp.Empty{})
		if err != nil {
			return fmt.Errorf("browser: history forward: %w", err)
		}
		if int(history.CurrentIndex)+1 >= len(history.Entries) {
			return fmt.Errorf("browser: no forward history entry")
		}
		_, runErr = chromedp.Call(ctx, page.NavigateToHistoryEntry, page.NavigateToHistoryEntryParams{EntryID: history.Entries[history.CurrentIndex+1].ID})
	case "reload":
		_, runErr = chromedp.Call(ctx, page.Reload, page.ReloadParams{})
	case "stop":
		_, runErr = chromedp.Call(ctx, page.StopLoading, cdp.Empty{})
	}
	if runErr != nil {
		return fmt.Errorf("browser: history %s: %w", action, runErr)
	}
	return nil
}

func (p *cdpPage) PageStatus(ctx context.Context) (pageStatus, error) {
	probe, err := chromedp.Run(ctx, chromedp.Evaluate[struct{ URL, Ready string }](`({url:location.href,ready:document.readyState})`))
	if err != nil {
		return pageStatus{}, err
	}
	idle := p.network.idle(time.Now(), 500*time.Millisecond)
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: idle}, nil
}

func (p *cdpPage) NavigationMark(ctx context.Context) (navigationMark, error) {
	var mark navigationMark
	location, err := chromedp.Run(ctx, chromedp.Location())
	if err != nil {
		return mark, err
	}
	mark.URL = location
	if tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{}); err == nil && tree.FrameTree != nil && tree.FrameTree.Frame != nil {
		mark.Loader = string(tree.FrameTree.Frame.LoaderID)
	}
	return mark, nil
}

func (p *cdpPage) Snapshot(ctx context.Context) (Snapshot, error) {
	snapshot, err := chromedp.Run(ctx, chromedp.Evaluate[Snapshot](snapshotExpression()))
	if err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	return snapshot, nil
}

func (p *cdpPage) Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, error) {
	params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatJpeg, Quality: new(int64(85)), FromSurface: new(true)}
	ratio, err := p.devicePixelRatio(ctx)
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot metrics: %w", err)
	}
	// Every capture is a clip in document coordinates at 1/ratio, so the
	// image is one pixel per CSS pixel whatever the display's scale.
	//
	// Only a capture that reaches past the viewport asks Chromium to
	// captureBeyondViewport. That flag lays the page out at the document's
	// size for the capture, which moves sticky and fixed elements and, on a
	// page that relayouts under it, leaves the scroll offset somewhere else
	// afterwards (measured live: a capture moved a page from 2000 to 2390).
	// The plain viewport capture is the clip at the current scroll offset
	// under the page's own layout, and the other two put the scroll back.
	imageScale := 1 / ratio
	view, err := chromedp.Run(ctx, chromedp.Evaluate[struct{ X, Y, Width, Height float64 }](`({x: window.scrollX, y: window.scrollY, width: window.innerWidth, height: window.innerHeight})`))
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot metrics: %w", err)
	}
	restoreScroll := false
	if opts.Clip != nil {
		clip := opts.Clip
		restoreScroll = true
		params.CaptureBeyondViewport = new(true)
		params.Clip = &page.Viewport{X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: imageScale}
	} else if !opts.FullPage {
		if view.Width > 0 && view.Height > 0 {
			params.Clip = &page.Viewport{X: view.X, Y: view.Y, Width: view.Width, Height: view.Height, Scale: imageScale}
		}
	} else {
		restoreScroll = true
		metrics, metricsErr := chromedp.Call(ctx, page.GetLayoutMetrics, cdp.Empty{})
		if metricsErr != nil {
			return nil, fmt.Errorf("browser: screenshot metrics: %w", metricsErr)
		}
		if size := metrics.CSSContentSize; size != nil {
			height := size.Height
			width := size.Width
			if height > maxFullScreenshotHeight {
				height = maxFullScreenshotHeight
			}
			if width > maxFullScreenshotWidth {
				width = maxFullScreenshotWidth
			}
			params.CaptureBeyondViewport = new(true)
			params.Clip = &page.Viewport{X: 0, Y: 0, Width: width, Height: height, Scale: imageScale}
		}
	}
	capture, err := chromedp.Call(ctx, page.CaptureScreenshot, params)
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
	}
	if restoreScroll {
		restore := fmt.Sprintf(`(() => { if (window.scrollX !== %f || window.scrollY !== %f) window.scrollTo({left: %f, top: %f, behavior: "instant"}); return true; })()`, view.X, view.Y, view.X, view.Y)
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](restore)); err != nil {
			return nil, fmt.Errorf("browser: screenshot: restore scroll: %w", err)
		}
	}
	return capture.Data, nil
}

// Evaluate evaluates source with Runtime.evaluate, which Chrome exempts from
// the page's script policy. With readOnly set, Chrome's side-effect check
// refuses anything it cannot prove side-effect free before it happens; the
// refusal escapes the runner's own catch, so the code cannot swallow it.
func (p *cdpPage) Evaluate(ctx context.Context, source string, readOnly bool) (json.RawMessage, error) {
	result, err := chromedp.Call(ctx, cdpruntime.Evaluate, cdpruntime.EvaluateParams{
		Expression: source, ReturnByValue: new(true), AwaitPromise: new(true), ThrowOnSideEffect: new(readOnly),
	})
	if err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil {
		return nil, cdpEvaluateException(result.ExceptionDetails, readOnly)
	}
	if result.Result == nil {
		return nil, errors.New("the engine returned no result")
	}
	return evaluationAnswer(json.RawMessage(result.Result.Value))
}

// cdpEvaluateException reports an exception that escaped the runner: the
// source did not compile, or the side-effect check refused it. The
// description's first line is the "Name: message" the WebKit engines report.
func cdpEvaluateException(exception *cdpruntime.ExceptionDetails, readOnly bool) error {
	message := exception.Text
	if exception.Exception != nil && exception.Exception.Description != "" {
		message, _, _ = strings.Cut(exception.Exception.Description, "\n")
	}
	if exception.Exception != nil && exception.Exception.ClassName == "SyntaxError" {
		return &evaluateSyntaxError{message: message}
	}
	if readOnly && strings.HasPrefix(message, "EvalError: Possible side-effect") {
		return sideEffectRejectedError{}
	}
	return errors.New(message)
}

// sideEffectRejectedError is Chrome's refusal of a read-only evaluation it
// cannot prove side-effect free, in the words the agent reads.
type sideEffectRejectedError struct{}

func (sideEffectRejectedError) Error() string {
	return "Chrome rejected a possible side effect: it refuses anything it cannot prove side-effect free, including some reads such as getElementById and creating an Error; read with querySelector, or use browser_evaluate"
}

// ReadOnlyCaveat is empty: Chrome rejects the side effect itself, in the
// engine, so the tool result needs no qualifier.
func (p *cdpPage) ReadOnlyCaveat() string { return "" }

// SetViewport pins the page's layout viewport. The override's device scale
// factor is 0 (the display's own), so a presented page rasters at native
// DPI; screenshots normalize to one image pixel per CSS pixel themselves.
// The controller's window size is irrelevant to layout and capture under
// the override (spike 2026-09-14), which is what lets a hidden page keep a
// real viewport inside a 1x1 clip.
func (p *cdpPage) SetViewport(ctx context.Context, width, height int) error {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	p.viewportW, p.viewportH = width, height
	return p.applyMetricsLocked(ctx)
}

// SetViewScale sets the factor the presented view is drawn at: a page larger
// than the pane is shown scaled down rather than resized. 1 while hidden.
func (p *cdpPage) SetViewScale(ctx context.Context, scale float64) error {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	if scale <= 0 {
		scale = 1
	}
	if p.metricsSent && p.viewScale == scale {
		return nil
	}
	p.viewScale = scale
	if p.viewportW <= 0 || p.viewportH <= 0 {
		// No viewport yet: the Manager's SetViewport follows page creation
		// and carries the scale with it.
		return nil
	}
	return p.applyMetricsLocked(ctx)
}

func (p *cdpPage) applyMetricsLocked(ctx context.Context) error {
	scale := p.viewScale
	if scale <= 0 {
		scale = 1
	}
	_, err := chromedp.Call(ctx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width: int64(p.viewportW), Height: int64(p.viewportH), DeviceScaleFactor: 0, Mobile: false, Scale: scale,
	})
	p.metricsSent = err == nil
	return err
}

// devicePixelRatio is the ratio the page rasters at under the native
// device scale factor, which every capture divides out so an image pixel
// is a CSS pixel on any display.
func (p *cdpPage) devicePixelRatio(ctx context.Context) (float64, error) {
	ratio, err := chromedp.Run(ctx, chromedp.Evaluate[float64](`window.devicePixelRatio`))
	if err != nil {
		return 0, err
	}
	if ratio <= 0 {
		ratio = 1
	}
	return ratio, nil
}
