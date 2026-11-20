//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/ids"
)

// One WebKitWebView, driven entirely through JavaScript. WebKitGTK exposes no
// remote-debugging protocol, so where the CDP driver sends a protocol command
// this one evaluates a function body built in webkitjs.go.
//
// Everything the Manager decides — ownership, bounds, node ids, artifact
// quotas — stays on its side of the seam. What lives here is only how an
// operation is carried out.

const (
	// webkitPollInterval is how often a load or visibility wait re-samples.
	// Each sample is one GTK main-thread dispatch, so this is a real cost and
	// not a free spin.
	webkitPollInterval = 50 * time.Millisecond

	// webkitNetworkQuiet is how long after the last load transition the page
	// counts as network-idle. WebKit reports no per-request lifecycle, so this
	// is the honest approximation of CDP's judgement.
	webkitNetworkQuiet = 500 * time.Millisecond

	// webkitMaxInPageAssetBytes bounds one asset read. WebKit has no
	// out-of-band resource stream, so an asset crosses as base64 inside a JSON
	// result — which is why this engine's per-asset ceiling is well below the
	// Manager's 128 MiB bundle cap rather than equal to it.
	webkitMaxInPageAssetBytes = 32 << 20

	// webkitScreenshotQuality matches what the CDP driver asks Chrome for, so
	// a screenshot does not change size or fidelity with the engine.
	webkitScreenshotQuality = 85
)

type webkitPage struct {
	engine  *webkitEngine
	profile *webkitProfile
	id      uint64
	handle  ids.PageHandle
	hooks   pageHooks
	view    unsafe.Pointer
	// popup is the handle the page was adopted under when WebKit opened it
	// as a popup, and opener the handle of the page that opened it; both
	// empty otherwise. Set before the view answers to the page.
	popup, opener ids.PageHandle

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	closed       bool
	lastActivity time.Time
	// findCount is the last search's answer, which its steps report.
	findCount findResult
	// loadOrder numbers the view's loading changes, on the GTK thread
	// (pageHooks.Loading).
	loadOrder atomic.Uint64
	// infoOrder numbers the view's address and title reports, on the GTK
	// thread (pageHooks.Info).
	infoOrder atomic.Uint64
}

func (p *webkitPage) Lifetime() context.Context { return p.ctx }

// liveView is the view as an operation reaches it: refused once the page
// closed (webkitView.do).
func (p *webkitPage) liveView() webkitView   { return webkitView{ptr: p.view, life: p.ctx} }
func (p *webkitPage) Handle() ids.PageHandle { return p.handle }

// OwnsFrame answers for this page's own handle: this engine addresses every
// event by page, so a page's only frame identity is itself.
func (p *webkitPage) OwnsFrame(frame frameID) bool { return frame == frameID(p.handle) }

// ReadOnlyCaveat is non-empty because WebKit has no equivalent of CDP's
// throwOnSideEffect: the expression is evaluated as written. The Manager
// carries this into the tool result so the difference is stated rather than
// silently assumed away.
func (p *webkitPage) ReadOnlyCaveat() string {
	return "Note: this browser engine cannot reject side effects before they happen. The expression was evaluated as written, so treat browser_evaluate_readonly here as read-only by convention, not by enforcement."
}

// discard unwinds a half-built page. Nothing GTK-side exists yet.
func (p *webkitPage) discard() {
	p.cancel()
	webkitPageByID.Delete(p.id)
	p.profile.mu.Lock()
	delete(p.profile.pages, p)
	p.profile.mu.Unlock()
}

func (p *webkitPage) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.discard()
	p.engine.forgetPane(p.id)
	webkitCloseView(p.view)
}

// noteLoad records that the page reported its address or title now. It is the
// bookkeeping half of the network-idle answer, and it runs on the GTK thread.
func (p *webkitPage) noteLoad() {
	p.mu.Lock()
	p.lastActivity = time.Now()
	p.mu.Unlock()
}

// consoleMessage decodes one entry from the injected capture script. A payload
// that is not the shape the script posts is not a console entry.
func (p *webkitPage) consoleMessage(payload string) {
	var entry struct {
		Level   string `json:"level"`
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	if json.Unmarshal([]byte(payload), &entry) != nil || p.hooks.Console == nil {
		return
	}
	url := entry.URL
	if url == "" && p.hooks.PageURL != nil {
		url = p.hooks.PageURL()
	}
	p.hooks.Console(ConsoleLog{
		Level: normalizeConsoleLevel(entry.Level), Message: entry.Message,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), URL: url,
	})
}

