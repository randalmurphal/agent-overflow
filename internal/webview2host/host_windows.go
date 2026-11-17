//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"agent-overflow/internal/keybindings"

	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

// Host owns the pane WebView2 environment and one controller per browser
// tab, all parented into the launcher's existing Win32 window.
//
// Threading: WebView2 requires every COM and window call on the thread
// that owns the host window, which is the launcher's UI thread. Directive
// handling arrives on a bridge goroutine, so every COM call below goes
// through Config.OnMain, and every completion handler body runs INLINE on
// the UI thread (WebView2 invokes them from the message pump) and
// therefore must never call OnMain itself.
type Host struct {
	config Config

	// env is the pane environment, created once per generation; clear-data
	// starts a new generation (environment.go).
	env *paneEnvironment[paneEnv]

	mu    sync.Mutex
	pages map[string]*hostPage
	// accelerators is the bound-chord set every page's key events are gated
	// on (accelerators_windows.go). Replaced whole by OpAccelerators.
	accelerators keybindings.AcceleratorSet

	cdpPort int
	// downloadToken and downloadSeq make the launcher's download ids
	// (download.go); downloadSeq is UI thread only.
	downloadToken string
	downloadSeq   uint64
	// popupSeq numbers the popups' page ids (popupPageID), and
	// pendingPopups counts the window requests whose deferral is open
	// (MaxPendingPopups). UI thread only.
	popupSeq      uint64
	pendingPopups int
	// cancelTimer runs fn after wait, on a goroutine of its own
	// (settleCancelAfter); nil is time.AfterFunc. A test stands in its own.
	cancelTimer func(wait time.Duration, fn func())
	// foreground answers whether the launcher window is the foreground
	// window (isForeground). A smoke stands in its answer so it never takes
	// the user's foreground.
	foreground func(hwnd uintptr) bool
}

// Config wires the host to the launcher without importing Wails here.
type Config struct {
	// HostWindow resolves the launcher window's HWND. It is a function
	// rather than a value because the window may not exist when the host
	// is constructed, and because Wails can recreate the WebView2 side of
	// it during a renderer-hang recovery.
	HostWindow func() uintptr
	// UserDataDir is the pane environment's user-data folder. It must
	// already exist and be validated (see prepareBrowserProfileStorage in
	// the launcher): an empty value is refused rather than letting
	// WebView2 pick a shared default.
	UserDataDir string
	// ExtraBrowserArgs are Chromium switches the launcher adds to the pane
	// environment beside the host's own DevTools port, so a display
	// preference set for the app window applies to the pane too. Validated
	// by ValidateExtraBrowserArgs at construction.
	ExtraBrowserArgs []string
	// Logf writes to launcher.log.
	Logf func(string, ...any)
	// Report posts one BrowserHostReport back to the backend.
	Report func(pageID string, kind ReportKind, detail string)
	// OnMain runs fn on the UI thread and returns once it has run.
	OnMain func(fn func())
	// Geometry holds the SPA's own pane geometry reports (geometry.go). A
	// page bound to a mount with a report there is placed from it; nil, or
	// no report, places pages from the backend's bounds alone.
	Geometry *GeometryMailbox
	// Focus holds the SPA's focus intent arms and withdrawals and the
	// delivered focus directives (focusintent.go). Nil keeps directives
	// alone, with no withdrawal heard.
	Focus *FocusIntents
}

