package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"agent-overflow/internal/browser/ids"
)

// cdpPage drives one Chrome target. It owns the CDP bookkeeping the tools need
// (the frame set download events are routed by, the in-flight request set the
// network-idle wait reads) and nothing about ownership, limits, or AO state.
type cdpPage struct {
	ctx    context.Context
	cancel context.CancelFunc
	handle ids.PageHandle
	hooks  pageHooks

	frames  *frameTracker
	network *networkTracker
	loads   cdpLoadTracker

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

// startCDPPage attaches chromedp to the page of pageCtx and installs its
// handlers. setup bounds the attach: its end cancels the page until the
// driver is returned, so a browser that leaves a command of the attach
// unanswered fails the page rather than holding its caller.
func startCDPPage(setup, controller, pageCtx context.Context, pageCancel context.CancelFunc, hooks pageHooks) (pageDriver, error) {
	stop := context.AfterFunc(setup, pageCancel)
	driver, err := attachCDPPage(controller, pageCtx, pageCancel, hooks)
	if !stop() {
		// The setup ended first, and its cancel ended the page.
		return nil, fmt.Errorf("browser: attach the page: %w", context.Cause(setup))
	}
	return driver, err
}

func attachCDPPage(controller, pageCtx context.Context, pageCancel context.CancelFunc, hooks pageHooks) (pageDriver, error) {
	if err := chromedp.Do(pageCtx); err != nil {
		pageCancel()
		// A dead controller is the usual reason a target never attaches, and
		// its own error is what makes the failure diagnosable.
		return nil, fmt.Errorf("browser: create page: %w (controller: %v)", err, controller.Err())
	}
	p := &cdpPage{
		ctx: pageCtx, cancel: pageCancel, hooks: hooks,
		handle:  ids.PageHandle(chromedp.FromContext(pageCtx).Target.TargetID),
		frames:  newFrameTracker(),
		network: newNetworkTracker(time.Now()),
	}
	if err := p.installHandlers(); err != nil {
		pageCancel()
		return nil, err
	}
	return p, nil
}

// cdpPageSetups is the page attaches under way on one CDP engine (the
// setup of startCDPPage). Each is bounded, and the engine's Interrupt ends
// them all, so a shutdown never waits behind a browser that stopped
// answering a page's attach.
type cdpPageSetups struct {
	mu      sync.Mutex
	next    uint64
	cancels map[uint64]context.CancelFunc
}

// begin answers the setup context of one attach, ctx bounded by timeout and
// ended by interrupt, and its release, which the caller calls once the
// attach returned.
func (s *cdpPageSetups) begin(ctx context.Context, timeout time.Duration) (context.Context, func()) {
	setup, cancel := context.WithTimeout(ctx, timeout)
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = make(map[uint64]context.CancelFunc)
	}
	s.next++
	id := s.next
	s.cancels[id] = cancel
	s.mu.Unlock()
	return setup, func() {
		s.mu.Lock()
		delete(s.cancels, id)
		s.mu.Unlock()
		cancel()
	}
}

