package provider

// ItemKind for persisted items in the database.
type ItemKind string

const (
	ItemUserText       ItemKind = "user_text"
	ItemAssistantText  ItemKind = "assistant_text"
	ItemThinking       ItemKind = "thinking"
	ItemToolCall       ItemKind = "tool_call"
	ItemToolCompletion ItemKind = "tool_completion"
	ItemError          ItemKind = "error"
	ItemCompaction     ItemKind = "compaction"
	ItemNotification   ItemKind = "notification"
	// ItemAPIRetry is the live-updating retry indicator. Deterministic
	// per-turn id (retry:N where N is turnIndex) so subsequent api_retry
	// events upsert in place. Hidden until the 4th attempt to mirror
	// Claude Code's SystemAPIErrorMessage hidden-until-attempt-4 behavior.
	ItemAPIRetry ItemKind = "api_retry"
	// ItemAPIError is a retry-exhausted assistant API error. Distinguished
	// from ItemError so the renderer can branch on the assistant.error
	// enum value (rate_limit, authentication_failed, billing_error, etc.)
	// for kind-specific actionable copy. Generic ItemError stays for
	// non-API errors and Codex provider errors.
	ItemAPIError ItemKind = "api_error"
	// ItemTerminalInteraction is the minimal "Waited for background
	// terminal" marker persisted when the Codex model polled a
	// backgrounded PTY via an empty-stdin `write_stdin` call. No
	// payload — the kind alone carries the semantic; meta carries
	// process_id for debugging.
	ItemTerminalInteraction ItemKind = "terminal_interaction"
)