type hostPage struct {
	id        string
	profileID string
	ephemeral bool
	// popup links a page a window request opened to that request
	// (popup_windows.go); nil for a page the backend created.
	popup *popupState
	// beforeChildren is the host's child-window list captured
	// immediately before CreateCoreWebView2ControllerWithOptions, so the
	// completion handler can diff out this controller's own child HWND.
	// Used only by the unclipped fallback: the normal path creates the
	// controller under its final clip container and raises that container.
	beforeChildren []uintptr

	controller *iCoreWebView2Controller
	// controller2 is the same object through ICoreWebView2Controller2,
	// which is where the default background colour lives. Nil on a runtime
	// too old to have the interface; the pane still works, it just keeps
	// the engine's white.
	controller2 *iCoreWebView2Controller2
	view        *iCoreWebView2
	child       uintptr
	// container is the page's clip window: an intermediate child of the
	// host that the controller's own window lives inside. Positioning it at
	// the VISIBLE rect while the controller keeps the FULL rect is what
	// crops a pane running under the sidebar instead of hiding it or
	// letting it overhang. Zero when the container could not be created, in
	// which case the pane falls back to the unclipped, directly-parented
	// behaviour.
	container uintptr
	// bg is the last colour actually applied (applyBackground).
	bg string
	// bgUnsupported records that this page has already logged a missing
	// ICoreWebView2Controller2, so an old runtime costs one line, not one
	// per background directive.
	bgUnsupported bool
	// failHandler, keyHandler and targetInfo are the page's callbacks that
	// WebView2 holds; retireHandlers detaches them when the page goes.
	failHandler *iProcessFailedHandler
	keyHandler  *iAcceleratorKeyPressedHandler
	targetInfo  *iDevToolsCompletedHandler
	// newWindow and closeRequested take the windows the page opens and its
	// own close (popup_windows.go); dialogOpened closes WebView2's download
	// dialog (download_dialog_windows.go).
	newWindow      *iNewWindowRequestedHandler
	closeRequested *iViewEventHandler
	dialogOpened   *iViewEventHandler
	// closeAfterDownloads is set when the page asked to close while a
	// download ran; it closes when the last one ends. UI thread only.
	closeAfterDownloads bool
	// findStarted is the page's one Start completion handler, made on the
	// first find (find_windows.go). The page holds its constructor's
	// reference until retireHandlers.
	findStarted *iFindStartCompletedHandler
	// downloadStarting takes the page's downloads, downloads holds each one
	// until it ends, and ended keeps the terminal snapshots that answer a
	// later directive (download_windows.go). UI thread only.
	downloadStarting *iDownloadStartingHandler
	downloads        map[string]*hostDownload
	ended            endedDownloads
	// pres is what the backend said about the page's presentation, and
	// shown what the host last applied (presenter.go). UI thread only.
	pres  pagePresentation
	shown paneView
}

// raiseTarget is the window that must sit on top of the host's child
// z-order for this page to be visible: the clip container when there is
// one, otherwise the controller's own child window.
func (p *hostPage) raiseTarget() uintptr {
	if p.container != 0 {
		return p.container
	}
	return p.child
}

// envCreateTimeout bounds the cold environment create. WebView2 launching
// its browser process is the slow part; past this the pane is broken and
// the backend should hear so rather than block a directive forever.
const envCreateTimeout = 30 * time.Second

const (
	// clearRetryBudget bounds the whole delete. The WebView2 browser process
	// exits shortly after its last controller closes and the environment
	// refs drop, but it can hold file locks in the user-data folder for a
	// moment past that, and on Windows a locked file fails the delete rather
	// than deferring it. Retrying is the only correct answer; retrying
	// forever is not, so the clear reports a failure past this.
	clearRetryBudget = 15 * time.Second
	// clearRetryStart / clearRetryMax bound the backoff between attempts.
	// The common case settles on the second attempt.
	clearRetryStart = 100 * time.Millisecond
	clearRetryMax   = 2 * time.Second
)

// New validates the configuration and allocates the CDP port. It creates
// nothing: Start kicks the environment off on the UI thread.
func New(config Config) (*Host, error) {
	if config.HostWindow == nil {
		return nil, errors.New("browser host window resolver is required")
	}
	if config.UserDataDir == "" {
		return nil, errors.New("browser host user data folder is required")
	}
	if config.OnMain == nil {
		return nil, errors.New("browser host main-thread dispatcher is required")
	}
	if err := ValidateExtraBrowserArgs(config.ExtraBrowserArgs); err != nil {
		return nil, err
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	if config.Focus == nil {
		config.Focus = NewFocusIntents()
	}
	if config.Report == nil {
		config.Report = func(string, ReportKind, string) {}
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("reserve browser host debugging port: %w", err)
	}
	h := &Host{
		config:  config,
		pages:   make(map[string]*hostPage, 4),
		cdpPort: port,

		foreground:    isForeground,
		downloadToken: newDownloadToken(),
	}
	h.env = newPaneEnvironment(config.OnMain, h.startEnvironment, freePaneEnv, config.Logf, envCreateTimeout)
	return h, nil
}

// CDPPort is the loopback port the pane environment's browser process
// serves DevTools on. It is fixed for the host's lifetime and is the only
// port the tunnel may reach.
func (h *Host) CDPPort() int { return h.cdpPort }

// Apply validates and executes one directive. It is safe to call from any
// goroutine and blocks until the work has run on the UI thread.
//
// An invalid directive is logged and dropped. That is deliberate at both
// ends: the launcher never guesses at a command that creates OS windows,
// and dropping one costs a retry rather than tearing down the bridge the
// retry would arrive on.
func (h *Host) Apply(directive Directive) {
	if err := directive.Validate(); err != nil {
		h.config.Logf("browser host: ignore invalid directive: %v", err)
		return
	}
	switch directive.Op {
	case OpCreate:
		h.create(directive)
	case OpBounds:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.noteBounds(directive)
			h.present(page, false)
		})
	case OpBackground:
		h.withPage(directive.PageID, func(page *hostPage) { h.applyBackground(page, directive.Bg) })
	case OpShow:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.backendShown = true
			h.present(page, true)
		})
	case OpHide:
		h.withPage(directive.PageID, func(page *hostPage) {
			page.pres.backendShown = false
			h.present(page, false)
		})
	case OpDevTools:
		h.withPage(directive.PageID, func(page *hostPage) {
			if err := page.view.openDevToolsWindow(); err != nil {
				h.config.Logf("browser host: page %s devtools: %v", page.id, err)
			}
		})
	case OpClose:
		h.closePages(func(page *hostPage) bool { return page.id == directive.PageID })
	case OpCloseProfile:
		h.closePages(func(page *hostPage) bool { return page.profileID == directive.ProfileID })
	case OpClearData:
		h.clearData(directive.PageID)
	case OpAccelerators:
		h.setAccelerators(directive.Accelerators)
	case OpFind:
		h.withPage(directive.PageID, func(page *hostPage) { h.startFind(page, directive.Query) })
	case OpFindStop:
		h.withPage(directive.PageID, h.stopFind)
	case OpFocus:
		h.config.Focus.Deliver(directive.Mount, directive.Intent, directive.PageID)
		h.withPage(directive.PageID, h.applyPendingFocus)
	case OpCancelDownload:
		h.withPage(directive.PageID, func(page *hostPage) { h.answerDownload(page, directive.Download, true) })
	case OpDownloadStatus:
		h.withPage(directive.PageID, func(page *hostPage) { h.answerDownload(page, directive.Download, false) })
	}
}

