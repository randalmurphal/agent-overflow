package browser

import (
	"context"
	"encoding/json"
	"io"
	"regexp"

	"github.com/chromedp/cdproto/network"
)

// The engine seam. `Manager` owns policy — Access checks, the page registry and
// its per-thread ownership, labels, session/visibility state, every cap and
// bound, artifact quotas, the AO-managed per-tab clipboard, and the MCP server.
// An engine owns only how an operation is carried out against a live page.
//
// Three interfaces, one per lifetime: `browserEngine` is the process/profile
// factory, `engineProfile` is one workspace's isolated site data, and
// `pageDriver` is one page's tool-operation surface. Adding an engine
// (spec: docs/specs/embedded-browser.md §6) means implementing these and
// nothing else; no policy may migrate across the seam in either direction.

// browserEngine is the process half: whatever must exist before a page can.
type browserEngine interface {
	// Start makes the engine live. It is idempotent, and the caller has
	// already decided the feature is enabled.
	Start(ctx context.Context) error
	// Running reports whether pages can be created right now.
	Running() bool
	// Interrupt cancels an in-flight Start so a concurrent shutdown can
	// proceed. It does not tear down a started engine.
	Interrupt()
	// Stop tears the engine down. Profiles are disposed by their owner first.
	Stop()
	// NewProfile creates one isolated site-data profile.
	NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error)
	// DiscardPage closes an engine page the Manager declined to adopt.
	DiscardPage(handle string)
}

// engineProfile is one canonical workspace's isolated site data: the unit that
// owns its pages, download behavior, and cookies.
type engineProfile interface {
	// Handle identifies this profile in engine events.
	Handle() string
	// NewPage creates a hidden page in this profile.
	NewPage(ctx context.Context, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error)
	// AttachPage adopts a page the engine created on its own (a popup).
	AttachPage(ctx context.Context, handle string, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error)
	// Cookies reads this profile's cookies for the encrypted checkpoint.
	// The CDP-shaped record is the persisted format, which spec §4 deletes
	// along with the whole checkpoint system.
	Cookies(ctx context.Context) ([]*network.CookieParam, error)
	// CancelDownload aborts one in-flight download by its engine id.
	CancelDownload(id string)
	// Dispose destroys the profile and every page still in it.
	Dispose(ctx context.Context) error
}

// profileOptions is the AO-owned configuration a profile is created against.
type profileOptions struct {
	// Workspace is the canonical workspace root the profile isolates.
	Workspace string
	// DownloadDir is the AO artifact directory downloads must land in.
	DownloadDir string
	// Cookies seeds the profile from the persisted checkpoint.
	Cookies []*network.CookieParam
	// Persist is the user's site-data setting. An engine whose site data
	// lives on disk (spec §4) keeps it only when this is true; managed
	// Chrome is always incognito and ignores it, restoring from Cookies
	// instead. The hosted engine forwards it as the directive's Ephemeral
	// flag.
	Persist bool
}

// pageHooks are the AO-owned callbacks a page driver reports into. They carry
// no policy: the driver reports a fact, the Manager decides what it means.
type pageHooks struct {
	// Console appends one captured console or log entry.
	Console func(ConsoleLog)
	// PageURL reports the page's last known URL, attributed to entries the
	// engine reports without one of their own.
	PageURL func() string
	// Allow answers the navigation-authority question for one request URL.
	Allow func(url string) bool
	// Screencast receives one base64 JPEG frame for the streamed companion
	// pane. CDP-only, and deleted with that pane (spec §9); an engine
	// without a screencast never calls it.
	Screencast func(frame string)
}

