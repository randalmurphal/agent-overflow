package provider

import "context"

// Session is the minimal interface both provider.{claude,codex}.Session
// satisfy. The app layer uses this to avoid branching on
// `switch { case sess.claude != nil: ...; case sess.codex != nil: ... }`
// for the handful of methods every provider session exposes the same
// way. Anything that's only on one provider (e.g. Claude's
// SessionID(), Codex's SetDynamicToolHandler) stays behind the
// concrete type; the wrapper in app.go still carries the typed
// pointers so those call sites are unaffected.
type Session interface {
	// Send delivers a user turn to the provider.
	Send(ctx context.Context, content string) error
	// Interrupt asks the provider to abort the current turn.
	Interrupt(ctx context.Context) error
	// RespondToApproval forwards the user's decision on a pending
	// interactive request back to the provider.
	RespondToApproval(ctx context.Context, resp ApprovalResponse) error
	// RespondToUserInput forwards answers for a structured user-input
	// request back to the provider.
	RespondToUserInput(ctx context.Context, resp UserInputResponse) error
	// Close tears down the session and any provider subprocess it owns.
	Close() error
}