// applyPendingFocus moves keyboard focus into the page for the focus intent
// delivered for it, while the host shows the page, the launcher window is
// the foreground window and the intent still stands (focusDecision). A page
// not shown or a window in the background leaves the intent pending, and the
// page's next show applies it (present). Runs on the UI thread, so the view
// it checks is the one on screen.
func (h *Host) applyPendingFocus(page *hostPage) {
	var mount string
	var intent uint64
	take := func() bool {
		var ok bool
		mount, intent, ok = h.config.Focus.Take(page.id)
		return ok
	}
	if focusDecision(page.shown, h.foreground(h.config.HostWindow()), take) != focusMove {
		return
	}
	report := FocusReport{Mount: mount, Intent: intent}
	if err := page.controller.moveFocus(moveFocusProgrammatic); err != nil {
		h.config.Logf("browser host: page %s focus: %v", page.id, err)
		report.Error = err.Error()
		h.config.Report(page.id, ReportFocusFailed, report.Encode())
		return
	}
	h.config.Report(page.id, ReportFocused, report.Encode())
}

// ApplyGeometry re-presents every page bound to a mount the reports name.
// It runs on the UI thread, as the launcher's single pending drain of the
// geometry mailbox, and must not be called from anywhere else.
func (h *Host) ApplyGeometry(reports []PaneGeometry) {
	if len(reports) == 0 {
		return
	}
	mounts := make(map[string]bool, len(reports))
	for _, report := range reports {
		mounts[report.Mount] = true
	}
	h.mu.Lock()
	pages := make([]*hostPage, 0, len(h.pages))
	for _, page := range h.pages {
		pages = append(pages, page)
	}
	h.mu.Unlock()
	for _, page := range pages {
		if page.controller != nil && page.pres.mount != "" && mounts[page.pres.mount] {
			h.present(page, false)
		}
	}
}

// present brings one page to the view its presentation calls for
// (presenter.go): moved before it is shown, never moved while hidden. raise
// re-raises a page already shown. Runs on the UI thread.
func (h *Host) present(page *hostPage, raise bool) {
	local, hasLocal := h.config.Geometry.Latest(page.pres.mount)
	step, next := nextPresentStep(page.shown, page.pres.view(local, hasLocal), raise)
	if step.move {
		h.applyBounds(page, step.bounds)
	}
	if step.show && !h.showPage(page) {
		next.visible = page.shown.visible
	}
	if step.hide {
		h.hidePage(page)
	}
	page.shown = next
	if step.show {
		h.applyPendingFocus(page)
	}
}

// showPage shows the page's clip container and raises it.
func (h *Host) showPage(page *hostPage) bool {
	if err := page.controller.putIsVisible(true); err != nil {
		h.config.Logf("browser host: page %s show: %v", page.id, err)
		return false
	}
	// The container tracks visibility too. It is not decoration: a shown
	// container with nothing in it is an invisible window swallowing every
	// click over its rectangle, which is exactly what a hidden pane must not
	// do.
	showWindow(page.container, true)
	// Every backend show re-raises: Wails' own recovery path can recreate
	// the SPA controller, which would land it above the pane again, and a
	// raise is cheap.
	raiseChild(page.raiseTarget())
	return true
}

