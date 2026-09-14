package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
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
// — the frame set download events are routed by, the in-flight request set the
// network-idle wait reads — and nothing about ownership, limits, or AO state.
type cdpPage struct {
	ctx    context.Context
	cancel context.CancelFunc
	handle string
	hooks  pageHooks

	frameMu sync.RWMutex
	frames  map[cdp.FrameID]struct{}

	networkMu   sync.Mutex
	requests    map[network.RequestID]struct{}
	lastNetwork time.Time

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
	if err := chromedp.Run(pageCtx); err != nil {
		pageCancel()
		// A dead controller is the usual reason a target never attaches, and
		// its own error is what makes the failure diagnosable.
		return nil, fmt.Errorf("browser: create page: %w (controller: %v)", err, controller.Err())
	}
	p := &cdpPage{
		ctx: pageCtx, cancel: pageCancel, hooks: hooks,
		handle:   string(chromedp.FromContext(pageCtx).Target.TargetID),
		frames:   make(map[cdp.FrameID]struct{}),
		requests: make(map[network.RequestID]struct{}), lastNetwork: time.Now(),
	}
	if err := p.installHandlers(); err != nil {
		pageCancel()
		return nil, err
	}
	return p, nil
}

// browserCommandContext and targetCommandContext re-address a CDP command at
// the browser-wide or the target-wide executor. They live beside the page
// driver because the page driver is the only place a CDP command is issued
// from now that the managed-Chrome engine is gone; the hosted engine reuses
// them for the browser-level commands it sends through the relay.
func browserCommandContext(ctx context.Context) context.Context {
	chromedpContext := chromedp.FromContext(ctx)
	if chromedpContext == nil || chromedpContext.Browser == nil {
		return ctx
	}
	return cdp.WithExecutor(ctx, chromedpContext.Browser)
}

func targetCommandContext(ctx context.Context) context.Context {
	chromedpContext := chromedp.FromContext(ctx)
	if chromedpContext == nil || chromedpContext.Target == nil {
		return ctx
	}
	return cdp.WithExecutor(ctx, chromedpContext.Target)
}

// dialCDPBrowser establishes the browser-level CDP connection on browserCtx
// WITHOUT creating any target, and enables target discovery.
//
// Shared by every CDP engine (the launcher-hosted one and the headless
// Chromium one), because both want the same two properties and neither can
// get them from chromedp.Run. It is chromedp's own initContextBrowser
// through the exported surface — FromContext, one Allocator.Allocate,
// publish the Browser on the context so every later Run (all of them
// WithTargetID) finds the shared connection — followed by the
// Target.setDiscoverTargets(true) chromedp's skipped first-context path
// would have sent.
//
// Not creating a target is load-bearing on both engines. Run against a
// target-less context issues Target.createTarget, which WebView2 refuses
// with `-32000 no browser is open` (a WebView2 target exists only as a
// launcher-created controller, 2026-08-31) and which real Chromium answers
// with a throwaway tab nobody owns — a whole renderer process per profile,
// paid for forever. Discovery is what feeds ListenBrowser the target
// lifecycle events both engines re-key into the seam's vocabulary.
func dialCDPBrowser(browserCtx context.Context, logf func(string, ...any)) error {
	c := chromedp.FromContext(browserCtx)
	if c == nil || c.Allocator == nil {
		return errors.New("not a chromedp context")
	}
	browser, err := c.Allocator.Allocate(browserCtx, chromedp.WithBrowserErrorf(func(format string, args ...any) {
		logf("browser: chromedp: "+format, args...)
	}))
	if err != nil {
		return err
	}
	c.Browser = browser
	return target.SetDiscoverTargets(true).Do(cdp.WithExecutor(browserCtx, browser))
}

