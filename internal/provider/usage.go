package provider

// UsageEvent is the frontend-facing channel payload for the context-window
// meter. `usage` updates the ring; `reset` clears it after compaction;
// `rate_limits` carries a rate-limits snapshot folded onto the same channel
// for future UI, but does not change the context ring.
type UsageEvent struct {
	Action                string  `json:"action"` // "usage" | "reset" | "rate_limits"
	ThreadID              string  `json:"threadId"`
	UsedTokens            int     `json:"usedTokens,omitempty"`
	MaxTokens             int     `json:"maxTokens,omitempty"`
	ContextPercent        float64 `json:"contextPercent,omitempty"`
	AutoCompactPercent    int     `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int     `json:"autoCompactTokenLimit,omitempty"`
	// Exceeded is the wire signal that the model returned
	// `ContextWindowExceeded` and the provider pegged usage to the window
	// size as a sentinel. UI should render this distinctly from a real
	// 100% reading. Codex-only today (`codex-rs/protocol/src/protocol.rs`
	// `set_total_tokens_full`); Claude has no equivalent sentinel.
	Exceeded   bool                `json:"exceeded,omitempty"`
	RateLimits *RateLimitsSnapshot `json:"rateLimits,omitempty"`
}

// FastModeStatus is the provider's own report of whether the running
// session is actually serving turns in fast mode, and why not when it
// isn't. It is LIVE session state, never history: the CLI restates it on
// every `system/init` and every `result` envelope, so the newest report
// is the whole answer and nothing older is worth keeping.
//
// Claude carries it as `fast_mode_state` / `fast_mode_disabled_reason`.
// BOTH fields are optional on the wire, and `fast_mode_disabled_reason`
// only exists on newer binaries (absent on 2.1.105, present on 2.1.219),
// so an absent field means "no signal", NEVER "off" — a nil
// *FastModeStatus and a status whose State is "" are both silence and
// must not be rendered as a denial.
//
// Observed State values: "on", "off", "cooldown" (paused after a rate
// limit). Observed DisabledReason values (2.1.219): not_first_party,
// disabled_by_env, unknown, model_not_allowed, sdk_opt_in_required,
// pending, free, preference, extra_usage_disabled, network_error. Both
// are carried as raw provider strings — the enum is undocumented and
// version-dependent, so mapping to display copy happens at the UI edge
// with a passthrough default rather than being narrowed here.
type FastModeStatus struct {
	State          string `json:"state,omitempty"`
	DisabledReason string `json:"disabledReason,omitempty"`
}

// IsZero reports whether the wire carried no fast-mode signal at all.
func (s FastModeStatus) IsZero() bool {
	return s.State == "" && s.DisabledReason == ""
}

// SessionInfo contains metadata from the provider init/handshake.
type SessionInfo struct {
	SessionID  string      `json:"sessionId"`
	Model      string      `json:"model"`
	CWD        string      `json:"cwd"`
	Tools      []string    `json:"tools,omitempty"`
	Version    string      `json:"version,omitempty"`
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
	// FastMode is Claude's `system/init` fast-mode report. Nil when the
	// envelope carried neither key (older CLI, or a provider with no
	// fast-mode concept).
	FastMode *FastModeStatus `json:"fastMode,omitempty"`
	// SlashCommands is Claude's `system/init` `slash_commands[]`: the NAMES
	// (no leading slash, no descriptions) of every provider-executed command
	// this session can run. It is the only discovery surface that includes MCP
	// prompt commands (`mcp__server__prompt`); the richer `initialize`
	// control_response list does not. Empty means the envelope said nothing —
	// never "this session has no commands".
	SlashCommands []string `json:"slashCommands,omitempty"`
	// Skills is `system/init`'s `skills[]` — names only, same absence rule.
	// Skills are also invocable as `/name`, so they appear in SlashCommands
	// too; this list is what tells them apart.
	Skills []string `json:"skills,omitempty"`
	// Plugins is `system/init`'s `plugins[]`. Same absence rule.
	Plugins []PluginInfo `json:"plugins,omitempty"`
	// OutputStyle is Claude's `system/init.output_style` — the name of the
	// response style the session actually launched with, which is the CLI's
	// own answer to what AO put in the `--settings` block. The CLI reports
	// the literal `"default"` when no style is selected, which is a real
	// value, not an absence: the echo exists so a settings-source style AO
	// never sent can still be seen. No other provider has the concept.
	OutputStyle string `json:"outputStyle,omitempty"`
	// MCPServerErrors reports servers the provider REFUSED at startup.
	// Claude emits it as `system/init.mcp_server_errors` (2.1.237). The
	// affected names are ABSENT from MCPServers — that is the whole point
	// of the field: without it a server that failed config validation is
	// indistinguishable from one that was never configured, and the UI
	// can only infer "not connected" instead of saying why. Same absence
	// rule as the discovery arrays above: an omitted key means "no
	// errors", never "unknown".
	MCPServerErrors []MCPServerError `json:"mcpServerErrors,omitempty"`
	// Capabilities is Claude's `system/init.capabilities` — the protocol
	// feature tokens the running CLI advertises so a client can
	// feature-detect instead of parsing `claude_code_version`. The CLI
	// documents it as an OPEN SET ("ignore unknown values; check each
	// capability for exactly the behavior you use"), so it passes through
	// as data, never an enum, and an ABSENT key means "this build says
	// nothing" — never "no capabilities". Known tokens and the version
	// floors they replace are listed in the Claude area guide.
	Capabilities []string `json:"capabilities,omitempty"`
}

// MCPServerError is one entry of Claude's `system/init.mcp_server_errors`
// — a server config entry that failed validation and was skipped.
//
// Type is a coarse category the CLI documents as an OPEN SET (2.1.237
// lists unknown_type / url_missing_type / invalid_config / reserved_name
// and instructs clients to treat unrecognized values as a generic skip),
// so it passes through as data, never an enum. Message is the
// human-readable explanation and is the only field a user can act on.
type MCPServerError struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// MCPServer reports an MCP server's name and provider-reported
// connection state at session init time. Claude emits this via
// `system/init.mcp_servers`; Codex equivalents land on a different
// event (mcpServerStatus). Status is the raw provider string —
// triage/app projects it onto the shared mcpstatus.Status enum.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

// AccountInfo describes the authenticated provider account surfaced
// through a startup probe. For Claude the data lands on the inner
// `response.response.account` of a `control_request{subtype:"initialize"}`
// reply (NOT on `system/init`, which doesn't carry account fields on
// the live wire). For Codex the data lands on the `RateLimitSnapshot`
// returned by `account/rateLimits/read` (planType + apiProvider hint).
//
// Do NOT re-derive "is this account logged in" from these fields.
// Which of them a provider populates is provider- and backend-specific
// (Claude fills only APIProvider on non-firstParty backends, and only
// Email on a firstParty profile login; Codex hardcodes APIProvider and
// may legitimately report nothing else). The one Claude answer is
// `providerstatus.ClaudeUnauthenticated`; Codex deliberately has none.
// OrgID / OrgName carry the organization (Claude) or ChatGPT workspace
// (Codex) behind the login. One email can hold a separate login per
// organization, so email alone cannot key an account. Neither field is
// guaranteed: Claude's probe wire carries only the display name (the
// uuid lives in `~/.claude.json`'s oauthAccount and is enriched at
// adoption time), Codex's `account/read` carries neither (the id is
// parsed out of `auth.json` where the caller holds those bytes), and
// API-key auth has no organization at all. Blank means UNKNOWN, never
// "no organization" — matching must treat it as compatible with any.
type AccountInfo struct {
	Email            string `json:"email,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
	TokenSource      string `json:"tokenSource,omitempty"`
	APIProvider      string `json:"apiProvider,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
}

// TokenUsage tracks per-turn token/cost accounting. Values are per-turn
// deltas, never provider-cumulative — the provider parsers own the
// cumulative→delta subtraction (Claude: modelUsage snapshot deltas in
// parse_result.go; Codex: thread/tokenUsage/updated total deltas in
// usage_accounting.go) so everything downstream can sum rows safely.
//
// InputTokens is NON-cached input for both providers (Codex's wire
// inputTokens includes cachedInputTokens; the parser subtracts —
// see codex-rs protocol.rs TokenUsage::non_cached_input).
// ReasoningOutputTokens is Codex-only and informational: it is already
// included in OutputTokens on the wire.
//
// TotalCostUSD is wire-reported only (Claude computes it CLI-side).
// There is deliberately no client-side pricing fallback — a missing
// cost stays 0 rather than being estimated from a rate table.
type TokenUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	ReasoningOutputTokens    int     `json:"reasoningOutputTokens,omitempty"`
	TotalCostUSD             float64 `json:"totalCostUsd,omitempty"`
}

// IsZero reports whether the usage carries no accounting signal at all.
func (u TokenUsage) IsZero() bool {
	return u == TokenUsage{}
}

// Add accumulates other into u field-wise.
func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
	u.ReasoningOutputTokens += other.ReasoningOutputTokens
	u.TotalCostUSD += other.TotalCostUSD
}

// Sub subtracts other from u field-wise, CLAMPED AT ZERO on every field.
//
// The clamp is what makes this safe for the cumulative→delta subtraction both
// parsers run: a provider cumulative is supposed to be monotonic, but a
// session resume, a compaction, or a re-keyed model row can hand back a
// snapshot lower than the one already accounted for. Clamping reports "no new
// spend" for that turn, where a signed delta would emit a NEGATIVE usage row
// that every downstream sum (ledger, dashboard, workflow budget) would then
// subtract from real spend.
func (u *TokenUsage) Sub(other TokenUsage) {
	u.InputTokens = max(u.InputTokens-other.InputTokens, 0)
	u.OutputTokens = max(u.OutputTokens-other.OutputTokens, 0)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens-other.CacheReadInputTokens, 0)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens-other.CacheCreationInputTokens, 0)
	u.ReasoningOutputTokens = max(u.ReasoningOutputTokens-other.ReasoningOutputTokens, 0)
	u.TotalCostUSD = max(u.TotalCostUSD-other.TotalCostUSD, 0)
}

// ModelTokenUsage attributes a per-turn usage delta to one model. A turn
// that ran several models (parent + Task subagents on Claude) produces one
// entry per model; Codex cannot attribute per-model and produces a single
// entry for the session's configured model.
type ModelTokenUsage struct {
	Model string `json:"model"`
	TokenUsage
}

// UsageProviderFamily normalizes a thread provider name to its billing
// family for usage-ledger attribution. claude-tui drives the same claude
// binary and bills against the same account, so its usage must not split
// into a separate provider bucket in aggregates.
func UsageProviderFamily(providerName string) string {
	if providerName == string(ClaudeTUI) {
		return string(Claude)
	}
	return providerName
}

// ContextWindow describes provider context window usage.
type ContextWindow struct {
	UsedTokens            int     `json:"usedTokens"`
	MaxTokens             int     `json:"maxTokens,omitempty"`
	UsedPercentage        float64 `json:"usedPercentage,omitempty"`
	AutoCompactPercent    int     `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int     `json:"autoCompactTokenLimit,omitempty"`
	// Exceeded mirrors UsageEvent.Exceeded — set by the Codex parser when
	// the wire reports `last.totalTokens == modelContextWindow` exactly,
	// which is the Codex sentinel for `ContextWindowExceeded` (not a
	// real reading at 100%).
	Exceeded bool `json:"exceeded,omitempty"`
}