// hidePage hides the page's clip container. Hiding is the CONTAINER's: the
// controller stays IsVisible so Chromium keeps compositing the page (rAF,
// scroll state, and the frame a screenshot copies). IsVisible=false stops
// all of that and evicts the surface, so a hidden page's first capture would
// wait for a frame that never comes (spike 2026-09-14). Only the unclipped
// fallback, with no container to hide, has to hide the controller itself.
func (h *Host) hidePage(page *hostPage) {
	held := focusWithin(page.raiseTarget())
	if page.container != 0 {
		showWindow(page.container, false)
	} else if err := page.controller.putIsVisible(false); err != nil {
		h.config.Logf("browser host: page %s hide: %v", page.id, err)
	}
	h.releaseFocus(page, held)
}

// applyBounds positions one page. Runs on the UI thread.
//
// The rect arrives in the SPA's CSS pixels with the pixel ratio and viewport
// it was measured under (deviceScale). BOTH rects are scaled by that one
// factor pair: a clip scaled differently from the rect it crops would slide
// across the page as the window resized.
func (h *Host) applyBounds(page *hostPage, b paneBounds) {
	cw, ch, hasClient := clientSize(h.config.HostWindow())
	sx, sy := deviceScale(b, cw, ch, hasClient)
	layout := resolvePaneLayout(b, sx, sy)

	bounds := layout.Controller
	if page.container == 0 {
		// No container: the controller is still a direct child of the host,
		// so its bounds are host-relative and there is nothing to clip
		// against. Position the full rect and let it overhang, which is the
		// behaviour that shipped before clipping existed.
		bounds = rect{
			Left:   layout.Container.Left + layout.Controller.Left,
			Top:    layout.Container.Top + layout.Controller.Top,
			Right:  layout.Container.Left + layout.Controller.Right,
			Bottom: layout.Container.Top + layout.Controller.Bottom,
		}
	} else {
		// Container first, then the controller inside it: moving the crop
		// before its content means a growing pane never shows a frame of
		// content outside the new clip.
		moveWindow(page.container, layout.Container)
	}
	if err := page.controller.putBounds(bounds); err != nil {
		h.config.Logf("browser host: page %s bounds: %v", page.id, err)
	}
}

// applyBackground sets the controller's default background colour (an
// OpBackground's) when it changed. Runs on the UI thread, shown or hidden.
func (h *Host) applyBackground(page *hostPage, bg string) {
	if bg == page.bg {
		return
	}
	if page.controller2 == nil {
		if !page.bgUnsupported {
			page.bgUnsupported = true
			h.config.Logf("browser host: page %s: installed WebView2 runtime has no "+
				"ICoreWebView2Controller2; pane background colour is unavailable", page.id)
		}
		return
	}
	color, ok := parsePaneColor(bg)
	if !ok {
		// Validate already refused every other spelling; reaching here
		// would mean the two halves of that rule drifted apart.
		h.config.Logf("browser host: page %s: ignoring unparseable background %q", page.id, bg)
		return
	}
	if err := page.controller2.putDefaultBackgroundColor(color); err != nil {
		h.config.Logf("browser host: page %s background %s: %v", page.id, bg, err)
		return
	}
	page.bg = bg
}

// Close tears every controller down. The environment itself is released
// with the process.
func (h *Host) Close() {
	h.closePages(func(*hostPage) bool { return true })
}

// withPage runs fn on the UI thread with the named page, or logs that it
// is gone. A directive for a page that already died is normal (the
// backend's view lags a process failure), not an error.
func (h *Host) withPage(pageID string, fn func(*hostPage)) {
	h.mu.Lock()
	page := h.pages[pageID]
	h.mu.Unlock()
	if page == nil || page.controller == nil {
		h.config.Logf("browser host: no live page %s", pageID)
		return
	}
	h.config.OnMain(func() { fn(page) })
}

func (h *Host) closePages(match func(*hostPage) bool) {
	h.mu.Lock()
	var closing []*hostPage
	for id, page := range h.pages {
		if match(page) {
			closing = append(closing, page)
			delete(h.pages, id)
		}
	}
	h.mu.Unlock()
	if len(closing) == 0 {
		return
	}
	var reported []*hostPage
	h.config.OnMain(func() {
		for _, page := range closing {
			// A popup still being created answers its window request with no
			// window. The backend hears of a close only for a page it knows.
			h.answerNoWindow(page)
			if page.popup == nil || page.popup.announced {
				reported = append(reported, page)
			}
			h.retireHandlers(page)
			if page.view != nil {
				page.view.release()
			}
			if page.controller2 != nil {
				page.controller2.release()
				page.controller2 = nil
			}
			if page.controller != nil {
				held := focusWithin(page.raiseTarget())
				if err := page.controller.close(); err != nil {
					h.config.Logf("browser host: close page %s: %v", page.id, err)
				}
				page.controller.release()
				// After the controller, always: closing it destroys the window
				// inside the container, and a container that outlives its page
				// is an invisible window eating every click over its rectangle.
				destroyWindow(page.container)
				page.container = 0
				h.releaseFocus(page, held)
			}
			// A container with no controller is the parent of an asynchronous
			// create still in flight. Its completion owns both the controller
			// and container after observing that this page was unregistered;
			// destroying the parent now destabilizes the WebView2 create itself.
		}
	})
	for _, page := range reported {
		h.config.Report(page.id, ReportClosed, "")
	}
}

