package browser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/keybindings"
	"agent-overflow/internal/webview2host"

	"github.com/google/uuid"
)

// browserProfileDir is the AO-owned tree an engine keeps one workspace's site
// data under (spec §4). Clearing site data deletes it wholesale.
const browserProfileDir = "browser-profiles"

// screenshotTimeout bounds the frame wait inside one capture. It is shorter
// than operationTimeout because a page that produces no frame is a fault to
// report, not a slow operation to wait out. A variable so a test can shorten
// the wait it asserts on.
var screenshotTimeout = 10 * time.Second

const (
	operationTimeout          = 30 * time.Second
	idleBrowserDelay          = 2 * time.Minute
	maxPagesPerThread         = 8
	maxPagesPerWorkspace      = 24
	maxPagesTotal             = 64
	maxWorkspaceContexts      = 12
	maxSnapshotText           = 100_000
	maxSnapshotElements       = 500
	maxEvaluateBytes          = 256_000
	maxScreenshotBytes        = 20 << 20
	maxFullScreenshotHeight   = 12_000
	maxFullScreenshotWidth    = 4_000
	maxConsoleEntries         = 500
	maxConsoleMessageBytes    = 16 << 10
	maxClipboardBytes         = 8 << 20
	maxDownloadBytes          = 512 << 20
	maxWorkspaceDownloadBytes = 2 << 30
	defaultViewportWidth      = 1280
	defaultViewportHeight     = 720
	maxBrowserURLBytes        = 64 << 10
	maxBrowserTitleBytes      = 4 << 10
	maxLocatorResultBytes     = 8 << 20
)

// Manager is the policy layer. It owns the page registry and its per-thread
// ownership, labels, session/visibility state, every cap and bound, artifact
// quotas, and the AO-managed per-tab clipboard. How an operation reaches a live
// page belongs to the engine behind `browserEngine` / `pageDriver` (driver.go).
type Manager struct {
	engine browserEngine
	// accelerators is ManagerOptions.Accelerators; read per keypress on the
	// engine's UI thread by keyChord.
	accelerators func() keybindings.AcceleratorSet
	profileDir   string

	startMu sync.Mutex
	mu      sync.Mutex
	config  Config
	closed  bool

	scopes map[string]*workspaceScope
	// suspended holds the pages whose engine page was unloaded (suspend.go),
	// by page id. They belong to their thread's tab set but hold no engine
	// resource. Guarded by mu.
	suspended map[string]*suspendedPage
	// recordsCleared counts forgetAllRecords calls, so a suspension that
	// began before its browser was turned off or its site data cleared
	// records nothing. Guarded by mu.
	recordsCleared uint64
	// recordDir holds the durable copy of the suspended pages, one file per
	// thread (page_records.go). persistMu orders its writes: each takes its
	// snapshot under mu while holding persistMu, so an older snapshot never
	// replaces a newer one.
	recordDir string
	persistMu sync.Mutex
	idleTimer *time.Timer
	eventSink func(CompanionEvent)
	panes     map[string]paneMount
	sessions  map[string]SessionInfo
	// viewportSyncs serializes viewport application per thread (viewport.go).
	viewportSyncs  map[string]*viewportSync
	artifactRoot   string
	artifactInitMu sync.Mutex
	artifactReady  bool
	artifactBytes  atomic.Int64
	// pageAdopted is a test seam for the asynchronous popup ownership handoff.
	// Production leaves it nil; the managed-Chrome integration test uses the
	// signal instead of polling on wall-clock sleeps.
	pageAdopted func()
	// viewportApplied is a test seam for the asynchronous pane-size follow:
	// called after each drain pass lays the thread's pages out. Production
	// leaves it nil.
	viewportApplied func(threadID string)
	// revealFileInFileManager is the test seam over the production subprocess
	// hand-off in companion_reveal.go. Production leaves it nil.
	revealFileInFileManager func(ctx context.Context, path string) error
}