// codexContextBaselineTokens is reserved for system prompt, tools, and the
// compact-call headroom by Codex's TUI before computing "% context left".
// See `codex-rs/protocol/src/protocol.rs` `percent_of_context_window_remaining`
// (BASELINE_TOKENS = 12000).
const codexContextBaselineTokens = 12000

// ComputeContextPercent returns the "used" percentage shown on the
// context-window meter for a given provider. The formula is
// provider-aware so the meter agrees with what each provider's native UX
// shows for the same wire numbers.
//
// Codex: mirrors `codex-rs/protocol/src/protocol.rs:2113-2159`. A baseline
// of 12000 tokens is subtracted from both numerator and denominator. The
// result is clamped to [0,100].
//
// Claude (and everything else): plain `used / max * 100`.
//
// Inputs with `max <= 0` return 0 (caller is responsible for falling back
// to a default before displaying anything). Callers with a string-valued
// provider name (e.g. from settings rows) should cast at the boundary:
// `ComputeContextPercent(ProviderKind(settings.Provider), used, max)`.
func ComputeContextPercent(kind ProviderKind, used, max int) float64 {
	if max <= 0 {
		return 0
	}
	if kind == Codex {
		if max <= codexContextBaselineTokens {
			return 100
		}
		effectiveWindow := max - codexContextBaselineTokens
		effectiveUsed := used - codexContextBaselineTokens
		if effectiveUsed < 0 {
			effectiveUsed = 0
		}
		pct := float64(effectiveUsed) / float64(effectiveWindow) * 100
		if pct < 0 {
			return 0
		}
		if pct > 100 {
			return 100
		}
		return pct
	}
	return float64(used) / float64(max) * 100
}

