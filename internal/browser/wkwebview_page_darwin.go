//go:build darwin && cgo && !ios && !server && !nogui

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/ids"
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
	handle  ids.PageHandle
	hooks   pageHooks
	view    unsafe.Pointer
	slot    int
	// popup is the handle the page was adopted under when WebKit opened it
	// as a popup, and opener the handle of the page that opened it; both
	// empty otherwise. Set before the view answers to the page.
	popup, opener ids.PageHandle

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	closed       bool
	lastURL      string
	lastActivity time.Time
	// findQuery is the running find's query, which a step searches again.
	findQuery string
	// snapshotMu serializes the resize/capture/restore a full-document
	// screenshot performs, so two captures cannot restore each other's size.
	snapshotMu sync.Mutex
	// loadOrder numbers the view's loading changes, on the main thread
	// (pageHooks.Loading).
	loadOrder atomic.Uint64
	// infoOrder numbers the view's address and title reports, on the main
	// thread (pageHooks.Info).
	infoOrder atomic.Uint64
}

func (p *wkPage) Lifetime() context.Context { return p.ctx }

// liveView is the view as an operation reaches it: refused once the page
// closed (wkView.do).
func (p *wkPage) liveView() wkView       { return wkView{ptr: p.view, life: p.ctx} }
func (p *wkPage) Handle() ids.PageHandle { return p.handle }

// OwnsFrame answers for this page's own handle: this engine addresses every
// event by page, so a page's only frame identity is itself.
func (p *wkPage) OwnsFrame(frame frameID) bool { return frame == frameID(p.handle) }

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

// evalBody runs one async-function body in the page's content world and
// returns its raw JSON result.
func (p *wkPage) evalBody(ctx context.Context, body string) (json.RawMessage, error) {
	return p.evalIn(ctx, "", body)
}

// evalIn is evalBody in the named isolated world, "" for the page's own.
func (p *wkPage) evalIn(ctx context.Context, world, body string) (json.RawMessage, error) {
	opCtx, cancel := p.bind(ctx)
	defer cancel()
	raw, err := wkEvaluate(opCtx, p.liveView(), world, body)
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
		return wkLoadURI(p.liveView(), target)
	}
	path := parsed.Path
	if path == "" {
		return fmt.Errorf("browser: file URL has no path")
	}
	return wkLoadFile(p.liveView(), path, filepath.Dir(path))
}

func (p *wkPage) HistoryState(context.Context) (bool, bool, error) {
	return wkCanGo(p.liveView(), false), wkCanGo(p.liveView(), true), nil
}

func (p *wkPage) History(ctx context.Context, action string) error {
	switch action {
	case "back":
		if !wkCanGo(p.liveView(), false) {
			return fmt.Errorf("browser: no previous history entry")
		}
		if err := wkHistory(p.liveView(), wkHistoryBack); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "forward":
		if !wkCanGo(p.liveView(), true) {
			return fmt.Errorf("browser: no forward history entry")
		}
		if err := wkHistory(p.liveView(), wkHistoryForward); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "reload":
		if err := wkHistory(p.liveView(), wkHistoryReload); err != nil {
			return err
		}
		return p.awaitLoad(ctx)
	case "stop":
		return wkHistory(p.liveView(), wkHistoryStop)
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
			if wkIsLoading(p.liveView()) {
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
	return pageStatus{URL: probe.URL, Ready: probe.Ready, NetworkIdle: quiet && !wkIsLoading(p.liveView())}, nil
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

// Evaluate has no side-effect check to ask for; ReadOnlyCaveat says so.
func (p *wkPage) Evaluate(ctx context.Context, source string, _ bool) (json.RawMessage, error) {
	return webkitEvaluate(ctx, p.evalBody, source)
}
