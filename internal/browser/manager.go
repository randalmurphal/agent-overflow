package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/browser/quota"
	"agent-overflow/internal/browser/turnorder"
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
	operationTimeout     = 30 * time.Second
	idleBrowserDelay     = 2 * time.Minute
	maxPagesPerThread    = 8
	maxPagesPerWorkspace = 24
	// maxPagesTotal is the launcher's bound on its controllers too, so a
	// page the launcher refuses is one the Manager would refuse.
	maxPagesTotal             = webview2host.MaxHostPages
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
	scopes map[jarKey]*jarScope
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
	suspended map[PageID]*suspendedPage
	// forgetting counts the forgets of every page running (beginForget),
	// during which no page becomes a record. Guarded by mu.
	forgetting int
	// threads is each thread's current browser lifetime, which owns the
	// thread's browser state (thread_life.go). Guarded by mu.
	threads map[ThreadID]*threadBrowser
	// pending holds the pages being created, and the failed creations of
	// tabs the user opened, by page id (lifecycle.go). Guarded by mu.
	pending map[PageID]*pendingPage
	// disposing holds, per jar key, the disposal in flight of a profile
	// releaseScopeLocked unregistered; the channel closes when it ends.
	// Guarded by mu.
	disposing map[jarKey]chan struct{}
	// retiring holds the teardown of each thread lifetime CloseThread
	// detached, until it has finished (thread_close.go). Guarded by mu.
	retiring map[*threadBrowser]*threadTeardown
	// lifeWork joins the work that runs after its caller returned (goLife):
	// page closes and profile disposals, what a page's reported close or
	// crash does (pageEndQueue), a popup's adoption. Each is admitted under
	// lifeGate unless lifeClosed, which Close sets under it before it waits.
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
	panes     map[MountID]paneMount
	// paneSeq numbers pane mounts in attach order (paneMount.seq).
	paneSeq uint64
	// recentGen is the forget generation of the threads' recent addresses,
	// and recentLaunch this Manager's identity the generation's token
	// carries (recent.go).
	recentGen    uint64
	recentLaunch string
	// viewportSyncs serializes viewport application per thread (viewport.go).
	viewportSyncs map[ThreadID]*viewportSync
	// presenting holds, per thread, whether a presentation pass runs and
	// whether another was asked for during it (syncPanePresentation); made
	// on first use, and an idle thread's entry is removed.
	presenting map[ThreadID]*presentSync
	// presented is, per thread, the page the last presentation pass showed
	// (presentPanes); a thread showing none has no entry. Guarded by mu.
	presented map[ThreadID]PageID
	// awaitingLayout is, per thread, the active page the last presentation
	// pass held hidden because it lagged the thread viewport. Whichever
	// apply lays it out first (applyViewportLocked) asks for the pass that
	// shows it. Guarded by mu.
	awaitingLayout map[ThreadID]PageID
	// agentLinger is how long a page keeps showing agent activity after the
	// last tool call on it ends (activity.go); a test shortens it.
	agentLinger    time.Duration
	artifactRoot   string
	artifactInitMu sync.Mutex
	artifactReady  bool
	// artifactDir is the artifact root held open (artifacts.go), and
	// artifactsClosed whether Close let it go. artifactsClosed is guarded
	// by artifactInitMu.
	artifactDir     atomic.Pointer[os.Root]
	artifactsClosed bool
	artifactBytes   *quota.Pool
	// workspacePools hands each workspace its one download pool
	// (workspaceAuthority).
	workspacePools *quota.Registry
	// downloadsMu guards each thread lifetime's downloads as its pane lists
	// them (threadBrowser.downloads, downloads_thread.go). It is taken
	// inside m.mu and inside a page's download log hold, and never held
	// around either. downloadGens is the last list generation taken
	// (threadDownloadList.gen), guarded by it too.
	downloadsMu  sync.Mutex
	downloadGens uint64
	downloadKeys atomic.Uint64
	// staged is each running staged download's file by engine id
	// (stageDownload in downloads.go). Guarded by stagedMu, which is taken
	// alone.
	stagedMu sync.Mutex
	staged   map[downloadID]string
	// publishing counts the finished downloads being moved into the
	// Downloads folder; publishIdle, when set, closes as it reaches zero.
	// Close waits for them (waitPublishes). Guarded by publishMu, taken
	// alone.
	publishMu   sync.Mutex
	publishing  int
	publishIdle chan struct{}
	// endPending is each page that left with a download whose end its
	// engine received and has yet to deliver, by download, until that end
	// settles the row (pageLeftLocked). Guarded by mu.
	endPending map[downloadID]*managedPage
	// carriers is the leave that carries the downloads of each of the
	// newest popups never listed that ended while the leaving page was
	// their nearest listed opener (popupCarrier), and endedPopups is each
	// such popup's end not carried yet, with its openers
	// (endedPopupLocked). Guarded by mu.
	carriers    *boundedMap[ids.PageHandle, popupCarrier]
	endedPopups *boundedMap[ids.PageHandle, []ids.PageHandle]
	// downloadPushInterval overrides downloadPushInterval when set (tests).
	downloadPushInterval time.Duration
	// pageAdopted is a test seam for the asynchronous popup ownership handoff.
	// Production leaves it nil; the managed-Chrome integration test uses the
	// signal instead of polling on wall-clock sleeps.
	pageAdopted func()
	// popupListing is a test seam: adoptPopup calls it between the hold that
	// lists a popup's tab with the downloads the popup started and the
	// application of their newest progress.
	popupListing func()
	// publishDecided is a test seam: publishPages calls it once it has
	// decided to push, before the push.
	publishDecided func()
	// adopting is each popup whose adoption runs, by handle, with the page
	// that reserves its jar once its opener is found (nil before), unlisted
	// the newest handles that will not be listed again, and
	// listingChanged, when set, closes as the next page joins or leaves the
	// tabs or an adoption ends (listingChangedLocked). Guarded by mu.
	adopting       map[ids.PageHandle]*managedPage
	unlisted       *boundedMap[ids.PageHandle, struct{}]
	listingChanged chan struct{}
	// admissions is each popup's admission from its engine's report until
	// its listing or its end, which decides whether it becomes a tab, and
	// pageEnds runs what each page's reported close and crash do, in the
	// order reported (engine_pages.go).
	admissions *popupadmit.Registry
	pageEnds   pageEndQueue
	// parked holds the downloads of popups not listed yet, by popup
	// handle, parkedIDs each one's popup by download id, and popupFates
	// what became of the newest popups. Guarded by parkMu, which is taken
	// before mu.
	parkMu     sync.Mutex
	parked     map[ids.PageHandle]*parkedPopup
	parkedIDs  map[downloadID]ids.PageHandle
	popupFates *boundedMap[ids.PageHandle, popupFate]
	// parkTimeout overrides popupParkTimeout when set (tests).
	parkTimeout time.Duration
	// creationWatch is the engine page creation in flight, nil for none
	// (creation_end.go). Guarded by mu.
	creationWatch *creationWatch
	// heldInfo holds a CDP engine's newest report for each handle no page
	// answers to yet (updatePageInfo). Bounded: a handle no page ever takes,
	// a target closed before its adoption, ages out.
	heldInfo *boundedMap[ids.PageHandle, heldInfo]
	// viewportApplied is a test seam for the asynchronous pane-size follow:
	// called after each drain pass lays the thread's pages out. Production
	// leaves it nil.
	viewportApplied func(threadID ThreadID)
	// pushCopied is a test seam: called, with m.mu held, once a push has
	// taken the unpushed records and copied the tabs. Production leaves it
	// nil.
	pushCopied func()
	// presentDeciding is a test seam: called, with no lock held, by a
	// presentation pass after it hid every page but its candidate and before
	// it decides whether to show it. Production leaves it nil.
	presentDeciding func(threadID ThreadID, candidate *managedPage)
	// infoRead is a test seam: called between a read of a page's info (or
	// history state) and its commit. Production leaves it nil.
	infoRead func(p *managedPage)
	// selectionGapTimer starts one wait of a numbered command for its
	// predecessors (turnorder). A test seam; production uses
	// turnorder.NewGapTimer.
	selectionGapTimer turnorder.GapTimer
	// selectionAdmitted is a test seam run between a numbered command's
	// admission and its commit (awaitTurn). Production leaves it nil.
	selectionAdmitted func(sel Numbered)
	// reopenRead is a test seam run between a reopen's read of its closed
	// tab and the tab's admission (ReopenClosedTab). Production leaves it
	// nil.
	reopenRead func()
	// switchWritten is a test seam run between a jar switch's write of its
	// saved copy and its commit (SetJar). Production leaves it nil.
	switchWritten func()
	// revealFileInFileManager hands a file to the OS file manager
	// (fileRevealer). A test replaces it with a recorder.
	revealFileInFileManager func(ctx context.Context, path string) error
	// downloadAdmitting is a test seam run between a download start's page
	// lookup and its admission, and for a download of a popup with a fate
	// in the m.mu hold that chooses its page (holdParkedOwner).
	// Production leaves it nil.
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
	KeepThread func(threadID ThreadID) (bool, error)

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

