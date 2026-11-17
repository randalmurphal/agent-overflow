package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// findGen numbers every find action and end across all pages, so the
	// order survives a page's suspend and restore (find.go).
	findGen atomic.Uint64
	// findPointHook, when set by a test, runs at the points inside a find
	// transition named by findPoint.
	findPointHook atomic.Pointer[func(point string)]
	// findWaitHook, when set by a test, runs with a find action's context
	// just before the action waits for the page lock, so a test can hold
	// the lock until the action is waiting on it or past its deadline.
	findWaitHook atomic.Pointer[func(ctx context.Context)]
	// findWork joins the goroutines that apply submitted find ends. Each is
	// admitted under findGate unless findClosed, which Close sets under it
	// before it waits, so no Add follows the Wait.
	findWork   sync.WaitGroup
	findGate   sync.Mutex
	findClosed bool
	// findTimeout is findActionTimeout; a test shortens it.
	findTimeout time.Duration
	profileDir  string

	startMu sync.Mutex
	mu      sync.Mutex
	config  Config
	closed  bool

	// scopes holds the open cookie jars by jarIdentity.key (jar.go).
	scopes map[string]*jarScope
	// authorities holds, per canonical workspace with a page in any jar, what
	// the workspace owns: its download directory and quota (jar.go).
	// Guarded by mu.
	authorities map[string]*workspaceAuthority
	// stateSeq numbers every thread state snapshot (CompanionEvent.Seq).
	// Guarded by mu.
	stateSeq uint64
	// suspended holds the pages whose engine page was unloaded (suspend.go),
	// by page id. They belong to their thread's tab set but hold no engine
	// resource. Guarded by mu.
	suspended map[string]*suspendedPage
	// recordsCleared counts forgetAllRecords calls, so a suspension that
	// began before its browser was turned off or its site data cleared
	// records nothing. Guarded by mu.
	recordsCleared uint64
	// pending holds the pages being created, and the failed creations of
	// tabs the user opened, by page id (lifecycle.go). Guarded by mu.
	pending map[string]*pendingPage
	// disposing holds, per jar key, the disposal in flight of a profile
	// releaseScopeLocked unregistered; the channel closes when it ends.
	// Guarded by mu.
	disposing map[string]chan struct{}
	// lifeWork joins the page closes and profile disposals that run after
	// their caller returned (goLife). Each is admitted under lifeGate unless
	// lifeClosed, which Close sets under it before it waits.
	lifeWork   sync.WaitGroup
	lifeGate   sync.Mutex
	lifeClosed bool
	// recordDir holds the durable copy of the suspended pages, one file per
	// thread (page_records.go). persistMu orders its writes: each takes its
	// snapshot under mu while holding persistMu, so an older snapshot never
	// replaces a newer one.
	recordDir string
	persistMu sync.Mutex
	idleTimer *time.Timer
	eventSink func(CompanionEvent)
	panes     map[string]paneMount
	// paneSeq numbers pane mounts in attach order (paneMount.seq).
	paneSeq  uint64
	sessions map[string]SessionInfo
	// recent is each thread's recent addresses, and recentGen their forget
	// generation (recent.go).
	recent    map[string][]RecentAddress
	recentGen uint64
	// viewportSyncs serializes viewport application per thread (viewport.go).
	viewportSyncs map[string]*viewportSync
	// presenting holds, per thread, whether a presentation pass runs and
	// whether another was asked for during it (syncPanePresentation); made
	// on first use, and an idle thread's entry is removed.
	presenting map[string]*presentSync
	// presented is, per thread, the page the last presentation pass showed
	// and the base it painted under it (presentPanes); a thread showing none
	// has no entry. Guarded by mu.
	presented map[string]presentedPage
	// awaitingLayout is, per thread, the active page the last presentation
	// pass held hidden because it lagged the thread viewport. Whichever
	// apply lays it out first (applyViewportLocked) asks for the pass that
	// shows it. Guarded by mu.
	awaitingLayout map[string]string
	// closedTabs is, per thread, the tabs the user closed that can be
	// reopened, most recent last (companion_tabs.go). Guarded by mu.
	closedTabs map[string]*closedTabs
	// agentLinger is how long a page keeps showing agent activity after the
	// last tool call on it ends (activity.go); a test shortens it.
	agentLinger    time.Duration
	artifactRoot   string
	artifactInitMu sync.Mutex
	artifactReady  bool
	artifactBytes  atomic.Int64
	// downloadsMu guards threadDownloads, each thread's downloads as its
	// pane lists them (downloads_thread.go). It is taken inside m.mu and
	// inside a page's downloadMu, and never held around either.
	downloadsMu     sync.Mutex
	threadDownloads map[string]*threadDownloadList
	downloadKeys    atomic.Uint64
	// downloadPushInterval overrides downloadPushInterval when set (tests).
	downloadPushInterval time.Duration
	// pageAdopted is a test seam for the asynchronous popup ownership handoff.
	// Production leaves it nil; the managed-Chrome integration test uses the
	// signal instead of polling on wall-clock sleeps.
	pageAdopted func()
	// heldInfo holds a CDP engine's newest report for each handle no page
	// answers to yet (updatePageInfo). Bounded: a handle no page ever takes,
	// a target closed before its adoption, ages out.
	heldInfo *boundedMap[string, heldInfo]
	// viewportApplied is a test seam for the asynchronous pane-size follow:
	// called after each drain pass lays the thread's pages out. Production
	// leaves it nil.
	viewportApplied func(threadID string)
	// pushCopied is a test seam: called, with m.mu held, once a push has
	// taken the unpushed records and copied the tabs. Production leaves it
	// nil.
	pushCopied func()
	// presentDeciding is a test seam: called, with no lock held, by a
	// presentation pass after it hid every page but its candidate and before
	// it decides whether to show it. Production leaves it nil.
	presentDeciding func(threadID string, candidate *managedPage)
	// infoRead is a test seam: called between a read of a page's info (or
	// history state) and its commit. Production leaves it nil.
	infoRead func(p *managedPage)
	// reopenRefused is a test seam: called between a reopen's refused open
	// and its putting the closed tab back (ReopenClosedTab). Production
	// leaves it nil.
	reopenRefused func()
	// revealFileInFileManager is the test seam over the production subprocess
	// hand-off in companion_reveal.go. Production leaves it nil.
	revealFileInFileManager func(ctx context.Context, path string) error
	// downloadAdmitting is a test seam run between a download start's page
	// lookup and its admission. Production leaves it nil.
	downloadAdmitting func()
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

