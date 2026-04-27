package sessionfork

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrUserTurnOutOfRange is returned when userTurnIndex points past the
// last real user prompt in the source transcript.
var ErrUserTurnOutOfRange = errors.New("sessionfork: user turn index out of range")

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

// isRealUserPrompt reports whether a JSONL entry represents a user-typed
// prompt rather than a tool-result echo or sidechain message.
//
// A real prompt:
//   - has type == "user"
//   - is not a sidechain (subagent transcript)
//   - has message.role == "user"
//   - has message.content that is either a string OR an array with at
//     least one non-tool_result block
//
// Tool-result echoes have type == "user" but their content is an array
// composed entirely of tool_result blocks.
func isRealUserPrompt(entry map[string]any) bool {
	if t, _ := entry["type"].(string); t != "user" {
		return false
	}
	if v, _ := entry["isSidechain"].(bool); v {
		return false
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
		return true
	case []any:
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				// Non-object content block — treat as user-authored to be
				// safe (very rare; e.g. a bare string in array form).
				return true
			}
			if t, _ := b["type"].(string); t != "tool_result" {
				return true
			}
		}
		return false
	default:
		// Unknown content shape. Conservative default: treat as a real
		// prompt so we don't silently swallow user input.
		return true
	}
}