// cdpDownloadEvent translates the two browser-level download events into the
// seam's vocabulary, reporting whether it recognised the event.
//
// Shared for the same reason as the dial: downloads are a browser-level CDP
// fact with no engine-specific identity in them — the GUID IS the handle on
// both engines — so a second copy could only drift. Frames, targets and
// page ids are the caller's business, which is why nothing here re-keys.
func cdpDownloadEvent(ev any, events engineEvents) bool {
	switch event := ev.(type) {
	case *cdpbrowser.EventDownloadWillBegin:
		events.DownloadStarted(downloadStart{
			Frame: string(event.FrameID), ID: event.GUID,
			URL: event.URL, SuggestedName: event.SuggestedFilename,
		})
		return true
	case *cdpbrowser.EventDownloadProgress:
		state := downloadInProgress
		switch event.State {
		case cdpbrowser.DownloadProgressStateCompleted:
			state = downloadCompleted
		case cdpbrowser.DownloadProgressStateCanceled:
			state = downloadCanceled
		}
		events.DownloadProgress(downloadProgress{
			ID: event.GUID, Received: event.ReceivedBytes, State: state, FilePath: event.FilePath,
		})
		return true
	}
	return false
}

func (p *cdpPage) Lifetime() context.Context { return p.ctx }
func (p *cdpPage) Handle() string            { return p.handle }
func (p *cdpPage) Close()                    { p.cancel() }

func (p *cdpPage) OwnsFrame(frame string) bool {
	p.frameMu.RLock()
	defer p.frameMu.RUnlock()
	_, ok := p.frames[cdp.FrameID(frame)]
	return ok
}

func (p *cdpPage) installHandlers() error {
	chromedp.ListenTarget(p.ctx, func(ev any) {
		switch event := ev.(type) {
		case *page.EventJavascriptDialogOpening:
			accept := event.Type == page.DialogTypeBeforeunload
			go func() {
				ctx, cancel := operationContext(context.Background(), p.ctx, 3*time.Second)
				defer cancel()
				_ = page.HandleJavaScriptDialog(accept).Do(targetCommandContext(ctx))
			}()
		case *fetch.EventRequestPaused:
			if event.Request == nil {
				return
			}
			requestID, rawURL := event.RequestID, event.Request.URL
			go func() {
				ctx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
				defer cancel()
				if p.hooks.Allow(rawURL) {
					_ = fetch.ContinueRequest(requestID).Do(targetCommandContext(ctx))
				} else {
					_ = fetch.FailRequest(requestID, network.ErrorReasonBlockedByClient).Do(targetCommandContext(ctx))
				}
			}()
		case *page.EventFrameAttached:
			p.frameMu.Lock()
			p.frames[event.FrameID] = struct{}{}
			p.frameMu.Unlock()
		case *page.EventFrameNavigated:
			if event.Frame != nil {
				p.frameMu.Lock()
				p.frames[event.Frame.ID] = struct{}{}
				p.frameMu.Unlock()
			}
		case *page.EventFrameDetached:
			p.frameMu.Lock()
			delete(p.frames, event.FrameID)
			p.frameMu.Unlock()
		case *cdpruntime.EventConsoleAPICalled:
			p.hooks.Console(consoleAPIEntry(event, p.hooks.PageURL()))
		case *cdplog.EventEntryAdded:
			if entry, ok := logEntry(event); ok {
				p.hooks.Console(entry)
			}
		case *network.EventRequestWillBeSent:
			p.networkMu.Lock()
			p.requests[event.RequestID] = struct{}{}
			p.lastNetwork = time.Now()
			p.networkMu.Unlock()
		case *network.EventLoadingFinished:
			p.networkMu.Lock()
			delete(p.requests, event.RequestID)
			p.lastNetwork = time.Now()
			p.networkMu.Unlock()
		case *network.EventLoadingFailed:
			p.networkMu.Lock()
			delete(p.requests, event.RequestID)
			p.lastNetwork = time.Now()
			p.networkMu.Unlock()
		}
	})
	patterns := []*fetch.RequestPattern{
		{ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest},
		// Document-only interception still lets a workspace HTML page embed an
		// outside-workspace file as an image/script. Intercept every local-file
		// request so the same authority check covers subresources too.
		{URLPattern: "file://*", RequestStage: fetch.RequestStageRequest},
	}
	if err := fetch.Enable().WithPatterns(patterns).Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: install navigation policy: %w", err)
	}
	if err := cdplog.Enable().Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: enable console log capture: %w", err)
	}
	if err := cdpruntime.Enable().Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: enable runtime capture: %w", err)
	}
	if err := network.Enable().Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: enable network lifecycle: %w", err)
	}
	return nil
}

