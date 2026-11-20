package browser

import (
	"errors"
	"fmt"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/keybindings"
)

// What an engine reports to the Manager across the engine seam (driver.go):
// its events (engineEvents), the popups and downloads they name, and where
// its downloads are written (downloadSaving).

// downloadSaving is where an engine's downloads are written and where a
// finished one ends up (browserEngine.DownloadSaving, downloads.go).
type downloadSaving int

const (
	// downloadsStaged: the engine writes each download into the staged file
	// profileOptions.StageDownload names, in the app's private staging
	// directory in the user's Downloads folder, and the Manager publishes
	// each finished file beside it (publishDownload). The engines in this
	// process: WebKitGTK, WKWebView.
	downloadsStaged downloadSaving = iota
	// downloadsAsArtifacts: the engine writes into profileOptions.DownloadDir
	// under engine ids, the Manager moves each finished file to its
	// workspace's artifact directory, and quotas, the per-download cap and
	// artifact pruning apply. Headless Chromium in serve mode alone: it runs
	// on a server, where no user has a Downloads folder to look in.
	downloadsAsArtifacts
	// downloadsSavedByEngine: the engine saves into the Downloads folder of
	// the machine its pages run on and reports the path. The launcher-hosted
	// WebView2.
	downloadsSavedByEngine
)

// enginePopup is a page the engine opened on its own behalf. Nothing about
// ownership is decided here: the Manager matches the opener and applies its
// own per-thread and per-workspace limits before adopting it.
type enginePopup struct {
	Profile profileHandle
	Opener  ids.PageHandle
	Handle  ids.PageHandle
	URL     string
	Title   string
}

// downloadStart is an engine download beginning in one page's frame.
type downloadStart struct {
	Frame         frameID
	ID            downloadID
	URL           string
	SuggestedName string
	// Popup, set when the frame is a popup's the engine reported
	// (PopupOpened), is that popup's handle, Openers the pages that opened
	// it, nearest first, and Profile the handle of its engine profile. The
	// Manager lists the download in the popup's tab, or in the nearest
	// listed opener's if the popup is never listed, and cancels it through
	// the profile when neither is (popup_parked.go).
	Popup   ids.PageHandle
	Openers []ids.PageHandle
	Profile profileHandle
	// EndReported, nil on an engine that delivers a download's end as it
	// learns it, reports whether the engine has received the download's
	// end and has yet to deliver it (the hosted engine's report lanes),
	// which it does even past Stop: the Manager leaves such a row to that
	// end when its page leaves (pageLeftLocked).
	EndReported func() bool
}

// downloadAdmission is the Manager's answer to a download's start
// (engineEvents.DownloadStarted). Only downloadAdmitted lets it continue.
// downloadRefused is a known owner's refusal (its page left, its
// workspace is full, a bound), which is final. downloadUnowned is a start
// whose owner is not known yet: a frame no listed page owns and no popup
// names, or a popup's whose opener chain gives it no open jar, workspace
// or listed tab yet (parkDownload). Nobody refused it, and an engine never
// treats it as admitted: headless Chromium, which can learn a popup's
// frame or its opener's listing after the download starts, offers such a
// start again for a bounded time and then cancels it and removes its file
// (headless_downloads.go); every other engine cancels it at once, as a
// refusal.
type downloadAdmission int

const (
	downloadAdmitted downloadAdmission = iota
	downloadRefused
	downloadUnowned
)

// errDownloadEnded is CancelDownload's answer when the engine had already
// let the download go: it finished or failed, and its terminal report is on
// the way. The cancel lost; that report settles the download.
var errDownloadEnded = errors.New("browser: the download already ended")

// downloadProgress is an engine download advancing. Total is the size the
// response announced, zero when unknown. State is what the engine observed
// of the download, its end's cause included; Error is the engine's text
// for it, display only.
type downloadProgress struct {
	ID       downloadID
	Received float64
	Total    float64
	State    downloadState
	FilePath string
	Error    string
}

// downloadState is what an engine observed of a download. The zero value is
// no state; every engine names one. Only the engine that saw a download end
// says why: the Manager reads the cause from here, never from whether it
// had asked for a cancel.
type downloadState int

const (
	// downloadInProgress: it runs.
	downloadInProgress downloadState = iota + 1
	// downloadCancelRefused: it runs on, because the engine refused the
	// cancel the Manager asked for after CancelDownload returned; Error is
	// why, and the cancel is withdrawn.
	downloadCancelRefused
	// downloadCompleted: the file is whole.
	downloadCompleted
	// downloadCanceled: the engine ended it as canceled, by the cancel the
	// Manager asked for.
	downloadCanceled
	// downloadFailed: the engine, the network, the server or the disk ended
	// it, a cancel pending or not; Error is why.
	downloadFailed
	// downloadUntracked: the engine stopped following it, so how it ends is
	// unknown; Error is why.
	downloadUntracked
	// downloadPageGone: the engine ended it because the page it began in
	// went before the Manager listed it; Error is why.
	downloadPageGone
)

