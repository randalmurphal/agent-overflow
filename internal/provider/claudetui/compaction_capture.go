package claudetui

import (
	"encoding/json"

	"agent-overflow/internal/provider"
)

// compaction_capture.go isolates the compaction-summarizer capture path that the
// turn driver hands off to: the reconstructor arms on PreCompact, claims the
// summarizer's classAgent request, streams its reasoning live under the reserved
// compaction scope, and finalizes the boundary on PostCompact. The capture
// branches themselves live on agentRequest.onSSE/end (turndriver.go); this file
// owns the standalone state + helpers, mirroring triage's compaction_reasoning.go.

// compactionCapture holds the compaction summarizer's committed summary between
// its /v1/messages response and the finalizing PostCompact hook — the fallback
// when the hook carries no compact_summary. The summarizer's reasoning is NOT
// held here: it streams live as a compaction_reasoning row (agentRequest.onSSE),
// so only the summary needs reconstructing for the boundary.
type compactionCapture struct {
	summary string
}

// armCompaction marks that a compaction attempt began (PreCompact hook). The
// next classAgent request is its summarizer and routes to the capture path.
// Resets any pending capture so an aborted-then-retried attempt discards the
// interrupted summarizer's partial thinking (the second PreCompact supersedes
// the first). Caller holds the session's recMu.
func (r *reconstructor) armCompaction() {
	r.compacting = true
	r.pendingCompaction = nil
	if r.debug != nil {
		r.debug(decisionLog{Event: "compaction_arm", Route: "main"})
	}
}

// beginCompactionCapture starts capture reconstruction for the compaction
// summarizer when armed, and returns nil otherwise (so the gateway falls through
// to normal subagent/main routing). It disarms immediately — the first armed
// classAgent request IS the summarizer — and returns an agentRequest whose
// onSSE/end fold the summarizer's output into pendingCompaction without emitting
// any turn envelope. Caller holds the session's recMu.
func (r *reconstructor) beginCompactionCapture() *agentRequest {
	if !r.compacting {
		return nil
	}
	r.compacting = false
	if r.debug != nil {
		r.debug(decisionLog{Event: "compaction_capture", Route: "main"})
	}
	return &agentRequest{r: r, asm: newMessageAssembler(), capture: true}
}

// finalizeCompaction emits the compact_boundary carrying the committed summary,
// then clears the pending capture. summary precedence: the PostCompact hook's
// authoritative compact_summary, falling back to the captured SSE text if the
// hook carried none. The summarizer's reasoning is NOT on the boundary — it
// streamed live as a compaction_reasoning row (onSSE). Emits the boundary even
// with no summary so the Compacted divider still lands. Caller holds the
// session's recMu.
func (r *reconstructor) finalizeCompaction(trigger, summary string) {
	if summary == "" && r.pendingCompaction != nil {
		summary = r.pendingCompaction.summary
	}
	r.pendingCompaction = nil
	r.compacting = false
	if r.debug != nil {
		r.debug(decisionLog{Event: "compaction_finalize", Route: "main", Stop: trigger})
	}
	r.emit(compactBoundaryLine(trigger, summary))
}

// compactionReasoningPassthrough returns the stream_event line to forward for a
// summarizer thinking-block frame (content_block_start / _delta / _stop whose
// block is a thinking block), tagged with the reserved compaction-reasoning
// scope so the shared parser emits EventContentBlockStart / EventThinking /
// EventContentBlockStop carrying that scope. ok is false for message-level frames
// and the summary (text) block, which stay suppressed so the summarizer never
// surfaces as a turn. asm must have already consumed `sse` so the block type is
// known by index (content_block_start records it before its deltas arrive).
//
// The {type,index} re-parse here is deliberate: it runs ONLY on the cold
// compaction-capture path, so it stays local rather than widening the hot-path
// consume signature (which every turn's every frame pays) just to thread the
// already-decoded envelope through.
func compactionReasoningPassthrough(asm *messageAssembler, sse json.RawMessage) (json.RawMessage, bool) {
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	if json.Unmarshal(sse, &ev) != nil {
		return nil, false
	}
	switch ev.Type {
	case "content_block_start", "content_block_delta", "content_block_stop":
		if asm.blockType(ev.Index) != "thinking" {
			return nil, false
		}
		return streamEventLine(sse, provider.CompactionReasoningScope), true
	default:
		return nil, false
	}
}
