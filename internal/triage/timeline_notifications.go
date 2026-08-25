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

// handleTodoUpdate routes EventTodoUpdate (Claude TodoWrite reroute,
// Codex turn/plan/updated) to the frontend and to the thread's own
// `live_todo` column, without persisting a timeline notification row —
// a todo list is state about the conversation, not an entry in it. SQLite
// IS its source of truth (migration v65): the list outlives the provider
// session that reported it and the app process that received it, which is
// what a user working through an unfinished list expects. ThreadPane
// remains only the visible projection. See TodoUpdateEvent /
// ActivityRailTodosBody.svelte for the rendering side.
//
// Clearing is delegated to projectTodoSnapshot: an empty list clears the
// column AND emits an empty provider:todo_update when something was stored,
// so a live pane (which holds the last list in memory and only re-reads the
// backend copy on refresh) drops it too. See that function for the gating
// rationale.
func (r *Router) handleTodoUpdate(evt provider.ProviderEvent) error {
	steps := decodeTodoSteps(evt.Meta)
	return r.projectTodoSnapshot(evt.ThreadID, steps, eventTimestampMillis(evt))
}

// projectTodoSnapshot is the shared write-path for all producers of the
// activity-rail todo list (Codex update_plan, legacy Claude TodoWrite,
// new Claude Task* family). Empty steps clears the stored list AND emits an
// empty provider:todo_update so a live pane drops it — but only when
// something was actually stored. A pane holds the last non-empty list in
// process memory and only re-reads the backend copy on refresh, so
// "absence" alone leaves a cleared list frozen on screen until reload
// (the 2026-06-14 Task* delete-to-empty incident: deletes arrive one at a
// time and the final one empties the map). Gating the emit on a stored list
// keeps a malformed/empty wire payload from spawning a no-op clear when
// nothing was showing. Callers do NOT re-dispatch a synthetic
// EventTodoUpdate through Handle; that would re-fire the stopped-thread
// check, api-retry forward-progress check, and the test-only eventHook.
//
// The store write happens BEFORE the emit so a refresh racing the push can
// never read a list older than the frame it just saw — except when the write
// itself failed, in which case the pane briefly shows a list a refresh would
// roll back; the error is returned so the caller reports it. A failed SET
// still emits because the live pane is the thing the user is looking at, and
// stalling it on a persistence failure trades a visible feature for an
// invisible one. A failed CLEAR emits too: the gate's question is really "is
// a pane showing something", the store's "was anything stored" is only a
// proxy for it, and a call that failed answered nothing — dropping the
// pane's list is the recoverable guess (a refresh restores whatever the
// column really holds), while staying silent over a stored list would leave
// the pane showing it until a refresh. Store failures leave exactly one
// residue each way, both bounded by the next refresh or report: a failed SET
// can leave a pane showing a list over an empty column (and a later
// trivially-successful clear of that empty column stays silent — the
// refresh, not the gate, is what heals that pane), and a failed CLEAR
// leaves the column holding a list the provider deleted, which refreshes
// re-serve until the next report overwrites it. Neither residue is silent:
// the error reaches the caller both times.
func (r *Router) projectTodoSnapshot(threadID string, steps []TodoStep, updatedAt int64) error {
	if r == nil || threadID == "" {
		return nil
	}
	if r.store == nil {
		// Storeless routers exist only in tests (same guard as the other
		// store-touching triage paths). Nothing can persist and nothing can
		// answer "was anything stored", so a list still reaches live panes
		// and a clear stays silent.
		if len(steps) > 0 {
			r.emit("provider:todo_update", TodoUpdateEvent{ThreadID: threadID, Steps: steps})
		}
		return nil
	}
	if len(steps) == 0 {
		existed, err := r.store.ClearThreadLiveTodo(threadID)
		if existed || err != nil {
			// Explicit empty slice (not nil) so the wire carries [] and honors
			// the non-nullable TodoUpdateEvent.steps type the frontend declares.
			r.emit("provider:todo_update", TodoUpdateEvent{ThreadID: threadID, Steps: []TodoStep{}})
		}
		return err
	}
	storeErr := r.store.SetThreadLiveTodo(threadID, storeLiveTodo(steps, updatedAt))
	r.emit("provider:todo_update", TodoUpdateEvent{
		ThreadID: threadID,
		Steps:    steps,
	})
	return storeErr
}

