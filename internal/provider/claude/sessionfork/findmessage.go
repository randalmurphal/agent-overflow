package sessionfork

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrUserTurnOutOfRange is returned when userTurnIndex points past the
// last real user prompt in the source transcript.
var ErrUserTurnOutOfRange = errors.New("sessionfork: user turn index out of range")

// ErrUserTurnAtTranscriptEnd is the off-by-exactly-one variant of
// ErrUserTurnOutOfRange. It is returned when userTurnIndex == count —
// the caller asked for the slice point AFTER the last persisted user
// prompt, which has no anchor entry in the JSONL because nothing came
// after it. The most common cause is the Claude subprocess dying
// before it persisted the user's most recent prompt to its session
// JSONL — AO's checkpoint table records the user_text row at TurnIndex
// N but the JSONL only has prompts 0..N-1. Revert callers should
// detect this and route to a "copy the JSONL as-is" path rather than
// failing the revert; the JSONL is already in the right state from
// Claude's perspective (it never saw the missing prompt), so a
// whole-transcript clone is the semantically correct slice point.
//
// Wraps ErrUserTurnOutOfRange so existing `errors.Is(err,
// ErrUserTurnOutOfRange)` callers continue to match; the more specific
// sentinel only matters for callers that want the recoverable-vs-fatal
// distinction.
var ErrUserTurnAtTranscriptEnd = fmt.Errorf("%w: at transcript end", ErrUserTurnOutOfRange)

// FindUUIDBeforeUserTurn streams the JSONL and returns the UUID of the
// entry immediately before the Nth (0-indexed) real user prompt — i.e.
// the parentUuid of that prompt. This is the right slice point for a
// "revert/fork to BEFORE this user message" operation: we want to keep
// the previous turn's assistant response in full, then start fresh.
//
// "Real user prompt" means a user-role message whose content is the
// user's actual input, not a tool-result echo. When the assistant calls
// a tool, the tool result comes back as a `type:"user"` JSONL entry
// (because the underlying API models tool results as user-role
// messages); these are NOT new turns and don't correspond to anything
// the user typed. A spike against a real session showed 4 user prompts
// produced 13 type:"user" entries (4 prompts + 9 tool-result echoes),
// so a naive "Nth user entry" count would slice at the wrong place.
//
// Returns ("", nil) when userTurnIndex == 0 — the first user prompt has
// no preceding turn, so callers should clear the session entirely
// rather than slice.
//
// Returns ErrUserTurnOutOfRange if userTurnIndex points past the last
// real user prompt.
//
// Why parentUuid: it points at whatever entry came right before the
// user prompt in the conversation chain (typically the previous turn's
// final assistant message, or a tool_result echo if the previous turn
// ended with one). The JSONL is written in chain order, so slicing at
// that UUID inclusive keeps the full prior turn intact.
//
// The function early-terminates as soon as the Nth real prompt is found —
// it does not parse to EOF for large sessions.
func FindUUIDBeforeUserTurn(r io.Reader, userTurnIndex int) (string, error) {
	if userTurnIndex < 0 {
		return "", fmt.Errorf("sessionfork: negative userTurnIndex: %d", userTurnIndex)
	}
	if userTurnIndex == 0 {
		return "", nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, scannerBufInitial), scannerBufMax)

	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if !isRealUserPrompt(entry) {
			continue
		}
		if count == userTurnIndex {
			parent, _ := entry["parentUuid"].(string)
			if parent == "" {
				return "", fmt.Errorf("sessionfork: user turn %d has no parentUuid", userTurnIndex)
			}
			return parent, nil
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("sessionfork: scan: %w", err)
	}
	if userTurnIndex == count {
		return "", fmt.Errorf("%w: requested %d, found %d", ErrUserTurnAtTranscriptEnd, userTurnIndex, count)
	}
	return "", fmt.Errorf("%w: requested %d, found %d", ErrUserTurnOutOfRange, userTurnIndex, count)
}

// SliceUUIDForLastKeptTurn opens the source JSONL and returns the UUID
// to slice at when forking or reverting Claude's session such that the
// resulting transcript includes turns 0..lastKeptTurn inclusive.
//
// Returns ("", nil) when lastKeptTurn < 0 — there's nothing to keep,
// caller should clear the session entirely rather than write an empty
// JSONL. Used by both fork-at-point and within-thread revert; both want
// the same slice semantics: keep through the END of lastKeptTurn so the
// previous turn's full assistant response (and any tool results) are
// preserved.
func SliceUUIDForLastKeptTurn(srcPath string, lastKeptTurn int) (string, error) {
	if lastKeptTurn < 0 {
		return "", nil
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("sessionfork: open claude session: %w", err)
	}
	defer f.Close()
	// FindUUIDBeforeUserTurn(N) returns the parent of the Nth user
	// prompt — i.e. the last entry of turn (N-1). With N = lastKeptTurn+1,
	// that's the end of turn lastKeptTurn. Inclusive slice keeps
	// everything through that entry.
	return FindUUIDBeforeUserTurn(f, lastKeptTurn+1)
}

