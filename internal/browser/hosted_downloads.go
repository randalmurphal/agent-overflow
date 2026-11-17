package browser

import (
	"context"
	"fmt"
	"strings"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Downloads on the hosted engine.
//
// The Windows-side browser writes a pane page's downloads straight into its
// jar's AO directory, through that directory's Windows view (`wslpath -w`,
// the `\\wsl.localhost\<distro>\...` UNC path the FileURL mapping uses).
// There is no copy step: the Manager renames the finished GUID-named file
// into the workspace directory exactly as it does for headless Chromium.
//
// Each WebView2 profile is its own CDP browser context, so the behavior is
// set per context: createPage reads its target's context and calls
// Browser.setDownloadBehavior (allowAndName, events enabled) with it before
// the Manager has the page, so nothing the page loads can download elsewhere.
// A refusal fails the page's creation with the engine's message; WebView2's
// default (the user's Downloads folder, no events) is never kept silently.
// Browser.cancelDownload carries the download's context for the same reason.
//
// NOT YET RUN AGAINST REAL WEBVIEW2. Three facts are unverified:
//   - whether WebView2 honors Browser.setDownloadBehavior through the
//     launcher's CDP relay for a named profile's context (Chromium's
//     DevTools delegate resolves a context id only for the default profile
//     and contexts DevTools created, so it may refuse, which fails every
//     pane page loudly until fixed);
//   - whether WebView2's own download flyout still appears;
//   - whether Chromium writes to the UNC path and reports it in the form the
//     mapping produced.
//
// The manual smoke in internal/webview2host (TestWebView2DownloadSmoke)
// answers all three.

// hostedDownloadScope is where one page's downloads land: its CDP browser
// context and the Windows view of its jar's AO download directory.
type hostedDownloadScope struct {
	context cdp.BrowserContextID
	dir     string
}

// keepDownloads points the downloads of targetID's browser context at the
// profile's AO directory and turns on the events the Manager accounts with.
func (e *hostedEngine) keepDownloads(browserCtx context.Context, targetID string, profile *hostedProfile) (hostedDownloadScope, error) {
	info, err := chromedp.CallBrowser(browserCtx, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: target.ID(targetID)})
	if err != nil {
		return hostedDownloadScope{}, fmt.Errorf("browser: read the pane page's browser context: %w", err)
	}
	if info.TargetInfo == nil {
		return hostedDownloadScope{}, fmt.Errorf("browser: the pane browser reported no target %s", targetID)
	}
	scope := hostedDownloadScope{context: info.TargetInfo.BrowserContextID, dir: profile.downloadDir}
	if _, err := chromedp.CallBrowser(browserCtx, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{
		Behavior:         cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName,
		BrowserContextID: scope.context,
		DownloadPath:     scope.dir,
		EventsEnabled:    new(true),
	}); err != nil {
		return hostedDownloadScope{}, fmt.Errorf("browser: keep the pane's downloads in %s: %w", scope.dir, err)
	}
	return scope, nil
}

// downloadEvents is the Manager's events with the hosted engine's part of a
// download in front: the start notes the download's scope, and progress
// trades the Windows path the engine reports for the Manager's own view.
func (e *hostedEngine) downloadEvents() engineEvents {
	events := e.events
	started, progress := events.DownloadStarted, events.DownloadProgress
	events.DownloadStarted = func(start downloadStart) bool {
		if scope, ok := e.frameDownloadScope(start.Frame); ok {
			e.mu.Lock()
			e.downloads.put(start.ID, scope)
			e.mu.Unlock()
		}
		return started(start)
	}
	events.DownloadProgress = func(event downloadProgress) {
		e.mu.Lock()
		scope, ok := e.downloads.get(event.ID)
		if downloadSettled(event.State) {
			e.downloads.delete(event.ID)
		}
		e.mu.Unlock()
		if ok && event.FilePath != "" {
			if hostedDownloadLanded(scope.dir, event.ID, event.FilePath) {
				// The Manager finds the file by its GUID in the jar directory.
				event.FilePath = ""
			} else {
				// Left as reported, which the Manager refuses as outside the
				// artifact directory; the log keeps where it went.
				e.logf("browser: pane download %s was written to %s, not %s", event.ID, event.FilePath, scope.dir)
			}
		}
		progress(event)
	}
	return events
}

// frameDownloadScope answers the scope of the page owning frame. A page's
// main frame id is its target id; a subframe is asked of each page.
func (e *hostedEngine) frameDownloadScope(frame string) (hostedDownloadScope, bool) {
	e.mu.Lock()
	if pageID, ok := e.pageByTarget[frame]; ok {
		scope, found := e.pageDownloads[pageID]
		e.mu.Unlock()
		return scope, found
	}
	drivers := make(map[string]*cdpPage, len(e.drivers))
	for pageID, driver := range e.drivers {
		drivers[pageID] = driver
	}
	e.mu.Unlock()
	for pageID, driver := range drivers {
		if !driver.OwnsFrame(frame) {
			continue
		}
		e.mu.Lock()
		scope, found := e.pageDownloads[pageID]
		e.mu.Unlock()
		return scope, found
	}
	return hostedDownloadScope{}, false
}

// hostedDownloadLanded reports whether a Windows path is the GUID-named file
// in dir. WSL's UNC host has two spellings; both name the same tree.
func hostedDownloadLanded(dir, id, windowsPath string) bool {
	normalize := func(path string) string {
		path = strings.ToLower(strings.ReplaceAll(path, "/", `\`))
		if rest, ok := strings.CutPrefix(path, `\\wsl$\`); ok {
			path = `\\wsl.localhost\` + rest
		}
		return strings.TrimRight(path, `\`)
	}
	return normalize(windowsPath) == normalize(dir)+`\`+strings.ToLower(id)
}

// cancelDownload cancels a download in the browser context it runs in.
func (e *hostedEngine) cancelDownload(id string) error {
	browserCtx, ok := e.browser()
	if !ok {
		return fmt.Errorf("browser: the pane browser is not connected")
	}
	e.mu.Lock()
	scope, _ := e.downloads.get(id)
	e.mu.Unlock()
	if _, err := chromedp.CallBrowser(browserCtx, cdpbrowser.CancelDownload, cdpbrowser.CancelDownloadParams{GUID: id, BrowserContextID: scope.context}); err != nil {
		return fmt.Errorf("browser: cancel download %s: %w", id, err)
	}
	return nil
}
