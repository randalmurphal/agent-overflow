//go:build !nogui

// main_webview_shell.go is the window half every GUI boot shares
// (webviewShell): the Wails application and its one WebviewWindow, the
// page URL it navigated to, and the raw messages it accepts from that page.
package main

import (
	"errors"
	"log"
	"sync"
	"unsafe"

	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/uiwindow"
	"agent-overflow/internal/windowgeom"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// webviewShell is the window half of a GUI boot: it builds the Wails
// application, opens one WebviewWindow on a live transport URL, restores
// and tracks the window's geometry, and runs the app loop until the
// window closes.
//
// Two callers with different backends share it. runDesktop registers the
// App as a Wails service and lets Wails drive its lifecycle; the
// isolated windowed boots (--harness/--soak --window, see
// main_harness_window.go) have already started and MarkReady'd their
// backend by the time they get here, so they register no services, no
// single instance, and no updater. Everything that does NOT differ —
// window size, background colour, keybindings, cid threading, the
// ApplicationStarted creation order and the post-Run geometry flush —
// lives here once, so a windowed harness is the same shell the user
// runs rather than a lookalike of it.
type webviewShell struct {
	// title is both the application name and the window title.
	title string
	// singleInstance registers the desktop single-instance id. Isolated
	// instances leave it off: they are collision-free by construction and
	// N of them (one per checkout) may be open at once, so bouncing a
	// second launch into the first window would be wrong.
	singleInstance bool
	// services builds the Wails service list, given a getter for the
	// window (which does not exist until ApplicationStarted). It is called
	// after beforeRun, which decides whether the boot starts them. Nil
	// registers none.
	services func(getWindow func() *application.WebviewWindow) []application.Service
	// withWindow receives the same getter, for a boot that needs the window
	// but is NOT registering a service to reach it. The embedded browser's
	// in-process engine is the case: it must be handed the window before
	// App.Start, and the isolated windowed boots register no services at
	// all, so `services` is not a hook they have.
	withWindow func(getWindow func() *application.WebviewWindow)
	// beforeRun runs after application.New, which claims the
	// single-instance identity, and before the app loop starts. runDesktop
	// reconciles the update record and boots its transport here, because
	// the updater must observe the application first. It returns false to
	// end the boot without opening the window.
	beforeRun func(app *application.App) bool
	// pages, when set, serves the startup pages as the application's
	// assets. A failure shown before the window opens is the window's page
	// instead of pageURL's.
	pages *desktopPages
	// pageURL returns the CURRENT page URL, bare and marked
	// webview-hosted (the transport's WebviewPageURL). A getter, not a
	// value: Ctrl+R re-reads it so a rebind (the LAN toggle) reloads
	// onto the new origin.
	pageURL func() string
	// mintTicket hands out one one-time page ticket. Separate from
	// pageURL because this shell owns the window: the credential is
	// injected into each document it loads rather than written into the
	// URL that loaded it (internal/uiwindow.DeliverPageTicket).
	mintTicket func() (string, error)
	// loadGeometry / persistGeometry are the saved window placement's
	// reader and writer. Both resolve through the boot data dir, so an
	// isolated instance remembers its own window, not the user's.
	loadGeometry    func() windowgeom.Geometry
	persistGeometry func(windowgeom.Geometry)
	// chords routes Ctrl/Cmd+R to the SPA while it asks for the chord
	// (uikeys.PageChords); run sets it.
	chords *uikeys.PageChords
	// navigated is the page URL the window last navigated to, the origin
	// its raw messages must come from; run sets it.
	navigated *navigatedPage
}

// navigatedPage is the page URL a window last navigated to. It is taken
// when the window navigates (its first page, each reload), so checking a
// raw message's origin calls no pageURL: the existing-backend desktop's
// reads the rendezvous and mints a window ticket over HTTP.
type navigatedPage struct {
	mu  sync.Mutex
	url string
}

func (p *navigatedPage) set(url string) {
	p.mu.Lock()
	p.url = url
	p.mu.Unlock()
}

func (p *navigatedPage) get() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// run opens the window and blocks until the app loop exits, returning
// the Wails run error. It does not tear down the backend: what
// shutdown means differs per caller, and the shell knows nothing about
// transports or App lifecycles.
func (s webviewShell) run() error {
	// window + its tracker flush are created on the ApplicationStarted handler
	// goroutine (see below) and read by the single-instance callback and the
	// post-Run backstop, so guard both.
	var (
		winMu         sync.Mutex
		window        *application.WebviewWindow
		flushGeometry func()
	)
	getWindow := func() *application.WebviewWindow {
		winMu.Lock()
		defer winMu.Unlock()
		return window
	}
	appOpts := desktopApplicationOptions(s.title)
	s.chords = uikeys.NewPageChords()
	s.navigated = &navigatedPage{}
	appOpts.RawMessageHandler = s.rawMessageHandler(getWindow)
	if s.singleInstance {
		appOpts.SingleInstance = desktopSingleInstanceOptions(getWindow)
	}
	if s.pages != nil {
		appOpts.Assets = application.AssetOptions{Handler: s.pages}
	}
	if s.withWindow != nil {
		s.withWindow(getWindow)
	}
	app := application.New(appOpts)
	if s.beforeRun != nil && !s.beforeRun(app) {
		return nil
	}
	if s.services != nil {
		for _, service := range s.services(getWindow) {
			app.RegisterService(service)
		}
	}
	opts, err := s.windowOptions()
	if err != nil {
		return err
	}
	// Reopen where we left off last. The window is created on ApplicationStarted
	// (not here) so it materializes synchronously against a live app loop — that
	// lets uiwindow.RestoreAndTrack maximize/fullscreen a restored window on the
	// monitor it was saved on, and reveal it already in that state, instead of
	// flashing at normal size or always landing on the primary. See
	// uiwindow.RestoreAndTrack for why creation must happen here.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		w, flush := uiwindow.RestoreAndTrack(app, opts, s.loadGeometry(), s.persistGeometry)
		uiwindow.DeliverPageTicket(w, s.mintTicket)
		w.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
			s.chords.DocumentLoaded(w)
		})
		if s.pages != nil {
			s.pages.attach(w, opts.URL)
		}
		winMu.Lock()
		window = w
		flushGeometry = flush
		winMu.Unlock()
	})

	runErr := app.Run()
	// Backstop the WindowClosing flush: persist the final placement from the
	// tracker's in-memory latest (safe even though the window is now gone).
	winMu.Lock()
	flush := flushGeometry
	winMu.Unlock()
	if flush != nil {
		flush()
	}
	return runErr
}

