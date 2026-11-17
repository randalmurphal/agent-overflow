package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
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
// The protocol is what real WebView2 accepts for a named profile
// (TestDownloadSmoke in internal/webview2host runs it):
//
//   - Browser.setDownloadBehavior naming a profile's browser context is
//     refused ("Failed to find browser context"), so the engine never sends
//     it. Once per connection it sends the call with no context and behavior
//     "default": the default profile, which AO never opens, keeps WebView2's
//     behavior, and Browser.download* events are reported for every profile.
//   - Page.setDownloadBehavior allowAndName on a page's own session sets the
//     behavior of the page's whole profile; the file is the download's GUID
//     in the directory, and Browser.downloadProgress reports its path.
//   - When the page whose session set it last goes away, the profile falls
//     back to WebView2's own behavior (the user's Downloads folder), so that
//     page hands the setting to another page of the profile first
//     (handOverDownloads).
//   - Browser.cancelDownload with no browser context cancels a download of
//     any profile, and Chromium removes the partial file.
//   - WebView2 writes a `<guid>:Zone.Identifier` stream beside a finished
//     file, which on the WSL filesystem is a second file; the engine removes
//     it before the Manager moves the download.
//
// A refusal never fails the page: the page opens, the reason is logged, the
// Manager tells the user once per profile (downloadsUnmanaged), and every
// download that starts in the profile is cancelled and reported failed with
// the reason, so nothing lands in the user's Downloads folder unaccounted.
// Whether WebView2's own download flyout opens has no CDP signal and is not
// checked.

// pageSetDownloadBehavior is Page.setDownloadBehavior, which cdproto no
// longer generates (the protocol marks it deprecated) and WebView2 is the
// one engine that needs.
var pageSetDownloadBehavior = cdp.Command[pageSetDownloadBehaviorParams, cdp.Empty]{Method: "Page.setDownloadBehavior"}

type pageSetDownloadBehaviorParams struct {
	Behavior     cdpbrowser.SetDownloadBehaviorBehavior `json:"behavior"`
	DownloadPath string                                 `json:"downloadPath,omitempty"`
}

// hostedDownloadCallTimeout bounds one download-behavior call.
const hostedDownloadCallTimeout = 10 * time.Second

// enableDownloadEvents turns on Browser.download* events for the connection.
// A refusal is kept: no page can then account for its downloads.
func (e *hostedEngine) enableDownloadEvents(browserCtx context.Context) {
	ctx, cancel := context.WithTimeout(browserCtx, hostedDownloadCallTimeout)
	defer cancel()
	_, err := chromedp.CallBrowser(ctx, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{
		Behavior: cdpbrowser.SetDownloadBehaviorBehaviorDefault, EventsEnabled: new(true),
	})
	refusal := ""
	if err != nil {
		refusal = fmt.Sprintf("the pane browser does not report downloads: %v", err)
		e.logf("browser: %s", refusal)
	}
	e.mu.Lock()
	e.downloadEventsRefusal = refusal
	e.mu.Unlock()
}

// keepDownloads points the downloads of the page's profile at the profile's
// AO directory, from the page's own session, and makes the page the one
// that holds the setting. Without download events it turns downloads off
// instead. Either refusal is recorded on the profile, never returned.
func (e *hostedEngine) keepDownloads(pageID string, page *cdpPage, profile *hostedProfile) {
	profile.behaviorMu.Lock()
	defer profile.behaviorMu.Unlock()
	e.mu.Lock()
	refusal := e.downloadEventsRefusal
	e.pageDownloads[pageID] = profile
	e.mu.Unlock()
	if refusal != "" {
		if err := setPageDownloadBehavior(page, cdpbrowser.SetDownloadBehaviorBehaviorDeny, ""); err != nil {
			refusal += fmt.Sprintf("; turning them off was refused too: %v", err)
		}
	} else if err := setPageDownloadBehavior(page, cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName, profile.downloadDir); err != nil {
		refusal = fmt.Sprintf("the pane browser refused AO's download directory %s: %v", profile.downloadDir, err)
	}
	e.mu.Lock()
	profile.downloadRefusal = refusal
	if refusal == "" {
		profile.downloadKeeper = pageID
	}
	e.mu.Unlock()
	if refusal != "" {
		e.logf("browser: downloads in pane profile %s are not managed: %s", profile.id, refusal)
	}
}

func setPageDownloadBehavior(page *cdpPage, behavior cdpbrowser.SetDownloadBehaviorBehavior, dir string) error {
	ctx, cancel := context.WithTimeout(page.ctx, hostedDownloadCallTimeout)
	defer cancel()
	_, err := chromedp.Call(ctx, pageSetDownloadBehavior, pageSetDownloadBehaviorParams{Behavior: behavior, DownloadPath: dir})
	return err
}