func (s downloadState) String() string {
	switch s {
	case downloadInProgress:
		return "inProgress"
	case downloadCancelRefused:
		return "cancelRefused"
	case downloadCompleted:
		return "completed"
	case downloadCanceled:
		return "canceled"
	case downloadFailed:
		return "failed"
	case downloadUntracked:
		return "untracked"
	case downloadPageGone:
		return "pageGone"
	}
	return fmt.Sprintf("downloadState(%d)", int(s))
}

// valid reports one of the states above; the Manager drops a report of
// any other.
func (s downloadState) valid() bool {
	return s >= downloadInProgress && s <= downloadPageGone
}

// running reports a state of a download still running.
func (s downloadState) running() bool {
	return s == downloadInProgress || s == downloadCancelRefused
}

// stopped reports an end that left no whole file.
func (s downloadState) stopped() bool {
	return s.valid() && !s.running() && s != downloadCompleted
}

// engineEvents are the engine-originated notifications the Manager subscribes
// to. The engine reports; the Manager alone routes and decides.
//
// PopupOpened, PageClosed and PageCrashed are the popup admission's
// inputs (engine_pages.go): each records the report and returns at once,
// acting on a goroutine of its own, so an engine calls them in the order
// it observed the facts, from its UI thread or event reader, and holds
// nothing back for a popup's listing. The Manager decides from them
// whether a popup becomes a tab. An engine reports a popup before it
// reports anything else of it, its close, its crash or a download it
// starts (headless Chromium reads its target and download events in the
// browser's order, a WebKit popup is reported in its creation, the
// launcher's reports reach the hosted engine one at a time): the Manager
// keeps no report of a handle it does not know for one that may come.
type engineEvents struct {
	// PopupOpened reports a page the engine opened by itself, which the
	// Manager adopts (AttachPage) or discards (DiscardPage).
	PopupOpened func(enginePopup)
	// PageClosed reports that the page closed by itself or its engine
	// lost it.
	PageClosed func(handle ids.PageHandle)
	// PageCrashed reports that the page's renderer or web content process
	// ended while the page was open; reason is the engine's name for how.
	// The engine page still exists and the Manager closes it (crash.go).
	PageCrashed func(handle ids.PageHandle, reason string)
	// PageEndReceived records, at once, a close (closed) or a crash
	// (crash, its reason) an engine received for a page and reports
	// through PageClosed or PageCrashed only once it has processed what it
	// received before (the hosted engine's report lanes), so a popup's or
	// a created page's listing that commits meanwhile reads it.
	PageEndReceived func(handle ids.PageHandle, closed bool, crash string)
	// PageInfoChanged is a CDP engine's report of a page's address and
	// title, from the browser's target events, by the page's handle; a
	// native engine reports through the page's own hook (pageHooks.Info).
	// order is as for pageHooks.Info. Reports can reach the Manager out of
	// order (headless Chromium delivers a held report while a newer one
	// is delivered), and one can name a handle no page answers to yet (an
	// engine page being created before the Manager attaches it, a popup
	// being adopted): the Manager holds the newest of those for the page
	// that takes the handle, so its order fence sees them.
	PageInfoChanged func(handle ids.PageHandle, url, title string, order uint64)
	// DownloadStarted answers whether the download may continue
	// (downloadAdmission).
	DownloadStarted  func(downloadStart) downloadAdmission
	DownloadProgress func(downloadProgress)
	// KeyChord is the one SYNCHRONOUS question on this seam: a modifier chord,
	// or a bare Escape, was pressed while the page's native view held
	// keyboard focus — does AO own it? True means the engine swallows the
	// event and AO dispatches the chord, keyboard focus already handed to
	// the app (engineAppFocus); false means the page keeps it, and its
	// focus. A bare Escape is always the page's; press is what the engine
	// knows of it besides the chord. It runs on the engine's UI thread, so
	// the answer must be a set lookup and nothing that waits.
	KeyChord func(handle ids.PageHandle, pressed keybindings.Accelerator, press keyPress) bool
	// PageFocusReported reports how a focus the host was asked for
	// (paneHost.FocusPage) ended after FocusPage answered, which only the
	// hosted engine does: the launcher moves focus after the directive.
	// mount and intent name the focus intent; err nil means the page took
	// the keyboard.
	PageFocusReported func(handle ids.PageHandle, mount MountID, intent uint64, err error)
}

// keyPress is what an in-process engine gathers about a key press besides
// its chord, for the Manager's decision on a bare Escape
// (pageEscapeAnnounced): Repeat is the OS's auto-repeat flag (WKWebView),
// Held is the engine's own record that the key went down before without its
// release (WebKitGTK, whose GTK 4 reports no repeat), and Composing is
// whether the page's input method has a composition in progress, which the
// Escape cancels.
type keyPress struct {
	Repeat, Held, Composing bool
}