type managedPage struct {
	id     string
	owner  string
	access Access
	driver pageDriver
	// jarGen is the generation of its thread's jar setting the page was
	// opened under, and authority its workspace's (jar.go). Set before the
	// page is registered; read under m.mu.
	jarGen    uint64
	authority *workspaceAuthority
	// ctx is the page's lifetime, taken from its driver. Operations are
	// bounded against it. cancelOps ends it before the driver closes, so a
	// close cancels the operations in flight (closePageAsync).
	ctx       context.Context
	cancelOps context.CancelFunc
	mu        sync.Mutex
	lastUse   atomic.Int64
	createdAt int64
	// tabOrder is the page's position key in the thread's tab strip: creation
	// time by default, renumbered by MoveCompanionPage. Atomic because tab
	// sorts run outside m.mu.
	tabOrder atomic.Int64
	metaMu   sync.RWMutex
	// info is what the page's tab shows; once the page is in its thread's
	// tabs only commitInfo changes it.
	info PageInfo
	// infoStamp issues the stamps of info writes; locationStamp and
	// historyStamp are the stamps of the writes that set those fields, and
	// infoReport is the order of the newest engine report committed, all
	// under metaMu (commitInfo).
	infoStamp, locationStamp, historyStamp uint64
	infoReport                             uint64
	// docTitle is the newest title the page's document reported of itself
	// and the address it was at (pageHooks.DocumentTitle), bounded as info
	// is, under metaMu (commitInfo).
	docTitle pageLocation
	// load is the page's loading state and favicon (pagestate.go), guarded
	// by metaMu.
	load        pageLoad
	logMu       sync.Mutex
	logs        []ConsoleLog
	clipboardMu sync.Mutex
	clipboard   []ClipboardItem
	downloadMu  sync.Mutex
	// downloadsEnded is set when the page stops being the Manager's
	// (pageLeftLocked); no download is admitted on it after. Guarded by
	// downloadMu.
	downloadsEnded bool
	downloadSeq    uint64
	downloads      []DownloadInfo
	downloadWait   chan struct{}
	assetMu        sync.Mutex
	inventories    map[string]AssetInventory
	assetOrder     []string
	nodeMu         sync.Mutex
	nodeRefs       map[string]nodeReference
	nodeOrder      []string
	// find is written only by a find transition, which holds mu (find.go);
	// findMu guards it for readers. findMu may be held around the Manager's
	// mu, never taken inside it. ends are the find ends submitted and not
	// yet applied; findBusy is whether a find transition is running.
	findMu   sync.Mutex
	find     findSession
	ends     findEnds
	findBusy atomic.Bool
	// incarnation tells this page apart from another under the same id: a
	// restored page reuses its id. docEpoch counts the documents its main
	// frame committed, stamped on the engine's own thread (find.go).
	incarnation uint64
	docEpoch    atomic.Uint64
	// userNav is the address-bar navigation running under mu, for the stop
	// button to mark (companion_navigate.go); navMu guards it.
	navMu   sync.Mutex
	userNav *userNavigation
	// viewport is the size the page last laid out at (packViewport), and
	// viewportFailed the size it last refused (viewport.go). A pane resize
	// lays out only the active page, so a hidden page lags its thread's
	// viewport until it is about to be shown or operated on.
	viewport       atomic.Uint64
	viewportFailed atomic.Uint64
	// activity is the agent tool calls acting on the page (activity.go).
	activity pageActivity
}

