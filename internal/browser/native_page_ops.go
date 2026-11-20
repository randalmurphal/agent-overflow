//go:build linux || darwin

package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// The page operations the WebKitGTK and WKWebView engines carry out alike.
// Neither exposes a remote-debugging protocol, so where the CDP driver
// sends a protocol command they evaluate a function body built in
// webkitjs.go; both are WebKit, and what differs between them is the call
// that carries a body or a history step to the view (nativeView), not the
// body. A page type embeds nativePageOps over its own nativeView.

const (
	// nativePollInterval is how often a load wait re-samples.
	// Each sample is one UI-thread dispatch, so this is a real cost and not
	// a free spin.
	nativePollInterval = 50 * time.Millisecond

	// nativeNetworkQuiet is how long after the last load transition the page
	// counts as network-idle. WebKit reports no per-request lifecycle, so
	// this is the honest approximation of CDP's judgement.
	nativeNetworkQuiet = 500 * time.Millisecond

	// nativeMaxInPageAssetBytes bounds one asset read. WebKit has no
	// out-of-band resource stream, so an asset crosses as base64 inside a
	// JSON result, which is why this per-asset ceiling is well below the
	// Manager's 128 MiB bundle cap rather than equal to it.
	nativeMaxInPageAssetBytes = 32 << 20

	// nativeScreenshotQuality matches what the CDP driver asks Chrome for,
	// so a screenshot does not change size or fidelity with the engine.
	nativeScreenshotQuality = 85
)

// nativeHistoryStep is one history action on a view, numbered as the
// engines' glue numbers it.
type nativeHistoryStep int

const (
	nativeHistoryBack nativeHistoryStep = iota
	nativeHistoryForward
	nativeHistoryReload
	nativeHistoryStop
)

// nativeView is what a WebKit page's engine does on the page's view, on its
// UI thread, for the shared operations. Each answers an error when the
// page is closed or the UI thread did not run the call.
type nativeView interface {
	// evaluate runs body as an async function in world, "" for the page's
	// own, and answers its JSON result, "" for none; ctx bounds it.
	evaluate(ctx context.Context, world, body string) (string, error)
	// canGo answers whether the view's history can go back, or forward.
	canGo(forward bool) (bool, error)
	// isLoading answers whether the view is loading.
	isLoading() (bool, error)
	// history takes one history step.
	history(step nativeHistoryStep) error
}

// nativePageOps is the shared half of a WebKit page: view is the page's
// own, life its lifetime, and lastActivity when the view last reported its
// address or title (noteLoad), guarded by activityMu.
type nativePageOps struct {
	view nativeView
	life context.Context

	activityMu   sync.Mutex
	lastActivity time.Time
}

// bind ties one operation to BOTH the caller's bound context and the page's
// lifetime, so closing a page ends whatever it was doing.
func (o *nativePageOps) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(o.life)
	stop := context.AfterFunc(ctx, cancel)
	return merged, func() { stop(); cancel() }
}

// evalBody runs one async-function body in the page's own world and
// returns its raw JSON result.
func (o *nativePageOps) evalBody(ctx context.Context, body string) (json.RawMessage, error) {
	return o.evalIn(ctx, "", body)
}