// ResetThreadTodo drops the thread's Task* correlation map and clears the
// persisted todo list, emitting the live clear when something was stored.
//
// For the app paths that discard the conversation a list was minted in —
// rollback to an earlier message, switching the thread's provider. Those
// paths start the next provider session from scratch (or fork it, which
// starts provider task state from scratch all the same), and Claude task ids
// are per-session small integers: leaving the dead list in the column would
// hand seedTasksFromStoredTodo entries the new session's ids collide with,
// resurrecting discarded tasks with stale statuses. Opting out clears what
// opting in stored. claude-tui reverts deliberately do NOT call this: the
// TUI's session (and its task list) stays live across its native Esc-revert,
// and the still-warm map keeps projecting the provider's real state.
func (r *Router) ResetThreadTodo(threadID string) error {
	if r == nil || threadID == "" {
		return nil
	}
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.tasks = nil
	}
	r.mu.Unlock()
	return r.projectTodoSnapshot(threadID, nil, 0)
}

// storeLiveTodo converts the wire step shape into the persisted one. The
// fields map 1:1 — the two types exist separately only because the wire shape
// is triage's and the column shape is the store's.
func storeLiveTodo(steps []TodoStep, updatedAt int64) store.ThreadLiveTodo {
	stored := store.ThreadLiveTodo{
		Steps:     make([]store.ThreadLiveTodoStep, 0, len(steps)),
		UpdatedAt: updatedAt,
	}
	for _, step := range steps {
		stored.Steps = append(stored.Steps, store.ThreadLiveTodoStep{
			Step:   step.Step,
			Status: step.Status,
			ID:     step.ID,
			Owner:  step.Owner,
		})
	}
	return stored
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
			ID     string `json:"id"`
			Owner  string `json:"owner"`
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
			Step: step,
			// Status is an enum on every known wire (pending / inProgress /
			// completed), but it arrives provider-controlled like the other
			// fields and the cap is what makes that a fact about the blob
			// rather than about the provider's good behavior.
			Status: truncateRunes(strings.TrimSpace(item.Status), maxTodoStatusRunes),
			ID:     truncateRunes(strings.TrimSpace(item.ID), maxTodoIDRunes),
			Owner:  truncateRunes(strings.TrimSpace(item.Owner), maxTodoOwnerRunes),
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
	// Claude's permission notices compose their sentence in the parser and
	// carry no `title`, so the generic "Provider notification" placeholder
	// would swallow the one thing the row exists to say.
	var notice permissionNoticeMeta
	isPermissionNotice := isPermissionNoticeKind(meta.Kind)
	if isPermissionNotice {
		if len(evt.Meta) > 0 {
			_ = json.Unmarshal(evt.Meta, &notice)
		}
		if summary == "" {
			summary = permissionNoticeSummaryFallback(notice)
		}
	}
	if summary == "" {
		summary = "Provider notification"
	}
	isDenial := isPermissionNotice && meta.Kind == permissionDeniedNotificationKind
	var deniedTool store.Item
	deniedToolFound := false
	if isDenial {
		// A subagent's denial belongs to that subagent: the notice takes
		// the denied tool row's scope (deniedToolCallScope).
		deniedTool, deniedToolFound = r.lookupDeniedToolCall(evt.ThreadID, notice)
		evt.ParentToolUseID = deniedToolCallScope(evt, deniedTool, deniedToolFound)
	}
	if err := r.persistTimelineNotification(evt, meta.Kind, summary); err != nil {
		return err
	}
	// The notice row is the durable record; the tool_call annotation is
	// the cross-reference. Order matters: the row must exist before the
	// chip points at it.
	if isDenial {
		r.annotateDeniedToolCall(evt, notice, deniedTool, deniedToolFound)
	}
	return nil
}

