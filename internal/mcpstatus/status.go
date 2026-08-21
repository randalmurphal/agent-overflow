package mcpstatus

import (
	"time"
)

// Provider identifies which native config a status entry was sourced
// from. The same server name can exist for both providers so every
// cache lookup is keyed by (Provider, Name) — see Key.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// Status is the normalized state every MCP server resolves to,
// regardless of which provider reported it. The string values match
// the JSON the bindings hand the frontend; the enum is closed so the
// UI can switch over it exhaustively.
//
// Provider-native → Status projection lives in the provider packages
// (`internal/provider/claude/mcpstatus.go`,
// `internal/provider/codex/mcpstatus.go`) so each adapter owns its
// own wire vocabulary.
type Status string

const (
	StatusUnknown   Status = "unknown"    // never observed, or ambiguous wire state
	StatusStarting  Status = "starting"   // Codex notification "starting" / Claude "pending"
	StatusConnected Status = "connected"  // Codex authStatus∈{unknown,unsupported,bearerToken,oAuth} ∧ initialize proven (serverInfo or tools) OR notif "ready" / Claude "connected"
	StatusNeedsAuth Status = "needs-auth" // Codex "notLoggedIn" / Claude "needs-auth"
	StatusFailed    Status = "failed"     // Codex notif "failed"|"cancelled", or a settled list probe with no initialize evidence / Claude "failed"
	StatusDisabled  Status = "disabled"   // Claude "disabled" (toggled off; the CLI keeps the row and reports it)
)

// Key uniquely identifies a status entry across both providers.
type Key struct {
	Provider Provider `json:"provider"`
	Name     string   `json:"name"`
}

// Source attributes how a ServerStatus was produced so callers can
// reason about freshness without consulting timestamps. "live-session"
// is authoritative (the provider's own process reported it);
// "notification" is a delta from a running provider; "ephemeral-fetch"
// is the on-demand stdout-parse/JSON-RPC-list path used when no live
// session exists.
type Source string

const (
	SourceLiveSession    Source = "live-session"
	SourceNotification   Source = "notification"
	SourceEphemeralFetch Source = "ephemeral-fetch"
)

// ServerStatus is the wire shape every binding speaks. ToolCount /
// Tools / AuthStatus / Error / Raw are best-effort — present when the
// wire source carries them, empty otherwise. Tools holds tool NAMES
// only: both providers' status responses also carry server config
// (args/env can hold live tokens) and tool schemas, and neither may
// ever reach this shape. CheckedAt is for "how stale" display; the
// cache uses its own clock for TTL.
type ServerStatus struct {
	Key
	Status     Status    `json:"status"`
	ToolCount  int       `json:"toolCount,omitempty"`
	Tools      []string  `json:"tools,omitempty"`
	AuthStatus string    `json:"authStatus,omitempty"`
	Error      string    `json:"error,omitempty"`
	Raw        string    `json:"raw,omitempty"`
	Source     Source    `json:"source"`
	CheckedAt  time.Time `json:"checkedAt"`
}
