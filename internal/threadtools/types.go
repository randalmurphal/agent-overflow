package threadtools

// Computer names one computer. Ids address, names are read by people, and
// every result row that names a thread carries both in the paired shape.
type Computer struct {
	ID   string `json:"computer_id"`
	Name string `json:"computer"`
}

// Shape is what the tool list and the instructions are computed from at
// each tools/list and initialize.
//
// Computers lists the PAIRED computers, never this one: an empty list is
// the single-computer shape, where computers and computer_id are absent
// from every schema, no result row carries a computer field and the "Other
// computers" paragraph is absent from the instructions.
//
// Defaults carries the calling thread's live provider, model, effort, mode
// and runtime mode. The thread_spawn descriptions state them so the common
// case needs no discovery call, which is why they are part of the shape
// rather than read at call time.
type Shape struct {
	Computers []Computer
	Defaults  SpawnDefaults
}

// Paired reports whether this shape has other computers in it.
func (s Shape) Paired() bool { return len(s.Computers) > 0 }

// SpawnDefaults is what a spawn inherits when the agent overrides nothing.
type SpawnDefaults struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Mode        string `json:"mode"`
	RuntimeMode string `json:"runtime_mode"`
}

// Caller identifies the thread a call came from, local or through a peer.
// Everything a result or a footer says about the sender comes from here.
type Caller struct {
	ThreadID     string
	ComputerID   string
	ComputerName string
	Title        string
}

// Thread is the App's row for one thread, including the derived columns
// the state derivation reads. It is this package's own DTO, not a mirror
// of the store row: group and project carry names because a result the
// model reads needs them beside their ids.
type Thread struct {
	ID            string `json:"thread_id"`
	Title         string `json:"title"`
	ProjectID     string `json:"project_id,omitempty"`
	Project       string `json:"project,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Mode          string `json:"mode,omitempty"`
	RuntimeMode   string `json:"runtime_mode,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Branch        string `json:"branch,omitempty"`
	GroupID       string `json:"group_id,omitempty"`
	Group         string `json:"group,omitempty"`
	// Pin is PinFront, PinBack or empty.
	Pin      string `json:"pin,omitempty"`
	Archived bool   `json:"archived"`
	Unread   bool   `json:"unread"`
	// LastActivity is the newest completed turn in Unix milliseconds, or
	// the thread's update time when it has never completed a turn.
	LastActivity int64 `json:"last_activity,omitempty"`

	// The four derived columns State reads, named as the frontend's
	// reference implementation names them.
	HasIncompleteTurn         bool   `json:"-"`
	HasFailedTurn             bool   `json:"-"`
	HasActionableProposedPlan bool   `json:"-"`
	WorktreeSetupState        string `json:"-"`
}

// Pin tiers. The app's front and back burner are the two pin tiers; the
// sidebar carries no third.
const (
	PinFront = "front"
	PinBack  = "back"
	PinNone  = "none"
)

// LiveState is the part of the app's live projection the state derivation
// needs. Everything here dies with the provider session; the durable
// fallbacks live on Thread.
type LiveState struct {
	// ActiveTurn is true between turn start and turn end.
	ActiveTurn bool
	// PendingSends is true while a queued or flushed message has not yet
	// produced its provider-visible echo. It keeps a thread reported as
	// running across the gap between send and turn start.
	PendingSends bool
	// PendingApprovals and PendingUserInputs count the interactive
	// requests the provider is blocked on. Either one means the target is
	// blocked on a person, which is what ends a wait early.
	PendingApprovals  int
	PendingUserInputs int
}

// Blocked reports whether the provider is waiting on a person.
func (l LiveState) Blocked() bool { return l.PendingApprovals > 0 || l.PendingUserInputs > 0 }

// Thread states, the UI's enum. setup-failed is not in the spec's listing
// of the thread_search state column but is what the reference derivation
// returns for a failed worktree setup, and dropping it would report such a
// thread as idle.
const (
	StateIdle            = "idle"
	StateRunning         = "running"
	StateAwaitingInput   = "awaiting-input"
	StatePendingApproval = "pending-approval"
	StatePlanReady       = "plan-ready"
	StateError           = "error"
	StateSetupFailed     = "setup-failed"
	StateInterrupted     = "interrupted"
)

