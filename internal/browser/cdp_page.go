package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// cdpPage drives one Chrome target. It owns the CDP bookkeeping the tools need
// (the frame set download events are routed by, the in-flight request set the
// network-idle wait reads) and nothing about ownership, limits, or AO state.
type cdpPage struct {
	ctx    context.Context
	cancel context.CancelFunc
	handle string
	hooks  pageHooks

	frames  *frameTracker
	network *networkTracker

	// The device-metrics override is one value with two owners: the
	// Manager sets the viewport (SetViewport, under the page lock) and the
	// pane host sets the presentation scale (SetViewScale, from the
	// presentation sync). Both go through applyMetrics under metricsMu so
	// neither can send the other's stale half.
	metricsMu   sync.Mutex
	viewportW   int
	viewportH   int
	viewScale   float64
	metricsSent bool
}

func startCDPPage(controller, pageCtx context.Context, pageCancel context.CancelFunc, hooks pageHooks) (pageDriver, error) {
	if err := chromedp.Do(pageCtx); err != nil {
		pageCancel()
		// A dead controller is the usual reason a target never attaches, and
		// its own error is what makes the failure diagnosable.
		return nil, fmt.Errorf("browser: create page: %w (controller: %v)", err, controller.Err())
	}
	p := &cdpPage{
		ctx: pageCtx, cancel: pageCancel, hooks: hooks,
		handle:  string(chromedp.FromContext(pageCtx).Target.TargetID),
		frames:  newFrameTracker(),
		network: newNetworkTracker(time.Now()),
	}
	if err := p.installHandlers(); err != nil {
		pageCancel()
		return nil, err
	}
	return p, nil
}

// dialCDPBrowser connects the browser-level CDP session on browserCtx
// WITHOUT creating any target, has subscribe register the browser events the
// engine reads, and then enables target discovery.
//
// Shared by every CDP engine (the launcher-hosted one and the headless
// Chromium one), because both want the same two properties and neither can
// get them from chromedp.Do, which attaches a target.
//
// Not creating a target is load-bearing on both engines. A target-less Do
// issues Target.createTarget, which WebView2 refuses with `-32000 no browser
// is open` (a WebView2 target exists only as a launcher-created controller,
// 2026-08-31) and which real Chromium answers with a throwaway tab nobody
// owns: a whole renderer process per profile, paid for forever. Discovery is
// what feeds the target lifecycle events both engines re-key into the seam's
// vocabulary.
//
// subscribe's first chromedp.BrowserEvents is what connects the browser. It
// sends no command, and it connects through chromedp's own path, which is
// also what cancels browserCtx when the connection is lost. Every
// subscription exists before discovery is enabled, so it sees every target
// discovery reports, a popup opened during the handshake included.
func dialCDPBrowser(browserCtx context.Context, subscribe func() error) error {
	if chromedp.FromContext(browserCtx) == nil {
		return errors.New("not a chromedp context")
	}
	if err := subscribe(); err != nil {
		return err
	}
	_, err := chromedp.CallBrowser(browserCtx, target.SetDiscoverTargets, target.SetDiscoverTargetsParams{Discover: true})
	return err
}

func (p *cdpPage) Lifetime() context.Context { return p.ctx }
func (p *cdpPage) Handle() string            { return p.handle }
func (p *cdpPage) Close()                    { p.cancel() }

// OwnsFrame answers from the frames the page has reported, and for its
// main frame, whose id is the target id, before it has reported any: a
// download can begin in a navigation that never commits.
func (p *cdpPage) OwnsFrame(frame string) bool {
	if frame == p.handle {
		return true
	}
	return p.frames.owns(cdp.FrameID(frame))
}

