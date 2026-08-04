package provider

import (
	"context"
	"encoding/json"
)

// SendOptions carries per-turn settings that are not fixed at provider
// session start. InteractionMode is explicit because Claude's permission mode
// changes on plan vs implementation turns without necessarily restarting the
// subprocess.
type SendOptions struct {
	InteractionMode InteractionMode
	Attachments     []ImageAttachment
	// OutputSchema is the per-turn structured-output schema. Codex sends it
	// as outputSchema on every schemaed turn; Claude ignores it because its
	// output schema is fixed on Config when the session process starts.
	OutputSchema json.RawMessage
	// UserMessageUUID, when non-empty, is the message id the app minted at
	// send time. Providers that let the client supply the user-message id
	// send it on the wire so the id is known before the provider echoes the
	// message back — letting a revert slice by a stable id it knew at send
	// time. Claude honours a client-supplied top-level `uuid` on the
	// stream-json envelope; Codex assigns its own ids and ignores this.
	// Optional: empty means "let the provider assign the id" (legacy
	// behaviour, id learned from the echo).
	UserMessageUUID string
	// AllowClaudeSlashCommand opts this send OUT of the Claude slash-command
	// guard, letting a message whose first word is command-shaped ("/usage",
	// "/workflow run x") reach the CLI's own command router.
	//
	// Default false, and that default is load-bearing: the Claude CLI routes
	// any stdin user message starting with `/word` to its command router and
	// SWALLOWS it — an unknown name answers "Unknown command: /x" with
	// num_turns 0 and the model never sees the text (verified 2.1.219,
	// 2026-08-03 live probe). Ordinary sends therefore go out guarded, with a
	// leading newline that defeats the CLI's `startsWith('/')` test. Set true
	// only when the user DELIBERATELY invoked a provider command.
	//
	// Claude-only: Codex has no text command router, and claude-tui types into
	// the real TUI where commands are already a first-class affordance. Both
	// ignore this field.
	AllowClaudeSlashCommand bool
}

// ImageAttachment is the provider-ready form of a user-attached image.
// Exactly one of Data / Path carries the image, chosen per provider so we
// load only the representation the send needs:
//   - Data: the raw bytes, base64-encoded inline on the wire (headless claude,
//     codex). Kept out of the store timeline metadata and loaded only for the
//     send that needs them.
//   - Path: the absolute on-disk path, for a provider that ingests an image by
//     path rather than by bytes. claude-tui pastes the path into the real TUI
//     composer, where Claude reads the file itself — so it never loads Data.
type ImageAttachment struct {
	ID       string
	Filename string
	MimeType string
	Size     int64
	Data     []byte
	Path     string
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
	// PID returns the OS process id of the session's provider subprocess,
	// or 0 if no process is live. Because the subprocess is its own
	// process-group leader (Setpgid), this doubles as the group id for a
	// negative-PID group kill — the orphan reaper uses it to tear down a
	// session's whole process tree if the app dies ungracefully.
	PID() int
	// Close tears down the session and any provider subprocess it owns.
	Close() error
}
