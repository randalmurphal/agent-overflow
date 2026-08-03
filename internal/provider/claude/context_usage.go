package claude

import (
	"context"
	"encoding/json"
	"fmt"
)

// ContextUsage is the canonical `/context` breakdown Claude returns for an
// outbound `control_request{subtype:"get_context_usage"}`. It is the same
// data the CLI's own `/context` slash command renders — the CLI routes both
// through one `collectContextData` path — so the numbers here are exact,
// not the passive per-turn estimate the live meter is driven from.
//
// This is a LIVE-SESSION read with no history value: it describes the
// process's context right now and is superseded the moment the next turn
// runs. Nothing about it is persisted (Core Principle 2).
//
// Fields the wire carries but we deliberately drop:
//
//   - `gridRows` — a pre-rendered square grid for the CLI's terminal UI,
//     built from the same categories. AO draws its own meter.
//   - `color` on each category — a CLI theme token ("promptBorder",
//     "purple_FOR_SUBAGENTS_ONLY"), meaningless outside its palette.
//   - `memoryFiles` / `mcpTools` / `agents` / `skills` / `slashCommands` /
//     `messageBreakdown` — per-item drilldowns already summarised by the
//     matching category row, and the paths in them are local FS detail.
//   - `apiUsage` — the last API call's token counts, i.e. the same signal
//     `message_delta.usage` already feeds the live meter with. It is also
//     `null` until the process makes its first API call (verified 2.1.219),
//     so it would render empty on exactly the fresh-session case where the
//     breakdown is most useful.
//
// Verified against Claude Code 2.1.219; see
// docs/references/claude-wire.md §get_context_usage and
// docs/references/fixtures/claude/context_usage_control_20260803.summary.json.
type ContextUsage struct {
	// TotalTokens is the context the model actually sees. It is the sum of
	// the non-deferred, non-free-space categories — deferred tool
	// definitions are listed but NOT counted (verified 2.1.219).
	TotalTokens int `json:"totalTokens"`
	// MaxTokens is the model's context window. `rawMaxTokens` is the same
	// number on every version we've captured; ParseContextUsage folds it in
	// as a fallback so a caller never has to know both names.
	MaxTokens int `json:"maxTokens"`
	// Percentage is the CLI's own rounded occupancy figure. Reported, never
	// recomputed — the CLI owns which denominator it used.
	Percentage int `json:"percentage"`
	// Model is the wire model slug the breakdown was computed for.
	Model string `json:"model"`
	// Categories is the breakdown in wire order. Category names are NOT an
	// enum on our side: any row the CLI reports (including ones added in a
	// future release) passes through untouched.
	Categories []ContextUsageCategory `json:"categories"`
}

// ContextUsageCategory is one row of the breakdown. Name is whatever the
// CLI called it — "System prompt", "Memory files", "Free space", and any
// category a later release adds.
type ContextUsageCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// Deferred marks tool definitions the CLI has not loaded into the
	// prompt. They are excluded from TotalTokens, so a consumer that sums
	// the rows must exclude them too or it will overcount.
	Deferred bool `json:"deferred,omitempty"`
}

// GetContextUsage asks the live session for the canonical context-window
// breakdown. This is an on-demand, user-initiated read: it costs a control
// round-trip and CLI-side tokenization work, and it is never polled.
//
// The CLI answers out of band — no turn is consumed and no API call is
// made — so it is safe mid-turn as well as idle.
//
// A CLI that never acks surfaces as a wrapped timeout error; as with every
// other outbound control_request we do NOT kill the session as a fallback
// (see the sendControlRequest contract).
func (s *Session) GetContextUsage(ctx context.Context) (*ContextUsage, error) {
	res, err := s.sendControlRequest(ctx, "get_context_usage", map[string]any{
		"subtype": "get_context_usage",
	})
	if err != nil {
		return nil, err
	}
	if !res.ok {
		if res.errMsg == "" {
			return nil, fmt.Errorf("claude: get_context_usage: provider returned unspecified error")
		}
		return nil, fmt.Errorf("claude: get_context_usage: %s", res.errMsg)
	}
	return ParseContextUsage(res.payload)
}

// ParseContextUsage decodes the `response.response` object of a successful
// get_context_usage round-trip.
//
// Exported so the shape can be tested (and replayed from a fixture) without
// standing up a subprocess.
func ParseContextUsage(payload json.RawMessage) (*ContextUsage, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("claude: get_context_usage: empty response payload")
	}
	var decoded struct {
		TotalTokens  int    `json:"totalTokens"`
		MaxTokens    int    `json:"maxTokens"`
		RawMaxTokens int    `json:"rawMaxTokens"`
		Percentage   int    `json:"percentage"`
		Model        string `json:"model"`
		Categories   []struct {
			Name       string `json:"name"`
			Tokens     int    `json:"tokens"`
			IsDeferred bool   `json:"isDeferred"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("claude: get_context_usage: decode response: %w", err)
	}

	// maxTokens and rawMaxTokens have been identical on every capture, but
	// the CLI declares both. Take whichever is present so the caller ends up
	// with one field it can always divide by.
	maxTokens := decoded.MaxTokens
	if maxTokens <= 0 {
		maxTokens = decoded.RawMaxTokens
	}
	if maxTokens <= 0 {
		// Without a window the breakdown cannot be rendered as occupancy,
		// and a zero denominator downstream would silently read as "0%
		// used" on a full context. Fail loudly instead.
		return nil, fmt.Errorf("claude: get_context_usage: response reported no context window")
	}

	usage := &ContextUsage{
		TotalTokens: decoded.TotalTokens,
		MaxTokens:   maxTokens,
		Percentage:  decoded.Percentage,
		Model:       decoded.Model,
		Categories:  make([]ContextUsageCategory, 0, len(decoded.Categories)),
	}
	for _, cat := range decoded.Categories {
		usage.Categories = append(usage.Categories, ContextUsageCategory{
			Name:     cat.Name,
			Tokens:   cat.Tokens,
			Deferred: cat.IsDeferred,
		})
	}
	return usage, nil
}