// downloadHandover answers the call that moves the profile's download
// setting off pageID onto another live page of the profile, nil when pageID
// does not hold it or no other page is left (the profile then has no page to
// download from). It must be taken while pageID is still bound; run it
// before the page's session goes when the page is closed on purpose.
func (e *hostedEngine) downloadHandover(pageID string) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	profile := e.pageDownloads[pageID]
	if profile == nil || profile.downloadKeeper != pageID {
		return nil
	}
	profile.downloadKeeper = ""
	for heirID, owner := range e.pageDownloads {
		heir := e.drivers[heirID]
		if heirID == pageID || owner != profile || heir == nil {
			continue
		}
		return func() { e.handOverDownloads(profile, heirID, heir) }
	}
	return nil
}

func (e *hostedEngine) handOverDownloads(profile *hostedProfile, heirID string, heir *cdpPage) {
	profile.behaviorMu.Lock()
	defer profile.behaviorMu.Unlock()
	e.mu.Lock()
	// A page created since has set it; it holds the setting now.
	taken := profile.downloadKeeper != "" || profile.downloadRefusal != ""
	e.mu.Unlock()
	if taken {
		return
	}
	err := setPageDownloadBehavior(heir, cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName, profile.downloadDir)
	e.mu.Lock()
	if err != nil {
		profile.downloadRefusal = fmt.Sprintf("the pane browser refused AO's download directory %s: %v", profile.downloadDir, err)
	} else if _, live := e.drivers[heirID]; live {
		profile.downloadKeeper = heirID
	}
	refusal := profile.downloadRefusal
	e.mu.Unlock()
	if refusal != "" {
		e.logf("browser: downloads in pane profile %s are not managed: %s", profile.id, refusal)
	}
}

// downloadsUnmanaged is why downloads in the page's profile are not
// managed, empty when they are. The Manager tells the user once per profile.
func (p *hostedPage) downloadsUnmanaged() string {
	p.engine.mu.Lock()
	defer p.engine.mu.Unlock()
	if profile := p.engine.pageDownloads[p.id]; profile != nil {
		return profile.downloadRefusal
	}
	return ""
}

// downloadEvents is the Manager's events with the hosted engine's part of a
// download in front: the start notes the download's profile and stops one
// the profile cannot account for, and progress checks the path the engine
// reports and removes the Zone.Identifier stream file.
func (e *hostedEngine) downloadEvents() engineEvents {
	events := e.events
	started, progress := events.DownloadStarted, events.DownloadProgress
	events.DownloadStarted = func(start downloadStart) bool {
		profile := e.frameDownloadProfile(start.Frame)
		refusal := ""
		if profile != nil {
			e.mu.Lock()
			e.downloads.put(start.ID, profile)
			refusal = profile.downloadRefusal
			e.mu.Unlock()
		}
		allowed := started(start)
		if allowed && refusal != "" {
			// WebView2 would save it to its own folder.
			go refusedDownloadCanceler(e.cancelDownload, e.logf)(start.ID)
			progress(downloadProgress{ID: start.ID, State: downloadCanceled, Error: "downloads are off in this browser profile: " + refusal})
		}
		return allowed
	}
	events.DownloadProgress = func(event downloadProgress) {
		e.mu.Lock()
		profile, ok := e.downloads.get(event.ID)
		if downloadSettled(event.State) {
			e.downloads.delete(event.ID)
		}
		e.mu.Unlock()
		if ok && downloadSettled(event.State) {
			stream := filepath.Join(profile.backendDownloadDir, filepath.Base(event.ID)+":Zone.Identifier")
			if err := os.Remove(stream); err != nil && !errors.Is(err, fs.ErrNotExist) {
				e.logf("browser: remove %s: %v", stream, err)
			}
		}
		if ok && event.FilePath != "" {
			if hostedDownloadLanded(profile.downloadDir, event.ID, event.FilePath) {
				// The Manager finds the file by its GUID in the jar directory.
				event.FilePath = ""
			} else {
				// Left as reported, which the Manager refuses as outside the
				// artifact directory; the log keeps where it went.
				e.logf("browser: pane download %s was written to %s, not %s", event.ID, event.FilePath, profile.downloadDir)
			}
		}
		progress(event)
	}
	return events
}

// frameDownloadProfile answers the profile of the page owning frame. A
// page's main frame id is its target id; a subframe is asked of each page.
func (e *hostedEngine) frameDownloadProfile(frame string) *hostedProfile {
	e.mu.Lock()
	if pageID, ok := e.pageByTarget[frame]; ok {
		profile := e.pageDownloads[pageID]
		e.mu.Unlock()
		return profile
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
		profile := e.pageDownloads[pageID]
		e.mu.Unlock()
		return profile
	}
	return nil
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

// cancelDownload cancels a download. WebView2 finds it by GUID alone; the
// call naming the profile's browser context is the one it refuses.
func (e *hostedEngine) cancelDownload(id string) error {
	browserCtx, ok := e.browser()
	if !ok {
		return fmt.Errorf("browser: the pane browser is not connected")
	}
	if _, err := chromedp.CallBrowser(browserCtx, cdpbrowser.CancelDownload, cdpbrowser.CancelDownloadParams{GUID: id}); err != nil {
		return fmt.Errorf("browser: cancel download %s: %w", id, err)
	}
	return nil
}
