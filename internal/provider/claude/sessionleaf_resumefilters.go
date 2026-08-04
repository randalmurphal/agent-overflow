package claude

import (
	"encoding/json"
	"strings"
)

// Mirror of the CLI's resume deserialization filters, applied to the
// active branch so AO never picks a `--resume-session-at` cursor the
// CLI is about to discard. On resume the CLI loads the branch chain and
// runs deserializeMessages (conversationRecovery.ts) BEFORE validating
// the cursor, in this order:
//
//  1. filterUnresolvedToolUses (messages.ts): drop every assistant
//     message that has ≥1 client `tool_use` block and ALL of them lack
//     a matching `tool_result` anywhere in the chain. A host crash
//     mid-tool-execution leaves exactly this row as the transcript
//     leaf (incident 2026-08-03).
//  2. filterOrphanedThinkingOnlyMessages (messages.ts): drop every
//     assistant message whose blocks are all thinking/redacted_thinking
//     unless another REMAINING assistant message with the same
//     message.id carries non-thinking content (streaming persists one
//     row per content block, so the thinking row's survival hangs on
//     its sibling's).
//  3. filterWhitespaceOnlyAssistantMessages (messages.ts): drop every
//     assistant message whose blocks are all whitespace-only text. If
//     ANY row was dropped here, the CLI additionally merges every
//     adjacent user-row run into its first row (mergeUserMessages) —
//     which can erase a later user row's uuid.
//
// The mirror is deliberately CONSERVATIVE: the CLI's chain load can
// recover parallel-tool sibling rows the linear parentUuid walk
// orphans (recoverOrphanedParallelToolResults, sessionStorage.ts),
// which only ever ADDS resolutions and survivors. Every uuid this
// mirror blesses therefore survives the CLI's larger list; a uuid it
// rejects at worst falls back to a shallower — still valid — cursor,
// or to no cursor at all (claude's own default-leaf semantics). The
// one unmodeled corner is a recovered tool_result row landing directly
// before a blessed user row while a whitespace merge is active; that
// requires a legacy parallel-tool DAG AND a whitespace row AND
// tail-adjacency at once, and its failure mode is the loud pre-init
// resume error, not corruption.
//
// Source citations (claude-code 2.1.219; local mirror
// ~/repos/claude-code-source-code):
//   - cli/print.ts — resumeSessionAt findIndex over the deserialized
//     list; miss emits "No message found with message.uuid of: ..."
//     and exits pre-init.
//   - utils/conversationRecovery.ts deserializeMessagesWithInterruptDetection
//     — the filter order above.
//   - utils/messages.ts filterUnresolvedToolUses,
//     filterOrphanedThinkingOnlyMessages,
//     filterWhitespaceOnlyAssistantMessages, mergeUserMessages.

// claudeRowContentFlags is the per-row digest of message content the
// filter mirror consumes. Parsed once at ingest; the raw bytes alias
// the scanner buffer and are not retained.
type claudeRowContentFlags struct {
	// hasToolUse / toolUseIDs cover client `tool_use` blocks. IDs keep
	// their raw (trimmed) value; an empty id can never resolve, which
	// keeps the mirror conservative for malformed rows.
	hasToolUse bool
	toolUseIDs []string
	// toolResultIDs are the `tool_result` block tool_use_ids this row
	// resolves (the CLI collects them from user and assistant rows).
	toolResultIDs []string
	messageID     string
	// hasNonThinking: any block that is not thinking/redacted_thinking.
	hasNonThinking bool
	// allThinking: non-empty content, every block thinking-typed.
	allThinking bool
	// whitespaceOnly: non-empty content, every block a text block whose
	// text trims to "" (hasOnlyWhitespaceTextContent; an absent text
	// field counts as whitespace there too).
	whitespaceOnly bool
}

func parseClaudeRowContentFlags(message json.RawMessage) claudeRowContentFlags {
	var flags claudeRowContentFlags
	if len(message) == 0 {
		return flags
	}
	var msg struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &msg); err != nil {
		return flags
	}
	flags.messageID = strings.TrimSpace(msg.ID)
	var blocks []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
		Text      string `json:"text"`
	}
	// User rows can carry plain-string content; that shape has no
	// blocks to digest and user rows survive every filter regardless.
	if err := json.Unmarshal(msg.Content, &blocks); err != nil || len(blocks) == 0 {
		return flags
	}
	flags.allThinking = true
	flags.whitespaceOnly = true
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			flags.hasToolUse = true
			flags.toolUseIDs = append(flags.toolUseIDs, strings.TrimSpace(block.ID))
		case "tool_result":
			if id := strings.TrimSpace(block.ToolUseID); id != "" {
				flags.toolResultIDs = append(flags.toolResultIDs, id)
			}
		}
		if block.Type != "thinking" && block.Type != "redacted_thinking" {
			flags.allThinking = false
			flags.hasNonThinking = true
		}
		if block.Type != "text" || strings.TrimSpace(block.Text) != "" {
			flags.whitespaceOnly = false
		}
	}
	return flags
}