// syntheticUserEntryFlags names the top-level boolean fields Claude
// sets on a `type:"user"` JSONL entry that's NOT a user-typed prompt.
//
//   - isMeta — Skill bodies, settings injections, other system-side
//     user-role attachments (createUserMessage({content, isMeta:true})
//     at claude-code-source-code/src/utils/messages.ts:502-507).
//   - isReplay — the CLI's wrapper for queued task-notification XML
//     when a backgrounded subagent completes during a concurrent
//     foreground tool_result (see parse_user_replay.go and
//     docs/references/claude-wire.md §Synthetic-XML delivery channel).
//   - isCompactSummary — written after /compact or auto-compact;
//     carries the compacted summary as a user-role string.
//   - isVisibleInTranscriptOnly — UI-only injection, never user input;
//     pairs with isCompactSummary on auto-compact rows.
//
// Drift surface — keep synced with Claude Code's
// `selectableUserMessagesFilter` at
// claude-code-source-code/src/components/MessageSelector.tsx:767-792.
// That filter rejects synthetic entries along three axes; this slice
// (boolean flags) is one. The other two are the injected-XML wrappers
// (InjectedUserContentWrappers, below) and the synthetic-content
// sentinels (syntheticUserContentMessages / isSyntheticUserContent,
// below) — keep all three in mind when reconciling against upstream.
var syntheticUserEntryFlags = []string{
	"isMeta",
	"isReplay",
	"isCompactSummary",
	"isVisibleInTranscriptOnly",
}

// Claude's synthetic user-content sentinels. Claude writes each as a
// user-role entry whose `message.content` is an ARRAY whose first block
// is `{type:"text", text:<sentinel>}` — e.g. an interrupted turn yields
// `[Request interrupted by user]` via createUserInterruptionMessage
// (claude-code-source-code/src/utils/messages.ts:545-560). These are
// not user-typed prompts, and Claude's own message selector skips them
// via isSyntheticMessage. AO's ordinal walk must skip the identical set
// or it counts one phantom prompt per prior interrupt/cancel/reject and
// slices the resumed session a full turn too far back. The strings are
// verbatim copies of the upstream constants — one character of drift
// silently disables the filter.
//
// Drift surface — keep synced with Claude Code's SYNTHETIC_MESSAGES set
// and isSyntheticMessage at
// claude-code-source-code/src/utils/messages.ts:302-319 (the underlying
// constants are defined at :207-240). REJECT_MESSAGE_WITH_REASON_PREFIX
// and the SUBAGENT_* variants are deliberately NOT in upstream's set, so
// they are deliberately absent here too.
const (
	claudeInterruptMessage           = "[Request interrupted by user]"
	claudeInterruptMessageForToolUse = "[Request interrupted by user for tool use]"
	claudeCancelMessage              = "The user doesn't want to take this action right now. STOP what you are doing and wait for the user to tell you how to proceed."
	claudeRejectMessage              = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."
	claudeNoResponseRequested        = "No response requested."
)

var syntheticUserContentMessages = map[string]struct{}{
	claudeInterruptMessage:           {},
	claudeInterruptMessageForToolUse: {},
	claudeCancelMessage:              {},
	claudeRejectMessage:              {},
	claudeNoResponseRequested:        {},
}

// InjectedUserContentWrapper is one balanced XML wrapper Claude injects
// into user-role message content for non-user-authored payloads.
type InjectedUserContentWrapper struct{ Open, Close string }

// InjectedUserContentWrappers is the CANONICAL set of balanced XML
// wrappers Claude injects into user-role message content for
// non-user-authored payloads. Content that contains BOTH the open and
// close tag of any entry is a CLI injection, not a real prompt. The
// open-half of an attribute-bearing wrapper (`task-notification`,
// `agent-message`) is the prefix WITHOUT the closing `>` so both the
// bare shape and any attribute-bearing variant land; fixed-shape
// wrappers use the full open tag.
//
// Requiring BOTH halves is the load-bearing anti-false-positive
// guard: a real user typing `<system-reminder>` in chat (or quoting
// example XML in a prompt) won't trigger because the matching
// `</system-reminder>` won't be present in their text. The
// suppression we want catches Claude's own wrapped payloads, which
// always emit balanced.
//
// This is the ONE definition. `parse_user_replay.go`'s live-wire
// suppression (`isClaudeInjectedReplayContent`) and this package's
// fork-point user-turn detection (`hasInjectedUserContentTag`) both
// range over it, so the entry set can never drift between the two
// paths again — an uncatalogued `agent-message` drifting across a
// silent copy is exactly what shipped subagent reports as top-level
// `user:wire` bubbles (incident 2026-07).
//
// Drift surface — keep synced with upstream. Confirmed against the
// installed CLI binary (2.1.202); the local source copy lags:
//   - <task-notification>      claude-code-source-code/src/tasks/LocalShellTask/LocalShellTask.tsx:160-165
//   - <agent-message from=…>   2.1.202 binary — a subagent's final report injected into the parent as a `queued_command` attachment; NOT present in the local source copy
//   - <system-reminder>        claude-code-source-code/src/utils/messages.ts (pervasive wrapper)
//   - <bash-input/-stdout/-stderr>  claude-code-source-code/src/services/processBashCommand.tsx
//   - <local-command-stdout>   claude-code-source-code/src/services/processSlashCommand.tsx
var InjectedUserContentWrappers = []InjectedUserContentWrapper{
	{"<task-notification", "</task-notification>"},
	{"<agent-message", "</agent-message>"},
	{"<system-reminder>", "</system-reminder>"},
	{"<bash-input>", "</bash-input>"},
	{"<bash-stdout>", "</bash-stdout>"},
	{"<bash-stderr>", "</bash-stderr>"},
	{"<local-command-stdout>", "</local-command-stdout>"},
}

