package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Live subagent progress (docs/specs/agent-visibility.md).
//
// A running subagent's counters — tool count, token spend, elapsed,
// current activity line — are live UI state, never history: Claude
// emits a `task_progress` tick after every tool round and Codex a
// `thread/tokenUsage/updated` per child turn, and persisting each tick
// would write a row per round for work the provider already records.
// Triage therefore holds the LATEST tick per launch in memory, fans it
// out on `provider:subagent_progress`, and persists only the FINAL
// numbers onto the launch row when that launch reaches its terminal
// (TakeSubagentProgress is the one consumer; see
// persistSubagentFinalProgress).
//
// This is coordination state in the same class as pendingApprovals and
// pendingWakeupByThread, not a read model: nothing is derived from it,
// it is replaced wholesale by every tick, and it dies with the session
// (cleanupThread / MarkThreadActive sweep the thread's entries, since a
// replacement process never carries the previous process's tasks).

const (
	// subagentProgressEventName is the frontend channel for live ticks.
	subagentProgressEventName = "subagent_progress"
	// subagentProgressMetaKey is the launch-row meta key the final
	// numbers persist under (a provider.SubagentProgressMeta object).
	subagentProgressMetaKey = "subagentProgress"
	// subagentBackgroundedAtMetaKey is the launch-row meta key stamped with
	// the epoch-ms timestamp at which a FOREGROUND agent was moved to the
	// background mid-flight — the point its sidechain streaming stopped.
	subagentBackgroundedAtMetaKey = "subagentBackgroundedAt"
)

// SubagentProgressEvent is the `provider:subagent_progress` payload.
type SubagentProgressEvent struct {
	ThreadID string `json:"threadId"`
	// ItemID is the launch tool_use the progress belongs to.
	ItemID string `json:"itemId"`
	// ParentID is the launch's own parent tool_use ("" at top level), so a
	// consumer can attribute a nested agent's tick without a row lookup.
	ParentID  string                        `json:"parentId,omitempty"`
	Progress  provider.SubagentProgressMeta `json:"progress"`
	UpdatedAt int64                         `json:"updatedAt"`
}

func subagentProgressKey(threadID, itemID string) string {
	return threadID + ":" + itemID
}

// handleSubagentProgress merges a tick into the launch's live entry and
// fans the merged state out. Merge, not replace: every field is
// cumulative and a provider that cannot report one leaves it zero, so
// replacing would zero Claude's tool count on a tick that carried only
// tokens.
func (r *Router) handleSubagentProgress(evt provider.ProviderEvent) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return nil
	}
	var tick provider.SubagentProgressMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &tick); err != nil {
			return fmt.Errorf("triage: decode subagent progress meta for %s/%s: %w", evt.ThreadID, itemID, err)
		}
	}
	key := subagentProgressKey(evt.ThreadID, itemID)
	r.mu.Lock()
	merged := mergeSubagentProgress(r.subagentProgress[key], tick)
	if len(r.subagentProgress) >= subagentProgressCap {
		if _, present := r.subagentProgress[key]; !present {
			// Bounded like the parser's task map: a runaway session must
			// not grow router memory without limit. Dropping the oldest
			// would need ordering state for a case that never happens in
			// practice (the cap is far above any real agent fan-out), so
			// the whole map resets and live cards re-fill on the next tick.
			log.Printf("triage: subagent progress map hit cap %d; resetting", subagentProgressCap)
			r.subagentProgress = make(map[string]provider.SubagentProgressMeta)
		}
	}
	r.subagentProgress[key] = merged
	r.mu.Unlock()

	r.emit("provider:"+subagentProgressEventName, SubagentProgressEvent{
		ThreadID:  evt.ThreadID,
		ItemID:    itemID,
		ParentID:  eventParentID(evt),
		Progress:  merged,
		UpdatedAt: eventTimestampMillis(evt),
	})
	return nil
}

const subagentProgressCap = 4096

func mergeSubagentProgress(base, tick provider.SubagentProgressMeta) provider.SubagentProgressMeta {
	out := base
	if tick.TaskID != "" {
		out.TaskID = tick.TaskID
	}
	if tick.ToolUses > out.ToolUses {
		out.ToolUses = tick.ToolUses
	}
	// Latest wins, not max. Codex's figure is cumulative and can only
	// grow, so the two rules agree there. CLAUDE's cannot: its
	// task_progress number is LATEST input plus all output (see
	// provider.SubagentProgressMeta), so an agent that compacts its own
	// context legitimately reports a SMALLER one afterwards, and a max
	// merge would pin the card to the pre-compaction peak for the rest of
	// the run while Claude's own UI moved on. Latest also lets a
	// terminal's authoritative usage correct an earlier, larger tick.
	// Zero still means "this provider did not report it", which is what
	// the guard is for — the other counters here are genuinely monotonic
	// and keep their max.
	if tick.TotalTokens > 0 {
		out.TotalTokens = tick.TotalTokens
	}
	if tick.DurationMs > out.DurationMs {
		out.DurationMs = tick.DurationMs
	}
	if tick.Activity != "" {
		out.Activity = tick.Activity
	}
	if tick.LastToolName != "" {
		out.LastToolName = tick.LastToolName
	}
	if tick.AgentType != "" {
		out.AgentType = tick.AgentType
	}
	if tick.Summary != "" {
		out.Summary = tick.Summary
	}
	return out
}