// ---------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------

// paneEnv is one adopted pane environment.
type paneEnv struct {
	env   *iCoreWebView2Environment
	env10 *iCoreWebView2Environment10
}

// freePaneEnv drops an environment's COM references. Runs on the UI thread.
func freePaneEnv(e paneEnv) {
	if e.env10 != nil {
		e.env10.release()
	}
	if e.env != nil {
		e.env.release()
	}
}

// startEnvironment runs on the UI thread and kicks off gen's async create
// (paneEnvironment.begin). It does not wait: WebView2 invokes the completion
// handler from the message pump, which cannot run while this call holds the
// thread.
func (h *Host) startEnvironment(gen uint64) {
	// The scrub must precede EVERY environment creation in this process.
	// See EnvOverrideNames: a set-but-empty WEBVIEW2_USER_DATA_FOLDER
	// silently collapses the pane and the SPA onto one profile, with no
	// error anywhere. The launcher also scrubs at boot, before Wails
	// builds the SPA environment; this is the second half of the same
	// rule, in the package that owns it.
	if removed := ScrubEnvOverrides(); len(removed) > 0 {
		h.config.Logf("browser host: cleared WebView2 env overrides: %s", FormatScrub(removed))
	}

	installed, err := webviewloader.GetInstalledVersion()
	if err != nil {
		h.config.Logf("browser host: reading installed WebView2 version: %v", err)
	}

	folder, err := windows.UTF16PtrFromString(h.config.UserDataDir)
	if err != nil {
		h.env.settle(gen, fmt.Errorf("encode user data folder: %w", err))
		return
	}

	// The port is the pane environment's alone; see paneEnvironmentArgs.
	args := paneEnvironmentArgs(h.cdpPort, h.config.ExtraBrowserArgs)
	options := newEnvOptions(args, installed)
	handler := newEnvironmentCompletedHandler(func(hr uintptr, env *iCoreWebView2Environment) {
		h.environmentCompleted(gen, hr, env)
	})
	// WebView2 holds its own references to both for as long as it needs
	// them; these are the constructors' (comref_windows.go).
	defer options.ref.release()
	defer handler.ref.release()

	hr, err := webviewloader.CreateCoreWebView2EnvironmentWithOptions(
		nil,
		folder,
		uintptr(unsafe.Pointer(options)),
		uintptr(unsafe.Pointer(handler)),
	)
	if err != nil {
		h.env.settle(gen, fmt.Errorf("load WebView2 runtime: %w", err))
		return
	}
	if hrErr := hresult(hr); hrErr != nil {
		h.env.settle(gen, fmt.Errorf("create pane environment: %w", hrErr))
	}
}

// environmentCompleted runs on the UI thread, from the message pump.
// paneEnvironment.completed decides whether gen still wants the environment.
func (h *Host) environmentCompleted(gen uint64, hr uintptr, env *iCoreWebView2Environment) {
	if err := hresult(hr); err != nil {
		h.env.completed(gen, paneEnv{}, fmt.Errorf("create pane environment: %w", err))
		return
	}
	if env == nil {
		h.env.completed(gen, paneEnv{}, errors.New("pane environment completed with no environment"))
		return
	}
	env.addRef()
	env10 := env.queryEnvironment10()
	if env10 == nil {
		// Per-workspace isolation IS ICoreWebView2ControllerOptions'
		// named profiles. Without them every workspace would share one
		// cookie jar, so this fails closed rather than degrading: a
		// silent shared profile is precisely the failure the whole
		// isolation design exists to prevent.
		env.release()
		h.env.completed(gen, paneEnv{}, errors.New(
			"installed WebView2 runtime has no ICoreWebView2Environment10; "+
				"per-workspace browser profiles need runtime 110 or newer"))
		return
	}
	h.env.completed(gen, paneEnv{env: env, env10: env10}, nil)
}

// ---------------------------------------------------------------------
// Clear site data
// ---------------------------------------------------------------------

