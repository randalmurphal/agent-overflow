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
	// ClientUserMessageID is the caller's own id for this user message,
	// stamped onto the outbound turn so the provider's echo can be matched
	// back to the row that produced it without relying on ordering.
	//
	// Codex sends it as `clientUserMessageId` on every `turn/start` and
	// `turn/steer` (supported since codex 0.136; AO's provider floor is
	// 0.143, so there is no version gate) and echoes it back on the
	// `userMessage` ThreadItem as `clientId`. It is deliberately NOT
	// UserMessageUUID: that field is the provider-assigned message id
	// Claude accepts on its stream-json envelope and must be a UUID, while
	// this one is an opaque correlation handle in AO's own grammar.
	//
	// Claude and claude-tui ignore it. Empty means "no correlation handle" —
	// upstream mints one of its own and the echo carries that instead.
	ClientUserMessageID string
	// GuardClaudeSlashCommand forces a command-shaped leading word to reach
	// Claude as model prose instead of entering the CLI's command router.
	//
	// The zero value deliberately preserves Claude's native syntax: `/usage`,
	// installed skills, plugin commands, MCP prompts, aliases, and unknown
	// `/word` inputs all go to the router. Discovery is asynchronous and can
	// never be a safe send-time gate. Classifying from a cached command list
	// made the same text run as a command or as model prose depending on
	// whether that cache had answered before Enter was pressed.
	//
	// Set this only when Agent Overflow expanded one of its own composer
	// commands while retaining the typed `/name` at the front of the payload.
	// The appended block is model context, so allowing Claude to claim the
	// first word would silently discard it. Unknown commands now fail visibly
	// through Claude's own "Unknown command" result instead of being
	// reinterpreted as prose.
	//
	// Claude-only: Codex has no text command router, and claude-tui types into
	// the real TUI where commands are already a first-class affordance.
	GuardClaudeSlashCommand bool
	// InternalCommand marks a slash command Agent Overflow issues on its OWN
	// behalf rather than because the user asked for it — peer-session naming
	// (`/rename`) and the live-config `/effort` / `/fast` writes.
	//
	// The CLI answers every local command with a `<synthetic>` assistant
	// envelope, which triage persists as a `command_result` row. For a command
	// the user never typed, that row is AO's bookkeeping showing up in the
	// user's transcript. Setting this suppresses the ROW ONLY: the command
	// still runs, its lifecycle bracket still settles, and the output event
	// still reaches the app-layer observers that read it.
	//
	// The suppression it buys is UNCONDITIONAL — the reply text is not
	// consulted — which is exactly why it must not be set on a command the
	// user typed. AO surfaces its own commands' failures through its own
	// reconcilers, so a row would be noise; a user's command that failed has
	// nowhere else to be said. A user-typed command whose output merely
	// restates state AO renders itself (`/effort xhigh`, `/fast on`,
	// `/model <slug>`) needs no flag: the Claude package recognises it from
	// the outbound text and then suppresses the row only if the REPLY is a
	// recognised confirmation.
	//
	// Claude-only. Internal commands are always sent unguarded.
	InternalCommand bool
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