// RateLimitEntry represents a single rate limit window.
type RateLimitEntry struct {
	LimitID     string  `json:"limitId"`
	LimitName   string  `json:"limitName"`
	UsedPercent float64 `json:"usedPercent"`
	WindowMins  int     `json:"windowMins"`
	ResetsAt    int64   `json:"resetsAt"`
}

// RateLimitsSnapshot is a point-in-time view of all rate limits.
type RateLimitsSnapshot struct {
	Provider  string           `json:"provider"`
	AccountID string           `json:"accountId,omitempty"`
	Limits    []RateLimitEntry `json:"limits"`
	UpdatedAt int64            `json:"updatedAt"`
	// Complete reports that Limits is the provider's WHOLE answer for this
	// account, so a cached limit the reading omits no longer exists and must
	// be dropped rather than kept at its last value. Only a source that reads
	// every bucket in one response may set it: Claude's OAuth usage endpoint
	// and Codex's `rateLimitsByLimitId` map. A single-window wire event, a
	// per-bucket notification, or a reading that had to skip a malformed
	// limit is PARTIAL and leaves this false, so the merge only updates the
	// windows it names.
	//
	// A provider drops a bucket from its answer when the bucket has no usage,
	// which is exactly what a mid-window reset produces: without this flag an
	// additive merge kept the pre-reset percentage forever (2026-09-01, a
	// Fable weekly row frozen at 90% while session and all-models read 0%).
	Complete bool `json:"complete,omitempty"`
}