// pageIncarnations numbers every page the process creates or restores.
var pageIncarnations atomic.Uint64

// newManagedPage allocates the AO-owned half of a page. Its driver is attached
// once the engine has produced one, so the driver's own event handlers can
// already report into this page while it is being created.
func newManagedPage(access Access) *managedPage {
	p := &managedPage{
		id: uuid.NewString(), owner: access.ThreadID, access: access,
		createdAt:    time.Now().UnixNano(),
		downloadWait: make(chan struct{}), inventories: make(map[string]AssetInventory),
		nodeRefs:    make(map[string]nodeReference),
		incarnation: pageIncarnations.Add(1),
	}
	p.tabOrder.Store(p.createdAt)
	return p
}

func (p *managedPage) attach(driver pageDriver) {
	p.driver = driver
	p.ctx, p.cancelOps = context.WithCancel(driver.Lifetime())
}

func NewManager(configDir string, config Config, opts ManagerOptions) *Manager {
	m := &Manager{
		config:        config,
		profileDir:    filepath.Join(configDir, browserProfileDir),
		scopes:        make(map[string]*jarScope),
		suspended:     make(map[string]*suspendedPage),
		pending:       make(map[string]*pendingPage),
		heldInfo:      newBoundedMap[string, heldInfo](orderSlack),
		disposing:     make(map[string]chan struct{}),
		recordDir:     filepath.Join(configDir, browserPageRecordDir),
		panes:         make(map[string]paneMount),
		sessions:      make(map[string]SessionInfo),
		viewportSyncs: make(map[string]*viewportSync),
		artifactRoot:  filepath.Join(configDir, "browser-artifacts"),
		findTimeout:   findActionTimeout,
		closedTabs:    make(map[string]*closedTabs),
		agentLinger:   agentActivityLinger,
		recentGen:     uint64(time.Now().UnixNano()),
	}
	m.accelerators = opts.Accelerators
	m.engine = selectEngine(configDir, opts, engineEvents{
		PopupOpened:       m.adoptPopup,
		PageClosed:        m.removeClosedPage,
		PageCrashed:       m.pageCrashed,
		PageInfoChanged:   m.updatePageInfo,
		DownloadStarted:   m.downloadStarted,
		DownloadProgress:  m.downloadProgress,
		KeyChord:          m.keyChord,
		PageFocusReported: m.pageFocusReported,

		DownloadPolicyChanged: m.downloadPolicyChanged,
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
		// the other site-data mode would not be the page that was open. The
		// threads' jar settings stay unless the browser is off.
		return m.forgetAllPages(context.Background(), config.Enabled)
	}
	return nil
}
