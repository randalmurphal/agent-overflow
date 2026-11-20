package browser

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/browser/popupadmit"
)

// A tab's page as the Manager holds it (managedPage): the AO-owned half,
// allocated before its engine page exists and attached to its driver once
// the engine has made one.

type managedPage struct {
	id PageID
	// owner is the thread lifetime the page is a tab of (thread_life.go).
	owner  *threadBrowser
	access Caller
	driver pageDriver
	// jarGen is the generation of its thread's jar setting the page was
	// opened under, and authority its workspace's (jar.go). Set before the
	// page is registered; read under m.mu.
	jarGen    uint64
	authority *workspaceAuthority
	// unlistedEnd is what its engine reported ending the page after its
	// attach and before its listing (creation_end.go). Guarded by m.mu.
	unlistedEnd popupadmit.Ending
	// ctx is the page's lifetime, taken from its driver. Operations are
	// bounded against it. cancelOps ends it before the driver closes, so a
	// close cancels the operations in flight (closePageAsync).
	ctx       context.Context
	cancelOps context.CancelFunc
	mu        pageLock
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
	// baseMu orders the page's base applications, and base is the base
	// color its engine was last told (applyPageBase).
	baseMu sync.Mutex
	base   string
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
	// downloads is the page's download log (downloads_log.go).
	downloads   pageDownloadLog
	assetMu     sync.Mutex
	inventories map[string]AssetInventory
	assetOrder  []string
	nodeMu      sync.Mutex
	nodeRefs    map[string]nodeReference
	nodeOrder   []string
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
	// frame committed, stamped on the engine's own thread (find_ends.go).
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
func newManagedPage(owner *threadBrowser, access Caller) *managedPage {
	p := &managedPage{
		id: PageID(uuid.NewString()), owner: owner, access: access,
		createdAt:   time.Now().UnixNano(),
		inventories: make(map[string]AssetInventory),
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