type ManagerOptions struct {
	// Accelerators answers the effective bound-chord set, refreshed by the
	// App whenever the keybindings change. A chord in it pressed while a
	// page's native view has keyboard focus is taken from the page and
	// dispatched by the SPA (spec §7). Nil means no chord is ever taken.
	Accelerators func() keybindings.AcceleratorSet

	// FakeEngine selects the in-memory engine (fake_engine.go) whose pages
	// exist and navigate but render nothing. Set by the harness and soak
	// boots, which have to draw the pane's chrome and host rect on machines
	// with no display (spec §10). Takes precedence over every other wiring
	// fact, because an isolated boot must never reach a real engine.
	FakeEngine bool

	// PaneHost, when set, selects the launcher-hosted engine: pages become
	// WebView2 controllers in the Windows launcher, driven over CDP through
	// the relay tunnel (hosted_engine.go). Set on the Windows/WSL
	// deployment, where the launcher is what owns a window a browser view
	// can live in; nil everywhere else. Takes precedence over NativeWindow,
	// which that deployment never sets.
	PaneHost *PaneHostOptions

	// HeadlessChromium, when set, selects the headless Chromium engine:
	// pages become targets in a Chromium this process launches, one per
	// workspace profile (headless_engine.go). Set ONLY by the serve boot,
	// which has no window to host a browser view in and asks for this
	// engine on purpose. It is never inferred from the absence of a
	// window — that absence is exactly what leaves `go test` and a remote
	// `--connect` backend with no engine at all.
	HeadlessChromium *HeadlessChromiumOptions

	// KeepThread answers at boot whether a thread whose saved pages were
	// found still exists and is not archived. The pages of a thread it
	// rejects are removed; an error keeps them for the next boot. Nil keeps
	// every saved page.
	KeepThread func(threadID string) (bool, error)

	// NativeWindow returns the desktop window an in-process engine hosts its
	// views inside (spec docs/specs/embedded-browser.md §6), or nil when this
	// process has none — a remote `--connect` backend, a headless serve mode,
	// or a test. Nil, or a getter that answers nil, leaves the deployment
	// with NO engine. Platforms whose engine lives in another process ignore
	// it.
	NativeWindow func() unsafe.Pointer
}

// PaneHostOptions is what the hosted engine needs from the process around
// it. Both halves are supplied by internal/app: one emits on
// eventchan.BrowserHost, the other is the backend end of the launcher's
// CDP tunnel.
type PaneHostOptions struct {
	// Directive emits one browser:host frame to the launcher.
	Directive func(webview2host.Directive)
	// Relay reaches the pane environment's CDP endpoint through the tunnel.
	Relay CDPRelay
}

// CDPRelay is the Manager's view of internal/cdprelay.Endpoint. Narrow on
// purpose: the browser package must not learn what a tunnel, a listener or
// a WebSocket is.
type CDPRelay interface {
	BrowserWebSocketURL(ctx context.Context) (string, error)
}

type workspaceScope struct {
	workspace string
	profile   engineProfile
	pages     map[string]*managedPage
	// creating counts the pages createPage is creating in this scope. Guarded
	// by m.mu.
	creating      int
	downloadDir   string
	downloadBytes atomic.Int64
}

type managedPage struct {
	id     string
	owner  string
	access Access
	driver pageDriver
	// ctx is the page's lifetime, taken from its driver. Operations are
	// bounded against it.
	ctx       context.Context
	mu        sync.Mutex
	lastUse   atomic.Int64
	createdAt int64
	// tabOrder is the page's position key in the thread's tab strip: creation
	// time by default, renumbered by MoveCompanionPage. Atomic because tab
	// sorts run outside m.mu.
	tabOrder     atomic.Int64
	metaMu       sync.RWMutex
	info         PageInfo
	logMu        sync.Mutex
	logs         []ConsoleLog
	clipboardMu  sync.Mutex
	clipboard    []ClipboardItem
	downloadMu   sync.Mutex
	downloadSeq  uint64
	downloads    []DownloadInfo
	downloadWait chan struct{}
	assetMu      sync.Mutex
	inventories  map[string]AssetInventory
	assetOrder   []string
	nodeMu       sync.Mutex
	nodeRefs     map[string]nodeReference
	nodeOrder    []string
}