// clearData destroys the pane environment's whole user-data folder, which
// is where the Windows/WSL deployment's site data actually lives: the
// backend's own browser-profiles/ tree is empty there, so the Settings
// button would otherwise be a silent no-op.
//
// Runs on the bridge goroutine, deliberately. The COM teardown is
// dispatched to the UI thread like every other COM call, but the delete
// retry loop SLEEPS, and sleeping on the UI thread would freeze the
// launcher window for the whole retry budget.
//
// Every path reports. A clear the backend never hears about is a Settings
// button that spins until its own timeout.
func (h *Host) clearData(correlationID string) {
	// Order is the same load-bearing order the Manager uses: nothing may
	// hold the profile open while it is deleted, or it writes its cookie
	// jar back out over the cleared state. Releasing the environment starts
	// a new generation, so the next directive builds a fresh one against
	// the recreated folder.
	h.closePages(func(*hostPage) bool { return true })
	h.config.OnMain(h.env.release)

	if err := clearUserDataDir(h.config.UserDataDir); err != nil {
		h.config.Logf("browser host: clear site data: %v", err)
		h.config.Report(correlationID, ReportClearFailed, TruncateDetail(err.Error()))
		return
	}
	h.config.Logf("browser host: cleared site data in %s", h.config.UserDataDir)
	h.config.Report(correlationID, ReportCleared, "")
}

// clearUserDataDir deletes the folder and recreates it empty.
//
// The retry is not defensive padding: the WebView2 browser process exits
// shortly after its last controller closes and the environment refs drop,
// but until it does it holds handles inside this folder, and Windows fails
// the unlink rather than deferring it.
//
// The folder is recreated because the launcher validated and created it at
// boot (prepareBrowserProfileStorage) and environment creation expects it
// to exist. Same mode, 0o700, for the same reason: it accumulates the
// cookies of whatever the user browses in the pane.
func clearUserDataDir(dir string) error {
	deadline := time.Now().Add(clearRetryBudget)
	delay := clearRetryStart
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("delete %s: %w", dir, err)
		}
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
		if delay *= 2; delay > clearRetryMax {
			delay = clearRetryMax
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recreate %s: %w", dir, err)
	}
	return nil
}

// ---------------------------------------------------------------------
// Controllers
// ---------------------------------------------------------------------

func (h *Host) create(directive Directive) {
	env, err := h.env.acquire()
	if err != nil {
		h.config.Report(directive.PageID, ReportCreateFailed, TruncateDetail(err.Error()))
		return
	}
	env10 := env.env10
	h.mu.Lock()
	if _, exists := h.pages[directive.PageID]; exists {
		h.mu.Unlock()
		h.config.Report(directive.PageID, ReportCreateFailed, "page id already exists")
		return
	}
	page := &hostPage{id: directive.PageID, profileID: directive.ProfileID, ephemeral: directive.Ephemeral}
	h.pages[directive.PageID] = page
	h.mu.Unlock()

	hwnd := h.config.HostWindow()
	if hwnd == 0 {
		h.failUncreated(page, "launcher window is not available")
		return
	}

	h.config.OnMain(func() {
		// close/close-profile may have retired the page while this UI-thread
		// hop was queued. Do not start a controller whose id is already
		// settled and may already have been reused by a newer create.
		if !h.pageRegistered(page) {
			return
		}
		h.startController(page, env10, hwnd)
	})
}

// startController starts the asynchronous create of page's controller in
// its profile, under hwnd. Runs on the UI thread.
func (h *Host) startController(page *hostPage, env10 *iCoreWebView2Environment10, hwnd uintptr) {
	if hwnd == 0 {
		h.failUncreated(page, "launcher window is not available")
		return
	}
	options, err := env10.createControllerOptions()
	if err != nil {
		h.failUncreated(page, "controller options: "+err.Error())
		return
	}
	defer options.release()
	if err := options.putProfileName(page.profileID); err != nil {
		h.failUncreated(page, "profile name: "+err.Error())
		return
	}
	if err := options.putInPrivate(page.ephemeral); err != nil {
		h.failUncreated(page, "inprivate: "+err.Error())
		return
	}

	// Create the final parent BEFORE the controller. Reparenting a live
	// windowed WebView2 controller can leave its compositor presenting
	// stale/blank strips while the document scrolls. Passing the clip
	// container to controller creation gives Chromium one stable parent
	// for the controller's whole lifetime.
	parent := hwnd
	container, err := createClipContainer(hwnd)
	if err != nil {
		// Clipping is a presentation refinement, not a precondition. Keep
		// the older direct-parent path and identify its child so it can be
		// raised over the SPA controller.
		h.config.Logf("browser host: page %s clip container: %v; pane will not clip", page.id, err)
		page.beforeChildren = childWindows(hwnd)
	} else {
		page.container = container
		parent = container
	}

	handler := newControllerCompletedHandler(func(hr uintptr, controller *iCoreWebView2Controller) {
		h.controllerCompleted(page, hwnd, hr, controller)
	})
	// WebView2 holds its own reference until it calls back; this is the
	// constructor's. The handler is not detached when the page closes:
	// its completion closes a controller that outlived the page.
	defer handler.ref.release()
	if err := env10.createControllerWithOptions(parent, options, handler); err != nil {
		h.failUncreated(page, "create controller: "+err.Error())
	}
}