// windowOptions are the window's options: on the app's page, or on the
// failure the boot shows instead.
func (s webviewShell) windowOptions() (application.WebviewWindowOptions, error) {
	opts := application.WebviewWindowOptions{
		Title:            s.title,
		Width:            1280,
		Height:           800,
		MinWidth:         800,
		MinHeight:        600,
		BackgroundColour: bootWindowBackgroundColour(),
	}
	if s.pages.showing() {
		opts.URL = startuppage.FailurePath
		opts.KeyBindings = uikeys.WithDevTools(uikeys.Browser())
		return opts, nil
	}

	// Assert non-empty before constructing the WebviewWindowOptions.
	// Wails maps URL "" back to its built-in scheme, which would expose
	// the in-process IPC surface we deliberately replaced with HTTP+WS.
	// AppURL() returns "" pre-Start or when both the cached and live
	// listener addresses fail to parse — both are pathological boot
	// states that warrant an obvious failure rather than a silent IPC
	// fallthrough or a port-less URL hitting port 80.
	appURL := s.pageURL()
	if appURL == "" {
		return opts, errors.New("transport: page URL is empty after Start; refusing to fall through to the Wails IPC scheme")
	}

	// Thread the durable UI-state client ID onto the page URL (and the
	// Ctrl+R reload URL) so the frontend's per-client ui_state bucket
	// survives the per-launch origin change. Empty cid degrades to the
	// frontend's browser-cached fallback identity.
	clientID := ensureClientID()
	withClientID := func(pageURL string) string { return appURLWithClientID(pageURL, clientID) }
	navigate := func(pageURL string) string {
		if s.navigated != nil {
			s.navigated.set(pageURL)
		}
		return withClientID(pageURL)
	}
	reloadURL := func() string { return navigate(s.pageURL()) }

	// Context-menu policy lives in the frontend guard
	// (browserHistoryGuard.ts): native menu allowed in editable fields
	// and on selected text, suppressed elsewhere. Don't set
	// DefaultContextMenuDisabled — it would hard-disable the allowed
	// menus below the JS layer (on the platforms where it does
	// anything at all). F12 devtools is a compiled no-op in production
	// builds, so WithDevTools is safe unconditionally here.
	opts.URL = navigate(appURL)
	opts.KeyBindings = uikeys.WithDevTools(uikeys.BrowserWithReload(reloadURL))
	if s.chords != nil {
		opts.KeyBindings = s.chords.Bindings(opts.KeyBindings)
	}
	return opts, nil
}

// rawMessageHandler is Options.RawMessageHandler: the SPA's browser pane
// focus reports, accepted only from this window's page on the origin the
// window last navigated to.
func (s webviewShell) rawMessageHandler(getWindow func() *application.WebviewWindow) func(application.Window, string, *application.OriginInfo) {
	return func(from application.Window, message string, origin *application.OriginInfo) {
		own := getWindow()
		if s.chords == nil || s.navigated == nil || own == nil || from == nil || from.ID() != own.ID() ||
			!uikeys.TrustedPage(origin, s.navigated.get()) {
			log.Printf("desktop: ignore raw webview message from an untrusted document")
			return
		}
		if !s.chords.HandleMessage(from, message) {
			log.Printf("desktop: ignore unknown raw webview message (%d bytes)", len(message))
		}
	}
}

// nativeWindowPointer adapts a Wails window getter to the raw handle the
// browser engine wants. nil answers "no window yet": the window is created
// on ApplicationStarted, and the engine resolves this only when its first
// page starts, so the gap is real and expected on every windowed boot.
func nativeWindowPointer(getWindow func() *application.WebviewWindow) func() unsafe.Pointer {
	return func() unsafe.Pointer {
		window := getWindow()
		if window == nil {
			return nil
		}
		return window.NativeWindow()
	}
}
