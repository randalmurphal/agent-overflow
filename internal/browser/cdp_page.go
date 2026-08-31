package browser

import (
	"context"
	"encoding/json"
	"fmt"
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
}

func startCDPPage(controller, pageCtx context.Context, pageCancel context.CancelFunc, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
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
	if err := p.installStorageRestore(restore); err != nil {
		pageCancel()
		return nil, err
	}
	if err := p.installHandlers(); err != nil {
		pageCancel()
		return nil, err
	}
	return p, nil
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

func (p *cdpPage) installStorageRestore(values map[string]map[string]string) error {
	script, err := storageRestoreScript(values)
	if err != nil {
		return err
	}
	if _, err := page.AddScriptToEvaluateOnNewDocument(script).WithRunImmediately(true).Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: install storage restore: %w", err)
	}
	return nil
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
		case *page.EventScreencastFrame:
			sessionID := event.SessionID
			go func() {
				ctx, cancel := operationContext(context.Background(), p.ctx, 3*time.Second)
				defer cancel()
				_ = page.ScreencastFrameAck(sessionID).Do(targetCommandContext(ctx))
			}()
			if p.hooks.Screencast != nil {
				p.hooks.Screencast(event.Data)
			}
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
	if opts.Clip != nil {
		clip := opts.Clip
		params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: 1})
	} else if opts.FullPage {
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
			params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: 0, Y: 0, Width: width, Height: height, Scale: 1})
		}
	}
	data, err := params.Do(targetCommandContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
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

func (p *cdpPage) LocalStorage(ctx context.Context) (string, map[string]string, error) {
	var value struct {
		Origin string            `json:"origin"`
		Data   map[string]string `json:"data"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(localStorageExpression(), &value)); err != nil {
		return "", nil, err
	}
	return value.Origin, value.Data, nil
}

func (p *cdpPage) SetViewport(ctx context.Context, width, height int) error {
	return emulation.SetDeviceMetricsOverride(int64(width), int64(height), 1, false).Do(targetCommandContext(ctx))
}

func (p *cdpPage) ClearViewport(ctx context.Context) error {
	return emulation.ClearDeviceMetricsOverride().Do(targetCommandContext(ctx))
}