// bind ties one operation to BOTH the caller's bound context and the page's
// lifetime, so closing a page ends whatever it was doing.
func (p *webkitPage) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(p.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return merged, func() { stop(); cancel() }
}

// evalBody runs one async-function body in the page's script world and
// returns its raw JSON result.
func (p *webkitPage) evalBody(ctx context.Context, body string) (json.RawMessage, error) {
	return p.evalIn(ctx, "", body)
}

// evalIn is evalBody in the named isolated world, "" for the page's own.
func (p *webkitPage) evalIn(ctx context.Context, world, body string) (json.RawMessage, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	raw, err := gtkEvaluate(opCtx, p.liveView(), world, body)
	if err != nil {
		return nil, err
	}
	if raw == "" || raw == "null" {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// evalInto runs a body and decodes its result into out.
func (p *webkitPage) evalInto(ctx context.Context, body string, out any) error {
	raw, err := p.evalBody(ctx, body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (p *webkitPage) Info(ctx context.Context) (string, string, error) {
	var probe struct{ URL, Title string }
	if err := p.evalInto(ctx, webkitInfoScript, &probe); err != nil {
		return "", "", fmt.Errorf("browser: read page state: %w", err)
	}
	return probe.URL, probe.Title, nil
}

func (p *webkitPage) Navigate(ctx context.Context, url string) error {
	if err := webkitLoadURI(p.liveView(), url); err != nil {
		return err
	}
	return p.awaitLoad(ctx)
}

func (p *webkitPage) HistoryState(context.Context) (bool, bool, error) {
	return webkitCanGo(p.liveView(), false), webkitCanGo(p.liveView(), true), nil
}

func (p *webkitPage) History(ctx context.Context, action string) error {
	switch action {
	case "back":
		if !webkitCanGo(p.liveView(), false) {
			return fmt.Errorf("browser: no previous history entry")
		}
		if err := webkitHistory(p.liveView(), webkitHistoryBack); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "forward":
		if !webkitCanGo(p.liveView(), true) {
			return fmt.Errorf("browser: no forward history entry")
		}
		if err := webkitHistory(p.liveView(), webkitHistoryForward); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "reload":
		if err := webkitHistory(p.liveView(), webkitHistoryReload); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "stop":
		return webkitHistory(p.liveView(), webkitHistoryStop)
	}
	return fmt.Errorf("browser: unsupported history action %q", action)
}

// awaitLoad waits for the load to settle, the way chromedp.Navigate does. The
// first poll deliberately runs after a tick: WebKit has not necessarily entered
// the loading state by the time load_uri returns.
func (p *webkitPage) awaitLoad(ctx context.Context) error {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	ticker := time.NewTicker(webkitPollInterval)
	defer ticker.Stop()
	settled := 0
	for {
		select {
		case <-opCtx.Done():
			return opCtx.Err()
		case <-ticker.C:
			if webkitIsLoading(p.liveView()) {
				settled = 0
				continue
			}
			// Two consecutive quiet samples, so the gap between load_uri
			// returning and the load actually starting is not read as done.
			settled++
			if settled >= 2 {
				return nil
			}
		}
	}
}

func (p *webkitPage) PageStatus(ctx context.Context) (pageStatus, error) {
	var probe struct{ URL, Ready string }
	if err := p.evalInto(ctx, webkitPageStatusScript, &probe); err != nil {
		return pageStatus{}, err
	}
	p.mu.Lock()
	quiet := time.Since(p.lastActivity) >= webkitNetworkQuiet
	p.mu.Unlock()
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: quiet && !webkitIsLoading(p.liveView())}, nil
}

func (p *webkitPage) NavigationMark(ctx context.Context) (navigationMark, error) {
	var mark navigationMark
	if err := p.evalInto(ctx, webkitNavigationMarkScript, &mark); err != nil {
		return navigationMark{}, err
	}
	return mark, nil
}

func (p *webkitPage) Snapshot(ctx context.Context) (Snapshot, error) {
	var snapshot Snapshot
	if err := p.evalInto(ctx, "return "+snapshotExpression()+";", &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	return snapshot, nil
}

// Evaluate has no side-effect check to ask for; ReadOnlyCaveat says so.
func (p *webkitPage) Evaluate(ctx context.Context, source string, _ bool) (json.RawMessage, error) {
	return webkitEvaluate(ctx, p.evalBody, source)
}