// newManagedPage allocates the AO-owned half of a page. Its driver is attached
// once the engine has produced one, so the driver's own event handlers can
// already report into this page while it is being created.
func newManagedPage(access Access) *managedPage {
	p := &managedPage{
		id: uuid.NewString(), owner: access.ThreadID, access: access,
		createdAt:    time.Now().UnixNano(),
		downloadWait: make(chan struct{}), inventories: make(map[string]AssetInventory),
		nodeRefs: make(map[string]nodeReference),
	}
	p.tabOrder.Store(p.createdAt)
	return p
}

func (p *managedPage) attach(driver pageDriver) {
	p.driver = driver
	p.ctx = driver.Lifetime()
}

func NewManager(configDir string, config Config, opts ManagerOptions) *Manager {
	m := &Manager{
		config:        config,
		profileDir:    filepath.Join(configDir, browserProfileDir),
		scopes:        make(map[string]*workspaceScope),
		suspended:     make(map[string]*suspendedPage),
		recordDir:     filepath.Join(configDir, browserPageRecordDir),
		panes:         make(map[string]paneMount),
		sessions:      make(map[string]SessionInfo),
		viewportSyncs: make(map[string]*viewportSync),
		artifactRoot:  filepath.Join(configDir, "browser-artifacts"),
	}
	m.accelerators = opts.Accelerators
	m.engine = selectEngine(configDir, opts, engineEvents{
		PopupOpened:      m.adoptPopup,
		PageClosed:       m.removeClosedPage,
		PageInfoChanged:  m.updatePageInfo,
		DownloadStarted:  m.downloadStarted,
		DownloadProgress: m.downloadProgress,
		KeyChord:         m.keyChord,
	})
	pruneEncryptedCheckpoints(configDir)
	m.loadPageRecords(opts.KeepThread)
	return m
}

// pruneEncryptedCheckpoints deletes the AES-GCM site-data checkpoints and the
// key file the pre-engine browser wrote (spec §4). They hold cookies and
// localStorage for a Chrome that no longer exists and cannot be imported into
// an engine profile, so first boot of this code is where they stop existing.
// Best-effort by design: a checkpoint we cannot unlink is unreadable anyway.
func pruneEncryptedCheckpoints(configDir string) {
	_ = os.RemoveAll(filepath.Join(configDir, "browser-state"))
	_ = os.Remove(filepath.Join(configDir, "browser-state.key"))
}

// Available reports whether this deployment has a browser engine at all. It is
// the question the App answers before offering a thread the browser MCP
// server: a windowless deployment gets no browser tools rather than 28 tools
// that all refuse (spec §9).
func (m *Manager) Available() bool {
	_, none := m.engine.(unavailableEngine)
	return !none
}

// ReportPaneHost routes one launcher report (created / create-failed /
// closed / process-failed) to the hosted engine. The App's
// BrowserHostReport binding is the only caller: the report arrives over
// the notification bridge, and the Manager is the one object that knows
// which engine is live.
//
// No policy crosses here. The engine settles its own create waiter and
// reports a closed page back through engineEvents, where the Manager
// applies the same registry rules a Chrome target destruction would.
func (m *Manager) ReportPaneHost(pageID string, kind webview2host.ReportKind, detail string) error {
	if err := webview2host.ValidatePageID(pageID); err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	if !webview2host.ValidKind(kind) {
		return fmt.Errorf("browser: unknown pane host report kind %q", kind)
	}
	host, ok := m.engine.(*hostedEngine)
	if !ok {
		return errors.New("browser: this deployment has no pane host")
	}
	host.Report(pageID, kind, webview2host.TruncateDetail(detail))
	return nil
}

func (m *Manager) SetEventSink(sink func(CompanionEvent)) {
	m.mu.Lock()
	m.eventSink = sink
	m.mu.Unlock()
}

func (m *Manager) Reconfigure(config Config) error {
	m.mu.Lock()
	changedPersistence := m.config.PersistSiteData != config.PersistSiteData
	m.config = config
	m.mu.Unlock()
	if !config.Enabled || changedPersistence {
		// The pages close as they always have, suspended ones included: a
		// disabled browser keeps no browser state, and a page restored into
		// the other site-data mode would not be the page that was open.
		return errors.Join(m.forgetAllRecords(), m.closeBrowser(context.Background()))
	}
	return nil
}

