// Package sessionfork forks a Claude Code session JSONL transcript at an
// arbitrary past message, producing a new session file that the CLI's
// `--resume <newID>` can load with the truncated history.
//
// Mirrors Anthropic's official Python SDK fork_session(session_id,
// up_to_message_id) at:
//
//	~/repos/claude-agent-sdk-python/src/claude_agent_sdk/_internal/session_mutations.py
//
// The transform: read JSONL, filter sidechains, slice up to the chosen
// message UUID, remap every UUID, re-chain parentUuid (skipping progress
// entries), strip session-leakage fields, stamp forkedFrom, append
// content-replacement and custom-title entries, write to a new
// <newID>.jsonl atomically.
//
// Streaming I/O throughout — bufio.Scanner with a bumped buffer keeps
// memory bounded for multi-MB sessions.
package sessionfork
