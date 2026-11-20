package browser

import (
	"os"

	"agent-overflow/internal/browser/quota"
)

// The browser tools' arguments and results (mcp.go, Controller): what an
// MCP call decodes into and what its answer carries. The companion's own
// DTOs are in types.go.

type Snapshot struct {
	PageInfo
	Text     string            `json:"text"`
	Elements []SnapshotElement `json:"elements"`
}

type SnapshotElement struct {
	NodeID      string `json:"nodeId"`
	Selector    string `json:"selector"`
	Tag         string `json:"tag"`
	Role        string `json:"role,omitempty"`
	Text        string `json:"text,omitempty"`
	Label       string `json:"label,omitempty"`
	Type        string `json:"type,omitempty"`
	Href        string `json:"href,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

type OpenOptions struct {
	PageID PageID
	// tab, when set, is the one incarnation of a tab the open loads
	// (tabHold), in place of PageID: an operation that crossed a wait
	// after it found the page never resolves the tab by id again.
	tab *tabHold
}

type TypeOptions struct {
	PageID   PageID
	Selector string
	Text     string
	Clear    bool
}

type ScreenshotOptions struct {
	PageID   PageID
	FullPage bool
	Clip     *ClipRect
}

type ClipRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Locator is a serializable, stateless equivalent of the supported Playwright
// locator builder. Nested locators are evaluated in the same document/frame.
type Locator struct {
	CSS         string     `json:"css,omitempty"`
	Role        string     `json:"role,omitempty"`
	Name        string     `json:"name,omitempty"`
	Text        string     `json:"text,omitempty"`
	Label       string     `json:"label,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
	TestID      string     `json:"test_id,omitempty"`
	Exact       bool       `json:"exact,omitempty"`
	Regex       bool       `json:"regex,omitempty"`
	RegexFlags  string     `json:"regex_flags,omitempty"`
	Frames      []string   `json:"frames,omitempty"`
	Scope       *Locator   `json:"scope,omitempty"`
	Has         *Locator   `json:"has,omitempty"`
	HasNot      *Locator   `json:"has_not,omitempty"`
	HasText     string     `json:"has_text,omitempty"`
	HasNotText  string     `json:"has_not_text,omitempty"`
	Visible     *bool      `json:"visible,omitempty"`
	And         []*Locator `json:"and,omitempty"`
	Or          []*Locator `json:"or,omitempty"`
	Index       *int       `json:"index,omitempty"`
}

type LocatorOptions struct {
	PageID           PageID      `json:"page_id,omitempty"`
	Locator          Locator     `json:"locator"`
	Action           string      `json:"action"`
	Value            string      `json:"value,omitempty"`
	Values           []string    `json:"values,omitempty"`
	Attribute        string      `json:"attribute,omitempty"`
	Checked          *bool       `json:"checked,omitempty"`
	Button           string      `json:"button,omitempty"`
	Modifiers        []string    `json:"modifiers,omitempty"`
	Force            bool        `json:"force,omitempty"`
	TimeoutMS        int         `json:"timeout_ms,omitempty"`
	WaitState        string      `json:"state,omitempty"`
	ExpectNavigation bool        `json:"expect_navigation,omitempty"`
	ExpectDownload   bool        `json:"expect_download,omitempty"`
	URL              string      `json:"url,omitempty"`
	WaitUntil        string      `json:"wait_until,omitempty"`
	Select           []SelectArg `json:"select,omitempty"`
}

type SelectArg struct {
	Value *string `json:"value,omitempty"`
	Label *string `json:"label,omitempty"`
	Index *int    `json:"index,omitempty"`
}

type LocatorResult struct {
	Page      PageInfo       `json:"page"`
	Count     int            `json:"count"`
	Value     any            `json:"value,omitempty"`
	Values    []string       `json:"values,omitempty"`
	Matches   []LocatorMatch `json:"matches,omitempty"`
	Download  *DownloadInfo  `json:"download,omitempty"`
	Navigated bool           `json:"navigated,omitempty"`
}

type LocatorMatch struct {
	NodeID     string `json:"nodeId"`
	Selector   string `json:"selector"`
	Tag        string `json:"tag"`
	Role       string `json:"role,omitempty"`
	Name       string `json:"name,omitempty"`
	Text       string `json:"text,omitempty"`
	InnerText  string `json:"innerText,omitempty"`
	Visible    bool   `json:"visible"`
	Enabled    bool   `json:"enabled"`
	Checked    *bool  `json:"checked,omitempty"`
	Value      string `json:"value,omitempty"`
	FrameDepth int    `json:"frameDepth,omitempty"`
}