// controllerCompleted runs on the UI thread from the message pump.
func (h *Host) controllerCompleted(page *hostPage, hwnd uintptr, hr uintptr, controller *iCoreWebView2Controller) {
	if err := hresult(hr); err != nil {
		// ERROR_INVALID_STATE (0x8007139F) here means "same user data
		// folder, different browser arguments", not a process-wide
		// conflict: the error surfaces at CONTROLLER creation even
		// though the mismatch is the ENVIRONMENT's. If it ever appears,
		// suspect a collapsed user data folder (see EnvOverrideNames)
		// before suspecting the argument rule.
		h.failUncreated(page, err.Error())
		return
	}
	if controller == nil {
		h.failUncreated(page, "controller completed with no controller")
		return
	}
	controller.addRef()
	if !h.pageRegistered(page) {
		// A close or close-profile settled this page while the create was
		// in flight; the backend has already heard closed. Adopting the
		// controller now would leak a window no directive can address, and
		// reporting created would arrive after closed.
		if err := controller.close(); err != nil {
			h.config.Logf("browser host: page %s close raced controller: %v", page.id, err)
		}
		controller.release()
		destroyWindow(page.container)
		page.container = 0
		return
	}
	page.controller = controller
	// ICoreWebView2Controller2 is the whole background-colour surface.
	// Querying once here keeps every background directive off
	// QueryInterface; a nil result is reported the first time a colour is
	// actually asked for, not now.
	page.controller2 = controller.queryController2()

	// Pages start hidden: the clip container is created unshown, and
	// browser_visibility is what shows it. The controller itself is visible
	// from the start, for the reason OpHide gives.
	if err := controller.putIsVisible(page.container != 0); err != nil {
		h.config.Logf("browser host: page %s initial visibility: %v", page.id, err)
	}

	if page.container != 0 {
		// Normal path: the container was the controller's parent from its
		// creation call, so no live WebView2 window is ever reparented.
		raiseChild(page.container)
	} else {
		// Container creation failed before the controller started. The
		// controller is a direct child of the host, so identify and raise it
		// over the SPA's own WebView2.
		page.child = newChildWindow(page.beforeChildren, childWindows(hwnd))
		page.beforeChildren = nil
		if page.child == 0 {
			h.config.Logf("browser host: page %s child window not identified; "+
				"the pane may render behind the app and cannot be clipped", page.id)
		} else {
			raiseChild(page.child)
		}
	}

	view, err := controller.coreWebView2()
	if err != nil || view == nil {
		h.discardCreated(page, "core webview: "+errString(err))
		return
	}
	page.view = view

	failHandler := newProcessFailedHandler(func(kind uint32) {
		h.pageReport(page, ReportProcessFailed, processFailedKindName(kind))
	})
	page.failHandler = failHandler
	if _, err := view.addProcessFailed(failHandler); err != nil {
		h.config.Logf("browser host: page %s process-failed subscription: %v", page.id, err)
	}
	failHandler.ref.release()
	keyHandler := newAcceleratorKeyPressedHandler(func(args *iAcceleratorKeyPressedEventArgs) {
		h.acceleratorPressed(page, args)
	})
	page.keyHandler = keyHandler
	if _, err := controller.addAcceleratorKeyPressed(keyHandler); err != nil {
		h.config.Logf("browser host: page %s accelerator subscription: %v", page.id, err)
	}
	keyHandler.ref.release()
	h.watchDownloads(page, view)
	h.watchDownloadDialog(page, view)
	h.watchWindows(page, view)

	// A runtime without the find API has to be known before the page is
	// used, so its report precedes created; a popup's follows its popup
	// report at once.
	if !h.findSupported(view) {
		h.pageReport(page, ReportFindUnavailable, "")
	}
	if page.popup != nil {
		h.popupReady(page, view)
		return
	}

	// Target.getTargetInfo through the controller's own DevTools session
	// answers with THIS page's targetInfo, which is the handle the
	// backend attaches chromedp to. Reporting created only after it
	// lands means the backend never sees a page it cannot drive.
	done := newDevToolsCompletedHandler(func(hr uintptr, resultJSON string) {
		if !h.pageRegistered(page) {
			// Closed while the target-info round trip was in flight; the
			// closed report has already settled this page.
			return
		}
		if err := hresult(hr); err != nil {
			h.discardCreated(page, "Target.getTargetInfo: "+err.Error())
			return
		}
		targetID, err := ParseTargetID(resultJSON)
		if err != nil {
			h.discardCreated(page, err.Error())
			return
		}
		h.config.Report(page.id, ReportCreated, targetID)
	})
	page.targetInfo = done
	err = view.callDevToolsProtocolMethod("Target.getTargetInfo", "{}", done)
	done.ref.release()
	if err != nil {
		h.discardCreated(page, "Target.getTargetInfo dispatch: "+err.Error())
	}
}

