package browser

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// restoreEngine is the fake engine with the two observation points the
// suspend tests need: how many engine pages were created, and a gate that
// holds a page's navigation until the test releases it.
type restoreEngine struct {
	*fakeEngine
	pagesCreated atomic.Int64
	mu           sync.Mutex
	// gate, when set, holds every navigation until it closes; navigating
	// receives one value per navigation that reached the gate.
	gate       chan struct{}
	navigating chan string
	// infoGate, when set, holds every address read the same way; reading
	// receives one value per read that reached it.
	infoGate chan struct{}
	reading  chan struct{}
	// lastPage is the engine page created last.
	lastPage *gatedPage
}

func newRestoreManager(t *testing.T, dir string, opts ManagerOptions) (*Manager, *restoreEngine) {
	t.Helper()
	opts.FakeEngine = true
	manager := NewManager(dir, Config{Enabled: true}, opts)
	t.Cleanup(func() { _ = manager.Close() })
	engine := &restoreEngine{fakeEngine: newFakeEngine(), navigating: make(chan string, 16), reading: make(chan struct{}, 16)}
	manager.engine = engine
	return manager, engine
}

func (e *restoreEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &restoreProfile{engineProfile: profile, engine: e}, nil
}

// hold makes every later navigation wait for release.
func (e *restoreEngine) hold() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.gate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// holdInfo makes every later address read wait for release.
func (e *restoreEngine) holdInfo() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.infoGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

type restoreProfile struct {
	engineProfile
	engine *restoreEngine
}

func (p *restoreProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	driver, err := p.engineProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	p.engine.pagesCreated.Add(1)
	page := &gatedPage{pageDriver: driver, engine: p.engine}
	p.engine.mu.Lock()
	p.engine.lastPage = page
	p.engine.mu.Unlock()
	return page, nil
}

type gatedPage struct {
	pageDriver
	engine *restoreEngine
}

func (p *gatedPage) Navigate(ctx context.Context, rawURL string) error {
	p.engine.mu.Lock()
	gate := p.engine.gate
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.navigating <- rawURL
		<-gate
	}
	return p.pageDriver.Navigate(ctx, rawURL)
}

func (p *gatedPage) Info(ctx context.Context) (string, string, error) {
	p.engine.mu.Lock()
	gate := p.engine.infoGate
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.reading <- struct{}{}
		<-gate
	}
	return p.pageDriver.Info(ctx)
}

func (p *gatedPage) fake() *fakePage { return p.pageDriver.(*fakePage) }

// liveFakePage answers the fake page behind a live page.
func liveFakePage(t *testing.T, m *Manager, access Access, pageID string) *fakePage {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatalf("page %s is not live: %v", pageID, err)
	}
	return p.driver.(*gatedPage).fake()
}

func suspendedIDs(m *Manager, threadID string) []string {
	var ids []string
	for _, page := range m.CompanionState(Caller{ThreadID: ThreadID(threadID)}).Pages {
		if page.Suspended {
			ids = append(ids, page.ID)
		}
	}
	return ids
}

func mustOpen(t *testing.T, m *Manager, access Access, rawURL string) PageInfo {
	t.Helper()
	info, err := m.Open(t.Context(), access, rawURL, OpenOptions{})
	if err != nil {
		t.Fatalf("open %s for %s: %v", rawURL, access.ThreadID(), err)
	}
	return info
}

func boolPtr(value bool) *bool { return &value }