// isRealUserPrompt reports whether a JSONL entry represents a user-typed
// prompt rather than a tool-result echo, sidechain message, or
// CLI-injected synthetic user-role entry (Skill body, compact
// summary, replayed task notification, etc.).
//
// A real prompt:
//   - has type == "user"
//   - is not a sidechain (subagent transcript)
//   - has NO synthetic-injection flag set (isMeta / isReplay /
//     isCompactSummary / isVisibleInTranscriptOnly)
//   - has message.role == "user"
//   - has message.content that is either a string (not wrapped in any
//     Claude-injected XML envelope) OR an array that is not a synthetic
//     sentinel (interrupt / cancel / reject / no-response) and has at
//     least one non-tool_result block whose text (if any) isn't wrapped XML
//
// Tool-result echoes have type == "user" but their content is an array
// composed entirely of tool_result blocks. Synthetic sentinels (e.g.
// `[Request interrupted by user]`) are array entries whose first text
// block is one of Claude's fixed strings; see isSyntheticUserContent.
func isRealUserPrompt(entry map[string]any) bool {
	if t, _ := entry["type"].(string); t != "user" {
		return false
	}
	if v, _ := entry["isSidechain"].(bool); v {
		return false
	}
	for _, flag := range syntheticUserEntryFlags {
		if v, _ := entry[flag].(bool); v {
			return false
		}
	}
	msg, ok := entry["message"].(map[string]any)
	if !ok {
		return false
	}
	if role, _ := msg["role"].(string); role != "user" {
		return false
	}
	switch content := msg["content"].(type) {
	case string:
		if hasInjectedUserContentTag(content) {
			return false
		}
		return true
	case []any:
		if isSyntheticUserContent(content) {
			return false
		}
		hasNonToolResult := false
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				// Non-object content block — treat as user-authored to be
				// safe (very rare; e.g. a bare string in array form).
				hasNonToolResult = true
				continue
			}
			t, _ := b["type"].(string)
			if t == "tool_result" {
				continue
			}
			hasNonToolResult = true
			// Reject when ANY text block carries a CLI-injected XML
			// wrapper — these are non-user-authored payloads even when
			// they ride an `isMeta:false` entry (rare but observed for
			// command-output / system-reminder one-shots).
			if t == "text" {
				if text, _ := b["text"].(string); hasInjectedUserContentTag(text) {
					return false
				}
			}
		}
		return hasNonToolResult
	default:
		// Unknown content shape. Conservative default: treat as a real
		// prompt so we don't silently swallow user input.
		return true
	}
}

// isSyntheticUserContent mirrors Claude's isSyntheticMessage for the
// array-content case: a user entry whose FIRST content block is a text
// block carrying one of Claude's synthetic sentinels (interrupt,
// cancel, reject, no-response). Claude excludes these from selectable
// user messages, so the fork/revert ordinal walk must skip them too or
// it counts one phantom "prompt" per prior interrupt and slices the
// session too far back.
//
// Only the FIRST block is inspected and only array-shaped content is
// matched — both deliberate, to stay byte-for-byte with upstream
// (claude-code-source-code/src/utils/messages.ts:310-319). A user who
// literally types `[Request interrupted by user]` as a plain string is
// therefore still a real prompt, exactly as Claude treats it.
func isSyntheticUserContent(content []any) bool {
	if len(content) == 0 {
		return false
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return false
	}
	if t, _ := first["type"].(string); t != "text" {
		return false
	}
	text, _ := first["text"].(string)
	_, synthetic := syntheticUserContentMessages[text]
	return synthetic
}

// hasInjectedUserContentTag reports whether s contains any of the
// CLI-injected XML wrappers enumerated in InjectedUserContentWrappers.
// Requires BOTH the open and close tag of an entry to be present
// (mirrors parse_user_replay.go::isClaudeInjectedReplayContent's
// balanced-matching contract) so a user typing a single tag-shaped
// token in chat doesn't false-positive.
func hasInjectedUserContentTag(s string) bool {
	if s == "" {
		return false
	}
	for _, w := range InjectedUserContentWrappers {
		if strings.Contains(s, w.Open) && strings.Contains(s, w.Close) {
			return true
		}
	}
	return false
}