// failUncreated settles a create that failed on the UI thread and tears down
// the clip container that may already have become its final parent. The exact
// pointer comparison keeps a late callback from deleting or reporting failure
// for a newer page that reused the same id after this one was closed.
func (h *Host) failUncreated(page *hostPage, detail string) {
	h.mu.Lock()
	mine := h.pages[page.id] == page
	if mine {
		delete(h.pages, page.id)
	}
	h.mu.Unlock()
	destroyWindow(page.container)
	page.container = 0
	if page.popup != nil {
		// The backend never heard of the popup: its window request is
		// answered with no window.
		h.answerNoWindow(page)
		h.config.Logf("browser host: popup %s of page %s: %s", page.id, page.popup.opener.id, detail)
		return
	}
	if mine {
		h.config.Report(page.id, ReportCreateFailed, TruncateDetail(detail))
	}
}

// pageRegistered reports whether page is still the registry's entry for
// its id. A page that lost a race with close or close-profile is already
// settled: its report went out and its teardown belongs to whoever
// removed it.
func (h *Host) pageRegistered(page *hostPage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pages[page.id] == page
}

// discardCreated unwinds a create that failed AFTER its controller came
// up: a create-failed report with a live controller left behind would be
// a window no directive can ever address. Runs on the UI thread, like
// every caller.
func (h *Host) discardCreated(page *hostPage, detail string) {
	// Take the registry entry atomically: whichever of this and
	// closePages removes the page owns its teardown, so the controller
	// can never be closed twice.
	h.mu.Lock()
	mine := h.pages[page.id] == page
	if mine {
		delete(h.pages, page.id)
	}
	h.mu.Unlock()
	if !mine {
		// Already settled by close or close-profile; their teardown owns
		// the controller and their closed report is the final word.
		return
	}
	// A popup's window request is answered before its view goes.
	h.answerNoWindow(page)
	h.retireHandlers(page)
	if page.view != nil {
		page.view.release()
		page.view = nil
	}
	if page.controller2 != nil {
		page.controller2.release()
		page.controller2 = nil
	}
	if page.controller != nil {
		if err := page.controller.close(); err != nil {
			h.config.Logf("browser host: page %s discard: %v", page.id, err)
		}
		page.controller.release()
		page.controller = nil
	}
	destroyWindow(page.container)
	page.container = 0
	if page.popup != nil {
		// Reached before the popup report: the backend never heard of it.
		h.config.Logf("browser host: popup %s of page %s: %s", page.id, page.popup.opener.id, detail)
		return
	}
	h.config.Report(page.id, ReportCreateFailed, TruncateDetail(detail))
}

// retireHandlers detaches the callbacks of a page that is going, so a late
// call from WebView2 does nothing and a reference it never releases keeps
// only the small handler, not the page. The controller-completed handler is
// not among them: its completion must still close a controller that
// outlived the page. Runs on the UI thread.
func (h *Host) retireHandlers(page *hostPage) {
	if page.failHandler != nil {
		page.failHandler.done.detach()
	}
	if page.keyHandler != nil {
		page.keyHandler.pressed.detach()
	}
	if page.targetInfo != nil {
		page.targetInfo.done.detach()
	}
	if page.newWindow != nil {
		page.newWindow.requested.detach()
	}
	if page.closeRequested != nil {
		page.closeRequested.event.detach()
	}
	if page.dialogOpened != nil {
		page.dialogOpened.event.detach()
	}
	if page.findStarted != nil {
		page.findStarted.done.detach()
		page.findStarted.ref.release()
		page.findStarted = nil
	}
	h.retireDownloads(page)
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// freeLoopbackPort reserves an ephemeral port by binding and immediately
// releasing it, then hands the number to Chromium's
// --remote-debugging-port. The bind is loopback-only and momentary; it is
// how the port is chosen, not a probe of anything. A race with another
// process claiming the same port between release and Chromium's bind is
// possible and shows up as a dead debug endpoint, which the tunnel
// reports as a failed dial rather than hiding.
func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("loopback listener returned a non-TCP address")
	}
	return addr.Port, nil
}
