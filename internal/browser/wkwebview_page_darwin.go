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

// One WKWebView, driven entirely through JavaScript: the operations it
// shares with WebKitGTK are nativePageOps (native_page_ops.go), over the
// main-thread calls below (nativeView).
//
// Everything the Manager decides (ownership, bounds, node ids, artifact
// quotas) stays on its side of the seam. What lives here is only how an
// operation is carried out.

type wkPage struct {
	nativePageOps

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

	mu     sync.Mutex
	closed bool
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

// evaluate, canGo, isLoading and history are the page's nativeView: main
// thread calls on its view.
func (p *wkPage) evaluate(ctx context.Context, world, body string) (string, error) {
	return wkEvaluate(ctx, p.liveView(), world, body)
}

func (p *wkPage) canGo(forward bool) (bool, error) { return wkCanGo(p.liveView(), forward) }

func (p *wkPage) isLoading() (bool, error) { return wkIsLoading(p.liveView()) }

func (p *wkPage) history(step nativeHistoryStep) error {
	return wkHistory(p.liveView(), step)
}

// liveView is the view as an operation reaches it: refused once the page
// closed (wkView.do).
func (p *wkPage) liveView() wkView       { return wkView{ptr: p.view, life: p.ctx} }
func (p *wkPage) Handle() ids.PageHandle { return p.handle }

// OwnsFrame answers for this page's own handle: this engine addresses every
// event by page, so a page's only frame identity is itself.
func (p *wkPage) OwnsFrame(frame frameID) bool { return frame == frameID(p.handle) }

// ReadOnlyCaveat is non-empty because WKWebView has no equivalent of CDP's
// throwOnSideEffect: the expression is evaluated as written. The Manager carries
// this into the tool result so the difference is stated. It is the sentence
// the WebKitGTK driver returns.
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

func (p *wkPage) Navigate(ctx context.Context, target string) error {
	if err := p.load(target); err != nil {
		return err
	}
	return p.awaitLoad(ctx)
}

// load picks the one API that can actually open the URL. WKWebView refuses a
// file URL handed to -loadRequest: (the page loads blank with no error), so
// `browser_open_file` goes through -loadFileURL:allowingReadAccessToURL:.
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
