//go:build windows

// browserhost.go is the launcher's half of the embedded browser pane. The
// backend owns what the pane shows and where it sits; this process owns
// the WebView2 controllers that draw it, because they must be child
// windows of the launcher's own HWND and driven from its UI thread. The
// backend cannot make a single one of those calls from inside the distro.
//
// Two pieces, built together on the first directive:
//
//   - webview2host.Host: a SECOND WebView2 environment, with its own
//     user-data folder and one named CoreWebView2Profile per workspace,
//     started with --remote-debugging-port on a free loopback port.
//   - webview2host.CDPTunnel: the launcher DIALS the backend's existing
//     bridge URL and relays that debugging port to it. Nothing listens
//     across the WSL boundary, and the direction matches the notification
//     bridge exactly, down to the launch token.
//
// Lazily, rather than at boot: the feature costs a browser process and a
// profile directory, most sessions never open a pane, and a backend
// without the feature simply never emits on the channel. It also avoids
// a bootstrap-schema flag that would have to be kept in sync for no gain
// (the first directive proves the backend has the feature better than a
// field claiming it does).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/uikeys"
	"agent-overflow/internal/webview2host"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// browserProfilesDir is the pane environment's user-data folder, beside
// the SPA's own under %APPDATA%. Empty when %APPDATA% is unresolvable,
// which refuses the host rather than letting WebView2 pick a default
// folder shared with the SPA.
func browserProfilesDir(mode string) string {
	dir, ok := wsldistro.WSLConfigDir()
	if !ok {
		return ""
	}
	return filepath.Join(dir, appidentity.BrowserProfilesDir(mode))
}

