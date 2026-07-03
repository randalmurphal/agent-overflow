package store

// DiscussionParticipant defines a single participant in a discussion.
type DiscussionParticipant struct {
	Role        string `json:"role"`
	Description string `json:"description"`
	System      string `json:"system"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
}

// DiscussionSettings contains runtime discussion configuration.
type DiscussionSettings struct {
	MaxTurns int `json:"maxTurns"`
}

// DiscussionDefinition is the persisted discussion registry record.
type DiscussionDefinition struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Description  string                  `json:"description"`
	Scope        string                  `json:"scope"`
	ProjectID    string                  `json:"projectId,omitempty"`
	Participants []DiscussionParticipant `json:"participants"`
	Settings     DiscussionSettings      `json:"settings"`
	CreatedAt    int64                   `json:"createdAt"`
	UpdatedAt    int64                   `json:"updatedAt"`
}

// Channel is a persisted discussion channel.
type Channel struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	// MaxTurns is the deliberation circuit-breaker turn count, carried
	// on the channel row so a restart can rebuild the in-memory FSM
	// (internal/discussion.Deliberation) with the same limit the
	// original discussion.DiscussionDefinition specified. Zero/unset on
	// pre-v12 rows defaults to 8 via the migration's column default.
	MaxTurns  int   `json:"maxTurns"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// ChannelMessage is one ordered message within a channel.
//
// Meta is JSON-shaped optional sidecar data persisted alongside the
// raw message content. Today it carries the pathlinks allowlist
// (`pathRefs`) stamped at PostMessage time so the frontend's markdown
// linkifier knows which file references in the rendered text point
// at real files in the thread's workspace. The column is nullable
// (pre-v2 rows have no value); callers should treat absence as
// "no allowlist".
type ChannelMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channelId"`
	Sequence  int    `json:"sequence"`
	FromType  string `json:"fromType"`
	FromID    string `json:"fromId"`
	FromRole  string `json:"fromRole,omitempty"`
	Content   string `json:"content"`
	Meta      string `json:"meta,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}
