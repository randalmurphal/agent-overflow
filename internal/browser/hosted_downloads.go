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
//     back to WebView2's own behavior (the user's Downloads folder). Setting
//     it again from another page first keeps it, so every leaving page has
//     the profile's other live pages set it again (leaveDownloads).
//   - Browser.cancelDownload with no browser context cancels a download of
//     any profile, and Chromium removes the partial file. A cancel of a
//     download that already finished is accepted and reports nothing.
//   - Page.setDownloadBehavior deny holds for the profile: a download is
//     reported begun and then canceled, and no file is written.
//   - WebView2 writes a `<guid>:Zone.Identifier` stream beside a finished
//     file, which on the WSL filesystem is a second file; the engine removes
//     it before the Manager moves the download.
//
// A refusal never fails the page: the page opens, the reason is logged, and
// the change is reported to the Manager (DownloadPolicyChanged), which tells
// the user once per change, whether a page's creation or a later re-set made
// it. A refused directory, or refused download events, turns the profile's
// downloads off with deny; each download the browser cancels is reported
// failed with the reason. Only when deny is refused too does a download
// reach the system Downloads folder: the user is told so, and AO cancels
// each download that starts and reports it as possibly saved there.
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

// hostedDownloadMode is what a profile does with a download.
type hostedDownloadMode int

const (
	// downloadsManaged: the download lands in the profile's AO directory.
	downloadsManaged hostedDownloadMode = iota
	// downloadsDenied: AO's directory or the download events were refused
	// and the profile denies downloads; the browser cancels each one itself.
	downloadsDenied
	// downloadsUncontained: denying was refused too, so WebView2 saves a
	// download to the system Downloads folder unless AO's cancel wins.
	downloadsUncontained
)

// addDownloadPage registers a new page's downloads under its profile and
// sets the profile's download behavior from the page's own session. Called
// before the Manager has the page, so nothing it loads downloads anywhere
// else.
func (e *hostedEngine) addDownloadPage(pageID string, page *cdpPage, profile *hostedProfile) {
	profile.behaviorMu.Lock()
	defer profile.behaviorMu.Unlock()
	e.mu.Lock()
	e.pageDownloads[pageID] = profile
	e.mu.Unlock()
	e.setDownloadBehavior(profile, pageID, page)
}

// leaveDownloads is called once per page before its session goes. The
// profile's behavior belongs to the session that set it last and falls back
// to WebView2's own (the system Downloads folder) when that session goes, so
// the profile's other live pages are asked, in turn, to set it again until
// one does. Whichever page set it last, the last page asked is one that was
// not leaving when it was asked, and every leaving page walks after it is
// marked; the walks are serialized per profile.
func (e *hostedEngine) leaveDownloads(pageID string) {
	e.mu.Lock()
	profile := e.pageDownloads[pageID]
	first := profile != nil && !e.downloadLeaving[pageID]
	if first {
		e.downloadLeaving[pageID] = true
	}
	e.mu.Unlock()
	if first {
		e.reassertDownloads(profile)
	}
}

// reassertDownloads sets the profile's download behavior from the first of
// its live pages, not leaving, that takes the call. A page that went away
// meanwhile is skipped; only a live page's refusal changes the profile.
func (e *hostedEngine) reassertDownloads(profile *hostedProfile) {
	profile.behaviorMu.Lock()
	defer profile.behaviorMu.Unlock()
	type candidate struct {
		id   string
		page *cdpPage
	}
	var candidates []candidate
	e.mu.Lock()
	for pageID, owner := range e.pageDownloads {
		if page := e.drivers[pageID]; owner == profile && page != nil && !e.downloadLeaving[pageID] {
			candidates = append(candidates, candidate{pageID, page})
		}
	}
	e.mu.Unlock()
	for _, c := range candidates {
		if e.setDownloadBehavior(profile, c.id, c.page) {
			return
		}
	}
}

