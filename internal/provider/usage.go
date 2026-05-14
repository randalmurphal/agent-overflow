package provider

// UsageEvent is the frontend-facing channel payload for the context-window
// meter. `usage` updates the ring; `reset` clears it after compaction;
// `rate_limits` carries a rate-limits snapshot folded onto the same channel
// for future UI, but does not change the context ring.
type UsageEvent struct {
	Action                string              `json:"action"` // "usage" | "reset" | "rate_limits"
	ThreadID              string              `json:"threadId"`
	UsedTokens            int                 `json:"usedTokens,omitempty"`
	MaxTokens             int                 `json:"maxTokens,omitempty"`
	ContextPercent        float64             `json:"contextPercent,omitempty"`
	AutoCompactPercent    int                 `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int                 `json:"autoCompactTokenLimit,omitempty"`
	RateLimits            *RateLimitsSnapshot `json:"rateLimits,omitempty"`
}

// SessionInfo contains metadata from the provider init/handshake.
type SessionInfo struct {
	SessionID string   `json:"sessionId"`
	Model     string   `json:"model"`
	CWD       string   `json:"cwd"`
	Tools     []string `json:"tools,omitempty"`
	Version   string   `json:"version,omitempty"`
}

// AccountInfo describes the authenticated provider account surfaced
// through a startup probe. For Claude the data lands on the inner
// `response.response.account` of a `control_request{subtype:"initialize"}`
// reply (NOT on `system/init`, which doesn't carry account fields on
// the live wire). For Codex the data lands on the `RateLimitSnapshot`
// returned by `account/rateLimits/read` (planType + apiProvider hint).
//
// Empty SubscriptionType + empty TokenSource are the unauthenticated
// signal forge has used historically; consumers branch on that.
type AccountInfo struct {
	SubscriptionType string `json:"subscriptionType,omitempty"`
	TokenSource      string `json:"tokenSource,omitempty"`
	APIProvider      string `json:"apiProvider,omitempty"`
}

// TokenUsage tracks turn token/cost accounting.
type TokenUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	TotalCostUSD             float64 `json:"totalCostUsd,omitempty"`
}

// ContextWindow describes provider context window usage.
type ContextWindow struct {
	UsedTokens            int     `json:"usedTokens"`
	MaxTokens             int     `json:"maxTokens,omitempty"`
	UsedPercentage        float64 `json:"usedPercentage,omitempty"`
	AutoCompactPercent    int     `json:"autoCompactPercent,omitempty"`
	AutoCompactTokenLimit int     `json:"autoCompactTokenLimit,omitempty"`
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
	Limits    []RateLimitEntry `json:"limits"`
	UpdatedAt int64            `json:"updatedAt"`
}
