package browser

import (
	"context"
	"encoding/json"
	"time"

	"agent-overflow/internal/keybindings"
)

const ServerName = "ao-browser-tools"

type Config struct {
	Enabled               bool
	PersistSiteData       bool
	AllowOutsideWorkspace bool
}

// CompanionEvent is live browser state for the calling thread's companion
// pane, emitted on page lifecycle and navigation changes. It carries no
// pixels: the pane's page content is a real view the engine presents behind
// the pane's host rect (spec §7).
type CompanionEvent struct {
	Kind         string     `json:"kind"`
	ThreadID     string     `json:"threadId"`
	Pages        []PageInfo `json:"pages,omitempty"`
	ActivePageID string     `json:"activePageId,omitempty"`
	PageID       string     `json:"pageId,omitempty"`
	Error        string     `json:"error,omitempty"`
	Visible      *bool      `json:"visible,omitempty"`
	SessionName  string     `json:"sessionName,omitempty"`
	// ViewportWidth/Height ride a Kind "state" event: the size every page of
	// the thread lays out at (the pane size, a pinned size, or the default),
	// which the pane shows scaled down to fit when it is larger than the pane.
	ViewportWidth  int `json:"viewportWidth,omitempty"`
	ViewportHeight int `json:"viewportHeight,omitempty"`
	// ViewportSet is whether browser_viewport pinned that size; otherwise it
	// follows the pane and the pane shows the page at 1:1.
	ViewportSet bool `json:"viewportSet,omitempty"`
	// Jar rides a Kind "state" event: the cookie jar the thread's pages
	// open in (SessionInfo.Jar).
	Jar string `json:"jar,omitempty"`
	// RecentGen rides a Kind "state" event: the Manager's forget generation
	// of recent addresses (recent.go), an opaque token. A client holding a
	// thread's list drops it when this changes.
	RecentGen string `json:"recentGen,omitempty"`
	// Accelerator rides a Kind "accelerator" event: a bound chord pressed
	// while the thread's page view held keyboard focus, for the frontend to
	// dispatch as if the SPA had received it. Its PageID is the page the
	// chord was pressed in, which a command it runs on a tab acts on.
	Accelerator *keybindings.Accelerator `json:"accelerator,omitempty"`
	// Mount rides a Kind "accelerator" or "escape" event: the pane mount
	// (AttachPane) the thread's pages are presented in. The event reaches
	// every host client, and only the one holding that mount acts on it.
	// It also rides a Kind "focus" event, which names the mount whose
	// FocusIntent ended: the page took the keyboard, or Error says why not.
	Mount       string `json:"mount,omitempty"`
	FocusIntent uint64 `json:"focusIntent,omitempty"`
	// FindMode rides a Kind "state" event: how the active page's find is
	// presented (FindModeBar or FindModeEngine), empty when it has none.
	FindMode string `json:"findMode,omitempty"`
	// Find rides a Kind "find" event: a page's search ended without its
	// owner asking (navigation, a tab switch, a hidden pane, the page
	// closing, another owner's start), so a find bar showing it closes.
	// A Kind "escape" event names only ThreadID and PageID: a bare Escape
	// was pressed in that page while a pane showed it, and the page kept it.
	// A Kind "teardown" event names ThreadID, PageID and Error: a page that
	// already left the tab set failed to close or to release its profile.
	Find *FindState `json:"find,omitempty"`
	// ClosedTabs rides a Kind "state" event: how many tabs the thread can
	// reopen (companion_tabs.go).
	ClosedTabs int `json:"closedTabs,omitempty"`
	// Downloads rides a Kind "state" event: the thread's downloads, newest
	// first (downloads_thread.go). Download rides a Kind "download" event:
	// one download settled other than by the user's cancel, announced once.
	Downloads []ThreadDownload `json:"downloads,omitempty"`
	Download  *ThreadDownload  `json:"download,omitempty"`
	// Seq rides a Kind "state" event: this process's number for the
	// snapshot, taken under the manager's lock as the snapshot is read.
	// Snapshots are published after that lock is released, and calls answer
	// with one, so a client keeps the highest Seq it has seen for a thread.
	Seq uint64 `json:"seq,omitempty"`
}