// pageDriver is one engine's implementation of every per-page operation the
// browser tools perform. Bounds, validation, and ownership are applied by the
// Manager before and after each call.
type pageDriver interface {
	// Lifetime is the context the page dies with. Operations are bounded
	// against it.
	Lifetime() context.Context
	// Handle identifies this page in engine events.
	Handle() string
	// OwnsFrame reports whether an engine frame handle belongs to this page.
	// Engines that address events per page can answer false.
	OwnsFrame(frame string) bool
	// Close destroys the page.
	Close()

	// Info reads the page's current location and title, untruncated.
	Info(ctx context.Context) (url, title string, err error)
	// Navigate loads one already-authorized URL.
	Navigate(ctx context.Context, url string) error
	// History runs one of back, forward, reload, or stop.
	History(ctx context.Context, action string) error
	// PageStatus samples the load state a wait condition is evaluated against.
	PageStatus(ctx context.Context) (pageStatus, error)
	// NavigationMark captures the page's current navigation identity, so a
	// caller can tell that a navigation happened at all.
	NavigationMark(ctx context.Context) (navigationMark, error)

	// Snapshot returns the accessibility-ish page snapshot. Element node ids
	// are assigned by the Manager, not the driver.
	Snapshot(ctx context.Context) (Snapshot, error)
	// Screenshot captures the viewport, a clip, or the full page.
	Screenshot(ctx context.Context, opts ScreenshotOptions) ([]byte, error)
	// Evaluate runs an expression and awaits a promise result.
	Evaluate(ctx context.Context, expression string) (any, error)
	// EvaluateReadOnly runs an expression the engine rejects on side effects,
	// returning the undecoded result so the Manager can bound it.
	EvaluateReadOnly(ctx context.Context, expression string) (json.RawMessage, error)
	// ReadOnlyCaveat is what this engine can actually promise about
	// EvaluateReadOnly. Empty means the engine rejects side effects itself;
	// a non-empty string is carried into the tool result verbatim, so an
	// engine that can only be best-effort is never SILENTLY different. The
	// Manager attaches the answer without learning which engine gave it.
	ReadOnlyCaveat() string
	// LocalStorage reads the page origin's localStorage for the checkpoint.
	LocalStorage(ctx context.Context) (origin string, data map[string]string, err error)

	// ResolveLocator evaluates a locator and returns its matches.
	ResolveLocator(ctx context.Context, locator Locator, attribute string) ([]LocatorMatch, error)
	// ReadNode reads one property of one already-matched element. kind is
	// attribute, innerText, textContent, enabled, or visible.
	ReadNode(ctx context.Context, match LocatorMatch, locator Locator, kind, argument string) (any, error)
	// ActOnNode performs one policy-checked mutation on a matched element.
	ActOnNode(ctx context.Context, match LocatorMatch, locator Locator, act nodeAction) error
	// ScrollNode scrolls one remembered element by a delta.
	ScrollNode(ctx context.Context, ref nodeReference, x, y float64) error

	// Click scrolls a selector into view and clicks it.
	Click(ctx context.Context, selector string) error
	// Type focuses a selector, optionally clears it, and types text.
	Type(ctx context.Context, selector, text string, clear bool) error
	// Press sends one key chord, spelled the way the tool takes it.
	Press(ctx context.Context, key string) error
	// TypeText types text at the current focus, with no selector.
	TypeText(ctx context.Context, text string) error
	// SelectionText reads the page's current selection, for the copy chord.
	SelectionText(ctx context.Context) string
	// Pointer dispatches a raw pointer gesture at viewport coordinates.
	Pointer(ctx context.Context, opts PointerOptions) error
	// Scroll scrolls a selector, or the window when it is empty.
	Scroll(ctx context.Context, selector string, x, y float64) error
	// WaitVisible blocks until a selector is visible or the context ends.
	WaitVisible(ctx context.Context, selector string) error

	// SetViewport pins the page to a device-metrics override.
	SetViewport(ctx context.Context, width, height int) error
	// ClearViewport drops the override.
	ClearViewport(ctx context.Context) error

	// AssetInventory reports the page's referenced assets, unshaped.
	AssetInventory(ctx context.Context) (pageAssets, error)
	// AssetFetcher prepares a bundle-scoped fetcher bound to the page's
	// current document, so a bundle costs one frame lookup rather than one
	// per asset.
	AssetFetcher(ctx context.Context) (assetFetcher, error)
}

// assetFetcher opens one asset through the page's own credentials.
type assetFetcher func(url string) (assetStream, error)

// assetStream is one opened asset: what the server said it is, and a bounded
// copy of its bytes. Opening is separate from copying because the content type
// is reported in the failure record for a file the Manager could not even
// create.
type assetStream struct {
	ContentType string
	// Copy writes the asset out, refusing to exceed either limit.
	Copy func(out io.Writer, perFile, remaining int64) (int64, error)
	// Close releases the engine-side stream.
	Close func()
}

// pageStatus is one sample of the conditions a wait can be satisfied by.
type pageStatus struct {
	URL string
	// Ready is the document readiness: loading, interactive, or complete.
	Ready string
	// NetworkIdle reports the engine's own quiet-network judgement.
	NetworkIdle bool
}

// navigationMark identifies a page's current navigation. Two marks that differ
// mean a navigation happened, which is all a caller needs to know.
type navigationMark struct {
	URL string
	// Loader is the engine's per-navigation token, empty when it has none.
	Loader string
}

// nodeAction is the policy-checked mutation a locator action performs on one
// resolved element. The Manager has already applied strictness, actionability,
// and argument rules; the driver only carries the action out.
type nodeAction struct {
	// Kind is click, type, press, fill, or select_option.
	Kind string
	// Value is the text for type, fill, and press.
	Value string
	// Clicks is 1 or 2 for a click.
	Clicks int
	// Button and Modifiers apply to a click.
	Button    string
	Modifiers []string
	// Selections are the resolved select_option descriptors.
	Selections []SelectArg
}

// pageAssets is the raw asset inventory a page reports, before the Manager
// deduplicates, bounds, and assigns opaque ids to it.
type pageAssets struct {
	PageURL    string      `json:"pageUrl"`
	Assets     []AssetInfo `json:"assets"`
	InlineSVGs []InlineSVG `json:"inlineSvgs"`
}

// enginePopup is a page the engine opened on its own behalf. Nothing about
// ownership is decided here: the Manager matches the opener and applies its
// own per-thread and per-workspace limits before adopting it.
type enginePopup struct {
	Profile string
	Opener  string
	Handle  string
	URL     string
	Title   string
}

// downloadStart is an engine download beginning in one page's frame.
type downloadStart struct {
	Frame         string
	ID            string
	URL           string
	SuggestedName string
}

// downloadProgress is an engine download advancing.
type downloadProgress struct {
	ID       string
	Received float64
	State    string
	FilePath string
}

// The states downloadProgress.State may carry.
const (
	downloadInProgress = "inProgress"
	downloadCompleted  = "completed"
	downloadCanceled   = "canceled"
)

// engineEvents are the engine-originated notifications the Manager subscribes
// to. The engine reports; the Manager alone routes and decides.
type engineEvents struct {
	PopupOpened      func(enginePopup)
	PageClosed       func(handle string)
	PageInfoChanged  func(handle, url, title string)
	DownloadStarted  func(downloadStart)
	DownloadProgress func(downloadProgress)
}

// matchStatus reports whether a sampled page state satisfies a wait condition.
// Shared by every engine: only the sampling differs.
func matchStatus(status pageStatus, matcher *regexp.Regexp, state string) bool {
	if matcher != nil && !matcher.MatchString(status.URL) {
		return false
	}
	switch state {
	case "commit":
		return true
	case "domcontentloaded":
		return status.Ready != "loading"
	case "load":
		return status.Ready == "complete"
	case "networkidle":
		return status.Ready == "complete" && status.NetworkIdle
	}
	return false
}
