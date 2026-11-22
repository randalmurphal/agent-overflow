//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/ids"
)

// One WebKitWebView, driven entirely through JavaScript: the operations it
// shares with WKWebView are nativePageOps (native_page_ops.go), over the GTK
// calls below (nativeView).
//
// Everything the Manager decides (ownership, bounds, node ids, artifact
// quotas) stays on its side of the seam. What lives here is only how an
// operation is carried out.

type webkitPage struct {
	nativePageOps

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

	mu     sync.Mutex
	closed bool
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

// evaluate, canGo, isLoading and history are the page's nativeView: GTK
// calls on its view.
func (p *webkitPage) evaluate(ctx context.Context, world, body string) (string, error) {
	return gtkEvaluate(ctx, p.liveView(), world, body)
}

func (p *webkitPage) canGo(forward bool) (bool, error) { return webkitCanGo(p.liveView(), forward) }

func (p *webkitPage) isLoading() (bool, error) { return webkitIsLoading(p.liveView()) }

func (p *webkitPage) history(step nativeHistoryStep) error {
	return webkitHistory(p.liveView(), step)
}

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

func (p *webkitPage) Navigate(ctx context.Context, url string) error {
	if err := webkitLoadURI(p.liveView(), url); err != nil {
		return err
	}
	return p.awaitLoad(ctx)
}