// The find modes a page reports (pageFind.FindMode).
const (
	// FindModeBar: AO's find bar drives the engine's find and shows its answer.
	FindModeBar = "bar"
	// FindModeEngine: the engine shows its own find bar, opened by a start.
	FindModeEngine = "engine"
)

// FindState is a page's search as the find bar shows it, with the search's
// identity (find.go): the page and its incarnation, the pane mount and
// sequence that own it, the document it started on, and the generation of
// the action that answered or of the end.
type FindState struct {
	PageID string `json:"pageId"`
	// Incarnation tells apart pages that share PageID: a restored page
	// reuses its id.
	Incarnation uint64 `json:"incarnation"`
	// Mount is the pane mount that owns the search, "" for none.
	Mount string `json:"mount"`
	// Seq is the sequence of the start that made the search, 0 for none.
	Seq uint64 `json:"seq"`
	// Document is the epoch of the document the search started on.
	Document uint64 `json:"document"`
	// Session is the generation: it increases with every find action and
	// end across the Manager, so an answer older than an end is
	// recognizably stale.
	Session uint64 `json:"session"`
	// Foreign marks an answer for an action that changed nothing because
	// the search it named is gone or another owner's; Mount and Seq then
	// name the search's real owner, and the answer acknowledges nothing of
	// the asker's.
	Foreign bool   `json:"foreign"`
	Active  bool   `json:"active"`
	Query   string `json:"query"`
	Found   bool   `json:"found"`
	// Matches is the engine's count, -1 when the engine reports only whether
	// there is a match.
	Matches int `json:"matches"`
	// Current is the 0-based current match, -1 when there is none or the
	// engine cannot place it.
	Current int  `json:"current"`
	Capped  bool `json:"capped"`
}

// CompanionSubscription is what mounting a pane answers: the mount id the
// frontend detaches and reports rects with, plus the thread's current state so
// the pane renders without a second round trip.
type CompanionSubscription struct {
	ID    string         `json:"id"`
	State CompanionEvent `json:"state"`
}

// Caller is the thread an action is for and the paths its authority comes
// from. A read or a registration takes it as it is; an action that can add a
// tab takes it bound to the thread's lifetime (Access, thread_life.go).
type Caller struct {
	ThreadID    ThreadID
	Workspace   string
	ProjectRoot string
	// ProjectID is the store's id of the thread's project, which a
	// "project" cookie jar is keyed by (jar.go).
	ProjectID string
}

type PageInfo struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Selected   bool   `json:"selected,omitempty"`
	LastOpened string `json:"lastOpened,omitempty"`
	// CanGoBack / CanGoForward mirror the page's session history so the
	// companion toolbar can disable a button instead of offering a click
	// that can only error.
	CanGoBack    bool `json:"canGoBack"`
	CanGoForward bool `json:"canGoForward"`
	// Suspended marks a page with no engine page behind it (suspend.go): its
	// thread's idle session was reaped or the app restarted. The next tool
	// call on it, or presenting it, reloads its URL.
	Suspended bool `json:"suspended,omitempty"`
	// The fields below ride only the companion state (threadState); a tool
	// result never carries them.
	//
	// Creating marks a tab whose engine page is still being created, and
	// Restoring a suspended tab whose reload is in flight. Error is why a
	// tab the user opened could not be created, or, on a Suspended tab, why
	// its page stopped (crash.go); either tab stays until it is closed. None
	// of these has a page to present.
	Creating  bool   `json:"creating,omitempty"`
	Restoring bool   `json:"restoring,omitempty"`
	Error     string `json:"error,omitempty"`
	// Agent is whether an agent's tool call is acting on the page
	// (activity.go).
	Agent bool `json:"agent,omitempty"`
	// Loading is whether the page's main frame is loading, as its engine
	// reports it (pagestate.go).
	Loading bool `json:"loading,omitempty"`
	// Favicon is the page's icon as a bounded image data URL, read in the
	// page on an engine with a pane; empty when the page has none, the
	// engine cannot read one, or it was not an allowed image.
	Favicon string `json:"favicon,omitempty"`
}