func (r *Router) persistTimelineNotification(evt provider.ProviderEvent, notificationKind, summary string) error {
	turnIndex := r.timelineNotificationTurnIndex(evt)
	itemID := eventItemID(evt)
	if strings.TrimSpace(itemID) == "" {
		itemID = NotificationItemID(turnIndex, r.nextNotificationSequence(evt.ThreadID, turnIndex))
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
	turnIndex := r.timelineNotificationTurnIndex(evt)
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

// timelineNotificationTurnIndex is the turn a system-row (notification,
// command result, session death) belongs to: the thread's current turn,
// or the LAUNCH's turn for a row scoped to a subagent (invariant 10 —
// the two differ once a detached agent's transcript is replayed after
// the main turn moved on). 0 when no turn can be resolved, matching every
// other timeline-notification writer.
func (r *Router) timelineNotificationTurnIndex(evt provider.ProviderEvent) int {
	turnIndex, err := r.turnIndexForEvent(evt)
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
	return r.nextScopeSequence(threadID, turnIndex, "notification")
}

// nextScopeSequence allocates the next id ordinal for one (thread, turn,
// label) namespace. Id-allocating counter: swept by CleanupThread, never at a
// turn boundary — see the triage area guide's correlation-state categories.
func (r *Router) nextScopeSequence(threadID string, turnIndex int, label string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := scopeCounterKey(turnIndex, label)
	st := r.state(threadID)
	if st.timelineSeqByScope == nil {
		st.timelineSeqByScope = make(map[string]int)
	}
	seq := st.timelineSeqByScope[key]
	st.timelineSeqByScope[key] = seq + 1
	return seq
}

// NotificationItemID is the id of a timeline `notification` row that
// carried no provider id of its own: the Nth notification of a turn.
func NotificationItemID(turnIndex, seq int) string {
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
	case modelRefusalFallbackNotificationKind, modelAvailabilityFallbackKind, modelConsentFallbackKind:
		addModelFallbackNotificationMeta(meta, raw)
	case permissionDeniedNotificationKind, permissionRetryNotificationKind:
		addPermissionNoticeMeta(meta, raw)
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return `{}`
	}
	return string(encoded)
}

func addModelFallbackNotificationMeta(meta map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var fallback modelFallbackMeta
	if json.Unmarshal(raw, &fallback) != nil {
		return
	}
	if value := strings.TrimSpace(fallback.OriginalModel); value != "" {
		meta["originalModel"] = value
	}
	if value := strings.TrimSpace(fallback.FallbackModel); value != "" {
		meta["fallbackModel"] = value
	}
	if value := strings.TrimSpace(fallback.Reason); value != "" {
		meta["reason"] = value
	}
	if value := strings.TrimSpace(fallback.Category); value != "" {
		meta["category"] = value
	}
	if value := strings.TrimSpace(fallback.Explanation); value != "" {
		meta["explanation"] = value
	}
	if value := strings.TrimSpace(fallback.Trigger); value != "" {
		meta["trigger"] = value
	}
	if value := strings.TrimSpace(fallback.RefusedUserMessageUUID); value != "" {
		meta["refusedUserMessageUuid"] = value
	}
	// model_consent_fallback's own pair. Without them the consent row cannot
	// say whether the switch was permanent, which is the whole difference
	// between "for this turn" and "this is your default now".
	if value := strings.TrimSpace(fallback.Choice); value != "" {
		meta["choice"] = value
	}
	if fallback.PersistedAsDefault {
		meta["persistedAsDefault"] = true
	}
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
	if tail := strings.TrimSpace(info.StderrTail); tail != "" {
		meta["stderrTail"] = tail
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
	maxTodoSteps       = 256
	maxTodoStepRunes   = 300
	maxTodoOwnerRunes  = 64
	maxTodoIDRunes     = 64
	maxTodoStatusRunes = 32
)

func truncateRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Fast path: byte length is an upper bound on rune count, so the
	// great majority of inputs return without allocating []rune.
	if len(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}
