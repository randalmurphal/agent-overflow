package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type timelineNotificationMeta struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Details string `json:"details"`
}

// handleTodoUpdate routes EventTodoUpdate (Claude TodoWrite reroute,
// Codex turn/plan/updated) directly to the frontend without
// persisting a timeline notification row. The live todo list is session
// state owned by triage for refresh/reconnect; SQLite is not its source of
// truth, and ThreadPane is only the visible projection. See TodoUpdateEvent
// / ActivityRailTodosBody.svelte for the rendering side.
//
// An empty list clears the backend snapshot without emitting. The
// frontend also treats empty todo_update events as a clear signal, but
// provider parsers usually suppress empty updates before they reach
// users; keeping triage's snapshot clear prevents refresh from
// resurrecting stale todo state.
func (r *Router) handleTodoUpdate(evt provider.ProviderEvent) error {
	steps := decodeTodoSteps(evt.Meta)
	if len(steps) == 0 {
		r.clearLiveTodoSnapshot(evt.ThreadID)
		return nil
	}
	r.setLiveTodoSnapshot(LiveTodoSnapshot{
		ThreadID:  evt.ThreadID,
		Steps:     steps,
		UpdatedAt: eventTimestampMillis(evt),
	})
	r.emit("provider:todo_update", TodoUpdateEvent{
		ThreadID: evt.ThreadID,
		Steps:    steps,
	})
	return nil
}

func (r *Router) setLiveTodoSnapshot(snapshot LiveTodoSnapshot) {
	if r == nil || snapshot.ThreadID == "" || len(snapshot.Steps) == 0 {
		return
	}
	r.mu.Lock()
	r.latestTodoByThread[snapshot.ThreadID] = snapshot
	r.mu.Unlock()
}

func (r *Router) clearLiveTodoSnapshot(threadID string) {
	if r == nil || threadID == "" {
		return
	}
	r.mu.Lock()
	delete(r.latestTodoByThread, threadID)
	r.mu.Unlock()
}

// LiveTodoSnapshot returns the latest per-thread live todo/update_plan
// snapshot without handing callers the map-owned steps slice.
func (r *Router) LiveTodoSnapshot(threadID string) (LiveTodoSnapshot, bool) {
	if r == nil || threadID == "" {
		return LiveTodoSnapshot{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot := r.liveTodoSnapshotLocked(threadID, time.Now().UnixMilli()); snapshot != nil {
		return *snapshot, true
	}
	return LiveTodoSnapshot{}, false
}

const liveTodoCompletedSnapshotTTLMillis int64 = 5_000

func (r *Router) liveTodoSnapshotLocked(threadID string, nowMillis int64) *LiveTodoSnapshot {
	snapshot, ok := r.latestTodoByThread[threadID]
	if !ok {
		return nil
	}
	if liveTodoSnapshotExpired(snapshot, nowMillis) {
		delete(r.latestTodoByThread, threadID)
		return nil
	}
	steps := make([]TodoStep, len(snapshot.Steps))
	copy(steps, snapshot.Steps)
	snapshot.Steps = steps
	return &snapshot
}

func liveTodoSnapshotExpired(snapshot LiveTodoSnapshot, nowMillis int64) bool {
	if len(snapshot.Steps) == 0 {
		return true
	}
	for _, step := range snapshot.Steps {
		if step.Status != "completed" {
			return false
		}
	}
	return nowMillis-snapshot.UpdatedAt > liveTodoCompletedSnapshotTTLMillis
}

// decodeTodoSteps reads the wire-shaped {plan: [{step, status}]}
// payload off EventTodoUpdate.Meta. Empty steps and unmarshal failures
// produce nil so callers can `if len(...) == 0 { drop }` uniformly.
//
// Bounds the wire input on two axes so a misbehaving provider can't
// stuff the per-event WS payload (and the per-pane snapshot held in
// frontend memory): step count is capped at maxTodoSteps; per-step
// text is truncated via truncateRunes. Same shape as the
// addHookNotificationMeta path that bounds hook entries / runes.
func decodeTodoSteps(raw json.RawMessage) []TodoStep {
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
	steps := make([]TodoStep, 0, min(len(decoded.Plan), maxTodoSteps))
	for _, item := range decoded.Plan {
		if len(steps) >= maxTodoSteps {
			break
		}
		step := truncateRunes(strings.TrimSpace(item.Step), maxTodoStepRunes)
		if step == "" {
			continue
		}
		steps = append(steps, TodoStep{
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
	turnIndex := r.timelineNotificationTurnIndex(evt.ThreadID)
	itemID := eventItemID(evt)
	if strings.TrimSpace(itemID) == "" {
		itemID = nextTimelineNotificationID(turnIndex, r.nextNotificationSequence(evt.ThreadID, turnIndex))
	}
	_, err := r.persistTimelineNotificationWithID(evt, itemID, notificationKind, summary)
	return err
}

// persistTimelineNotificationWithID is the explicit-id variant. Used
// by callers (handleSessionDied) that need a deterministic id rather
// than the per-thread counter fallback so that re-emitted events
// upsert in place instead of producing duplicate rows. Returns
// wasNew=true on the row's first persistence; subsequent calls
// upsert the same row but return false so callers can skip
// already-fired side effects.
func (r *Router) persistTimelineNotificationWithID(evt provider.ProviderEvent, itemID, notificationKind, summary string) (bool, error) {
	notificationKind = strings.TrimSpace(notificationKind)
	if notificationKind == "" {
		notificationKind = "notification"
	}
	turnIndex := r.timelineNotificationTurnIndex(evt.ThreadID)
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
	existing, found, err := r.store.GetThreadItem(evt.ThreadID, item.ID)
	if err != nil {
		return false, fmt.Errorf("timeline notification existing lookup %s: %w", item.ID, err)
	}
	if found {
		item.CreatedAt = existing.CreatedAt
		item.ItemIndex = existing.ItemIndex
		item.PayloadID = existing.PayloadID
	}
	if err := r.persistItem(item, nil); err != nil {
		return false, err
	}
	return !found, nil
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
	case sessionDiedNotificationKind:
		addSessionDiedNotificationMeta(meta, raw)
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return `{}`
	}
	return string(encoded)
}

// addSessionDiedNotificationMeta forwards the wire ProcessExitInfo
// (reason / exitCode / signal) onto the notification's meta so the
// frontend's SessionDiedNotification component can render the exit
// detail without a second decode hop. The wire shape comes from
// provider.MarshalProcessExitMeta — keep the JSON tags aligned.
func addSessionDiedNotificationMeta(meta map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var info provider.ProcessExitInfo
	if json.Unmarshal(raw, &info) != nil {
		return
	}
	if reason := strings.TrimSpace(info.Reason); reason != "" {
		meta["reason"] = reason
	}
	if info.ExitCode != 0 {
		meta["exitCode"] = info.ExitCode
	}
	if signal := strings.TrimSpace(info.Signal); signal != "" {
		meta["signal"] = signal
	}
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
	// Bounds on EventTodoUpdate inputs. The panel itself truncates the
	// rendered list to 5 with a Show-more reveal; these caps are the
	// outer safety net so a provider that ships a multi-MB plan can't
	// blow up the WS payload or the pane snapshot.
	maxTodoSteps     = 256
	maxTodoStepRunes = 300
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