type SessionInfo struct {
	Name         string `json:"name,omitempty"`
	ActivePageID string `json:"activePageId,omitempty"`
	Visible      bool   `json:"visible"`
	ViewportW    int    `json:"viewportWidth"`
	ViewportH    int    `json:"viewportHeight"`
	ViewportSet  bool   `json:"viewportSet"`
	// PaneW/PaneH is the last mounted pane host size, whole CSS pixels inside
	// the viewport bounds; the viewport pages follow while none is pinned.
	// Zero until a pane reports a rect.
	PaneW int `json:"paneWidth,omitempty"`
	PaneH int `json:"paneHeight,omitempty"`
	// Jar is the cookie jar the thread's pages open in (jar.go): worktree,
	// project, thread or private.
	Jar       string    `json:"jar"`
	UpdatedAt time.Time `json:"-"`
	// jarGen advances with every change of Jar; a page opened under an
	// older generation is not admitted (jarCurrentLocked).
	jarGen uint64
	// jarTeardown holds the teardowns pending when Jar last changed (SetJar);
	// no restore of the thread is admitted until all are done
	// (jarTeardownLocked).
	jarTeardown []<-chan struct{}
	// privateJar is the token of the thread's live private jar, "" when it
	// has none.
	privateJar string
}

// activePage is the session's active tab; the DTO carries its id as a
// string.
func (s SessionInfo) activePage() PageID { return PageID(s.ActivePageID) }

type Controller interface {
	// Bind binds one tool call to its thread's browser lifetime
	// (Manager.Bind).
	Bind(Caller) Access
	Open(context.Context, Access, string, OpenOptions) (PageInfo, error)
	NewPage(context.Context, Access) (PageInfo, error)
	OpenFile(context.Context, Access, string, OpenOptions) (PageInfo, error)
	Pages(context.Context, Access) ([]PageInfo, error)
	SelectPage(context.Context, Access, PageID) (PageInfo, error)
	LabelPage(context.Context, Access, PageID, string) (PageInfo, error)
	NameSession(context.Context, Access, string) (SessionInfo, error)
	Visibility(context.Context, Access, *bool, PageID) (SessionInfo, error)
	Viewport(context.Context, Access, ViewportOptions) (SessionInfo, error)
	ClosePage(context.Context, Access, PageID) error
	Snapshot(context.Context, Access, PageID) (Snapshot, error)
	Screenshot(context.Context, Access, ScreenshotOptions) ([]byte, error)
	Locator(context.Context, Access, LocatorOptions) (LocatorResult, error)
	Pointer(context.Context, Access, PointerOptions) (PageInfo, error)
	DOMAction(context.Context, Access, DOMActionOptions) (any, error)
	Clipboard(context.Context, Access, ClipboardOptions) (any, error)
	ConsoleLogs(context.Context, Access, ConsoleOptions) ([]ConsoleLog, error)
	Downloads(context.Context, Access, DownloadOptions) (any, error)
	Assets(context.Context, Access, AssetOptions) (any, error)
	WaitAdvanced(context.Context, Access, WaitOptions) (PageInfo, error)
	Click(context.Context, Access, PageID, string) (PageInfo, error)
	Type(context.Context, Access, TypeOptions) (PageInfo, error)
	Press(context.Context, Access, PageID, string) (PageInfo, error)
	Scroll(context.Context, Access, PageID, string, float64, float64) (PageInfo, error)
	Wait(context.Context, Access, PageID, string, int) (PageInfo, error)
	History(context.Context, Access, PageID, string) (PageInfo, error)
	Evaluate(context.Context, Access, PageID, string, json.RawMessage) (any, string, error)
	EvaluateReadOnly(context.Context, Access, PageID, string, json.RawMessage) (any, string, error)
}