func (m *Manager) Open(ctx context.Context, access Access, rawURL string, opts OpenOptions) (PageInfo, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return PageInfo{}, fmt.Errorf("browser: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return PageInfo{}, fmt.Errorf("browser: URL scheme %q is not allowed; use browser_open_file for local files", parsed.Scheme)
	}
	if parsed.Host == "" {
		return PageInfo{}, fmt.Errorf("browser: URL host is required")
	}
	return m.navigate(ctx, access, parsed.String(), opts)
}

func (m *Manager) NewPage(ctx context.Context, access Access) (PageInfo, error) {
	p, err := m.pageForOpen(ctx, access, "", false)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	defer cancel()
	info, err := m.pageInfo(opCtx, p)
	if err == nil {
		p.setInfo(info)
		info = p.cachedInfo()
		m.pageChanged(p)
	}
	return info, err
}

func (m *Manager) OpenFile(ctx context.Context, access Access, path string, opts OpenOptions) (PageInfo, error) {
	resolved, err := m.authorizeFile(access, path)
	if err != nil {
		return PageInfo{}, err
	}
	// The URL must name the file as the RENDERER sees it. An engine whose
	// renderer is on the other side of a machine boundary answers through
	// engineFileURL; everywhere else the backend path is the renderer path.
	fileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}).String()
	if engine, ok := m.engine.(engineFileURL); ok {
		if fileURL, err = engine.FileURL(ctx, resolved); err != nil {
			return PageInfo{}, err
		}
	}
	return m.navigate(ctx, access, fileURL, opts)
}

func (m *Manager) navigate(ctx context.Context, access Access, targetURL string, opts OpenOptions) (PageInfo, error) {
	p, err := m.pageForOpen(ctx, access, opts.PageID, true)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if err := p.driver.Navigate(opCtx, targetURL); err != nil {
		return PageInfo{}, err
	}
	p.touch()
	info, err := m.pageInfo(opCtx, p)
	if err == nil {
		p.setInfo(info)
		info = p.cachedInfo()
	}
	m.pageChanged(p)
	return info, err
}

// Pages lists the thread's pages, suspended ones included as they were saved:
// listing is not a touch, so it restores nothing.
func (m *Manager) Pages(ctx context.Context, access Access) ([]PageInfo, error) {
	tabs := m.threadTabs(access.ThreadID)
	m.mu.Lock()
	activePageID := m.sessionLocked(access.ThreadID).ActivePageID
	m.mu.Unlock()
	out := make([]PageInfo, 0, len(tabs))
	for _, tab := range tabs {
		p := tab.live
		if p == nil {
			info := tab.info
			info.Selected = info.ID == activePageID
			info.LastOpened = time.Unix(0, tab.lastUse).UTC().Format(time.RFC3339Nano)
			out = append(out, info)
			continue
		}
		p.mu.Lock()
		opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
		info, err := m.pageInfo(opCtx, p)
		cancel()
		p.mu.Unlock()
		if err == nil {
			info.Selected = p.id == activePageID
			info.LastOpened = time.Unix(0, p.lastUse.Load()).UTC().Format(time.RFC3339Nano)
			p.setInfo(info)
			info = p.cachedInfo()
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastOpened > out[j].LastOpened })
	return out, nil
}

// ClosePage closes one of the caller's pages. A suspended page has no engine
// page to close; closing it forgets its record.
func (m *Manager) ClosePage(ctx context.Context, access Access, pageID string) error {
	err := m.closeLivePage(ctx, access, pageID)
	if errors.Is(err, errPageNotFound) {
		return m.forgetRecord(access.ThreadID, pageID)
	}
	return err
}

// closeLivePage closes a live page. It answers errPageNotFound when the page
// is not live.
func (m *Manager) closeLivePage(ctx context.Context, access Access, pageID string) error {
	p, scope, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	return m.closeFoundPage(ctx, access, p, scope)
}

