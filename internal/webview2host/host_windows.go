//go:build windows

package webview2host

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"

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

	mu sync.Mutex
	// envReady is closed once environment creation settles, success or
	// failure. Directives that arrive during the cold create wait on it.
	//
	// The create is one-shot per GENERATION rather than once per process:
	// clear-data releases the environment and bumps envGen, which re-arms
	// envStarting and swaps in a fresh envReady so the next directive pays
	// for a cold create against the recreated folder. Everything that
	// mutates envGen, envReady or envStarting past ensureEnvironment's arm
	// runs on the UI thread, which is what makes the close-once dance below
	// safe without a second lock.
	envReady    chan struct{}
	envGen      uint64
	envStarting bool
	env         *iCoreWebView2Environment
	env10       *iCoreWebView2Environment10
	envErr      error
	envKeep     *envOptions
	envHandle   *iEnvironmentCompletedHandler

	pages map[string]*hostPage

	cdpPort int
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
	// Logf writes to launcher.log.
	Logf func(string, ...any)
	// Report posts one BrowserHostReport back to the backend.
	Report func(pageID string, kind ReportKind, detail string)
	// OnMain runs fn on the UI thread and returns once it has run.
	OnMain func(fn func())
}

type hostPage struct {
	id        string
	profileID string
	// beforeChildren is the host's child-window list captured
	// immediately before CreateCoreWebView2ControllerWithOptions, so the
	// completion handler can diff out this controller's own child HWND.
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
	// letting it overhang. Zero when the controller's child window could
	// not be identified, in which case the pane falls back to the
	// unclipped, directly-parented behaviour.
	container uintptr
	// bg is the last colour actually applied, so a bounds directive — which
	// arrives on every scroll and resize — pays for a COM call only when
	// the pane surface's colour really changed.
	bg string
	// bgUnsupported records that this page has already logged a missing
	// ICoreWebView2Controller2, so an old runtime costs one line, not one
	// per bounds directive.
	bgUnsupported bool
	failHandler   *iProcessFailedHandler
	// pendingHandlers keeps every COM callback object this page handed to
	// WebView2 alive for as long as WebView2 can invoke it.
	pendingHandlers []any
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
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	if config.Report == nil {
		config.Report = func(string, ReportKind, string) {}
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("reserve browser host debugging port: %w", err)
	}
	return &Host{
		config:   config,
		envReady: make(chan struct{}),
		pages:    make(map[string]*hostPage, 4),
		cdpPort:  port,
	}, nil
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
		h.withPage(directive.PageID, func(page *hostPage) { h.applyBounds(page, directive) })
	case OpShow:
		h.withPage(directive.PageID, func(page *hostPage) {
			if err := page.controller.putIsVisible(true); err != nil {
				h.config.Logf("browser host: page %s show: %v", page.id, err)
				return
			}
			// The container tracks visibility too. It is not decoration: a
			// shown container with nothing in it is an invisible window
			// swallowing every click over its rectangle, which is exactly
			// what a hidden pane must not do.
			showWindow(page.container, true)
			// Every show re-raises: Wails' own recovery path can recreate
			// the SPA controller, which would land it above the pane
			// again, and a raise is cheap.
			raiseChild(page.raiseTarget())
		})
	case OpHide:
		h.withPage(directive.PageID, func(page *hostPage) {
			if err := page.controller.putIsVisible(false); err != nil {
				h.config.Logf("browser host: page %s hide: %v", page.id, err)
			}
			showWindow(page.container, false)
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
	}
}

// applyBounds positions one page and paints its surface colour. Runs on
// the UI thread.
//
// The rect arrives in the SPA's CSS pixels together with the viewport it
// was measured in; the client area is the same surface in physical pixels,
// so the proportion IS the whole DPI-and-zoom answer. A missing viewport
// means client pixels. BOTH rects are scaled by that one factor pair — a
// clip scaled differently from the rect it crops would slide across the
// page as the window resized.
func (h *Host) applyBounds(page *hostPage, directive Directive) {
	sx, sy := 1.0, 1.0
	if directive.VW > 0 && directive.VH > 0 {
		if cw, ch, ok := clientSize(h.config.HostWindow()); ok && cw > 0 && ch > 0 {
			sx = float64(cw) / directive.VW
			sy = float64(ch) / directive.VH
		}
	}
	layout := resolvePaneLayout(directive, sx, sy)

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
	h.applyBackground(page, directive.Bg)
}

// applyBackground sets the controller's default background colour when it
// changed. Bounds directives arrive on every scroll and resize, so the
// comparison is what keeps this off the hot path.
//
// An empty Bg leaves whatever is already set, including the engine
// default: the wire contract says "leave the engine default", and a pane
// that has been told a colour once should not lose it because a later
// directive omitted one.
func (h *Host) applyBackground(page *hostPage, bg string) {
	if bg == "" || bg == page.bg {
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
	h.config.OnMain(func() {
		for _, page := range closing {
			if page.view != nil {
				page.view.release()
			}
			if page.controller2 != nil {
				page.controller2.release()
				page.controller2 = nil
			}
			if page.controller != nil {
				if err := page.controller.close(); err != nil {
					h.config.Logf("browser host: close page %s: %v", page.id, err)
				}
				page.controller.release()
			}
			// After the controller, always: closing it destroys the window
			// inside the container, and a container that outlives its page
			// is an invisible window eating every click over its rectangle.
			destroyWindow(page.container)
			page.container = 0
		}
	})
	for _, page := range closing {
		h.config.Report(page.id, ReportClosed, "")
	}
}

// ---------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------

// ensureEnvironment starts the pane environment once per generation and
// waits for it to settle. Every caller of one generation gets the same
// error; a clear-data bumps the generation, so the next caller pays for a
// fresh cold create rather than inheriting an environment whose folder was
// deleted underneath it.
func (h *Host) ensureEnvironment() (*iCoreWebView2Environment10, error) {
	h.mu.Lock()
	ready, gen := h.envReady, h.envGen
	arm := !h.envStarting
	h.envStarting = true
	h.mu.Unlock()
	if arm {
		h.config.OnMain(h.startEnvironment)
	}
	select {
	case <-ready:
	case <-time.After(envCreateTimeout):
		return nil, fmt.Errorf("browser host environment did not start within %s", envCreateTimeout)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.envGen != gen {
		// A clear-data released this generation's environment while the
		// caller waited. Its own report already went out; this directive is
		// told to come back rather than handed a released COM pointer.
		return nil, errors.New("browser host environment was cleared; retry the directive")
	}
	if h.envErr != nil {
		return nil, h.envErr
	}
	if h.env10 == nil {
		return nil, errors.New("browser host environment settled with no environment")
	}
	return h.env10, nil
}

// startEnvironment runs on the UI thread and kicks off the async create.
// It does not wait: WebView2 invokes the completion handler from the
// message pump, which cannot run while this call holds the thread.
func (h *Host) startEnvironment() {
	h.mu.Lock()
	gen := h.envGen
	h.mu.Unlock()

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

	// --remote-debugging-port goes on the PANE environment ONLY. If two
	// environments carry it the first browser process to start wins the
	// port and the isolation inverts SILENTLY: the debug endpoint would
	// then serve the app's own UI and none of the pane's pages. The SPA
	// environment must never be given this flag "just in case".
	args := "--remote-debugging-port=" + strconv.Itoa(h.cdpPort)
	options, optionsPtr := newEnvOptions(args, installed)

	handler := newEnvironmentCompletedHandler(func(hr uintptr, env *iCoreWebView2Environment) {
		h.environmentCompleted(gen, hr, env)
	})
	folder, err := windows.UTF16PtrFromString(h.config.UserDataDir)
	if err != nil {
		h.settleEnvironment(gen, fmt.Errorf("encode user data folder: %w", err))
		return
	}

	h.mu.Lock()
	h.envKeep = options
	h.envHandle = handler
	h.mu.Unlock()

	hr, err := webviewloader.CreateCoreWebView2EnvironmentWithOptions(
		nil,
		folder,
		optionsPtr,
		uintptr(unsafe.Pointer(handler)),
	)
	if err != nil {
		h.settleEnvironment(gen, fmt.Errorf("load WebView2 runtime: %w", err))
		return
	}
	if hrErr := hresult(hr); hrErr != nil {
		h.settleEnvironment(gen, fmt.Errorf("create pane environment: %w", hrErr))
	}
}

// environmentCompleted runs on the UI thread, from the message pump.
func (h *Host) environmentCompleted(gen uint64, hr uintptr, env *iCoreWebView2Environment) {
	if err := hresult(hr); err != nil {
		h.settleEnvironment(gen, fmt.Errorf("create pane environment: %w", err))
		return
	}
	if env == nil {
		h.settleEnvironment(gen, errors.New("pane environment completed with no environment"))
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
		h.settleEnvironment(gen, errors.New(
			"installed WebView2 runtime has no ICoreWebView2Environment10; "+
				"per-workspace browser profiles need runtime 110 or newer"))
		return
	}
	h.mu.Lock()
	// Superseded-generation AND already-adopted both refuse the adopt. The
	// second can happen without the first: a clear-data can run between an
	// ensureEnvironment arm and its queued startEnvironment, whose create
	// then carries the NEW generation — releaseEnvironment re-armed
	// envStarting, so a later caller starts a second create for that same
	// generation. Overwriting the first adopt here would leak its refs and
	// keep the browser process pinning the folder a future clear deletes.
	stale := h.envGen != gen || h.env10 != nil
	if !stale {
		h.env = env
		h.env10 = env10
	}
	h.mu.Unlock()
	if stale {
		// Adopting a superseded environment would hand the next directive
		// one rooted in the folder a clear-data just deleted, so both refs
		// go back; the settled generation's answer (or the clear's own
		// report) stands.
		env10.release()
		env.release()
		return
	}
	h.settleEnvironment(gen, nil)
}

// settleEnvironment records one generation's outcome and releases its
// waiters. Runs on the UI thread, always, which is what makes the
// close-once check race-free.
func (h *Host) settleEnvironment(gen uint64, err error) {
	h.mu.Lock()
	if h.envGen != gen {
		h.mu.Unlock()
		return
	}
	// An error settles the generation only while it has no environment: a
	// duplicate create's failure must not poison a generation that already
	// adopted a healthy one (environmentCompleted refuses the duplicate's
	// adopt for the same reason).
	if h.envErr == nil && h.env10 == nil {
		h.envErr = err
	}
	ready := h.envReady
	h.mu.Unlock()
	if err != nil {
		h.config.Logf("browser host: %v", err)
	}
	select {
	case <-ready:
	default:
		close(ready)
	}
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
	// jar back out over the cleared state.
	h.closePages(func(*hostPage) bool { return true })
	h.config.OnMain(h.releaseEnvironment)

	if err := clearUserDataDir(h.config.UserDataDir); err != nil {
		h.config.Logf("browser host: clear site data: %v", err)
		h.config.Report(correlationID, ReportClearFailed, TruncateDetail(err.Error()))
		return
	}
	h.config.Logf("browser host: cleared site data in %s", h.config.UserDataDir)
	h.config.Report(correlationID, ReportCleared, "")
}

// releaseEnvironment drops the environment's COM objects and re-arms the
// creation state, so the next directive lazily builds a fresh environment
// against the recreated folder. Runs on the UI thread, like every other
// COM call.
//
// Bumping envGen is what makes this safe against a create still in flight:
// a completion handler for the old generation releases its own environment
// instead of adopting it, and a waiter parked in ensureEnvironment is told
// to retry rather than handed a pointer to a released object.
func (h *Host) releaseEnvironment() {
	h.mu.Lock()
	env, env10 := h.env, h.env10
	h.env, h.env10 = nil, nil
	// envKeep and envHandle are Go objects whose COM refcounting is a no-op
	// (see com_windows.go): dropping the reference IS the release, and it
	// must not happen before the environment's own refs go back.
	h.envKeep, h.envHandle = nil, nil
	h.envErr = nil
	h.envStarting = false
	h.envGen++
	// A fresh channel, and the old one is released rather than abandoned.
	// ensureEnvironment waits on whichever channel it captured, and the
	// superseded generation's settle no longer closes anything, so a caller
	// parked on a cold create would otherwise sit out its full 30s bound to
	// learn something already true. It wakes, sees the generation moved, and
	// is told to retry. Closing here is race-free for the same reason
	// settleEnvironment's is: both run on the UI thread.
	stale := h.envReady
	h.envReady = make(chan struct{})
	h.mu.Unlock()

	select {
	case <-stale:
	default:
		close(stale)
	}

	if env10 != nil {
		env10.release()
	}
	if env != nil {
		env.release()
	}
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
	env10, err := h.ensureEnvironment()
	if err != nil {
		h.config.Report(directive.PageID, ReportCreateFailed, TruncateDetail(err.Error()))
		return
	}
	h.mu.Lock()
	if _, exists := h.pages[directive.PageID]; exists {
		h.mu.Unlock()
		h.config.Report(directive.PageID, ReportCreateFailed, "page id already exists")
		return
	}
	page := &hostPage{id: directive.PageID, profileID: directive.ProfileID}
	h.pages[directive.PageID] = page
	h.mu.Unlock()

	hwnd := h.config.HostWindow()
	if hwnd == 0 {
		h.dropPage(page.id)
		h.config.Report(directive.PageID, ReportCreateFailed, "launcher window is not available")
		return
	}

	h.config.OnMain(func() {
		options, err := env10.createControllerOptions()
		if err != nil {
			h.dropPage(page.id)
			h.config.Report(page.id, ReportCreateFailed, TruncateDetail("controller options: "+err.Error()))
			return
		}
		defer options.release()
		if err := options.putProfileName(directive.ProfileID); err != nil {
			h.dropPage(page.id)
			h.config.Report(page.id, ReportCreateFailed, TruncateDetail("profile name: "+err.Error()))
			return
		}
		if err := options.putInPrivate(directive.Ephemeral); err != nil {
			h.dropPage(page.id)
			h.config.Report(page.id, ReportCreateFailed, TruncateDetail("inprivate: "+err.Error()))
			return
		}

		// Snapshot the host's children so the completion handler can
		// diff out this controller's own child HWND. WebView2 does not
		// expose it, and without it the pane cannot be raised.
		page.beforeChildren = childWindows(hwnd)

		handler := newControllerCompletedHandler(func(hr uintptr, controller *iCoreWebView2Controller) {
			h.controllerCompleted(page, hwnd, hr, controller)
		})
		page.pendingHandlers = append(page.pendingHandlers, handler)

		if err := env10.createControllerWithOptions(hwnd, options, handler); err != nil {
			h.dropPage(page.id)
			h.config.Report(page.id, ReportCreateFailed, TruncateDetail("create controller: "+err.Error()))
		}
	})
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
		h.dropPage(page.id)
		h.config.Report(page.id, ReportCreateFailed, TruncateDetail(err.Error()))
		return
	}
	if controller == nil {
		h.dropPage(page.id)
		h.config.Report(page.id, ReportCreateFailed, "controller completed with no controller")
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
		return
	}
	page.controller = controller
	// ICoreWebView2Controller2 is the whole background-colour surface.
	// Querying once here keeps every bounds directive off QueryInterface;
	// a nil result is reported the first time a colour is actually asked
	// for, not now, so a runtime that never sees a Bg stays quiet.
	page.controller2 = controller.queryController2()

	// Pages start hidden. browser_visibility is what presents one.
	if err := controller.putIsVisible(false); err != nil {
		h.config.Logf("browser host: page %s initial hide: %v", page.id, err)
	}

	// The child diff MUST be taken before the container is created:
	// creating it would add a second new child of the host and make the
	// diff ambiguous.
	page.child = newChildWindow(page.beforeChildren, childWindows(hwnd))
	page.beforeChildren = nil
	if page.child == 0 {
		h.config.Logf("browser host: page %s child window not identified; "+
			"the pane may render behind the app and cannot be clipped", page.id)
	} else if err := h.attachClipContainer(page, hwnd); err != nil {
		// Clipping is a presentation refinement, not a precondition: a
		// pane that overhangs is worse-looking than one that crops, and
		// far better than no pane. applyBounds falls back to host-relative
		// bounds whenever container is zero.
		h.config.Logf("browser host: page %s clip container: %v; pane will not clip", page.id, err)
		raiseChild(page.child)
	} else {
		raiseChild(page.container)
	}

	view, err := controller.coreWebView2()
	if err != nil || view == nil {
		h.discardCreated(page, "core webview: "+errString(err))
		return
	}
	page.view = view

	failHandler := newProcessFailedHandler(func(kind uint32) {
		h.config.Report(page.id, ReportProcessFailed, processFailedKindName(kind))
	})
	page.failHandler = failHandler
	page.pendingHandlers = append(page.pendingHandlers, failHandler)
	if _, err := view.addProcessFailed(failHandler); err != nil {
		h.config.Logf("browser host: page %s process-failed subscription: %v", page.id, err)
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
	page.pendingHandlers = append(page.pendingHandlers, done)
	if err := view.callDevToolsProtocolMethod("Target.getTargetInfo", "{}", done); err != nil {
		h.discardCreated(page, "Target.getTargetInfo dispatch: "+err.Error())
	}
}

// attachClipContainer creates the page's clip window and moves the
// controller into it through put_ParentWindow — WebView2's own reparent,
// so its cached parent (which Chromium places select dropdowns and IME
// candidate windows from) moves with the window. Runs on the UI thread.
//
// On failure the container is destroyed rather than left behind: a
// container the controller never entered is an invisible window that eats
// every click over its rectangle.
func (h *Host) attachClipContainer(page *hostPage, hwnd uintptr) error {
	container, err := createClipContainer(hwnd)
	if err != nil {
		return err
	}
	if err := page.controller.putParentWindow(container); err != nil {
		destroyWindow(container)
		return err
	}
	page.container = container
	return nil
}

func (h *Host) dropPage(pageID string) {
	h.mu.Lock()
	delete(h.pages, pageID)
	h.mu.Unlock()
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
	h.config.Report(page.id, ReportCreateFailed, TruncateDetail(detail))
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