// Resolution is one computer's answer to a thread reference.
type Resolution struct {
	// Matches are the threads on this computer whose id the reference
	// names. More than one means the prefix is ambiguous here.
	Matches []Candidate
	// MovedTo and MovedToName name the computer this one handed the
	// thread to, when the reference names a thread that moved away.
	MovedTo     string
	MovedToName string
}

// Candidate is one thread a reference could mean.
type Candidate struct {
	ThreadID   string `json:"thread_id"`
	Title      string `json:"title,omitempty"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
}

// Target is a resolved thread and the computer that owns it.
type Target struct {
	ThreadID   string
	Title      string
	ComputerID string
	Computer   string
	// Local is true when the thread lives on the caller's own computer.
	Local bool
	// Partial names the computers that did not answer the fan-out. The
	// match stands; the note says the answer was not complete.
	Partial []Computer
}

// Window kinds for thread_show.
const (
	WindowTail   = "tail"
	WindowHead   = "head"
	WindowSince  = "since"
	WindowAround = "around"
	WindowAll    = "all"
)

// WindowQuery asks the App to turn a requested window into positions.
type WindowQuery struct {
	ThreadID string
	Kind     string
	// Turns is the turn count for tail and head, and the turns of context
	// on each side for around.
	Turns int
	// ItemID anchors since and around.
	ItemID string
	// SinceUnixMs anchors since when ItemID is empty.
	SinceUnixMs int64
}

// WindowBounds is an absolute inclusive timeline position range. For a
// non-empty window all three positions are set: there is no open end and
// no zero that means "to the end", so a renderer reads the bounds as
// given.
type WindowBounds struct {
	From int64
	To   int64
	// HighWater is the greatest position that existed when the bounds
	// were read. A cursor pins it so streaming growth never shifts a page.
	HighWater int64
	// Empty is true when the thread has no items in this window. Only
	// then are From, To and HighWater meaningless.
	Empty bool
}

// TranscriptQuery asks for one page of items in timeline order.
type TranscriptQuery struct {
	ThreadID string
	// From and To bound the request by inclusive timeline position. Both
	// are absolute positions from WindowBounds; neither is optional and
	// zero is not a wildcard.
	From int64
	To   int64
	// Limit bounds the rows returned. The App must never exceed it.
	Limit int
	// Include names the optional content kinds whose body is wanted:
	// IncludeThinking, IncludeToolOutputs, IncludeDiffs, IncludeSubagents.
	// A kind left out still yields its row with a size and no body.
	Include []string
	// MaxItemBytes clips one item's body. Size still reports the whole
	// stored size so the renderer can point at thread_item.
	MaxItemBytes int
}

// Include kinds for thread_show.
const (
	IncludeThinking    = "thinking"
	IncludeToolOutputs = "tool_outputs"
	IncludeDiffs       = "diffs"
	IncludeSubagents   = "subagents"
	IncludeAll         = "all"
)

// TranscriptSlice is one page of items.
type TranscriptSlice struct {
	Items []Item
	// HighWater is the greatest position that existed at read time.
	HighWater int64
}

// Item is one timeline row.
type Item struct {
	ID       string `json:"item_id"`
	Position int64  `json:"position"`
	// Kind is the item kind: user_text, assistant_text, thinking,
	// tool_call, tool_output, diff, subagent, error.
	Kind string `json:"kind"`
	// Role is what the transcript prefixes the row with: user, assistant,
	// tool or system.
	Role   string `json:"role"`
	TurnID string `json:"turn_id,omitempty"`
	// Name is the tool name on a tool_call row.
	Name string `json:"name,omitempty"`
	// Text is the body, clipped to TranscriptQuery.MaxItemBytes.
	Text string `json:"text,omitempty"`
	// Size is the whole stored size of the body in bytes.
	Size int64 `json:"size"`
	// Clipped is true when Text is shorter than Size.
	Clipped bool `json:"clipped,omitempty"`
}

// PayloadQuery reads a byte range of one item.
type PayloadQuery struct {
	ThreadID string
	ItemID   string
	// Offset is an absolute byte offset from the start of the payload and
	// is never negative here: this package resolves a negative offset
	// against Size before it asks.
	Offset int64
	// MaxBytes bounds the bytes returned. Zero is a metadata read: it
	// returns Kind and Size with no bytes, which is how a negative offset
	// and a line walk learn the payload's size before reading any of it.
	MaxBytes int64
}

// Payload is one range of an item's stored bytes.
type Payload struct {
	// Kind is the item kind, so a result can say what was read.
	Kind string
	// Size is the whole payload size in bytes.
	Size int64
	// Offset is the absolute offset Bytes starts at.
	Offset int64
	Bytes  []byte
}

// SearchQuery is one computer's half of thread_search.
type SearchQuery struct {
	// Query is FTS5 match syntax. Empty means a listing by last activity.
	Query string
	// ThreadID restricts the search to one thread.
	ThreadID string
	// Kind is user, assistant, tool or title.
	Kind      string
	ProjectID string
	Provider  string
	// State filters on the derived thread state.
	State string
	// Archived is nil for the default: included with a query, excluded
	// without one.
	Archived *bool
	// SpawnedBy is the caller's thread id when spawned_by_me is set.
	SpawnedBy string
	// SinceUnixMs bounds last activity.
	SinceUnixMs int64
	Limit       int
	// Offset continues a page. It comes from this package's cursor and is
	// never chosen by the model.
	Offset int
}

// SearchPage is one computer's rows.
type SearchPage struct {
	Rows []Hit
	// Indexing is true while this computer is still building its index.
	Indexing bool
	// More is true when rows past this page exist.
	More bool
}

// Hit is one row of a search or a listing.
type Hit struct {
	Thread Thread
	Live   LiveState
	// ItemID and Snippet are set for a query hit.
	ItemID  string
	Snippet string
}

// ExportQuery renders a whole window to a file.
type ExportQuery struct {
	ThreadID string
	Bounds   WindowBounds
	Include  []string
}

// ExportFile is a rendered file on the computer that holds the thread.
type ExportFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// CatalogQuery narrows a large thread_options answer.
type CatalogQuery struct {
	Provider  string
	ProjectID string
}

// Catalog is what a spawn can choose from on one computer, rendered from
// the catalogs the app already keeps and never from a hand-written list.
type Catalog struct {
	// Reachable is false when this computer answered but cannot run work.
	Reachable bool
	OS        string
	Providers []ProviderOption
	Projects  []ProjectOption
}

// ProviderOption is one registered provider and its models.
type ProviderOption struct {
	ID           string        `json:"provider"`
	Name         string        `json:"name,omitempty"`
	DefaultModel string        `json:"default_model,omitempty"`
	Models       []ModelOption `json:"models"`
}

// ModelOption is one model of one provider.
type ModelOption struct {
	Slug          string   `json:"model"`
	Name          string   `json:"name,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	// Source is the catalog provenance, so a model that came from a
	// provider probe is told apart from a built-in entry.
	Source string `json:"source,omitempty"`
}

// ProjectOption is one registered project as that computer sees it.
type ProjectOption struct {
	ID string `json:"project_id"`
	// Name and Path are that computer's own: a WSL path is not a caller
	// path, and the result says so by naming the computer it came from.
	Name       string            `json:"project"`
	Path       string            `json:"path,omitempty"`
	Workspaces []WorkspaceOption `json:"workspaces,omitempty"`
	Groups     []GroupOption     `json:"groups,omitempty"`
}

// WorkspaceOption is a project root or one of its linked worktrees.
type WorkspaceOption struct {
	Path     string `json:"workspace_path"`
	Branch   string `json:"branch,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
}

// GroupOption is one thread group of one project.
type GroupOption struct {
	ID   string `json:"group_id"`
	Name string `json:"group"`
	Pin  string `json:"pin,omitempty"`
}