// closeFoundPage closes a page a lookup found live. It answers
// errPageNotFound when the page stopped being live before its lock was
// taken: suspended meanwhile, it is now a record for ClosePage to forget.
func (m *Manager) closeFoundPage(ctx context.Context, access Access, p *managedPage, scope *workspaceScope) error {
	p.mu.Lock()
	m.mu.Lock()
	registered := scope.pages[p.id] == p
	m.mu.Unlock()
	if !registered {
		p.mu.Unlock()
		return errPageNotFound
	}
	m.cancelPageDownloads(p, scope)
	p.driver.Close()
	p.mu.Unlock()
	m.mu.Lock()
	delete(scope.pages, p.id)
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	m.repairActivePage(access.ThreadID)
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	if release {
		return m.disposeScope(ctx, scope)
	}
	return nil
}

// CloseThread closes every page the thread owns, including a popup one of
// them opens while it runs, forgets its suspended pages and their saved copy,
// and forgets the thread's browser session. A page the engine closed
// meanwhile counts as closed. A workspace profile that fails to dispose once
// its last page is gone is logged rather than returned: the thread's pages
// are closed, and the profile is no longer registered for a retry to
// dispose. A saved copy that cannot be removed is logged the same way: its
// pages are gone from memory, and the next boot removes the saved copy of a
// deleted or archived thread.
//
// The records go first, so no restore can register a page after the loop
// below has closed the thread's live ones.
func (m *Manager) CloseThread(ctx context.Context, threadID string) error {
	if err := m.forgetThreadRecords(threadID); err != nil {
		log.Printf("browser: close thread %s: %v", threadID, err)
	}
	for pages := m.ownedPages(threadID); len(pages) > 0; pages = m.ownedPages(threadID) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("browser: close thread pages: %w", err)
		}
		for _, p := range pages {
			access := Access{ThreadID: threadID, Workspace: m.workspaceForPage(p.id)}
			if err := m.closeLivePage(ctx, access, p.id); err != nil && !errors.Is(err, errPageNotFound) {
				log.Printf("browser: close thread %s: %v", threadID, err)
			}
		}
	}
	m.mu.Lock()
	delete(m.sessions, threadID)
	m.mu.Unlock()
	return nil
}