// consoleAPIEntry decodes a Runtime.consoleAPICalled event. Chrome does not
// attribute the entry to a URL, so the page's last known one is used.
func consoleAPIEntry(event *cdpruntime.EventConsoleAPICalled, pageURL string) ConsoleLog {
	parts := make([]string, 0, len(event.Args))
	for _, arg := range event.Args {
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
	timestamp := time.Now().UTC()
	if event.Timestamp != nil {
		timestamp = time.Time(*event.Timestamp).UTC()
	}
	return ConsoleLog{
		Level: normalizeConsoleLevel(string(event.Type)), Message: strings.Join(parts, " "),
		Timestamp: timestamp.Format(time.RFC3339Nano), URL: pageURL,
	}
}

// logEntry decodes a Log.entryAdded event. An entry-less event is not a log.
func logEntry(event *cdplog.EventEntryAdded) (ConsoleLog, bool) {
	if event.Entry == nil {
		return ConsoleLog{}, false
	}
	timestamp := time.Now().UTC()
	if event.Entry.Timestamp != nil {
		timestamp = time.Time(*event.Entry.Timestamp).UTC()
	}
	return ConsoleLog{
		Level: normalizeConsoleLevel(string(event.Entry.Level)), Message: event.Entry.Text,
		Timestamp: timestamp.Format(time.RFC3339Nano), URL: event.Entry.URL,
	}, true
}

func (p *cdpPage) Info(ctx context.Context) (string, string, error) {
	var location, title string
	if err := chromedp.Run(ctx, chromedp.Location(&location), chromedp.Title(&title)); err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	return location, title, nil
}

func (p *cdpPage) HistoryState(ctx context.Context) (bool, bool, error) {
	current, entries, err := page.GetNavigationHistory().Do(targetCommandContext(ctx))
	if err != nil {
		return false, false, fmt.Errorf("browser: read history state: %w", err)
	}
	return current > 0, int(current)+1 < len(entries), nil
}

func (p *cdpPage) Navigate(ctx context.Context, url string) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(url)); err != nil {
		return fmt.Errorf("browser: navigate: %w", err)
	}
	return nil
}

func (p *cdpPage) History(ctx context.Context, action string) error {
	var runErr error
	switch action {
	case "back":
		current, entries, err := page.GetNavigationHistory().Do(targetCommandContext(ctx))
		if err != nil {
			return fmt.Errorf("browser: history back: %w", err)
		}
		if current <= 0 {
			return fmt.Errorf("browser: no previous history entry")
		}
		runErr = page.NavigateToHistoryEntry(entries[current-1].ID).Do(targetCommandContext(ctx))
	case "forward":
		current, entries, err := page.GetNavigationHistory().Do(targetCommandContext(ctx))
		if err != nil {
			return fmt.Errorf("browser: history forward: %w", err)
		}
		if int(current)+1 >= len(entries) {
			return fmt.Errorf("browser: no forward history entry")
		}
		runErr = page.NavigateToHistoryEntry(entries[current+1].ID).Do(targetCommandContext(ctx))
	case "reload":
		runErr = page.Reload().Do(targetCommandContext(ctx))
	case "stop":
		runErr = page.StopLoading().Do(targetCommandContext(ctx))
	}
	if runErr != nil {
		return fmt.Errorf("browser: history %s: %w", action, runErr)
	}
	return nil
}