// prepareBrowserProfileStorage validates and creates the pane
// environment's user-data folder, refusing symlinked and reparse-point
// components exactly as prepareWebviewStorage does for the SPA's. Same
// reason, and it is stronger here: this folder accumulates the cookies
// and localStorage of whatever the user browses in the pane.
func prepareBrowserProfileStorage(mode string) (string, error) {
	dir := browserProfilesDir(mode)
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("AppData did not resolve to a private browser profile directory")
	}
	if err := validateWindowsStoragePath(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	if err := validateWindowsStoragePath(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// handleBrowserHostDirective is NotificationClientConfig.HandleBrowserHost.
// It runs on the bridge's lane for the directive's page, which takes the
// page's directives one at a time, so blocking on the UI thread here is
// expected; the first create also waits for the WebView2 environment.
func (a *launcherApp) handleBrowserHostDirective(bs *wsllauncher.Bootstrap, directive webview2host.Directive) {
	host, err := a.ensureBrowserHost(bs)
	if err != nil {
		log.Printf("browser host: cannot host directive %q: %v", directive.Op, err)
		// The two ops the backend BLOCKS on become visible failures rather
		// than a pane that never appears or a Settings button that spins
		// until its own timeout. The rest address a page that, by
		// definition, was never created.
		if directive.Op == webview2host.OpCreate {
			a.reportBrowserHost(directive.PageID, webview2host.ReportCreateFailed, err.Error())
		}
		if directive.Op == webview2host.OpClearData {
			a.reportBrowserHost(directive.PageID, webview2host.ReportClearFailed, err.Error())
		}
		return
	}
	host.Apply(directive)
}

// ensureBrowserHost builds the host and its tunnel once. A failure is not
// cached: every input to it (AppData, the profile directory, a free port)
// can come back, and the retry costs one directive rather than leaving
// the pane dead until the launcher restarts.
func (a *launcherApp) ensureBrowserHost(bs *wsllauncher.Bootstrap) (*webview2host.Host, error) {
	a.mu.Lock()
	existing := a.browserHost
	ctx := a.notificationContext
	a.mu.Unlock()
	if existing != nil {
		return existing, nil
	}

	userDataDir, err := prepareBrowserProfileStorage(launcherRuntimeMode())
	if err != nil {
		return nil, fmt.Errorf("browser profile storage: %w", err)
	}
	host, err := webview2host.New(webview2host.Config{
		// Resolved per call: the window is created on the
		// ApplicationStarted handler, and Wails' renderer-hang recovery can
		// replace the WebView2 inside it.
		HostWindow: func() uintptr {
			window := a.win()
			if window == nil {
				return 0
			}
			return uintptr(window.NativeWindow())
		},
		UserDataDir: userDataDir,
		// The display preferences the app window launched with
		// (displayBrowserArgs), so one restart changes both environments.
		ExtraBrowserArgs: displayBrowserArgs(a.disableGPUVsync),
		Logf:             log.Printf,
		Report:           a.reportBrowserHost,
		// WebView2 requires every COM and window call on the thread owning
		// the host window, which is the thread Wails runs its message pump
		// on.
		OnMain: application.InvokeSync,
		// Wails runs InvokeAsync inline when called on the UI thread; from
		// another goroutine it posts to the pump, so a post made inside a
		// WebView2 event runs after the event returns.
		Post:     func(fn func()) { go application.InvokeAsync(fn) },
		Geometry: a.paneGeometry,
		Focus:    a.paneFocus,
	})
	if err != nil {
		return nil, err
	}

	tunnel, err := webview2host.NewCDPTunnel(webview2host.CDPTunnelConfig{
		WSURL:   fmt.Sprintf("ws://127.0.0.1:%d%s", bs.Port, webview2host.CDPTunnelPath),
		Token:   bs.Token,
		CDPPort: host.CDPPort(),
		Logf:    log.Printf,
	})
	if err != nil {
		host.Close()
		return nil, err
	}

	a.mu.Lock()
	if a.browserHost != nil {
		// Another directive won the race. Keep the winner: two hosts would
		// mean two environments on one user-data folder, which WebView2
		// refuses outright.
		winner := a.browserHost
		a.mu.Unlock()
		host.Close()
		return winner, nil
	}
	a.browserHost = host
	a.mu.Unlock()

	log.Printf("browser host: ready, profiles in %s, cdp on 127.0.0.1:%d", userDataDir, host.CDPPort())
	if ctx == nil {
		ctx = context.Background()
	}
	go tunnel.Run(ctx)
	return host, nil
}

// handleRawMessage is Options.RawMessageHandler: the SPA's reload chord
// reports (uikeys.PageChords), its page focus intents and its pane geometry.
func (a *launcherApp) handleRawMessage(window application.Window, message string, origin *application.OriginInfo) {
	if strings.HasPrefix(message, webview2host.FocusMessagePrefix) {
		a.handlePaneFocusMessage(window, message, origin)
		return
	}
	if !strings.HasPrefix(message, uikeys.MessagePrefix) {
		a.handlePaneGeometryMessage(window, message, origin)
		return
	}
	if a.chords == nil || !a.trustedPaneGeometrySource(window, origin) {
		log.Printf("launcher: ignore a chord message from an untrusted document")
		return
	}
	a.chords.HandleMessage(window, message)
}

// handlePaneGeometryMessage takes the geometry messages. The SPA posts each
// browser pane geometry report here as well as to the backend, so a pane
// that scrolls or slides moves its page on this process's UI thread without
// the round trip through WSL (webview2host/geometry.go). Wails calls it on a
// goroutine per message: the mailbox orders reports by their sequence, and
// at most one drain is ever queued on the UI thread, which this never waits
// on.
func (a *launcherApp) handlePaneGeometryMessage(window application.Window, message string, origin *application.OriginInfo) {
	report, err := webview2host.ParseGeometryMessage(message)
	if errors.Is(err, webview2host.ErrNotGeometry) {
		log.Printf("launcher: ignore unknown raw webview message (%d bytes)", len(message))
		return
	}
	if err != nil {
		log.Printf("browser host: ignore invalid pane geometry: %v", err)
		return
	}
	if !a.trustedPaneGeometrySource(window, origin) {
		log.Printf("browser host: ignore pane geometry from an untrusted document")
		return
	}
	if a.paneGeometry.Put(report) {
		application.InvokeAsync(a.drainPaneGeometry)
	}
}

// handlePaneFocusMessage takes a page focus intent's arm or withdrawal
// (webview2host/focusintent.go), which must reach the launcher before a
// focus directive that crosses WSL after it.
func (a *launcherApp) handlePaneFocusMessage(window application.Window, message string, origin *application.OriginInfo) {
	msg, err := webview2host.ParseFocusIntentMessage(message)
	if err != nil {
		log.Printf("browser host: ignore invalid pane focus message: %v", err)
		return
	}
	if !a.trustedPaneGeometrySource(window, origin) {
		log.Printf("browser host: ignore a pane focus message from an untrusted document")
		return
	}
	a.paneFocus.Note(msg)
}

// trustedPaneGeometrySource accepts only the launcher's own window showing
// the backend's SPA.
func (a *launcherApp) trustedPaneGeometrySource(window application.Window, origin *application.OriginInfo) bool {
	a.mu.Lock()
	own := a.window
	bs := a.backendBootstrap
	a.mu.Unlock()
	if own == nil || window == nil || bs == nil || origin == nil || window.ID() != own.ID() {
		return false
	}
	return webview2host.TrustedGeometrySource(origin.Origin, origin.TopOrigin, bs.PageURL)
}

// drainPaneGeometry is the one queued UI-thread callback: it takes every
// report since the last drain and re-presents the pages bound to them.
// Reports that land before the host exists wait in the mailbox for the
// page's first bounds directive.
func (a *launcherApp) drainPaneGeometry() {
	reports := a.paneGeometry.Take()
	a.mu.Lock()
	host := a.browserHost
	a.mu.Unlock()
	if host != nil {
		host.ApplyGeometry(reports)
	}
}

// advertisePaneGeometry tells each document the window loads that this
// launcher takes geometry and page focus messages; a SPA posts none without
// it, so a newer SPA under an older launcher only reports to the backend.
func advertisePaneGeometry(window *application.WebviewWindow) {
	window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		window.ExecJS(webview2host.GeometryCapabilityScript)
		window.ExecJS(webview2host.FocusCapabilityScript)
	})
}