// ClearSiteData closes every engine page first, then deletes the site data
// (spec §4). The order is load-bearing: an engine still holding a profile open
// would write its cookie jar back out over the cleared state.
//
// Two halves, because two kinds of engine exist: the AO-owned profile tree is
// deleted here (WebKitGTK keeps its data under it), and an engine whose data
// lives somewhere this process cannot reach by path — the launcher's WebView2
// user-data folder, WebKit's own macOS data-store directory — implements
// engineSiteData and clears its own. Both halves run; a Settings button that
// silently clears nothing on some platforms is not an option.
func (m *Manager) ClearSiteData(ctx context.Context) error {
	if err := errors.Join(m.forgetAllRecords(), m.closeBrowser(ctx)); err != nil {
		return err
	}
	var errs []error
	if err := os.RemoveAll(m.profileDir); err != nil {
		errs = append(errs, fmt.Errorf("browser: clear site data: %w", err))
	}
	if engine, ok := m.engine.(engineSiteData); ok {
		if err := engine.ClearSiteData(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close shuts the manager down. The pages open at that moment are saved as
// suspended pages first, so they come back in their threads after a restart.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return errors.Join(m.saveOpenPages(), m.closeBrowser(context.Background()))
}

// pageForOpen answers the page an open navigates: the requested one, or a new
// page. A suspended page asked for by a caller about to navigate it is
// restored blank (skipLoad), since loading its old address first is wasted.
func (m *Manager) pageForOpen(ctx context.Context, access Access, requested string, skipLoad bool) (*managedPage, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return nil, fmt.Errorf("browser: invalid caller scope")
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		p, _, err := m.resolvePage(ctx, access, requested, skipLoad)
		return p, err
	}
	return m.createPage(ctx, access)
}

func (m *Manager) createPage(ctx context.Context, access Access) (*managedPage, error) {
	p := newManagedPage(access)
	p.info = PageInfo{ID: p.id, URL: "about:blank"}
	// One startMu hold spans the engine start and the page's registration, so
	// the idle close cannot stop the engine in between.
	m.startMu.Lock()
	defer m.startMu.Unlock()
	scope, err := m.openPageLocked(ctx, access, p, true)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	scope.creating--
	scope.pages[p.id] = p
	m.mu.Unlock()
	m.pageChanged(p)
	return p, nil
}

// openPageLocked starts the engine if it is not running, applies the page
// caps, and creates p's engine page in the scope of access's workspace, laid
// out at its thread's viewport. It returns with the scope reserved for p
// (scope.creating): the caller registers p or abandons it (abandonPage). The
// per-thread cap is skipped for a restore, whose record already holds its
// thread's slot. The caller holds startMu.
func (m *Manager) openPageLocked(ctx context.Context, access Access, p *managedPage, threadCap bool) (*workspaceScope, error) {
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		return nil, fmt.Errorf("browser: resolve workspace: %w", err)
	}
	access.Workspace = workspace
	p.access = access
	if err := m.startLocked(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		m.scheduleIdleClose()
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: manager closed")
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	scope := m.scopes[workspace]
	if scope == nil && len(m.scopes) >= maxWorkspaceContexts {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace context limit reached (%d)", maxWorkspaceContexts)
	}
	if threadCap && m.threadPageCountLocked(access.ThreadID) >= maxPagesPerThread {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for thread (%d); close a page first", maxPagesPerThread)
	}
	if scope != nil && scopeLoadLocked(scope) >= maxPagesPerWorkspace {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for workspace (%d)", maxPagesPerWorkspace)
	}
	if countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: process page limit reached (%d)", maxPagesTotal)
	}
	// The reservation keeps a concurrent close of the scope's last page from
	// disposing the profile this page is being created in.
	if scope != nil {
		scope.creating++
	}
	m.mu.Unlock()

	if scope == nil {
		scope, err = m.createScope(access)
		if err != nil {
			// The engine may be running for this page alone.
			m.scheduleIdleClose()
			return nil, err
		}
		scope.creating = 1
		m.mu.Lock()
		m.scopes[workspace] = scope
		m.mu.Unlock()
	}

	if err := m.newPage(ctx, scope, p); err != nil {
		m.mu.Lock()
		scope.creating--
		release := m.releaseScopeLocked(scope)
		m.mu.Unlock()
		if release {
			err = errors.Join(err, m.disposeScope(context.Background(), scope))
		}
		return nil, err
	}
	return scope, nil
}

// abandonPage closes a page openPageLocked created that will not be
// registered, ends its reservation, and disposes the scope if that left it
// empty.
func (m *Manager) abandonPage(scope *workspaceScope, p *managedPage) error {
	p.driver.Close()
	m.mu.Lock()
	scope.creating--
	release := m.releaseScopeLocked(scope)
	m.mu.Unlock()
	if release {
		return m.disposeScope(context.Background(), scope)
	}
	return nil
}

// newPage creates p's engine page in scope, laid out at its thread's
// viewport.
func (m *Manager) newPage(ctx context.Context, scope *workspaceScope, p *managedPage) error {
	driver, err := scope.profile.NewPage(ctx, m.pageHooks(p))
	if err != nil {
		return err
	}
	p.attach(driver)
	if err := m.applyViewport(p); err != nil {
		driver.Close()
		return err
	}
	p.touch()
	return nil
}

// pageHooks binds one page's AO-owned state to the engine events its driver
// reports. Nothing here decides ownership or limits.
func (m *Manager) pageHooks(p *managedPage) pageHooks {
	return pageHooks{
		Console: p.appendLog,
		PageURL: func() string { return p.cachedInfo().URL },
		Allow:   func(rawURL string) bool { return m.navigationAllowed(p.access, rawURL) },
	}
}