func (p *cdpPage) PageStatus(ctx context.Context) (pageStatus, error) {
	var probe struct{ URL, Ready string }
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({url:location.href,ready:document.readyState})`, &probe)); err != nil {
		return pageStatus{}, err
	}
	p.networkMu.Lock()
	idle := len(p.requests) == 0 && time.Since(p.lastNetwork) >= 500*time.Millisecond
	p.networkMu.Unlock()
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: idle}, nil
}

func (p *cdpPage) NavigationMark(ctx context.Context) (navigationMark, error) {
	var mark navigationMark
	if err := chromedp.Run(ctx, chromedp.Location(&mark.URL)); err != nil {
		return mark, err
	}
	if tree, err := page.GetFrameTree().Do(targetCommandContext(ctx)); err == nil && tree != nil && tree.Frame != nil {
		mark.Loader = string(tree.Frame.LoaderID)
	}
	return mark, nil
}

func (p *cdpPage) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	if err := chromedp.Run(ctx, chromedp.Evaluate(snapshotExpression(), &snapshot)); err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	return snapshot, nil
}

func (p *cdpPage) Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, error) {
	params := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(85).WithFromSurface(true)
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
	var view struct{ X, Y, Width, Height float64 }
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({x: window.scrollX, y: window.scrollY, width: window.innerWidth, height: window.innerHeight})`, &view)); err != nil {
		return nil, fmt.Errorf("browser: screenshot metrics: %w", err)
	}
	restoreScroll := false
	if opts.Clip != nil {
		clip := opts.Clip
		restoreScroll = true
		params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: imageScale})
	} else if !opts.FullPage {
		if view.Width > 0 && view.Height > 0 {
			params = params.WithClip(&page.Viewport{X: view.X, Y: view.Y, Width: view.Width, Height: view.Height, Scale: imageScale})
		}
	} else {
		restoreScroll = true
		_, _, contentSize, _, _, cssContentSize, metricsErr := page.GetLayoutMetrics().Do(targetCommandContext(ctx))
		if metricsErr != nil {
			return nil, fmt.Errorf("browser: screenshot metrics: %w", metricsErr)
		}
		size := cssContentSize
		if size == nil {
			size = contentSize
		}
		if size != nil {
			height := size.Height
			width := size.Width
			if height > maxFullScreenshotHeight {
				height = maxFullScreenshotHeight
			}
			if width > maxFullScreenshotWidth {
				width = maxFullScreenshotWidth
			}
			params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: 0, Y: 0, Width: width, Height: height, Scale: imageScale})
		}
	}
	data, err := params.Do(targetCommandContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
	}
	if restoreScroll {
		restore := fmt.Sprintf(`(() => { if (window.scrollX !== %f || window.scrollY !== %f) window.scrollTo({left: %f, top: %f, behavior: "instant"}); return true; })()`, view.X, view.Y, view.X, view.Y)
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(restore, &ok)); err != nil {
			return nil, fmt.Errorf("browser: screenshot: restore scroll: %w", err)
		}
	}
	return data, nil
}

func (p *cdpPage) Evaluate(ctx context.Context, expression string) (any, error) {
	var result any
	awaitPromise := func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return params.WithAwaitPromise(true)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &result, awaitPromise)); err != nil {
		return nil, fmt.Errorf("browser: evaluate: %w", err)
	}
	return result, nil
}

func (p *cdpPage) EvaluateReadOnly(ctx context.Context, expression string) (json.RawMessage, error) {
	remote, exception, err := cdpruntime.Evaluate(expression).WithReturnByValue(true).WithAwaitPromise(true).WithThrowOnSideEffect(true).Do(targetCommandContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: read-only evaluate: %w", err)
	}
	if exception != nil {
		return nil, fmt.Errorf("browser: read-only evaluate rejected a possible side effect: %s", exception.Text)
	}
	if remote == nil {
		return nil, nil
	}
	return json.RawMessage(remote.Value), nil
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

// SetViewScale sets the factor the presented view is drawn at: the pane
// shows the page scaled to fit rather than resizing it. 1 while hidden.
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
	err := emulation.SetDeviceMetricsOverride(int64(p.viewportW), int64(p.viewportH), 0, false).WithScale(scale).Do(targetCommandContext(ctx))
	p.metricsSent = err == nil
	return err
}

// devicePixelRatio is the ratio the page rasters at under the native
// device scale factor, which every capture divides out so an image pixel
// is a CSS pixel on any display.
func (p *cdpPage) devicePixelRatio(ctx context.Context) (float64, error) {
	var ratio float64
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.devicePixelRatio`, &ratio)); err != nil {
		return 0, err
	}
	if ratio <= 0 {
		ratio = 1
	}
	return ratio, nil
}