// setDownloadBehavior sets the profile's download behavior from page's
// session: AO's directory while the profile is managed, deny once AO's
// directory or the connection's download events were refused. It answers
// false when the page went away before the browser answered, which says
// nothing about the profile. Called under profile.behaviorMu.
func (e *hostedEngine) setDownloadBehavior(profile *hostedProfile, pageID string, page *cdpPage) bool {
	e.mu.Lock()
	refusal := profile.downloadRefusal
	if refusal == "" {
		refusal = e.downloadEventsRefusal
	}
	e.mu.Unlock()
	if refusal == "" {
		err := setPageDownloadBehavior(page, cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName, profile.downloadDir)
		if err == nil {
			return true
		}
		// A page that went away refuses deny too, and is skipped there.
		refusal = fmt.Sprintf("the pane browser refused AO's download directory %s: %v", profile.downloadDir, err)
	}
	mode, denyRefusal := downloadsDenied, ""
	if err := setPageDownloadBehavior(page, cdpbrowser.SetDownloadBehaviorBehaviorDeny, ""); err != nil {
		if !e.downloadPageLive(pageID, page) {
			return false
		}
		mode, denyRefusal = downloadsUncontained, err.Error()
	}
	e.mu.Lock()
	changed := profile.downloadMode != mode || profile.downloadRefusal != refusal
	profile.downloadMode, profile.downloadRefusal, profile.downloadDenyRefusal = mode, refusal, denyRefusal
	e.mu.Unlock()
	if changed {
		e.logf("browser: %s", profile.downloadWarning(mode, refusal, denyRefusal))
		if report := e.events.DownloadPolicyChanged; report != nil {
			// Off this goroutine: a page close can run under the Manager's
			// lock, and the Manager reads the current warning anyway.
			go report(profile.id)
		}
	}
	return true
}

// downloadPageLive reports whether page is still the engine's live page
// pageID, so its refusal is the browser's answer and not its going away.
func (e *hostedEngine) downloadPageLive(pageID string, page *cdpPage) bool {
	if page.ctx.Err() != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drivers[pageID] == page
}

func setPageDownloadBehavior(page *cdpPage, behavior cdpbrowser.SetDownloadBehaviorBehavior, dir string) error {
	ctx, cancel := context.WithTimeout(page.ctx, hostedDownloadCallTimeout)
	defer cancel()
	_, err := chromedp.Call(ctx, pageSetDownloadBehavior, pageSetDownloadBehaviorParams{Behavior: behavior, DownloadPath: dir})
	return err
}

// downloadWarning is what the user is told about a profile whose downloads
// AO does not manage.
func (p *hostedProfile) downloadWarning(mode hostedDownloadMode, refusal, denyRefusal string) string {
	switch mode {
	case downloadsDenied:
		return "Downloads are off in this browser profile: " + refusal
	case downloadsUncontained:
		return "Downloads in this browser profile go to the system Downloads folder, outside AO: " + refusal + "; turning downloads off was refused too: " + denyRefusal
	}
	return ""
}

// downloadsWarning is the profile's warning, empty while AO manages its
// downloads (downloadPolicy).
func (p *hostedProfile) downloadsWarning() string {
	p.engine.mu.Lock()
	defer p.engine.mu.Unlock()
	return p.downloadWarning(p.downloadMode, p.downloadRefusal, p.downloadDenyRefusal)
}

// downloadEvents is the Manager's events with the hosted engine's part of a
// download in front: the start notes the download's profile, progress says
// why a denied profile's browser canceled it, checks the path the engine
// reports and removes the Zone.Identifier stream file. In a profile that
// could not be made to deny downloads, the start cancels the download and
// reports it failed as possibly saved to the system Downloads folder.
func (e *hostedEngine) downloadEvents() engineEvents {
	events := e.events
	started, progress := events.DownloadStarted, events.DownloadProgress
	events.DownloadStarted = func(start downloadStart) bool {
		profile := e.frameDownloadProfile(start.Frame)
		mode, refusal := downloadsManaged, ""
		if profile != nil {
			e.mu.Lock()
			e.downloads.put(start.ID, profile)
			mode, refusal = profile.downloadMode, profile.downloadRefusal
			e.mu.Unlock()
		}
		allowed := started(start)
		if allowed && mode == downloadsUncontained {
			go refusedDownloadCanceler(e.cancelDownload, e.logf)(start.ID)
			progress(downloadProgress{ID: start.ID, State: downloadCanceled, Error: "AO could not keep this download out of the system Downloads folder, where the browser may have saved it: " + refusal})
		}
		return allowed
	}
	events.DownloadProgress = func(event downloadProgress) {
		e.mu.Lock()
		profile, ok := e.downloads.get(event.ID)
		if downloadSettled(event.State) {
			e.downloads.delete(event.ID)
		}
		if ok && event.State == downloadCanceled && event.Error == "" && profile.downloadMode == downloadsDenied {
			event.Error = "downloads are off in this browser profile: " + profile.downloadRefusal
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