// interrupt ends every attach under way.
func (s *cdpPageSetups) interrupt() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, cancel := range s.cancels {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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

// Document titles on the CDP engines.
//
// Chromium's Target.targetInfoChanged reports a navigation's commit, with
// the placeholder title the browser holds before the document has one (the
// address without its scheme), and nothing when the document then sets its
// title, whether its <title> is parsed or a script assigns document.title.
// Both WebView2 and headless Chromium behave so (observed 2026-11-16), and
// the Page domain has no title event. So each document reports its own:
// a watcher installed in an isolated world of every new document sends
// [location.href, document.title] through a binding exposed only to that
// world, deduplicated and at most once per documentTitleIntervalMs. A
// frame's document is not the tab's and reports nothing. Page scripts see
// neither the binding nor the watcher, and nothing a page does to events
// stops it: it starts when it is evaluated, before the document has a root.
//
// The title element it follows is the one document.title reads: under an
// SVG root, the root's first SVG title child; under any other root, the
// document's first HTML title element in tree order, wherever it is (an
// inline SVG's title is not one). It observes the nodes a title change
// passes through and nothing else: the Document's own children (a root
// replaced, or removed by document.open), the root's children, the head's
// children, and that title element's subtree. Each observed mutation and
// each report looks the title element up again and re-points the
// observations when the root, the head or the title element changed; the
// lookup over the document runs only then, so a body's mutations cost
// nothing. The stated limit: a title element a script inserts into or
// removes from the body after load, while nothing observed changes, is not
// seen until the next observed change or report; an explicit read
// (pageDriver.Info) asks document.title and is right. The Navigation API's
// entry change reports a same-document address change. A document still
// loading with no title yet sends nothing first; it is looked at again each
// interval, so an untitled page reports once it has loaded. Each report is
// one event and no round trip.
const (
	documentTitleWorld      = "ao-document-title"
	documentTitleBinding    = "__aoDocumentTitle"
	documentTitleIntervalMs = 250
)

// documentTitleWatcher is the isolated-world script. It cuts what it sends
// at the Manager's bounds counted in UTF-16 units, so a page cannot push an
// unbounded payload through the relay; commitInfo then bounds the bytes.
// The world's __aoDocumentTitleWatch counts its observer callbacks.
var documentTitleWatcher = fmt.Sprintf(`(() => {
  const send = globalThis[%[1]q];
  if (typeof send !== "function" || globalThis.__aoDocumentTitleWatch || window.top !== window) return;
  const watch = globalThis.__aoDocumentTitleWatch = {callbacks: 0};
  const svg = "http://www.w3.org/2000/svg";
  const htmlTitles = document.getElementsByTagNameNS("http://www.w3.org/1999/xhtml", "title");
  let sent = "", last = 0, timer = 0, root = null, head = null, title = null;
  const flush = () => {
    timer = 0;
    point();
    const now = String(document.title);
    if (!sent && !now && document.readyState === "loading") {
      timer = setTimeout(flush, %[4]d);
      return;
    }
    const report = JSON.stringify([String(location.href).slice(0, %[2]d), now.slice(0, %[3]d)]);
    if (report === sent) return;
    sent = report;
    last = Date.now();
    send(report);
  };
  const changed = () => {
    if (!timer) timer = setTimeout(flush, Math.max(0, last + %[4]d - Date.now()));
  };
  const titleOf = (r) => {
    if (!r) return null;
    if (r.namespaceURI !== svg) return htmlTitles[0] || null;
    for (const child of r.children) if (child.localName === "title" && child.namespaceURI === svg) return child;
    return null;
  };
  const observer = new MutationObserver(() => {
    watch.callbacks++;
    point();
    changed();
  });
  const point = () => {
    const r = document.documentElement;
    const h = r && r.namespaceURI !== svg ? document.head : null;
    const t = titleOf(r);
    if (r === root && h === head && t === title) return;
    root = r, head = h, title = t;
    observer.disconnect();
    observer.observe(document, {childList: true});
    if (r) observer.observe(r, {childList: true});
    if (h) observer.observe(h, {childList: true});
    if (t) observer.observe(t, {childList: true, characterData: true, subtree: true});
  };
  observer.observe(document, {childList: true});
  point();
  if (globalThis.navigation) navigation.addEventListener("currententrychange", changed);
  changed();
})()`, documentTitleBinding, maxBrowserURLBytes, maxBrowserTitleBytes, documentTitleIntervalMs)

// watchDocumentTitle installs the title watcher and routes its reports to
// hooks.DocumentTitle. The binding and the script are both registered for
// documents still to come and run on the current one, so a page attached
// after its load reports too.
func (p *cdpPage) watchDocumentTitle() error {
	if p.hooks.DocumentTitle == nil {
		return nil
	}
	ctx := p.ctx
	onTargetEvent(ctx, cdpruntime.BindingCalled, log.Printf, func(event cdpruntime.EventBindingCalled) {
		if event.Name != documentTitleBinding {
			return
		}
		var report []string
		if err := json.Unmarshal([]byte(event.Payload), &report); err != nil || len(report) != 2 {
			return
		}
		p.hooks.DocumentTitle(report[0], report[1])
	})
	if _, err := chromedp.Call(ctx, cdpruntime.AddBinding, cdpruntime.AddBindingParams{
		Name: documentTitleBinding, ExecutionContextName: documentTitleWorld,
	}); err != nil {
		return fmt.Errorf("browser: install the document title binding: %w", err)
	}
	if _, err := chromedp.Call(ctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{
		Source: documentTitleWatcher, WorldName: documentTitleWorld, RunImmediately: new(true),
	}); err != nil {
		return fmt.Errorf("browser: install the document title watcher: %w", err)
	}
	return nil
}

// reportLoading hands a loading change to the Manager.
func (p *cdpPage) reportLoading(loading bool, order uint64) {
	if p.hooks.Loading != nil {
		p.hooks.Loading(loading, order)
	}
}

func (p *cdpPage) Lifetime() context.Context { return p.ctx }
func (p *cdpPage) Handle() ids.PageHandle    { return p.handle }
func (p *cdpPage) Close()                    { p.cancel() }

// OwnsFrame answers from the frames the page has reported, and for its
// main frame, whose id is the target id, before it has reported any: a
// download can begin in a navigation that never commits.
func (p *cdpPage) OwnsFrame(frame frameID) bool {
	if frame == frameID(p.handle) {
		return true
	}
	return p.frames.owns(cdp.FrameID(frame))
}
