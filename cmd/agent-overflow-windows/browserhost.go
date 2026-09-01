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
	"agent-overflow/internal/webview2host"
	"agent-overflow/internal/wsldistro"
	"agent-overflow/internal/wsllauncher"

	"github.com/wailsapp/wails/v3/pkg/application"
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
// It runs on the bridge's per-directive goroutine, so blocking on the UI
// thread here is expected; the first call additionally blocks on a cold
// WebView2 environment create.
func (a *launcherApp) handleBrowserHostDirective(bs *wsllauncher.Bootstrap, directive webview2host.Directive) {
	host, err := a.ensureBrowserHost(bs)
	if err != nil {
		log.Printf("browser host: cannot host directive %q: %v", directive.Op, err)
		// The two ops the backend BLOCKS on become visible failures rather
		// than a pane that never appears or a Settings button that spins
		// until its own timeout. The rest address a page that, by
		// definition, was never created.
		switch directive.Op {
		case webview2host.OpCreate:
			a.reportBrowserHost(directive.PageID, webview2host.ReportCreateFailed, err.Error())
		case webview2host.OpClearData:
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
		Logf:        log.Printf,
		Report:      a.reportBrowserHost,
		// WebView2 requires every COM and window call on the thread owning
		// the host window, which is the thread Wails runs its message pump
		// on.
		OnMain: application.InvokeSync,
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

// reportBrowserHost posts one answer back over the bridge the directive
// arrived on.
//
// It must not block its caller: the host reports `created` from a WebView2
// completion handler, which runs INLINE on the UI thread, and an RPC that
// waits up to its timeout there would freeze the whole launcher window. It
// must not be a bare `go` either — the backend would then be free to see
// `closed` before the `created` that carries the page's CDP target id. A
// serial queue is both: submission returns at once, delivery stays in
// order.
//
// Delivery itself is best effort. A lost report costs the backend one page
// handle it re-derives on the next round trip.
func (a *launcherApp) reportBrowserHost(pageID string, kind webview2host.ReportKind, detail string) {
	a.browserReports.Go(func() {
		a.mu.Lock()
		client := a.notificationClient
		ctx := a.notificationContext
		a.mu.Unlock()
		if client == nil {
			log.Printf("browser host: cannot report %s for page %s: bridge is down", kind, pageID)
			return
		}
		if ctx == nil {
			ctx = context.Background()
		}
		if err := client.ReportBrowserHost(ctx, pageID, kind, detail); err != nil {
			log.Printf("browser host: report %s for page %s: %v", kind, pageID, err)
		}
	})
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
