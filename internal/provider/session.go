package provider

import "context"

// SendOptions carries per-turn settings that are not fixed at provider
// session start. InteractionMode is explicit because Claude's permission mode
// changes on plan vs implementation turns without necessarily restarting the
// subprocess.
type SendOptions struct {
	InteractionMode InteractionMode
	Attachments     []ImageAttachment
}

// ImageAttachment is the provider-ready form of a user-attached image.
// Bytes are kept out of the store timeline metadata and loaded only for the
// provider send that needs them.
type ImageAttachment struct {
	ID       string
	Filename string
	MimeType string
	Size     int64
	Data     []byte
}

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
	Send(ctx context.Context, content string, opts SendOptions) error
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
