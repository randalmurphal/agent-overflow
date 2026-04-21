package highlight

// Content kinds that flow through RenderForKind. These mirror the values
// stored in items.kind / payloads.kind; the package depends on them as plain
// strings to avoid importing internal/store.
const (
	KindAssistantText  = "assistant_text"
	KindProposedPlan   = "proposed_plan"
	KindThinking       = "thinking"
	KindCommandOutput  = "command_output"
	KindToolResult     = "tool_result"
	KindDiff           = "diff"
	KindUserText       = "user_text"
	KindToolCall       = "tool_call"
	KindToolCompletion = "tool_completion"
	KindError          = "error"
	KindCompaction     = "compaction"
)

// renderForKind picks the right renderer for a timeline item or payload kind.
// Kinds that should not be server-rendered (diffs, plain user text, structured
// tool events, anything unrecognized) return the empty string; callers are
// expected to skip the write in that case.
//
// Empty content always returns empty — no point running either renderer, and
// the frontend already treats empty strings as "nothing to paint".
func (r *Renderer) renderForKind(kind, content string) string {
	if content == "" {
		return ""
	}
	switch kind {
	case KindAssistantText, KindProposedPlan:
		return r.renderMarkdown(content)
	case KindThinking, KindCommandOutput, KindToolResult:
		return r.renderANSI(content)
	default:
		return ""
	}
}