// evalIn is evalBody in the named isolated world, "" for the page's own.
func (o *nativePageOps) evalIn(ctx context.Context, world, body string) (json.RawMessage, error) {
	opCtx, cancel := o.bind(ctx)
	defer cancel()
	raw, err := o.view.evaluate(opCtx, world, body)
	if err != nil {
		return nil, err
	}
	if raw == "" || raw == "null" {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// evalInto runs a body and decodes its result into out.
func (o *nativePageOps) evalInto(ctx context.Context, body string, out any) error {
	raw, err := o.evalBody(ctx, body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// noteLoad records that the page reported its address or title now. It is
// the bookkeeping half of the network-idle answer, and it runs on the UI
// thread.
func (o *nativePageOps) noteLoad() {
	o.activityMu.Lock()
	o.lastActivity = time.Now()
	o.activityMu.Unlock()
}

func (o *nativePageOps) Info(ctx context.Context) (string, string, error) {
	var probe struct{ URL, Title string }
	if err := o.evalInto(ctx, webkitInfoScript, &probe); err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	return probe.URL, probe.Title, nil
}

func (o *nativePageOps) HistoryState(context.Context) (bool, bool, error) {
	back, err := o.view.canGo(false)
	if err != nil {
		return false, false, fmt.Errorf("browser: read the page's history: %w", err)
	}
	forward, err := o.view.canGo(true)
	if err != nil {
		return false, false, fmt.Errorf("browser: read the page's history: %w", err)
	}
	return back, forward, nil
}

func (o *nativePageOps) History(ctx context.Context, action string) error {
	switch action {
	case "back", "forward":
		forward := action == "forward"
		can, err := o.view.canGo(forward)
		switch {
		case err != nil:
			return fmt.Errorf("browser: read the page's history: %w", err)
		case !can && forward:
			return fmt.Errorf("browser: no forward history entry")
		case !can:
			return fmt.Errorf("browser: no previous history entry")
		}
		step := nativeHistoryBack
		if forward {
			step = nativeHistoryForward
		}
		if err := o.view.history(step); err != nil {
			return err
		}
		return o.awaitLoad(ctx)
	case "reload":
		if err := o.view.history(nativeHistoryReload); err != nil {
			return err
		}
		return o.awaitLoad(ctx)
	case "stop":
		return o.view.history(nativeHistoryStop)
	}
	return fmt.Errorf("browser: unsupported history action %q", action)
}

// awaitLoad waits for the load to settle, the way chromedp.Navigate does. The
// first poll deliberately runs after a tick: WebKit has not necessarily
// entered the loading state by the time the load call returns. A view whose
// loading state cannot be read ends the wait with that error.
func (o *nativePageOps) awaitLoad(ctx context.Context) error {
	opCtx, cancel := o.bind(ctx)
	defer cancel()
	ticker := time.NewTicker(nativePollInterval)
	defer ticker.Stop()
	settled := 0
	for {
		select {
		case <-opCtx.Done():
			return opCtx.Err()
		case <-ticker.C:
			loading, err := o.view.isLoading()
			if err != nil {
				return fmt.Errorf("browser: read the page's loading state: %w", err)
			}
			if loading {
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

func (o *nativePageOps) PageStatus(ctx context.Context) (pageStatus, error) {
	var probe struct{ URL, Ready string }
	if err := o.evalInto(ctx, webkitPageStatusScript, &probe); err != nil {
		return pageStatus{}, err
	}
	loading, err := o.view.isLoading()
	if err != nil {
		return pageStatus{}, fmt.Errorf("browser: read the page's loading state: %w", err)
	}
	o.activityMu.Lock()
	quiet := time.Since(o.lastActivity) >= nativeNetworkQuiet
	o.activityMu.Unlock()
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: quiet && !loading}, nil
}

func (o *nativePageOps) NavigationMark(ctx context.Context) (navigationMark, error) {
	var mark navigationMark
	if err := o.evalInto(ctx, webkitNavigationMarkScript, &mark); err != nil {
		return navigationMark{}, err
	}
	return mark, nil
}

func (o *nativePageOps) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	if err := o.evalInto(ctx, "return "+snapshotExpression()+";", &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	return snapshot, nil
}

// Evaluate has no side-effect check to ask for; ReadOnlyCaveat says so.
func (o *nativePageOps) Evaluate(ctx context.Context, source string, _ bool) (json.RawMessage, error) {
	return webkitEvaluate(ctx, o.evalBody, source)
}

func (o *nativePageOps) ResolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error) {
	raw, err := o.evalBody(ctx, webkitLocatorResolveScript(locator, attribute))
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

func (o *nativePageOps) ReadNode(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error) {
	fn, err := nodeReadFunction(kind, argument)
	if err != nil {
		return nil, err
	}
	return o.callElement(ctx, locator.Frames, match.Selector, fn)
}

func (o *nativePageOps) ActOnNode(ctx context.Context, match LocatorMatch, locator Locator, act nodeAction) error {
	script, err := webkitNodeActScript(locator.Frames, match.Selector, act)
	if err != nil {
		return err
	}
	_, err = o.evalBody(ctx, script)
	return err
}

func (o *nativePageOps) ScrollNode(ctx context.Context, ref nodeReference, x, y float64) error {
	_, err := o.callElement(ctx, ref.Frames, ref.Selector, nodeScrollFunction(x, y))
	return err
}

// callElement runs one shared element function against a locator's element and
// bounds the result the way the CDP driver bounds an element call.
func (o *nativePageOps) callElement(ctx context.Context, frames []string, selector, fn string) (any, error) {
	raw, err := o.evalBody(ctx, webkitElementCallScript(frames, selector, fn))
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

func (o *nativePageOps) Click(ctx context.Context, selector string) error {
	_, err := o.evalBody(ctx, webkitSelectorClickScript(selector))
	return err
}

func (o *nativePageOps) Type(ctx context.Context, selector, text string, clear bool) error {
	_, err := o.evalBody(ctx, webkitSelectorTypeScript(selector, text, clear))
	return err
}

func (o *nativePageOps) Press(ctx context.Context, raw string) error {
	if !chordHasKey(raw) {
		return fmt.Errorf("browser: key is required")
	}
	if _, err := o.evalBody(ctx, webkitFocusedPressScript(raw)); err != nil {
		return fmt.Errorf("browser: press key: %w", err)
	}
	return nil
}

func (o *nativePageOps) TypeText(ctx context.Context, text string) error {
	_, err := o.evalBody(ctx, webkitTypeTextScript(text))
	return err
}

func (o *nativePageOps) SelectionText(ctx context.Context) (string, error) {
	var selected string
	if err := o.evalInto(ctx, webkitSelectionTextScript, &selected); err != nil {
		return "", fmt.Errorf("browser: read the page's selection: %w", err)
	}
	return selected, nil
}

func (o *nativePageOps) Scroll(ctx context.Context, selector string, x, y float64) error {
	_, err := o.evalBody(ctx, webkitScrollScript(selector, x, y))
	return err
}

func (o *nativePageOps) Pointer(ctx context.Context, opts PointerOptions) error {
	script, err := webkitPointerScript(opts)
	if err != nil {
		return err
	}
	_, err = o.evalBody(ctx, script)
	return err
}

func (o *nativePageOps) AssetInventory(ctx context.Context) (pageAssets, error) {
	var raw pageAssets
	if err := o.evalInto(ctx, "return "+assetInventoryExpression()+";", &raw); err != nil {
		return pageAssets{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	return raw, nil
}

// AssetFetcher reads assets THROUGH the page, which is what makes the page's
// own cookies and origin apply. WebKit has no out-of-band resource stream, so
// each asset crosses as base64 in one bounded read rather than as a stream.
func (o *nativePageOps) AssetFetcher(ctx context.Context) (assetFetcher, error) {
	return func(url string) (assetStream, error) {
		var payload struct {
			ContentType string `json:"contentType"`
			Base64      string `json:"base64"`
		}
		if err := o.evalInto(ctx, webkitAssetFetchScript(url, nativeMaxInPageAssetBytes), &payload); err != nil {
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