// createScope creates the profile for access's canonical workspace. The
// profile's navigation policy is the workspace's: a workspace belongs to one
// project, so access's roots are those of every thread with a page in it.
func (m *Manager) createScope(access Access) (*workspaceScope, error) {
	m.mu.Lock()
	persist := m.config.PersistSiteData
	m.mu.Unlock()
	workspace := access.Workspace
	digest := sha256.Sum256([]byte(workspace))
	downloadDir := filepath.Join(m.artifactRoot, "downloads", fmt.Sprintf("%x", digest[:12]))
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create download directory: %w", err)
	}
	roots := Access{Workspace: workspace, ProjectRoot: access.ProjectRoot}
	profile, err := m.engine.NewProfile(context.Background(), profileOptions{
		Workspace: workspace, DownloadDir: downloadDir, Persist: persist,
		Allow: func(rawURL string) bool { return m.navigationAllowed(roots, rawURL) },
	})
	if err != nil {
		return nil, err
	}
	return &workspaceScope{workspace: workspace, profile: profile, pages: make(map[string]*managedPage), downloadDir: downloadDir}, nil
}

// startLocked starts the engine unless it is running. The caller holds
// startMu.
func (m *Manager) startLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("browser: manager closed")
	}
	if !m.config.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("browser: tools are disabled")
	}
	m.mu.Unlock()
	if m.engine.Running() {
		return nil
	}
	return m.engine.Start(ctx)
}

// adoptPopup takes ownership of a page the engine opened by itself. The opener
// decides the thread; this manager's own limits decide whether it survives.
func (m *Manager) adoptPopup(popup enginePopup) {
	m.mu.Lock()
	var scope *workspaceScope
	var owner string
	var access Access
	for _, candidate := range m.scopes {
		if candidate.profile.Handle() != popup.Profile {
			continue
		}
		scope = candidate
		if opener := pageWithHandle(candidate, popup.Opener); opener != nil {
			owner, access = opener.owner, opener.access
		}
		break
	}
	tooMany := owner != "" && (m.threadPageCountLocked(owner) >= maxPagesPerThread || scopeLoadLocked(scope) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal)
	m.mu.Unlock()
	if scope == nil || owner == "" || tooMany {
		m.engine.DiscardPage(popup.Handle)
		return
	}

	p := newManagedPage(access)
	p.info = PageInfo{ID: p.id, URL: truncateUTF8(popup.URL, maxBrowserURLBytes), Title: truncateUTF8(popup.Title, maxBrowserTitleBytes)}
	p.touch()
	driver, err := scope.profile.AttachPage(context.Background(), popup.Handle, m.pageHooks(p))
	if err != nil {
		log.Printf("browser: adopt popup %s: %v", popup.Handle, err)
		m.engine.DiscardPage(popup.Handle)
		return
	}
	p.attach(driver)
	if err := m.applyViewport(p); err != nil {
		log.Printf("browser: size popup %s: %v", popup.Handle, err)
		driver.Close()
		return
	}
	m.mu.Lock()
	// The opener may have closed meanwhile, with its thread's other pages.
	current := m.scopes[scope.workspace]
	if current != scope || pageWithHandle(scope, popup.Opener) == nil || m.threadPageCountLocked(owner) >= maxPagesPerThread || scopeLoadLocked(scope) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		driver.Close()
		return
	}
	scope.pages[p.id] = p
	m.mu.Unlock()
	m.pageChanged(p)
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
}

// pageWithHandle returns the scope's page whose driver has handle, or nil.
// The caller holds m.mu.
func pageWithHandle(scope *workspaceScope, handle string) *managedPage {
	for _, p := range scope.pages {
		if p.driver.Handle() == handle {
			return p
		}
	}
	return nil
}

func (m *Manager) removeClosedPage(handle string) {
	m.mu.Lock()
	var owner string
	var released *workspaceScope
	for _, scope := range m.scopes {
		if p := pageWithHandle(scope, handle); p != nil {
			owner = p.owner
			delete(scope.pages, p.id)
			if m.releaseScopeLocked(scope) {
				released = scope
			}
			break
		}
	}
	m.mu.Unlock()
	if owner != "" {
		m.repairActivePage(owner)
		m.emitThreadState(owner)
		m.syncPanePresentation(owner)
	}
	if released != nil {
		if err := m.disposeScope(context.Background(), released); err != nil {
			log.Printf("browser: dispose the profile of %s: %v", released.workspace, err)
		}
	}
}

