package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type timelineNotificationMeta struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Details string `json:"details"`
}

// handlePlanUpdate routes EventPlanUpdate (Claude TodoWrite reroute,
// Codex turn/plan/updated) directly to the frontend without
// persisting a timeline notification row. The live plan is session
// state owned by ThreadPane; SQLite is not its source of truth. See
// PlanUpdateEvent / LivePlanPanel.svelte for the rendering side.
//
// An empty plan is dropped: the parser already skips empty TodoWrite
// inputs, but defensive normalisation here means a malformed wire
// payload also can't render an empty panel.
func (r *Router) handlePlanUpdate(evt provider.ProviderEvent) error {
	steps := decodePlanSteps(evt.Meta)
	if len(steps) == 0 {
		return nil
	}
	r.emit("provider:plan_update", PlanUpdateEvent{
		ThreadID: evt.ThreadID,
		Steps:    steps,
	})
	return nil
}

// decodePlanSteps reads the wire-shaped {plan: [{step, status}]}
// payload off EventPlanUpdate.Meta. Empty steps and unmarshal failures
// produce nil so callers can `if len(...) == 0 { drop }` uniformly.
//
// Bounds the wire input on two axes so a misbehaving provider can't
// stuff the per-event WS payload (and the per-pane snapshot held in
// frontend memory): step count is capped at maxPlanSteps; per-step
// text is truncated via truncateRunes. Same shape as the
// addHookNotificationMeta path that bounds hook entries / runes.
func decodePlanSteps(raw json.RawMessage) []PlanStep {
	if len(raw) == 0 {
		return nil
	}
	var decoded struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return nil
	}
	steps := make([]PlanStep, 0, min(len(decoded.Plan), maxPlanSteps))
	for _, item := range decoded.Plan {
		if len(steps) >= maxPlanSteps {
			break
		}
		step := truncateRunes(strings.TrimSpace(item.Step), maxPlanStepRunes)
		if step == "" {
			continue
		}
		steps = append(steps, PlanStep{
			Step:   step,
			Status: strings.TrimSpace(item.Status),
		})
	}
	return steps
}

func (r *Router) handleTimelineNotification(evt provider.ProviderEvent) error {
	meta := decodeTimelineNotificationMeta(evt.Meta)
	summary := strings.TrimSpace(evt.Content)
	if summary == "" {
		summary = strings.TrimSpace(meta.Title)
	}
	if summary == "" {
		summary = "Provider notification"
	}
	return r.persistTimelineNotification(evt, meta.Kind, summary)
}

func (r *Router) persistTimelineNotification(evt provider.ProviderEvent, notificationKind, summary string) error {
	notificationKind = strings.TrimSpace(notificationKind)
	if notificationKind == "" {
		notificationKind = "notification"
	}
	turnIndex := r.timelineNotificationTurnIndex(evt.ThreadID)
	itemID := eventItemID(evt)
	if strings.TrimSpace(itemID) == "" {
		itemID = nextTimelineNotificationID(turnIndex, r.nextNotificationSequence(evt.ThreadID, turnIndex))
	}
	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindNotification,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   strings.TrimSpace(summary),
		ParentID:  eventParentID(evt),
		ToolName:  notificationKind,
		Meta:      sanitizedTimelineNotificationMeta(notificationKind, summary, evt.Meta),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID); err == nil && found {
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
	} else if err != nil {
		return fmt.Errorf("timeline notification existing lookup %s: %w", item.ID, err)
	}
	return r.persistItem(item, nil)
}

func (r *Router) timelineNotificationTurnIndex(threadID string) int {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err == nil {
		return turnIndex
	}
	return 0
}

func decodeTimelineNotificationMeta(raw json.RawMessage) timelineNotificationMeta {
	if len(raw) == 0 {
		return timelineNotificationMeta{}
	}
	var decoded timelineNotificationMeta
	_ = json.Unmarshal(raw, &decoded)
	return decoded
}

func (r *Router) nextNotificationSequence(threadID string, turnIndex int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := scopeCounterKey(threadID, turnIndex, "notification")
	seq := r.notificationSeqByScope[key]
	r.notificationSeqByScope[key] = seq + 1
	return seq
}

func nextTimelineNotificationID(turnIndex, seq int) string {
	return fmt.Sprintf("notification:%d:%d", turnIndex, seq)
}

func sanitizedTimelineNotificationMeta(notificationKind, summary string, raw json.RawMessage) string {
	notificationKind = strings.TrimSpace(notificationKind)
	if notificationKind == "" {
		notificationKind = "notification"
	}
	meta := map[string]any{
		"kind":  notificationKind,
		"title": strings.TrimSpace(summary),
	}

	switch notificationKind {
	case "hook":
		addHookNotificationMeta(meta, raw)
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return `{}`
	}
	return string(encoded)
}

func addHookNotificationMeta(meta map[string]any, raw json.RawMessage) {
	var decoded struct {
		Run struct {
			EventName string `json:"eventName"`
			Status    string `json:"status"`
			Entries   []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"entries"`
		} `json:"run"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &decoded) != nil {
		return
	}
	run := map[string]any{}
	if eventName := strings.TrimSpace(decoded.Run.EventName); eventName != "" {
		run["eventName"] = eventName
	}
	if status := strings.TrimSpace(decoded.Run.Status); status != "" {
		run["status"] = status
	}
	entries := make([]map[string]string, 0, min(len(decoded.Run.Entries), maxTimelineHookEntries))
	for _, entry := range decoded.Run.Entries {
		if len(entries) >= maxTimelineHookEntries {
			break
		}
		text := truncateRunes(strings.TrimSpace(entry.Text), maxTimelineHookEntryRunes)
		if text == "" {
			continue
		}
		entries = append(entries, map[string]string{
			"kind": strings.TrimSpace(entry.Kind),
			"text": text,
		})
	}
	if len(entries) > 0 {
		run["entries"] = entries
	}
	if len(run) > 0 {
		meta["run"] = run
	}
}

const (
	maxTimelineHookEntries    = 8
	maxTimelineHookEntryRunes = 300
	// Bounds on EventPlanUpdate inputs. The panel itself truncates the
	// rendered list to 5 with a Show-more reveal; these caps are the
	// outer safety net so a provider that ships a multi-MB plan can't
	// blow up the WS payload or the pane snapshot.
	maxPlanSteps     = 256
	maxPlanStepRunes = 300
)

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}
