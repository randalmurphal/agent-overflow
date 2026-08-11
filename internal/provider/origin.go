package provider

// The origin markers Agent Overflow stamps on the provider sessions it starts.
// Each provider records its client's marker in the session file it writes —
// Claude as the transcript header's `entrypoint`, Codex as `session_meta`'s
// `originator` — which is what lets a later reader tell a session AO ran from
// one the user ran themselves.
//
// They live here rather than beside the spawns that write them because they
// have a second, non-adjacent consumer: the session-import scan
// (`internal/sessionimport`) reads them back off provider files and compares
// against these exact strings to decide `Row.RanInAgentOverflow`. Declared once
// per side, renaming a writer would leave every test green while making that
// answer permanently false for every session written afterwards — a silent
// failure with no symptom until a user wonders why the "already imported in
// Agent Overflow" filter stopped hiding anything.
//
// The two spellings differ because the wires do, and neither is free to change:
// Claude's is an environment-variable value the CLI stores verbatim, Codex's is
// a JSON-RPC `clientInfo.name`. Both are already written into session files on
// disk, so a rename would only orphan the history it was meant to describe.
const (
	// ClaudeEntrypointOrigin is the CLAUDE_CODE_ENTRYPOINT value pinned on
	// every Claude spawn (`claude/session.go`).
	ClaudeEntrypointOrigin = "agent-overflow"
	// CodexClientOrigin is the `clientInfo.name` sent on every Codex
	// app-server handshake (`codex/session.go`, `codex/models.go`).
	CodexClientOrigin = "agent_overflow"
)