// releaseScopeLocked unregisters scope when it has no page and none is being
// created, and reports whether the caller must dispose it. Deciding and
// unregistering under one m.mu hold means a concurrent createPage either
// reserved the scope first or creates a new one, and a scope that is no
// longer registered is never disposed again. The caller holds m.mu.
func (m *Manager) releaseScopeLocked(scope *workspaceScope) bool {
	if len(scope.pages) > 0 || scope.creating > 0 || m.scopes[scope.workspace] != scope {
		return false
	}
	delete(m.scopes, scope.workspace)
	return true
}

// disposeScope disposes a scope releaseScopeLocked unregistered and starts
// the idle close once no scope remains.
func (m *Manager) disposeScope(ctx context.Context, scope *workspaceScope) error {
	err := scope.profile.Dispose(ctx)
	m.mu.Lock()
	noScopes := len(m.scopes) == 0
	m.mu.Unlock()
	if noScopes {
		m.scheduleIdleClose()
	}
	return err
}

func (m *Manager) closeBrowser(caller context.Context) error {
	// Release an in-flight engine start first: it holds startMu, and this
	// shutdown needs it.
	m.engine.Interrupt()
	m.startMu.Lock()
	defer m.startMu.Unlock()
	return m.closeBrowserLocked(caller)
}

// closeIdleBrowser is the idle timer's close. It decides under startMu, which
// createPage holds from the engine start to the page's registration, so a
// page being created keeps the engine running.
func (m *Manager) closeIdleBrowser() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	idle := len(m.scopes) == 0
	m.mu.Unlock()
	if !idle {
		return
	}
	if err := m.closeBrowserLocked(context.Background()); err != nil {
		log.Printf("browser: idle close: %v", err)
	}
}

// closeBrowserLocked disposes every profile and stops the engine. The caller
// holds startMu.
func (m *Manager) closeBrowserLocked(caller context.Context) error {
	m.mu.Lock()
	scopes := make([]*workspaceScope, 0, len(m.scopes))
	owners := make(map[string]struct{})
	for _, scope := range m.scopes {
		scopes = append(scopes, scope)
		for _, p := range scope.pages {
			owners[p.owner] = struct{}{}
		}
	}
	m.scopes = make(map[string]*workspaceScope)
	for owner := range owners {
		info := m.sessionLocked(owner)
		info.ActivePageID = ""
		info.Visible = false
		info.UpdatedAt = time.Now()
		m.sessions[owner] = info
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	m.mu.Unlock()
	for owner := range owners {
		m.emitThreadState(owner)
	}
	closeCtx, closeCancel := context.WithTimeout(caller, 20*time.Second)
	defer closeCancel()
	errs := make(chan error, len(scopes))
	done := make(chan struct{})
	if ui, ok := m.engine.(engineUIThread); ok && ui.OnUIThread() {
		// The caller is the UI thread itself (Wails runs ServiceShutdown on it
		// and blocks it). Every native call below is a dispatch to that thread:
		// inline from here, a deadlock-until-timeout from a goroutine. So
		// sequential, on this thread, bounded by the engine and not the clock.
		for _, scope := range scopes {
			if err := scope.profile.Dispose(closeCtx); err != nil {
				errs <- err
			}
		}
		close(done)
	} else {
		var wg sync.WaitGroup
		for _, scope := range scopes {
			wg.Go(func() {
				if err := scope.profile.Dispose(closeCtx); err != nil {
					errs <- err
				}
			})
		}
		go func() { wg.Wait(); close(done) }()
	}
	timedOut := false
	select {
	case <-done:
	case <-closeCtx.Done():
		timedOut = true
	}
	m.engine.Stop()
	if timedOut {
		// Engine teardown releases any command that was still waiting.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	joined := make([]error, 0, len(errs)+1)
	for len(errs) > 0 {
		err := <-errs
		joined = append(joined, err)
	}
	if timedOut {
		joined = append(joined, closeCtx.Err())
	} else if caller.Err() != nil {
		joined = append(joined, caller.Err())
	}
	return errors.Join(joined...)
}

func (m *Manager) scheduleIdleClose() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(idleBrowserDelay, m.closeIdleBrowser)
}
