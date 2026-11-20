package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/browser/ids"
)

// A fake engine's page (fake_engine.go): it navigates, keeps a history and
// a title, finds in that title and lays out at a viewport, and refuses by
// name everything that needs real page content.

type fakePage struct {
	profile *fakeProfile
	hooks   pageHooks
	ctx     context.Context
	cancel  context.CancelFunc
	handle  ids.PageHandle
	// loadOrder numbers the loading reports (pageHooks.Loading).
	loadOrder atomic.Uint64

	mu      sync.Mutex
	url     string
	title   string
	history []string
	index   int
	closed  bool
	// viewports records every SetViewport, newest last, so a test can assert
	// what size the Manager laid the page out at.
	viewports [][2]int
	// screenshot, when set, answers Screenshot instead of the no-page error.
	screenshot func(context.Context) ([]byte, error)
	// snapshot, when set, answers Snapshot instead of the no-page error.
	snapshot func(context.Context) (Snapshot, error)
	// evaluate, when set, answers Evaluate instead of the no-page error.
	evaluate func(ctx context.Context, source string, readOnly bool) (json.RawMessage, error)
	// viewportErr, when set, makes SetViewport fail so a test can drive the
	// failure path.
	viewportErr error
	// viewportHook, when set, sees every SetViewport after it is recorded,
	// so a test can hold a layout in flight.
	viewportHook func(width, height int)
	// navigateHook, when set, runs inside Navigate after the address is
	// allowed and before the load, so a test can hold a navigation in flight
	// or fail it; an error fails the navigation.
	navigateHook func(ctx context.Context, rawURL string) error
	// stops counts the "stop" history actions the page received. stopHook,
	// when set, runs first; an error fails the stop.
	stops    int
	stopHook func() error
	// selection and selectionErr answer SelectionText. presses makes Press
	// accept a key, which it otherwise refuses for want of a page.
	selection    string
	selectionErr error
	presses      bool
	// findMode answers FindMode; FindModeBar unless a test changes it.
	findMode string
	// findQuery is the running search, "" when there is none.
	findQuery string
	// findHook, when set, sees every find call before it is answered:
	// "start <query>", "step", "back", or "stop".
	findHook func(call string)
	// findAnswer, when set, replaces the answer the title gives.
	findAnswer func(findResult) findResult
	// findErr, when set, fails every find call.
	findErr error
	// findStartErr, when set, fails FindStart alone, after it replaced the
	// search as a real engine's failed start can.
	findStartErr error
	// findStopErr, when set, is asked on each FindStop, with the stop's
	// context, whether it fails.
	findStopErr func(context.Context) error
}

func (p *fakePage) Lifetime() context.Context { return p.ctx }
func (p *fakePage) Handle() ids.PageHandle    { return p.handle }

// OwnsFrame answers for the page's handle, the one frame a fake page has.
func (p *fakePage) OwnsFrame(frame frameID) bool { return frame == frameID(p.handle) }

func (p *fakePage) Close() {
	p.mu.Lock()
	first := !p.closed
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	if !first {
		return
	}
	p.profile.forget(p)
}

func (p *fakePage) Info(context.Context) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url, p.title, nil
}

func (p *fakePage) Navigate(ctx context.Context, rawURL string) error {
	if p.hooks.Allow != nil && !p.hooks.Allow(rawURL) {
		return fmt.Errorf("browser: navigation to %s is not allowed", rawURL)
	}
	p.mu.Lock()
	hook := p.navigateHook
	p.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, rawURL); err != nil {
			return err
		}
	}
	if download, ok := fakeDownloadAddress(rawURL); ok {
		// A response a browser saves rather than shows: the page stays.
		// It begins in the page's one frame, its handle.
		return p.profile.download(frameID(p.handle), download)
	}
	p.mu.Lock()
	// A navigation truncates the forward entries AFTER the current one and
	// keeps everything up to and including it, exactly like a real session
	// history — dropping the current entry would make "back" impossible after
	// the second navigation.
	if len(p.history) == 0 {
		p.history = []string{rawURL}
	} else {
		p.history = append(p.history[:p.index+1], rawURL)
	}
	p.index = len(p.history) - 1
	// A move to a fragment of the same address stays in the document, as
	// in a real engine; anything else loads a new one.
	commits := fakeDocument(p.url) != fakeDocument(rawURL) || !strings.Contains(rawURL, "#")
	p.url, p.title = rawURL, fakeTitle(rawURL)
	p.mu.Unlock()
	if commits {
		p.reportLoading(true)
		p.commitDocument()
		p.reportLoading(false)
	}
	return nil
}