type PointerOptions struct {
	PageID    PageID   `json:"page_id,omitempty"`
	Action    string   `json:"action"`
	X         float64  `json:"x,omitempty"`
	Y         float64  `json:"y,omitempty"`
	Button    string   `json:"button,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`
	ScrollX   float64  `json:"scroll_x,omitempty"`
	ScrollY   float64  `json:"scroll_y,omitempty"`
	Path      []Point  `json:"path,omitempty"`
}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type DOMActionOptions struct {
	PageID PageID   `json:"page_id,omitempty"`
	Action string   `json:"action"`
	NodeID string   `json:"node_id,omitempty"`
	Text   string   `json:"text,omitempty"`
	Key    string   `json:"key,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	X      float64  `json:"x,omitempty"`
	Y      float64  `json:"y,omitempty"`
}

type WaitOptions struct {
	PageID       PageID   `json:"page_id,omitempty"`
	TimeoutMS    int      `json:"timeout_ms,omitempty"`
	Milliseconds int      `json:"milliseconds,omitempty"`
	Selector     string   `json:"selector,omitempty"`
	Locator      *Locator `json:"locator,omitempty"`
	State        string   `json:"state,omitempty"`
	URL          string   `json:"url,omitempty"`
	LoadState    string   `json:"load_state,omitempty"`
}

type ClipboardEntry struct {
	MIMEType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Base64   string `json:"base64,omitempty"`
}

type ClipboardItem struct {
	Entries           []ClipboardEntry `json:"entries"`
	PresentationStyle string           `json:"presentationStyle,omitempty"`
}

type ClipboardOptions struct {
	PageID PageID          `json:"page_id,omitempty"`
	Action string          `json:"action"`
	Text   string          `json:"text,omitempty"`
	Items  []ClipboardItem `json:"items,omitempty"`
}

type ConsoleLog struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
	URL       string `json:"url,omitempty"`
}

type ConsoleOptions struct {
	PageID PageID   `json:"page_id,omitempty"`
	Filter string   `json:"filter,omitempty"`
	Levels []string `json:"levels,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// DownloadInfo is one download in a page's log, as browser_downloads lists
// it. State is in_progress, completed, failed (the engine stopped it on its
// own, or the finished file could not be kept) or canceled (AO stopped it:
// the user, a quota, the page closing). A completed one with an Error saved
// a file that is not openable from AO, and the Error says why.
type DownloadInfo struct {
	Sequence      uint64 `json:"sequence"`
	ID            string `json:"id"`
	URL           string `json:"url"`
	SuggestedName string `json:"suggestedName"`
	Path          string `json:"path,omitempty"`
	State         string `json:"state"`
	Bytes         int64  `json:"bytes,omitempty"`
	TotalBytes    int64  `json:"totalBytes,omitempty"`
	Error         string `json:"error,omitempty"`
	StartedAt     string `json:"startedAt"`
	// reservation is the quota a download AO keeps holds until it settles
	// (quota.Reservation), nil for one that holds none.
	reservation *quota.Reservation
	// source is the file AO had the engine write the download into, named
	// at admission, which its discard removes (downloads_discard.go).
	source downloadFile
	// key is the download's entry in its thread's list (downloads_thread.go).
	key DownloadKey
	// phase is where the download is in its life (downloads_phase.go),
	// written only by advance and settle.
	phase downloadRowPhase
	// file is the finished file as it was when the download settled, so a
	// later Open or Show in folder acts on that file and no other.
	file os.FileInfo
	// settled numbers the entry among its page's settled ones in the order
	// they settled, from 1; zero while it runs (trimDownloadsLocked).
	settled uint64
}

type DownloadOptions struct {
	PageID    PageID `json:"page_id,omitempty"`
	Action    string `json:"action"`
	After     uint64 `json:"after,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type AssetSource struct {
	Kind     string `json:"kind"`
	NodeID   string `json:"nodeId,omitempty"`
	Property string `json:"property,omitempty"`
	selector string
}

type AssetInfo struct {
	ID      string        `json:"id"`
	Kind    string        `json:"kind"`
	Name    string        `json:"name"`
	URL     string        `json:"url"`
	Sources []AssetSource `json:"sources"`
}

type InlineSVG struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Markup string `json:"markup"`
}

type AssetInventory struct {
	ID         string       `json:"id"`
	PageURL    string       `json:"pageUrl"`
	Assets     []AssetInfo  `json:"assets"`
	InlineSVGs []InlineSVG  `json:"inlineSvgs"`
	Summary    AssetSummary `json:"summary"`
}

type AssetSummary struct {
	ByKind         map[string]int `json:"byKind"`
	TotalCount     int            `json:"totalCount"`
	InlineSVGCount int            `json:"inlineSvgCount"`
}

type AssetOptions struct {
	PageID      PageID   `json:"page_id,omitempty"`
	Action      string   `json:"action"`
	InventoryID string   `json:"inventory_id,omitempty"`
	AssetIDs    []string `json:"asset_ids,omitempty"`
	Kinds       []string `json:"kinds,omitempty"`
}

type AssetBundle struct {
	DirectoryPath string         `json:"directoryPath"`
	ManifestPath  string         `json:"manifestPath"`
	Assets        []BundledAsset `json:"assets"`
	Failures      []AssetFailure `json:"failures"`
	Summary       map[string]any `json:"summary"`
}

type BundledAsset struct {
	AssetInfo
	Path        string `json:"path"`
	ContentType string `json:"contentType,omitempty"`
}

type AssetFailure struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Reason      string `json:"reason"`
	ContentType string `json:"contentType,omitempty"`
}

type ViewportOptions struct {
	Action string `json:"action"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}
