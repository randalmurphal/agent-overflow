package browser

import (
	"context"
	"time"
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
}

// CompanionSubscription is what mounting a pane answers: the mount id the
// frontend detaches and reports rects with, plus the thread's current state so
// the pane renders without a second round trip.
type CompanionSubscription struct {
	ID    string         `json:"id"`
	State CompanionEvent `json:"state"`
}

type Access struct {
	ThreadID    string
	Workspace   string
	ProjectRoot string
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
}

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
	PageID string
}

type TypeOptions struct {
	PageID   string
	Selector string
	Text     string
	Clear    bool
}

type ScreenshotOptions struct {
	PageID   string
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
	PageID           string      `json:"page_id,omitempty"`
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
	PageID    string   `json:"page_id,omitempty"`
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
	PageID string   `json:"page_id,omitempty"`
	Action string   `json:"action"`
	NodeID string   `json:"node_id,omitempty"`
	Text   string   `json:"text,omitempty"`
	Key    string   `json:"key,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	X      float64  `json:"x,omitempty"`
	Y      float64  `json:"y,omitempty"`
}

type WaitOptions struct {
	PageID       string   `json:"page_id,omitempty"`
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
	PageID string          `json:"page_id,omitempty"`
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
	PageID string   `json:"page_id,omitempty"`
	Filter string   `json:"filter,omitempty"`
	Levels []string `json:"levels,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

type DownloadInfo struct {
	Sequence      uint64 `json:"sequence"`
	ID            string `json:"id"`
	URL           string `json:"url"`
	SuggestedName string `json:"suggestedName"`
	Path          string `json:"path,omitempty"`
	State         string `json:"state"`
	Bytes         int64  `json:"bytes,omitempty"`
	Error         string `json:"error,omitempty"`
	StartedAt     string `json:"startedAt"`
	reservedBytes int64
}

type DownloadOptions struct {
	PageID    string `json:"page_id,omitempty"`
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
	PageID      string   `json:"page_id,omitempty"`
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

type SessionInfo struct {
	Name         string    `json:"name,omitempty"`
	ActivePageID string    `json:"activePageId,omitempty"`
	Visible      bool      `json:"visible"`
	ViewportW    int       `json:"viewportWidth"`
	ViewportH    int       `json:"viewportHeight"`
	ViewportSet  bool      `json:"viewportSet"`
	UpdatedAt    time.Time `json:"-"`
}

type Controller interface {
	Open(context.Context, Access, string, OpenOptions) (PageInfo, error)
	NewPage(context.Context, Access) (PageInfo, error)
	OpenFile(context.Context, Access, string, OpenOptions) (PageInfo, error)
	Pages(context.Context, Access) ([]PageInfo, error)
	SelectPage(context.Context, Access, string) (PageInfo, error)
	LabelPage(context.Context, Access, string, string) (PageInfo, error)
	NameSession(context.Context, Access, string) (SessionInfo, error)
	Visibility(context.Context, Access, *bool, string) (SessionInfo, error)
	Viewport(context.Context, Access, ViewportOptions) (SessionInfo, error)
	ClosePage(context.Context, Access, string) error
	Snapshot(context.Context, Access, string) (Snapshot, error)
	Screenshot(context.Context, Access, ScreenshotOptions) ([]byte, error)
	Locator(context.Context, Access, LocatorOptions) (LocatorResult, error)
	Pointer(context.Context, Access, PointerOptions) (PageInfo, error)
	DOMAction(context.Context, Access, DOMActionOptions) (any, error)
	Clipboard(context.Context, Access, ClipboardOptions) (any, error)
	ConsoleLogs(context.Context, Access, ConsoleOptions) ([]ConsoleLog, error)
	Downloads(context.Context, Access, DownloadOptions) (any, error)
	Assets(context.Context, Access, AssetOptions) (any, error)
	WaitAdvanced(context.Context, Access, WaitOptions) (PageInfo, error)
	Click(context.Context, Access, string, string) (PageInfo, error)
	Type(context.Context, Access, TypeOptions) (PageInfo, error)
	Press(context.Context, Access, string, string) (PageInfo, error)
	Scroll(context.Context, Access, string, string, float64, float64) (PageInfo, error)
	Wait(context.Context, Access, string, string, int) (PageInfo, error)
	History(context.Context, Access, string, string) (PageInfo, error)
	Evaluate(context.Context, Access, string, string) (any, error)
	EvaluateReadOnly(context.Context, Access, string, string) (any, string, error)
	CloseThread(context.Context, string) error
	Close() error
	ClearSiteData(context.Context) error
	Reconfigure(Config) error
}