func NewManager(configDir string, config Config, opts ManagerOptions) *Manager {
	m := &Manager{
		artifactBytes:  quota.NewPool("browser artifacts", maxArtifactRootBytes),
		workspacePools: quota.NewRegistry("workspace downloads", maxWorkspaceDownloadBytes),
		config:         config,
		profileDir:     filepath.Join(configDir, browserProfileDir),
		scopes:         make(map[jarKey]*jarScope),
		suspended:      make(map[PageID]*suspendedPage),
		pending:        make(map[PageID]*pendingPage),
		heldInfo:       newBoundedMap[ids.PageHandle, heldInfo](orderSlack),
		admissions:     popupadmit.New(orderSlack),
		disposing:      make(map[jarKey]chan struct{}),
		recordDir:      filepath.Join(configDir, browserPageRecordDir),
		panes:          make(map[MountID]paneMount),
		threads:        make(map[ThreadID]*threadBrowser),
		viewportSyncs:  make(map[ThreadID]*viewportSync),
		artifactRoot:   filepath.Join(configDir, "browser-artifacts"),
		findTimeout:    findActionTimeout,

		revealFileInFileManager: fileRevealer(opts.FakeEngine, testing.Testing()),
		agentLinger:             agentActivityLinger,
		recentLaunch:            uuid.NewString(),
	}
	m.selectionGapTimer = turnorder.NewGapTimer
	m.accelerators = opts.Accelerators
	m.engine = selectEngine(configDir, opts, engineEvents{
		PopupOpened:       m.popupOpened,
		PageClosed:        m.pageClosed,
		PageCrashed:       m.pageCrashedReport,
		PageEndReceived:   m.pageEndReceived,
		PageInfoChanged:   m.updatePageInfo,
		DownloadStarted:   m.downloadStarted,
		DownloadProgress:  m.downloadProgress,
		KeyChord:          m.keyChord,
		PageFocusReported: m.pageFocusReported,
	})
	pruneEncryptedCheckpoints(configDir)
	m.loadPageRecords(opts.KeepThread)
	return m
}

// pruneEncryptedCheckpoints deletes the AES-GCM site-data checkpoints and the
// key file the pre-engine browser wrote (spec §4). They hold cookies and
// localStorage for a Chrome that no longer exists and cannot be imported into
// an engine profile, so first boot of this code is where they stop existing.
// A removal that fails is logged and boot goes on: a checkpoint no engine
// reads holds nothing the browser uses. Their absence is not a failure.
func pruneEncryptedCheckpoints(configDir string) {
	if err := os.RemoveAll(filepath.Join(configDir, "browser-state")); err != nil {
		log.Printf("browser: remove the old site-data checkpoints: %v", err)
	}
	if err := os.Remove(filepath.Join(configDir, "browser-state.key")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the old site-data checkpoint key: %v", err)
	}
}

// Available reports whether this deployment has a browser engine at all. It is
// the question the App answers before offering a thread the browser MCP
// server: a windowless deployment gets no browser tools rather than tools
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
	host.Report(ids.PageHandle(pageID), kind, webview2host.TruncateDetail(detail))
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
		return m.forgetAllPages(context.Background(), config.Enabled, nil)
	}
	return nil
}