// resumeSurvivor is one chain row after the filter mirror. cursorSafe
// marks rows whose uuid is a valid `--resume-session-at` target:
// surviving user/assistant rows, minus user rows whose uuid a
// whitespace-triggered merge could erase. system/attachment/progress
// rows stay in the list — they break user-run adjacency for the merge
// rule — but are never cursors.
type resumeSurvivor struct {
	uuid       string
	rowType    string
	isMeta     bool
	cursorSafe bool
}

// applyClaudeResumeFilters runs the three-filter mirror over the active
// chain (root→tip order) and returns the surviving rows with cursor
// safety computed. See the file header for the semantics and citations.
func applyClaudeResumeFilters(chain []*claudeBranchRow) []resumeSurvivor {
	// Filter 1: unresolved client tool_uses. Resolution ids are
	// collected from the whole chain first, exactly like the CLI.
	resolved := make(map[string]struct{})
	for _, row := range chain {
		for _, id := range row.flags.toolResultIDs {
			resolved[id] = struct{}{}
		}
	}
	keep := make([]*claudeBranchRow, 0, len(chain))
	for _, row := range chain {
		if row.rowType == "assistant" && row.flags.hasToolUse {
			anyResolved := false
			for _, id := range row.flags.toolUseIDs {
				if _, ok := resolved[id]; ok && id != "" {
					anyResolved = true
					break
				}
			}
			if !anyResolved {
				continue
			}
		}
		keep = append(keep, row)
	}

	// Filter 2: orphaned thinking-only rows. The sibling lookup runs
	// over filter 1's survivors — a thinking row dies with the
	// tool_use sibling that was its only non-thinking companion.
	idsWithNonThinking := make(map[string]struct{})
	for _, row := range keep {
		if row.rowType == "assistant" && row.flags.hasNonThinking && row.flags.messageID != "" {
			idsWithNonThinking[row.flags.messageID] = struct{}{}
		}
	}
	keep2 := keep[:0]
	for _, row := range keep {
		if row.rowType == "assistant" && row.flags.allThinking {
			if _, ok := idsWithNonThinking[row.flags.messageID]; !ok || row.flags.messageID == "" {
				continue
			}
		}
		keep2 = append(keep2, row)
	}

	// Filter 3: whitespace-only rows.
	whitespaceRemoved := false
	keep3 := keep2[:0]
	for _, row := range keep2 {
		if row.rowType == "assistant" && row.flags.whitespaceOnly {
			whitespaceRemoved = true
			continue
		}
		keep3 = append(keep3, row)
	}

	survivors := make([]resumeSurvivor, len(keep3))
	for i, row := range keep3 {
		survivors[i] = resumeSurvivor{
			uuid:       row.uuid,
			rowType:    row.rowType,
			isMeta:     row.isMeta,
			cursorSafe: row.rowType == "user" || row.rowType == "assistant",
		}
	}
	if whitespaceRemoved {
		markMergedUserRowsUnsafe(survivors)
	}
	return survivors
}

// markMergedUserRowsUnsafe applies the merge rule that runs iff a
// whitespace-only row was removed: every adjacent run of user rows
// collapses into its first row via mergeUserMessages, which keeps the
// first row's uuid when that row is non-meta. Later rows in a run lose
// their uuid; a meta-headed run's surviving uuid depends on a CLI
// feature flag (HISTORY_SNIP) we cannot observe, so the whole run is
// conservatively unsafe.
func markMergedUserRowsUnsafe(survivors []resumeSurvivor) {
	for start := 0; start < len(survivors); {
		if survivors[start].rowType != "user" {
			start++
			continue
		}
		end := start + 1
		for end < len(survivors) && survivors[end].rowType == "user" {
			end++
		}
		if end-start > 1 {
			for i := start; i < end; i++ {
				survivors[i].cursorSafe = false
			}
			if !survivors[start].isMeta {
				survivors[start].cursorSafe = true
			}
		}
		start = end
	}
}