// TakeSubagentProgress returns and clears the launch's latest live
// progress. Single consumer: the terminal path that persists the final
// numbers onto the launch row (persistSubagentFinalProgress). ok=false
// when no tick was ever seen for the launch.
func (r *Router) TakeSubagentProgress(threadID, itemID string) (provider.SubagentProgressMeta, bool) {
	if r == nil {
		return provider.SubagentProgressMeta{}, false
	}
	key := subagentProgressKey(threadID, itemID)
	r.mu.Lock()
	defer r.mu.Unlock()
	progress, ok := r.subagentProgress[key]
	if ok {
		delete(r.subagentProgress, key)
	}
	return progress, ok
}

// PeekSubagentProgress is the read-only companion of TakeSubagentProgress.
func (r *Router) PeekSubagentProgress(threadID, itemID string) (provider.SubagentProgressMeta, bool) {
	if r == nil {
		return provider.SubagentProgressMeta{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	progress, ok := r.subagentProgress[subagentProgressKey(threadID, itemID)]
	return progress, ok
}

// persistSubagentFinalProgress folds the launch's last live tick, plus
// any authoritative final counters the terminal itself carried (Claude's
// task_notification `usage`), into the launch row's meta under
// subagentProgressMetaKey and emits the patch. Called by every terminal
// path that settles a launch row: the inline tool completion, the
// background completion sibling, and the Codex child terminal. A launch
// with no tick and no final counters is left untouched.
//
// Idempotent and order-free across the terminal paths: the persisted
// meta's own subagentProgress is the merge base, so a task_updated
// terminal (live tick only) followed by a task_notification (authoritative
// usage) lands the same final numbers as the reverse order.
func (r *Router) persistSubagentFinalProgress(launch store.Item, final provider.SubagentProgressMeta) error {
	base := persistedSubagentProgress(launch.Meta)
	live, _ := r.TakeSubagentProgress(launch.ThreadID, launch.ID)
	merged := mergeSubagentProgress(mergeSubagentProgress(base, live), final)
	if merged == (provider.SubagentProgressMeta{}) {
		return nil
	}
	merged.Activity = ""
	encoded, err := json.Marshal(map[string]any{subagentProgressMetaKey: merged})
	if err != nil {
		return fmt.Errorf("triage: marshal final subagent progress for %s: %w", launch.ID, err)
	}
	meta := mergeItemMetaJSON(launch.Meta, encoded)
	return r.persistItemFieldsAndPatch(launch.ThreadID, launch.ID, launch.Kind, store.ItemPartialUpdate{Meta: &meta})
}

// persistedSubagentProgress reads the final numbers already on a launch
// row's meta (zero value when absent or unreadable).
func persistedSubagentProgress(meta string) provider.SubagentProgressMeta {
	if strings.TrimSpace(meta) == "" {
		return provider.SubagentProgressMeta{}
	}
	var decoded struct {
		Progress provider.SubagentProgressMeta `json:"subagentProgress"`
	}
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		return provider.SubagentProgressMeta{}
	}
	return decoded.Progress
}

// dropSubagentProgressForThread sweeps every live entry of a thread. The
// ticks belong to the provider PROCESS: a replacement session never
// carries them forward, and a dead process will never deliver the
// terminal that would otherwise consume them.
func (r *Router) dropSubagentProgressForThread(threadID string) {
	prefix := threadID + ":"
	for key := range r.subagentProgress {
		if strings.HasPrefix(key, prefix) {
			delete(r.subagentProgress, key)
		}
	}
}

// handleSubagentBackgrounded stamps the launch row as backgrounded
// mid-flight: IsBackground flips (the async-ack tool_result that follows
// also does this, but the patch is the earlier and the only typed
// statement) and the cut timestamp lands in meta as the durable fact that
// this launch changed from foreground to detached execution.
func (r *Router) handleSubagentBackgrounded(evt provider.ProviderEvent) error {
	itemID := strings.TrimSpace(evt.ItemID)
	if itemID == "" {
		return nil
	}
	launch, found, err := r.store.GetThreadItem(evt.ThreadID, itemID)
	if err != nil {
		return fmt.Errorf("triage: subagent backgrounded lookup %s: %w", itemID, err)
	}
	if !found || launch.Kind != itemKindToolCall {
		log.Printf("triage: subagent_backgrounded for unknown launch thread=%s item=%s", evt.ThreadID, itemID)
		return nil
	}
	var existing map[string]any
	if strings.TrimSpace(launch.Meta) != "" {
		if err := json.Unmarshal([]byte(launch.Meta), &existing); err == nil {
			if _, stamped := existing[subagentBackgroundedAtMetaKey]; stamped {
				return nil
			}
		}
	}
	now := eventTimestampMillis(evt)
	encoded, err := json.Marshal(map[string]any{subagentBackgroundedAtMetaKey: now})
	if err != nil {
		return fmt.Errorf("triage: marshal subagent backgrounded stamp for %s: %w", itemID, err)
	}
	launch.Meta = mergeItemMetaJSON(launch.Meta, encoded)
	launch.IsBackground = true
	launch.UpdatedAt = now
	return r.persistItem(launch, nil)
}

// handleBackgroundTasksChanged forwards the level set. The channel's
// existing consumers (the activity-rail background controller, the
// workspace-change lock) refresh their tray listing on any frame; the
// set rides along so a consumer that wants reconnect-safe membership can
// swap to it without a round trip.
func (r *Router) handleBackgroundTasksChanged(evt provider.ProviderEvent) error {
	var meta provider.BackgroundTasksChangedMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("triage: decode background tasks changed meta for %s: %w", evt.ThreadID, err)
		}
	}
	if meta.Tasks == nil {
		meta.Tasks = []provider.BackgroundTaskRef{}
	}
	r.emit(codexBackgroundTasksChangedEventName, BackgroundTasksChangedEvent{ThreadID: evt.ThreadID, Tasks: meta.Tasks})
	return nil
}