// reportBrowserHost posts one answer back over the bridge the directive
// arrived on.
//
// It must not block its caller: the host reports `created` from a WebView2
// completion handler, which runs INLINE on the UI thread, and an RPC that
// waits up to its timeout there would freeze the whole launcher window. It
// must not be a bare `go` either — the backend would then be free to see
// `closed` before the `created` that carries the page's CDP target id. A
// serial queue is both: submission returns at once, delivery stays in
// order, and a download's waiting progress is replaced by its newer
// progress (webview2host.ReportQueue), so a stalled bridge does not queue
// a popup's report behind obsolete byte counts.
func (a *launcherApp) reportBrowserHost(pageID string, kind webview2host.ReportKind, detail string) {
	a.browserReports.Submit(pageID, kind, detail)
}

// deliverBrowserReport sends one queued report. Delivery is best effort,
// with no retry. A report that did not go through is handed back to the
// host (Host.ReportUndelivered), which closes a popup the backend may never
// adopt; any other lost report costs the backend one page handle it
// re-derives on the next round trip.
func (a *launcherApp) deliverBrowserReport(pageID string, kind webview2host.ReportKind, detail string) {
	a.mu.Lock()
	client := a.notificationClient
	ctx := a.notificationContext
	host := a.browserHost
	a.mu.Unlock()
	var err error
	if client == nil {
		err = errors.New("bridge is down")
	} else {
		if ctx == nil {
			ctx = context.Background()
		}
		err = client.ReportBrowserHost(ctx, pageID, kind, detail)
	}
	if err == nil {
		return
	}
	log.Printf("browser host: report %s for page %s: %v", kind, pageID, err)
	if host != nil {
		host.ReportUndelivered(pageID, kind)
	}
}

// closeBrowserHost tears every pane controller down. Called on shutdown,
// before the launcher window goes away: a controller outliving its parent
// HWND is a crash in the WebView2 layer, not a leak.
func (a *launcherApp) closeBrowserHost() {
	a.mu.Lock()
	host := a.browserHost
	a.browserHost = nil
	a.mu.Unlock()
	if host != nil {
		host.Close()
	}
}