// installHandlers subscribes to the page's events before enabling the
// domains that send them, so none is lost.
func (p *cdpPage) installHandlers() error {
	ctx, logf := p.ctx, log.Printf
	session := chromedp.FromContext(ctx).Target
	onTargetEvent(ctx, page.JavascriptDialogOpening, logf, func(event page.EventJavascriptDialogOpening) {
		accept := event.Type == page.DialogTypeBeforeunload
		answerCtx, cancel := operationContext(context.Background(), ctx, 3*time.Second)
		defer cancel()
		if _, err := cdp.Call(answerCtx, session, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: accept}); err != nil && ctx.Err() == nil {
			logf("browser: answer a %s dialog: %v", event.Type, err)
		}
	})
	onTargetEvent(ctx, fetch.RequestPaused, logf, func(event fetch.EventRequestPaused) {
		answerPausedRequest(ctx, session, event, p.hooks.Allow)
	})
	onTargetEvent(ctx, page.FrameAttached, logf, func(event page.EventFrameAttached) {
		p.frames.attached(event.FrameID)
	})
	onTargetEvent(ctx, page.FrameNavigated, logf, func(event page.EventFrameNavigated) {
		if event.Frame != nil {
			p.frames.navigated(event.Frame.ID)
		}
	})
	onTargetEvent(ctx, page.FrameDetached, logf, func(event page.EventFrameDetached) {
		p.frames.detached(event.FrameID)
	})
	// chromedp.Console is the one ordered join of several event methods that
	// chromedp offers: console API calls and browser log entries reach the
	// log in the order the page wrote them.
	messages := chromedp.Console(ctx)
	go func() {
		for message, err := range messages {
			if err != nil {
				if ctx.Err() == nil {
					logf("browser: console events stopped: %v", err)
				}
				return
			}
			if entry, ok := consoleEntry(message, p.hooks.PageURL); ok {
				p.hooks.Console(entry)
			}
		}
	}()
	onTargetEvent(ctx, network.RequestWillBeSent, logf, func(event network.EventRequestWillBeSent) {
		p.network.started(event.RequestID, event.RedirectResponse != nil, time.Now())
	})
	onTargetEvent(ctx, network.LoadingFinished, logf, func(event network.EventLoadingFinished) {
		p.network.ended(event.RequestID, time.Now())
	})
	onTargetEvent(ctx, network.LoadingFailed, logf, func(event network.EventLoadingFailed) {
		p.network.ended(event.RequestID, time.Now())
	})
	if _, err := chromedp.Call(ctx, fetch.Enable, fetch.EnableParams{Patterns: navigationPolicyPatterns}); err != nil {
		return fmt.Errorf("browser: install navigation policy: %w", err)
	}
	if _, err := chromedp.Call(ctx, cdplog.Enable, cdp.Empty{}); err != nil {
		return fmt.Errorf("browser: enable console log capture: %w", err)
	}
	if _, err := chromedp.Call(ctx, cdpruntime.Enable, cdp.Empty{}); err != nil {
		return fmt.Errorf("browser: enable runtime capture: %w", err)
	}
	if _, err := chromedp.Call(ctx, network.Enable, network.EnableParams{}); err != nil {
		return fmt.Errorf("browser: enable network lifecycle: %w", err)
	}
	return nil
}

// navigationPolicyPatterns are the requests the navigation policy decides.
// Document-only interception would still let a workspace HTML page embed an
// outside-workspace file as an image or script, so every local-file request
// is intercepted too.
var navigationPolicyPatterns = []*fetch.RequestPattern{
	{ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest},
	{URLPattern: "file://*", RequestStage: fetch.RequestStageRequest},
}

// answerPausedRequest continues a request the navigation policy paused when
// allow permits its URL and fails it otherwise. session is the one that
// paused it. The answer runs on its own goroutine, bounded by lifetime, so a
// slow check or round trip does not hold the requests paused behind it.
func answerPausedRequest(lifetime context.Context, session cdp.Session, event fetch.EventRequestPaused, allow func(url string) bool) {
	if event.Request == nil {
		return
	}
	requestID, rawURL := event.RequestID, event.Request.URL
	go func() {
		ctx, cancel := operationContext(context.Background(), lifetime, 5*time.Second)
		defer cancel()
		var err error
		if allow(rawURL) {
			_, err = cdp.Call(ctx, session, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: requestID})
		} else {
			_, err = cdp.Call(ctx, session, fetch.FailRequest, fetch.FailRequestParams{RequestID: requestID, ErrorReason: network.ErrorReasonBlockedByClient})
		}
		// A page or browser that has gone needs no answer. Any other refusal
		// leaves the request paused, which the page sees as a load that never
		// finishes, so it is logged.
		if err != nil && lifetime.Err() == nil {
			log.Printf("browser: answer paused request %s: %v", requestID, err)
		}
	}()
}

// consoleEntry converts one message of chromedp.Console. Uncaught exceptions
// were never part of the console log, so they are skipped. A console API
// call carries no URL of its own, so the page's last known one is used.
func consoleEntry(message chromedp.ConsoleMessage, pageURL func() string) (ConsoleLog, bool) {
	if message.IsException() {
		return ConsoleLog{}, false
	}
	timestamp := message.Time.UTC()
	if message.Time.IsZero() || message.Time.Unix() == 0 {
		timestamp = time.Now().UTC()
	}
	if message.Source != "" {
		// A browser log entry: only those carry a source. Console reports
		// the log level verbose as debug, which the log has always shown as
		// log.
		level := string(message.Type)
		if message.Type == chromedp.ConsoleDebug {
			level = "log"
		}
		return ConsoleLog{
			Level: normalizeConsoleLevel(level), Message: message.Text,
			Timestamp: timestamp.Format(time.RFC3339Nano), URL: message.URL,
		}, true
	}
	parts := make([]string, 0, len(message.Args))
	for _, arg := range message.Args {
		if arg == nil {
			continue
		}
		var value any
		if len(arg.Value) > 0 && json.Unmarshal(arg.Value, &value) == nil {
			parts = append(parts, fmt.Sprint(value))
		} else if arg.Description != "" {
			parts = append(parts, arg.Description)
		} else {
			parts = append(parts, string(arg.Type))
		}
	}
	return ConsoleLog{
		Level: normalizeConsoleLevel(string(message.Type)), Message: strings.Join(parts, " "),
		Timestamp: timestamp.Format(time.RFC3339Nano), URL: pageURL(),
	}, true
}

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
		return fmt.Errorf("browser: navigate: %w", err)
	}
	return nil
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
		return errors.New("Chrome rejected a possible side effect: it refuses anything it cannot prove side-effect free, including some reads such as getElementById and creating an Error; read with querySelector, or use browser_evaluate")
	}
	return errors.New(message)
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
