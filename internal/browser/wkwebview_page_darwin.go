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
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// One WKWebView, driven entirely through JavaScript. WKWebView exposes no
// remote-debugging protocol, so where the CDP driver sends a protocol command
// this one evaluates a function body built in webkitjs.go — the SAME builders
// the WebKitGTK driver uses, because both engines are WebKit and the difference
// between them is the call that carries the body, not the body.
//
// Everything the Manager decides — ownership, bounds, node ids, artifact
// quotas — stays on its side of the seam. What lives here is only how an
// operation is carried out.

const (
	// wkPollInterval is how often a load or visibility wait re-samples. Each
	// sample is one main-thread dispatch, so this is a real cost and not a free
	// spin.
	wkPollInterval = 50 * time.Millisecond

	// wkNetworkQuiet is how long after the last load transition the page counts
	// as network-idle. WKWebView reports no per-request lifecycle, so this is
	// the honest approximation of CDP's judgement.
	wkNetworkQuiet = 500 * time.Millisecond

	// wkMaxInPageAssetBytes bounds one asset read. WebKit has no out-of-band
	// resource stream, so an asset crosses as base64 inside a JSON result —
	// which is why this engine's per-asset ceiling is well below the Manager's
	// 128 MiB bundle cap rather than equal to it.
	wkMaxInPageAssetBytes = 32 << 20

	// wkScreenshotQuality matches what the CDP driver asks Chrome for, so a
	// screenshot does not change size or fidelity with the engine.
	wkScreenshotQuality = 85
)

type wkPage struct {
	engine  *wkEngine
	profile *wkProfile
	id      uint64
	handle  string
	hooks   pageHooks
	view    unsafe.Pointer
	slot    int

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	closed       bool
	lastURL      string
	lastActivity time.Time
	// snapshotMu serializes the resize/capture/restore a full-document
	// screenshot performs, so two captures cannot restore each other's size.
	snapshotMu sync.Mutex
}

func (p *wkPage) Lifetime() context.Context { return p.ctx }
func (p *wkPage) Handle() string            { return p.handle }

// OwnsFrame answers for this page's own handle: this engine addresses every
// event by page, so a page's only frame identity is itself.
func (p *wkPage) OwnsFrame(frame string) bool { return frame == p.handle }

// ReadOnlyCaveat is non-empty because WKWebView has no equivalent of CDP's
// throwOnSideEffect: the expression is evaluated as written. The Manager carries
// this into the tool result so the difference is stated rather than silently
// assumed away — and it is the SAME sentence the WebKitGTK driver returns, so a
// caller reading two engines' answers does not have to notice which is which.
func (p *wkPage) ReadOnlyCaveat() string {
	return "Note: this browser engine cannot reject side effects before they happen. The expression was evaluated as written, so treat browser_evaluate_readonly here as read-only by convention, not by enforcement."
}

// discard unwinds a half-built page. Nothing AppKit-side exists yet.
func (p *wkPage) discard() {
	p.cancel()
	wkPageByID.Delete(p.id)
	p.profile.mu.Lock()
	delete(p.profile.pages, p)
	p.profile.mu.Unlock()
}

func (p *wkPage) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	slot := p.slot
	p.mu.Unlock()
	p.discard()
	p.engine.forgetPane(p.id)
	if slot >= 0 {
		p.engine.releaseSlot(slot)
	}
	wkCloseView(p.view)
}

// noteLoad records what the engine last told us about this page. It is the
// bookkeeping half of the network-idle answer, and it runs on the main thread.
func (p *wkPage) noteLoad(url string) {
	p.mu.Lock()
	p.lastURL = url
	p.lastActivity = time.Now()
	p.mu.Unlock()
}

// consoleMessage decodes one entry from the injected capture script. A payload
// that is not the shape the script posts is not a console entry.
func (p *wkPage) consoleMessage(payload string) {
	var entry struct {
		Level   string `json:"level"`
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	if json.Unmarshal([]byte(payload), &entry) != nil || p.hooks.Console == nil {
		return
	}
	location := entry.URL
	if location == "" && p.hooks.PageURL != nil {
		location = p.hooks.PageURL()
	}
	p.hooks.Console(ConsoleLog{
		Level: normalizeConsoleLevel(entry.Level), Message: entry.Message,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), URL: location,
	})
}

// bind ties one operation to BOTH the caller's bound context and the page's
// lifetime, so closing a page ends whatever it was doing.
func (p *wkPage) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(p.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return merged, func() { stop(); cancel() }
}

