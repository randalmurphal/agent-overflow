// Package claudetui runs the real Claude Code interactive TUI in a PTY and
// reconstructs AO's normalized event stream from outside the process.
//
// Unlike internal/provider/claude (the headless stream-json provider), this
// provider never speaks a protocol to the CLI. It taps two live sources — a
// per-session loopback gateway over ANTHROPIC_BASE_URL (the raw /v1/messages
// SSE) and a small hook relay (PostToolUse / PostToolUseFailure / SessionStart
// / Pre+PostCompact / AskUserQuestion) — reconstructs Claude Code's
// stream-json envelope from them, and feeds that through the shared
// claude.Parser. The result is byte-identical provider.ProviderEvent output to
// the headless path with near-zero duplication.
//
// The transcript (~/.claude/projects/**/<session>.jsonl) is NOT a live source
// here — it lags by seconds. It is reserved for cold-resume history reads.
//
// See docs/architecture/claude-tui-provider.md for the full design.
package claudetui