// reportLoading reports a load starting or stopping, as a real engine's
// loading signal does around each new document.
func (p *fakePage) reportLoading(loading bool) {
	if p.hooks.Loading != nil {
		p.hooks.Loading(loading, p.loadOrder.Add(1))
	}
}

// fakeDocument is an address without its fragment.
func fakeDocument(rawURL string) string {
	document, _, _ := strings.Cut(rawURL, "#")
	return document
}

// commitDocument reports a new document, as a real engine's commit signal
// does, delivering the Manager's work at once.
func (p *fakePage) commitDocument() {
	p.commitDocumentLate()()
}

// commitDocumentLate stamps a new document now and returns the delivery of
// the Manager's work, which a test runs later: an action on another
// goroutine can run between a real engine's stamp and its delivery.
func (p *fakePage) commitDocumentLate() (deliver func()) {
	if p.hooks.DocumentCommitted == nil {
		return func() {}
	}
	if finish := p.hooks.DocumentCommitted(); finish != nil {
		return finish
	}
	return func() {}
}

func (p *fakePage) HistoryState(context.Context) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.index > 0, p.index+1 < len(p.history), nil
}

func (p *fakePage) History(_ context.Context, action string) error {
	p.mu.Lock()
	switch action {
	case "back":
		if p.index <= 0 {
			p.mu.Unlock()
			return errors.New("browser: no previous history entry")
		}
		p.index--
	case "forward":
		if p.index+1 >= len(p.history) {
			p.mu.Unlock()
			return errors.New("browser: no forward history entry")
		}
		p.index++
	case "reload":
		// A reload loads the same address as a new document.
		p.mu.Unlock()
		p.reportLoading(true)
		p.commitDocument()
		p.reportLoading(false)
		return nil
	case "stop":
		hook := p.stopHook
		p.mu.Unlock()
		if hook != nil {
			if err := hook(); err != nil {
				return err
			}
		}
		p.mu.Lock()
		p.stops++
		p.mu.Unlock()
		p.reportLoading(false)
		return nil
	}
	p.url = p.history[p.index]
	p.title = fakeTitle(p.url)
	p.mu.Unlock()
	p.reportLoading(true)
	p.commitDocument()
	p.reportLoading(false)
	return nil
}

func (p *fakePage) PageStatus(context.Context) (pageStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return pageStatus{URL: p.url, Ready: "complete", NetworkIdle: true}, nil
}

func (p *fakePage) NavigationMark(context.Context) (navigationMark, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return navigationMark{URL: p.url, Loader: fmt.Sprint(p.index)}, nil
}

func (p *fakePage) Snapshot(ctx context.Context) (Snapshot, error) {
	p.mu.Lock()
	capture := p.snapshot
	p.mu.Unlock()
	if capture != nil {
		return capture(ctx)
	}
	return Snapshot{}, errFakeEngineHasNoPage
}

func (p *fakePage) Screenshot(ctx context.Context, _ ScreenshotOptions) ([]byte, error) {
	p.mu.Lock()
	capture := p.screenshot
	p.mu.Unlock()
	if capture != nil {
		return capture(ctx)
	}
	return nil, errFakeEngineHasNoPage
}

func (p *fakePage) Evaluate(ctx context.Context, source string, readOnly bool) (json.RawMessage, error) {
	p.mu.Lock()
	evaluate := p.evaluate
	p.mu.Unlock()
	if evaluate != nil {
		return evaluate(ctx, source, readOnly)
	}
	return nil, errFakeEngineHasNoPage
}

func (p *fakePage) ReadOnlyCaveat() string { return "" }

func (p *fakePage) ResolveLocator(context.Context, Locator, string) ([]LocatorMatch, error) {
	return nil, errFakeEngineHasNoPage
}

