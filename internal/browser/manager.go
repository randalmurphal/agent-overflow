package browser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"agent-overflow/internal/webview2host"

	"github.com/google/uuid"
)

// browserProfileDir is the AO-owned tree an engine keeps one workspace's site
// data under (spec §4). Clearing site data deletes it wholesale.
const browserProfileDir = "browser-profiles"

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
	engine     browserEngine
	profileDir string

	startMu sync.Mutex
	mu      sync.Mutex
	config  Config
	closed  bool

	scopes         map[string]*workspaceScope
	idleTimer      *time.Timer
	eventSink      func(CompanionEvent)
	panes          map[string]paneMount
	sessions       map[string]SessionInfo
	artifactRoot   string
	artifactInitMu sync.Mutex
	artifactReady  bool
	artifactBytes  atomic.Int64
	// pageAdopted is a test seam for the asynchronous popup ownership handoff.
	// Production leaves it nil; the managed-Chrome integration test uses the
	// signal instead of polling on wall-clock sleeps.
	pageAdopted func()
	// copyFileToOSClipboard is the test seam over the production subprocess
	// hand-off in companion_clipboard.go. Production leaves it nil.
	copyFileToOSClipboard func(ctx context.Context, path string) error
}

type ManagerOptions struct {
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
	workspace     string
	profile       engineProfile
	pages         map[string]*managedPage
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
	ctx          context.Context
	mu           sync.Mutex
	lastUse      atomic.Int64
	createdAt    int64
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
	return &managedPage{
		id: uuid.NewString(), owner: access.ThreadID, access: access,
		createdAt:    time.Now().UnixNano(),
		downloadWait: make(chan struct{}), inventories: make(map[string]AssetInventory),
		nodeRefs: make(map[string]nodeReference),
	}
}

func (p *managedPage) attach(driver pageDriver) {
	p.driver = driver
	p.ctx = driver.Lifetime()
}

func NewManager(configDir string, config Config, opts ManagerOptions) *Manager {
	m := &Manager{
		config:       config,
		profileDir:   filepath.Join(configDir, browserProfileDir),
		scopes:       make(map[string]*workspaceScope),
		panes:        make(map[string]paneMount),
		sessions:     make(map[string]SessionInfo),
		artifactRoot: filepath.Join(configDir, "browser-artifacts"),
	}
	m.engine = selectEngine(configDir, opts, engineEvents{
		PopupOpened:      m.adoptPopup,
		PageClosed:       m.removeClosedPage,
		PageInfoChanged:  m.updatePageInfo,
		DownloadStarted:  m.downloadStarted,
		DownloadProgress: m.downloadProgress,
	})
	pruneEncryptedCheckpoints(configDir)
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
		return m.closeBrowser(context.Background())
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
	p, err := m.pageForOpen(ctx, access, "")
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
	return m.navigate(ctx, access, (&url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}).String(), opts)
}