// evalBody runs one async-function body and returns its raw JSON result.
func (p *wkPage) evalBody(ctx context.Context, body string) (json.RawMessage, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	raw, err := wkEvaluate(opCtx, p.view, body)
	if err != nil {
		return nil, err
	}
	if raw == "" || raw == "null" {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// evalInto runs a body and decodes its result into out.
func (p *wkPage) evalInto(ctx context.Context, body string, out any) error {
	raw, err := p.evalBody(ctx, body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (p *wkPage) Info(ctx context.Context) (string, string, error) {
	var probe struct{ URL, Title string }
	if err := p.evalInto(ctx, webkitInfoScript, &probe); err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	return probe.URL, probe.Title, nil
}

func (p *wkPage) Navigate(ctx context.Context, target string) error {
	if err := p.load(target); err != nil {
		return err
	}
	return p.awaitLoad(ctx)
}

// load picks the one API that can actually open the URL. WKWebView refuses a
// file URL handed to -loadRequest: — the page loads blank with no error — so
// `browser_open_file` has to go through -loadFileURL:allowingReadAccessToURL:.
// The read-access root is the file's own directory, which is the narrowest
// grant that still lets a local page load the assets beside it.
func (p *wkPage) load(target string) error {
	parsed, err := url.Parse(target)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
		return wkLoadURI(p.view, target)
	}
	path := parsed.Path
	if path == "" {
		return fmt.Errorf("browser: file URL has no path")
	}
	return wkLoadFile(p.view, path, filepath.Dir(path))
}

func (p *wkPage) History(ctx context.Context, action string) error {
	switch action {
	case "back":
		if !wkCanGo(p.view, false) {
			return fmt.Errorf("browser: no previous history entry")
		}
		if err := wkHistory(p.view, wkHistoryBack); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "forward":
		if !wkCanGo(p.view, true) {
			return fmt.Errorf("browser: no forward history entry")
		}
		if err := wkHistory(p.view, wkHistoryForward); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "reload":
		if err := wkHistory(p.view, wkHistoryReload); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "stop":
		return wkHistory(p.view, wkHistoryStop)
	}
	return fmt.Errorf("browser: unsupported history action %q", action)
}

// awaitLoad waits for the load to settle, the way chromedp.Navigate does. The
// first poll deliberately runs after a tick: WebKit has not necessarily entered
// the loading state by the time the load call returns.
func (p *wkPage) awaitLoad(ctx context.Context) error {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	ticker := time.NewTicker(wkPollInterval)
	defer ticker.Stop()
	settled := 0
	for {
		select {
		case <-opCtx.Done():
			return opCtx.Err()
		case <-ticker.C:
			if wkIsLoading(p.view) {
				settled = 0
				continue
			}
			// Two consecutive quiet samples, so the gap between the load call
			// returning and the load actually starting is not read as done.
			settled++
			if settled >= 2 {
				return nil
			}
		}
	}
}

func (p *wkPage) PageStatus(ctx context.Context) (pageStatus, error) {
	var probe struct{ URL, Ready string }
	if err := p.evalInto(ctx, webkitPageStatusScript, &probe); err != nil {
		return pageStatus{}, err
	}
	p.mu.Lock()
	quiet := time.Since(p.lastActivity) >= wkNetworkQuiet
	p.mu.Unlock()
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: quiet && !wkIsLoading(p.view)}, nil
}

func (p *wkPage) NavigationMark(ctx context.Context) (navigationMark, error) {
	var mark navigationMark
	if err := p.evalInto(ctx, webkitNavigationMarkScript, &mark); err != nil {
		return navigationMark{}, err
	}
	return mark, nil
}

func (p *wkPage) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	if err := p.evalInto(ctx, "return "+snapshotExpression()+";", &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	return snapshot, nil
}

func (p *wkPage) Evaluate(ctx context.Context, expression string) (any, error) {
	var value any
	if err := p.evalInto(ctx, webkitExpressionBody(expression), &value); err != nil {
		return nil, fmt.Errorf("browser: evaluate: %w", err)
	}
	return value, nil
}

func (p *wkPage) EvaluateReadOnly(ctx context.Context, expression string) (json.RawMessage, error) {
	raw, err := p.evalBody(ctx, webkitExpressionBody(expression))
	if err != nil {
		return nil, fmt.Errorf("browser: read-only evaluate: %w", err)
	}
	return raw, nil
}

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

// SetViewport resizes the view itself: WebKit has no device-metrics override,
// so the page's viewport IS the view's size.
func (p *wkPage) SetViewport(_ context.Context, width, height int) error {
	return wkSetViewSize(p.view, width, height)
}

func (p *wkPage) ClearViewport(context.Context) error {
	return wkSetViewSize(p.view, wkHiddenWidth, wkHiddenHeight)
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
		return wkSnapshot(ctx, p.view)
	}
	p.snapshotMu.Lock()
	defer p.snapshotMu.Unlock()
	width, height, err := wkViewSize(p.view)
	if err != nil {
		return nil, 0, 0, err
	}
	var document struct{ Width, Height int }
	if err := p.evalInto(ctx, wkDocumentSizeScript, &document); err != nil {
		return nil, 0, 0, err
	}
	target := clampDocumentCapture(document.Width, document.Height, width, height)
	if target.width == width && target.height == height {
		return wkSnapshot(ctx, p.view)
	}
	if err := wkSetViewSize(p.view, target.width, target.height); err != nil {
		return nil, 0, 0, err
	}
	pixels, capturedWidth, capturedHeight, err := wkSnapshot(ctx, p.view)
	// The restore runs whether or not the capture worked: a page left at the
	// document's size would keep reporting that as its viewport forever.
	if restoreErr := wkSetViewSize(p.view, width, height); err == nil {
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
