package provider

import "context"

// Kind identifies a provider backend.
type Kind string

const (
	KindCodex  Kind = "codex"
	KindClaude Kind = "claude"
)

// RuntimeEvent is a provider-level event emitted during a turn.
type RuntimeEvent struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Payload   any    `json:"payload"`
}

// Adapter is the interface every provider backend must implement.
type Adapter interface {
	// Kind returns the provider identifier.
	Kind() Kind

	// StartSession starts or resumes a provider session for the given thread.
	// If resumeCursor is non-empty, the adapter should attempt to resume.
	StartSession(ctx context.Context, threadID string, model string, resumeCursor string) error

	// SendTurn sends a user turn and streams runtime events to the callback.
	SendTurn(ctx context.Context, threadID string, content string, onEvent func(RuntimeEvent)) error

	// InterruptTurn cancels an in-progress turn.
	InterruptTurn(ctx context.Context, threadID string) error

	// StopSession tears down the provider session for a thread.
	StopSession(ctx context.Context, threadID string) error
}