func (m *Manager) navigate(ctx context.Context, access Access, targetURL string, opts OpenOptions) (PageInfo, error) {
	p, err := m.pageForOpen(ctx, access, opts.PageID)
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

func (m *Manager) Pages(ctx context.Context, access Access) ([]PageInfo, error) {
	pages := m.ownedPages(access.ThreadID)
	m.mu.Lock()
	activePageID := m.sessionLocked(access.ThreadID).ActivePageID
	m.mu.Unlock()
	out := make([]PageInfo, 0, len(pages))
	for _, p := range pages {
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

func (m *Manager) ClosePage(ctx context.Context, access Access, pageID string) error {
	p, scope, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	m.cancelPageDownloads(p, scope)
	p.driver.Close()
	p.mu.Unlock()
	m.mu.Lock()
	delete(scope.pages, p.id)
	empty := len(scope.pages) == 0
	m.mu.Unlock()
	m.repairActivePage(access.ThreadID)
	m.emitThreadState(access.ThreadID)
	m.syncPanePresentation(access.ThreadID)
	if empty {
		return m.disposeScope(ctx, scope.workspace)
	}
	return nil
}

func (m *Manager) CloseThread(ctx context.Context, threadID string) error {
	pages := m.ownedPages(threadID)
	var errs []error
	for _, p := range pages {
		access := Access{ThreadID: threadID, Workspace: m.workspaceForPage(p.id)}
		if err := m.ClosePage(ctx, access, p.id); err != nil {
			errs = append(errs, err)
		}
	}
	m.mu.Lock()
	delete(m.sessions, threadID)
	m.mu.Unlock()
	return errors.Join(errs...)
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
	if err := m.closeBrowser(ctx); err != nil {
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

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.closeBrowser(context.Background())
}

func (m *Manager) pageForOpen(ctx context.Context, access Access, requested string) (*managedPage, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return nil, fmt.Errorf("browser: invalid caller scope")
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		p, _, err := m.lookupOwnedPage(access, requested)
		return p, err
	}
	return m.createPage(ctx, access)
}

func (m *Manager) createPage(ctx context.Context, access Access) (*managedPage, error) {
	if err := m.ensureStarted(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		m.scheduleIdleClose()
		return nil, err
	}
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		return nil, fmt.Errorf("browser: resolve workspace: %w", err)
	}
	access.Workspace = workspace

	m.startMu.Lock()
	defer m.startMu.Unlock()
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
	newScope := scope == nil
	if scope == nil && len(m.scopes) >= maxWorkspaceContexts {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace context limit reached (%d)", maxWorkspaceContexts)
	}
	if countOwnedPagesLocked(m.scopes, access.ThreadID) >= maxPagesPerThread {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for thread (%d)", maxPagesPerThread)
	}
	if scope != nil && len(scope.pages) >= maxPagesPerWorkspace {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for workspace (%d)", maxPagesPerWorkspace)
	}
	if countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: process page limit reached (%d)", maxPagesTotal)
	}
	m.mu.Unlock()

	if scope == nil {
		scope, err = m.createScope(workspace)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.scopes[workspace] = scope
		m.mu.Unlock()
	}

	abandon := func() {
		if newScope {
			_ = m.disposeScope(context.Background(), workspace)
		}
	}
	p := newManagedPage(access)
	p.info = PageInfo{ID: p.id, URL: "about:blank"}
	driver, err := scope.profile.NewPage(ctx, m.pageHooks(p))
	if err != nil {
		abandon()
		return nil, err
	}
	p.attach(driver)
	if err := m.applyConfiguredViewport(p); err != nil {
		driver.Close()
		abandon()
		return nil, err
	}
	p.touch()
	m.mu.Lock()
	scope.pages[p.id] = p
	m.mu.Unlock()
	m.pageChanged(p)
	return p, nil
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

func (m *Manager) createScope(workspace string) (*workspaceScope, error) {
	m.mu.Lock()
	persist := m.config.PersistSiteData
	m.mu.Unlock()
	digest := sha256.Sum256([]byte(workspace))
	downloadDir := filepath.Join(m.artifactRoot, "downloads", fmt.Sprintf("%x", digest[:12]))
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create download directory: %w", err)
	}
	profile, err := m.engine.NewProfile(context.Background(), profileOptions{Workspace: workspace, DownloadDir: downloadDir, Persist: persist})
	if err != nil {
		return nil, err
	}
	return &workspaceScope{workspace: workspace, profile: profile, pages: make(map[string]*managedPage), downloadDir: downloadDir}, nil
}

func (m *Manager) ensureStarted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
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
		for _, p := range candidate.pages {
			if p.driver.Handle() == popup.Opener {
				owner = p.owner
				access = p.access
				break
			}
		}
		break
	}
	tooMany := owner != "" && (countOwnedPagesLocked(m.scopes, owner) >= maxPagesPerThread || len(scope.pages) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal)
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
		return
	}
	p.attach(driver)
	if err := m.applyConfiguredViewport(p); err != nil {
		driver.Close()
		return
	}
	m.mu.Lock()
	current := m.scopes[scope.workspace]
	if current != scope || countOwnedPagesLocked(m.scopes, owner) >= maxPagesPerThread || len(scope.pages) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal {
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

func (m *Manager) removeClosedPage(handle string) {
	m.mu.Lock()
	var emptyWorkspace string
	var owner string
	for workspace, scope := range m.scopes {
		for id, p := range scope.pages {
			if p.driver.Handle() == handle {
				owner = p.owner
				delete(scope.pages, id)
				if len(scope.pages) == 0 {
					emptyWorkspace = workspace
				}
				break
			}
		}
		if emptyWorkspace != "" {
			break
		}
	}
	m.mu.Unlock()
	if owner != "" {
		m.emitThreadState(owner)
		m.syncPanePresentation(owner)
	}
	if emptyWorkspace != "" {
		_ = m.disposeScope(context.Background(), emptyWorkspace)
	}
}

func (m *Manager) disposeScope(ctx context.Context, workspace string) error {
	m.mu.Lock()
	scope := m.scopes[workspace]
	if scope == nil {
		m.mu.Unlock()
		return nil
	}
	delete(m.scopes, workspace)
	noScopes := len(m.scopes) == 0
	m.mu.Unlock()
	err := scope.profile.Dispose(ctx)
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
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Go(func() {
			if err := scope.profile.Dispose(closeCtx); err != nil {
				errs <- err
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
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
	m.idleTimer = time.AfterFunc(idleBrowserDelay, func() {
		m.mu.Lock()
		empty := len(m.scopes) == 0
		m.mu.Unlock()
		if empty {
			_ = m.closeBrowser(context.Background())
		}
	})
}
