package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/browser/ids"
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

// fakeEngine opens no page of its own and loses none: every page it holds is
// one the Manager asked for and one the Manager closes. Its one engine fact
// is a download (fake_downloads.go), reported through events when the
// Manager wired them (selectEngine).
type fakeEngine struct {
	mu      sync.Mutex
	running bool
	seq     atomic.Uint64
	events  engineEvents
	// downloadReleased is a test seam run between the taking of a
	// download's end (fakeEnding) and its terminal report (finishHeld,
	// reportCancel), where a native engine releases its record. Nil
	// outside tests.
	downloadReleased func(id downloadID)
	// saving is the engine this fake stands in for: downloadsStaged, the
	// zero value, writes into the profile's directory for the Manager to
	// publish, as WebKitGTK does; downloadsAsArtifacts as headless Chromium
	// does; downloadsSavedByEngine saves into the user's Downloads folder
	// itself, as WebView2 does. Set by tests before the first profile.
	saving downloadSaving
	// beforeFinishReport is a test seam run after a download's file is
	// complete and before its finish is reported, with that file's path:
	// where an engine's report waits in its queue. Nil outside tests.
	beforeFinishReport func(file string)
	// beforeNewPage is a test seam run as a profile starts making a page,
	// before it holds the profile. Nil outside tests.
	beforeNewPage func()
	// beforePageClose is a test seam run as a page's first Close starts,
	// before the page is gone. Nil outside tests.
	beforePageClose func()
}

// DownloadSaving is the engine the fake stands in for (saving).
func (e *fakeEngine) DownloadSaving() downloadSaving { return e.saving }

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
func (e *fakeEngine) DiscardPage(context.Context, ids.PageHandle) error { return nil }

func (e *fakeEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	if !e.Running() {
		return nil, errors.New("browser: engine unavailable")
	}
	return &fakeProfile{
		engine: e, handle: profileHandle(fmt.Sprintf("fake-profile-%d", e.seq.Add(1))), jar: opts.Jar, ephemeral: opts.Ephemeral, allow: opts.Allow,
		downloadDir: opts.DownloadDir, stage: opts.StageDownload,
	}, nil
}

type fakeProfile struct {
	engine *fakeEngine
	handle profileHandle
	// jar, ephemeral, allow, downloadDir and stage are what the profile
	// was created with.
	jar         jarKey
	ephemeral   bool
	allow       func(owners []frameID, rawURL string) (allowed, resolved bool)
	downloadDir string
	stage       func(id downloadID) (string, error)

	mu       sync.Mutex
	disposed bool
	pages    map[*fakePage]struct{}
	// held are the downloads left running (fake_downloads.go), by id.
	held map[downloadID]fakeDownload
}

func (p *fakeProfile) Handle() profileHandle { return p.handle }

func (p *fakeProfile) NewPage(_ context.Context, hooks pageHooks) (pageDriver, error) {
	if hook := p.engine.beforeNewPage; hook != nil {
		hook()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disposed {
		return nil, errors.New("browser: workspace profile is closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	page := &fakePage{
		profile: p, hooks: hooks, ctx: ctx, cancel: cancel,
		handle:   ids.PageHandle(fmt.Sprintf("fake-page-%d", p.engine.seq.Add(1))),
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
func (p *fakeProfile) AttachPage(context.Context, ids.PageHandle, pageHooks) (pageDriver, error) {
	return nil, errors.New("browser: the fake browser engine opens no pages of its own")
}

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
	return p.disposeDownloads()
}

func (p *fakeProfile) forget(page *fakePage) {
	p.mu.Lock()
	delete(p.pages, page)
	p.mu.Unlock()
}
