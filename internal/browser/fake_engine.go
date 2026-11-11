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
)

// The fake engine (spec §10). Pages exist, navigate, carry a URL and a title,
// and close — everything the Manager's POLICY is made of and everything the
// pane's chrome renders from — with no browser behind them.
//
// It is production code, not a test fixture, because the harness and soak
// boots are production boot modes that must render the pane's tab strip,
// address bar and host rect on machines with no display (`make harness`,
// `make e2e`). The Manager's own policy tests use the same engine, so there is
// one fake rather than two.
//
// What it deliberately does NOT do is stand in for a renderer. Anything that
// needs real page content — snapshot, screenshot, evaluation, locators, input
// — refuses by name, so a test that thinks it is driving a page fails loudly
// instead of asserting against invented content. Find searches the one text a
// fake page has, the title its tab shows, so the pane's find bar has an
// answer to render.

var errFakeEngineHasNoPage = errors.New("browser: the fake browser engine renders no page content")

// fakeEngine takes no engineEvents: it opens no page of its own, loses none,
// and downloads nothing, so it has no fact to report. Every page it holds is
// one the Manager asked for and one the Manager closes.
type fakeEngine struct {
	mu      sync.Mutex
	running bool
	seq     atomic.Uint64
}

func newFakeEngine() *fakeEngine { return &fakeEngine{} }

func (e *fakeEngine) Start(context.Context) error {
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	return nil
}

func (e *fakeEngine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

func (e *fakeEngine) Interrupt() {}

func (e *fakeEngine) Stop() {
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
}

// DiscardPage has nothing to discard: this engine opens no page on its own, so
// the Manager is never offered one to decline.
func (e *fakeEngine) DiscardPage(string) {}

func (e *fakeEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, errors.New("browser: engine unavailable")
	}
	return &fakeProfile{engine: e, handle: fmt.Sprintf("fake-profile-%d", e.seq.Add(1)), workspace: opts.Workspace}, nil
}

type fakeProfile struct {
	engine    *fakeEngine
	handle    string
	workspace string

	mu       sync.Mutex
	disposed bool
	pages    map[*fakePage]struct{}
}

func (p *fakeProfile) Handle() string { return p.handle }

func (p *fakeProfile) NewPage(_ context.Context, hooks pageHooks) (pageDriver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return nil, errors.New("browser: workspace profile is closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	page := &fakePage{
		profile: p, hooks: hooks, ctx: ctx, cancel: cancel,
		handle:   fmt.Sprintf("fake-page-%d", p.engine.seq.Add(1)),
		url:      "about:blank",
		findMode: FindModeBar,
	}
	if p.pages == nil {
		p.pages = make(map[*fakePage]struct{})
	}
	p.pages[page] = struct{}{}
	return page, nil
}

// AttachPage is unreachable: this engine reports no popups, so a driver for a
// page nobody created would be worse than a loud failure.
func (p *fakeProfile) AttachPage(context.Context, string, pageHooks) (pageDriver, error) {
	return nil, errors.New("browser: the fake browser engine opens no pages of its own")
}

func (p *fakeProfile) CancelDownload(string) {}

func (p *fakeProfile) Dispose(context.Context) error {
	p.mu.Lock()
	pages := make([]*fakePage, 0, len(p.pages))
	for page := range p.pages {
		pages = append(pages, page)
	}
	p.pages = nil
	p.disposed = true
	p.mu.Unlock()
	for _, page := range pages {
		page.Close()
	}
	return nil
}

func (p *fakeProfile) forget(page *fakePage) {
	p.mu.Lock()
	delete(p.pages, page)
	p.mu.Unlock()
}

type fakePage struct {
	profile *fakeProfile
	hooks   pageHooks
	ctx     context.Context
	cancel  context.CancelFunc
	handle  string

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
	// evaluate, when set, answers Evaluate instead of the no-page error.
	evaluate func(ctx context.Context, source string, readOnly bool) (json.RawMessage, error)
	// viewportErr, when set, makes SetViewport fail so a test can drive the
	// failure path.
	viewportErr error
	// selection answers SelectionText.
	selection string
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
}

func (p *fakePage) Lifetime() context.Context { return p.ctx }
func (p *fakePage) Handle() string            { return p.handle }

// OwnsFrame answers for the page's handle, the one frame a fake page has.
func (p *fakePage) OwnsFrame(frame string) bool { return frame == p.handle }

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

func (p *fakePage) Navigate(_ context.Context, rawURL string) error {
	if p.hooks.Allow != nil && !p.hooks.Allow(rawURL) {
		return fmt.Errorf("browser: navigation to %s is not allowed", rawURL)
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
		p.commitDocument()
	}
	return nil
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
// the Manager's work, which a test runs later: the order a real engine's
// goroutine can land in.
func (p *fakePage) commitDocumentLate() (deliver func()) {
	if p.hooks.DocumentCommitted == nil {
		return func() {}
	}
	if finish := p.hooks.DocumentCommitted(); finish != nil {
		return finish
	}
	return func() {}
}

// SameDocumentNavigation changes the address the way history.pushState
// does: no new document. A test seam.
func (p *fakePage) SameDocumentNavigation(rawURL string) {
	p.mu.Lock()
	p.url, p.title = rawURL, fakeTitle(rawURL)
	p.mu.Unlock()
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
		p.commitDocument()
		return nil
	case "stop":
		p.mu.Unlock()
		return nil
	}
	p.url = p.history[p.index]
	p.title = fakeTitle(p.url)
	p.mu.Unlock()
	p.commitDocument()
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

func (p *fakePage) Snapshot(context.Context) (Snapshot, error) {
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
func (p *fakePage) Press(context.Context, string) error              { return errFakeEngineHasNoPage }
func (p *fakePage) TypeText(context.Context, string) error           { return errFakeEngineHasNoPage }
func (p *fakePage) SelectionText(context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selection
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
	p.mu.Unlock()
	return p.findCall("start " + query)
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

func (p *fakePage) FindStop(context.Context) error {
	p.mu.Lock()
	hook, err := p.findHook, p.findErr
	p.findQuery = ""
	p.mu.Unlock()
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
	p.mu.Unlock()
	return err
}

func (p *fakePage) lastViewport() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.viewports) == 0 {
		return 0, 0
	}
	last := p.viewports[len(p.viewports)-1]
	return last[0], last[1]
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
