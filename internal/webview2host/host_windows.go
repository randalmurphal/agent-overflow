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
//
// The registry (pages) is the UI thread's alone. A page is looked up,
// checked and used, or taken out and torn down, in one UI-thread turn, so
// whatever holds a page in a turn holds a live one, and a page out of the
// registry has no controller left but an asynchronous create's, which its
// completion closes.
type Host struct {
	config Config

	// env is the pane environment, created once per generation; clear-data
	// starts a new generation (environment.go).
	env *paneEnvironment[paneEnv]

	// pages are the live pages by id, and closed is set by Close, after
	// which no page is admitted (admit). UI thread only.
	pages    map[string]*hostPage
	closed   bool
	clearing bool

	// mu guards accelerators, the bound-chord set every page's key events
	// are gated on (accelerators_windows.go), replaced whole by
	// OpAccelerators on the bridge goroutine.
	mu           sync.Mutex
	accelerators keybindings.AcceleratorSet

	cdpPort int
	// downloadToken and downloadSeq make the launcher's download ids
	// (download.go); downloadSeq is UI thread only.
	downloadToken string
	downloadSeq   uint64
	// popupSeq numbers popup page ids. UI thread only.
	popupSeq uint64
	// pendingPopups counts outstanding native popup controller creates. UI thread only.
	pendingPopups int
	// afterFunc runs fn after wait, on a goroutine of its own (after); nil
	// is time.AfterFunc. A test stands in its own.
	afterFunc func(wait time.Duration, fn func())
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
	// OnMain runs fn on the UI thread and returns once it has run, inline
	// when called there.
	OnMain func(fn func())
	// Post runs fn on the UI thread later, never inside the caller's call,
	// and returns at once; it may be called on the UI thread. A page's own
	// close request is posted, so the controller is not closed inside its
	// own event.
	Post func(fn func())
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

// after runs fn after wait, on a goroutine of its own.
func (h *Host) after(wait time.Duration, fn func()) {
	if h.afterFunc != nil {
		h.afterFunc(wait, fn)
		return
	}
	time.AfterFunc(wait, fn)
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
	if config.OnMain == nil || config.Post == nil {
		return nil, errors.New("browser host main-thread dispatchers are required")
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
	h.env = newPaneEnvironment(h.startEnvironment, freePaneEnv, config.Logf, envCreateTimeout)
	return h, nil
}

// CDPPort is the loopback port the pane environment's browser process
// serves DevTools on. It is fixed for the host's lifetime and is the only
// port the tunnel may reach.
func (h *Host) CDPPort() int { return h.cdpPort }

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
	var refused string
	h.config.OnMain(func() {
		switch {
		case h.closed:
			refused = "the browser host is closed"
		case h.clearing:
			refused = "the browser host is already clearing site data"
		default:
			h.clearing = true
			h.closeTaken(h.take(func(*hostPage) bool { return true }), nil)
			h.env.release()
		}
	})
	if refused != "" {
		h.config.Report(correlationID, ReportClearFailed, refused)
		return
	}
	err := clearUserDataDir(h.config.UserDataDir)
	h.config.OnMain(func() { h.clearing = false })
	if err != nil {
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