func (p *fakePage) ReadNode(context.Context, LocatorMatch, Locator, string, string) (any, error) {
	return nil, errFakeEngineHasNoPage
}

func (p *fakePage) ActOnNode(context.Context, LocatorMatch, Locator, nodeAction) error {
	return errFakeEngineHasNoPage
}

func (p *fakePage) ScrollNode(context.Context, nodeReference, float64, float64) error {
	return errFakeEngineHasNoPage
}

func (p *fakePage) Click(context.Context, string) error              { return errFakeEngineHasNoPage }
func (p *fakePage) Type(context.Context, string, string, bool) error { return errFakeEngineHasNoPage }
func (p *fakePage) Press(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.presses {
		return nil
	}
	return errFakeEngineHasNoPage
}

func (p *fakePage) TypeText(context.Context, string) error { return errFakeEngineHasNoPage }

func (p *fakePage) SelectionText(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selection, p.selectionErr
}

func (p *fakePage) Pointer(context.Context, PointerOptions) error { return errFakeEngineHasNoPage }

func (p *fakePage) Scroll(context.Context, string, float64, float64) error {
	return errFakeEngineHasNoPage
}

func (p *fakePage) WaitVisible(context.Context, string) error { return errFakeEngineHasNoPage }

func (p *fakePage) FindMode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.findMode
}

// findCall runs the test seams for one call and answers it from the title:
// case-insensitive, non-overlapping matches of the running query.
func (p *fakePage) findCall(call string) (findResult, error) {
	p.mu.Lock()
	hook, answer, err := p.findHook, p.findAnswer, p.findErr
	query, title := p.findQuery, p.title
	p.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if err != nil {
		return findResult{}, err
	}
	count := 0
	if query != "" {
		count = strings.Count(strings.ToLower(title), strings.ToLower(query))
	}
	result := findResult{Found: count > 0, Matches: count}
	if answer != nil {
		result = answer(result)
	}
	return result, nil
}

func (p *fakePage) FindStart(_ context.Context, query string) (findResult, error) {
	p.mu.Lock()
	p.findQuery = query
	startErr := p.findStartErr
	p.mu.Unlock()
	result, err := p.findCall("start " + query)
	if err == nil && startErr != nil {
		return findResult{}, startErr
	}
	return result, err
}

func (p *fakePage) FindStep(_ context.Context, backwards bool) (findResult, error) {
	p.mu.Lock()
	running := p.findQuery != ""
	p.mu.Unlock()
	if !running {
		return findResult{}, errors.New("browser: no find is running on the fake page")
	}
	if backwards {
		return p.findCall("back")
	}
	return p.findCall("step")
}

func (p *fakePage) FindStop(ctx context.Context) error {
	p.mu.Lock()
	hook, err, stopErr := p.findHook, p.findErr, p.findStopErr
	p.mu.Unlock()
	if err == nil && stopErr != nil {
		err = stopErr(ctx)
	}
	// A refused stop leaves the search where it was.
	if err == nil {
		p.mu.Lock()
		p.findQuery = ""
		p.mu.Unlock()
	}
	if hook != nil {
		hook("stop")
	}
	return err
}

// SetViewport records the size and succeeds unless viewportErr is set: a
// viewport is AO state the Manager applies to every page, and refusing it
// would fail page creation itself.
func (p *fakePage) SetViewport(_ context.Context, width, height int) error {
	p.mu.Lock()
	p.viewports = append(p.viewports, [2]int{width, height})
	err := p.viewportErr
	hook := p.viewportHook
	p.mu.Unlock()
	if hook != nil {
		hook(width, height)
	}
	return err
}

func (p *fakePage) AssetInventory(context.Context) (pageAssets, error) {
	return pageAssets{}, errFakeEngineHasNoPage
}

func (p *fakePage) AssetFetcher(context.Context) (assetFetcher, error) {
	return nil, errFakeEngineHasNoPage
}

// fakeTitle stands in for a document title so the pane's tab strip renders
// something a human can tell apart. It reads the URL, never a document.
func fakeTitle(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if name := path.Base(strings.TrimSuffix(parsed.Path, "/")); name != "" && name != "." && name != "/" {
		return name
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return rawURL
}
