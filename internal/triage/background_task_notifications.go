package triage

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const claudeTaskOutputFileMaxBytes = 8 * 1024 * 1024

type backgroundTaskNotificationMeta struct {
	TaskID          string `json:"task_id"`
	ToolUseID       string `json:"tool_use_id,omitempty"`
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Source          string `json:"source,omitempty"`
	OutputFile      string `json:"output_file,omitempty"`
	// UUID is the notification ENVELOPE's own id (Claude's top-level
	// `uuid`; for the synthetic-XML channel, the wrapping user
	// envelope's). It is the per-EVENT half of the row id — see
	// nextTaskNotificationID.
	UUID string `json:"uuid,omitempty"`
	// Usage is the agent's AUTHORITATIVE final counters. Claude reports
	// `usage{total_tokens, tool_uses, duration_ms}` for the whole run on
	// exactly one envelope — this one — and the parser forwards it under
	// `usage` as a provider.SubagentProgressMeta (parse_system.go
	// §buildBackgroundTaskNotificationEvent; the key and type are
	// contract). Zero value when the envelope reported none: the
	// synthetic-XML channel never carries it, and neither does a
	// `local_bash` bookend.
	Usage provider.SubagentProgressMeta `json:"usage"`
}

func decodeBackgroundTaskNotificationMeta(raw json.RawMessage) backgroundTaskNotificationMeta {
	if len(raw) == 0 {
		return backgroundTaskNotificationMeta{}
	}
	var m backgroundTaskNotificationMeta
	if json.Unmarshal(raw, &m) != nil {
		return backgroundTaskNotificationMeta{}
	}
	return m
}

// handleBackgroundTaskNotification processes Claude
// `system/task_notification` envelopes. The notification row is the
// "agent attention signal" surface (invariant 21 — kept distinct from
// the lifecycle row); whether and where it leads to a tool_completion
// sibling depends on the stash table.
//
// Three cases:
//
//  1. Stash present with a resolved launch: the agent is observing
//     completion now (the queued attachment will surface to the model
//     on the next iteration). Drain the stash and write the sibling at
//     the current write head via writeBackgroundCompletionSibling. Tray
//     gets the "drained" event after the sibling is written.
//
//     No resolved launch means hidden subagent work that never had a
//     parent-thread row. Drain/drop it without writing a notification
//     row or completion sibling.
//
//  2. Sibling already exists (TaskOutput drained first): upgrade its
//     payload from output_file via
//     enrichExistingBackgroundCompletionFromNotification. No new sibling
//     is created; this is the "richer payload arriving second" path.
//
//  3. No stash, no sibling (foreground-stall task_notification with
//     output_file=""): notification row only. Per invariant 21,
//     task_notification is not a lifecycle source.
//
// The notification row write itself is unconditional and matches the
// previous flow (loading → loaded / error transitions on output_file).
//
// The stash drain runs BEFORE the notification row persists, and the
// order is user-visible: the frontend hides this notification row (the
// agent's full report text) only once a completed lifecycle row with
// the same task_id exists (filterRedundantNotifications), and a
// backgrounded launch is deliberately held at `running` until the
// sibling lands. Notification-first meant one wire flush where the
// report mounted as a full-width timeline row and then vanished when
// the sibling arrived — a multi-thousand-pixel content flash and
// scroll clamp at the tail (bug-report-20260801T024731Z). Sibling-first
// turns case 1 into case 2 on the frontend: the notification arrives
// already suppressed, and the enrichment calls attach the output-file
// payload onto the just-written sibling.
func (r *Router) handleBackgroundTaskNotification(evt provider.ProviderEvent) error {
	meta := decodeBackgroundTaskNotificationMeta(evt.Meta)
	if meta.TaskID == "" {
		return nil
	}
	if meta.Source == "" {
		meta.Source = "task_notification"
	}

	launch, found, err := r.resolveBackgroundTaskLaunch(evt.ThreadID, evt.ItemID, meta.ToolUseID, meta.TaskID)
	if err != nil {
		return err
	}
	if found && launch.Kind != itemKindToolCall {
		found = false
		launch = store.Item{}
	}
	if !found {
		if _, _, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, meta.TaskID); err != nil {
			log.Printf("triage: drain hidden task_notification stash %s: %v", meta.TaskID, err)
		}
		// Correct to drop: no resolvable launch means the work belongs
		// to a subagent whose private transcript was never projected
		// into this thread, and there is no parent-thread row a
		// notification could hang off. Logged because the drop was
		// previously silent — a task_notification vanishing here is
		// indistinguishable from one that never arrived, and that is
		// exactly the evidence the next investigation needs.
		log.Printf(
			"triage: drop task_notification with no resolvable launch thread=%s task_id=%s summary=%q",
			evt.ThreadID, meta.TaskID, ClampErrorSummary(evt.Content),
		)
		return nil
	}

	// The agent's final numbers, before any of the row work below and on
	// every path out of this handler. `task_notification` is the one
	// envelope that reports the whole run's `usage`, so it is the
	// authoritative half of the launch row's persisted progress —
	// available even when no live tick survived (a backgrounded agent
	// emits task_progress, but a session restart or a reconnect drops the
	// in-memory ticks outright). Folded for background Bash too when it
	// carries usage: the merge is order-free and a launch with no
	// counters is left untouched, so the only cost of not special-casing
	// the tool type is a comparison.
	if meta.Usage != (provider.SubagentProgressMeta{}) {
		if err := r.persistSubagentFinalProgress(launch, meta.Usage); err != nil {
			// Never fatal to the notification: the counters are a card
			// decoration and the bell is the user-visible signal.
			log.Printf("triage: persist final subagent progress for %s: %v", launch.ID, err)
		}
	}

	// Foreground tools (Bash without run_in_background:true) also receive
	// task_notification envelopes from Claude on completion — the CLI emits
	// them for every Bash/Task lifecycle, not just backgrounded ones (see
	// provider/claude/CLAUDE.md §task_started). The launch's own status
	// flip is the user-visible completion signal; an additional notification
	// row would be redundant (and the frontend filter at
	// notificationFilter.ts already drops it). Skip the row write but still
	// drain the stash defensively — foreground tools shouldn't populate it
	// today, but the drain is the one load-bearing side effect we must not
	// regress if a future wire change reroutes a backgrounded terminal.
	if !launch.IsBackground {
		return r.drainTaskNotificationStash(evt, meta, launch)
	}

	// Sibling first — see the ordering note in the function comment.
	if err := r.drainTaskNotificationStash(evt, meta, launch); err != nil {
		return err
	}

	now := eventTimestampMillis(evt)
	turnIndex, err := r.backgroundCompletionTurnIndex(evt.ThreadID, launch.TurnIndex)
	if err != nil {
		log.Printf("triage: task notification turn index %s: %v", meta.TaskID, err)
	}

	// Watch-ness is a property of the LAUNCH (the keep-running flip
	// copies `watch_task` onto its meta from the Monitor launch ack —
	// claude-wire.md §E7). It is stamped onto every notification row
	// here because the frontend's redundant-notification filter has to
	// tell the two shapes apart at RENDER time, long after the launch
	// row may have been windowed out of the pane: an ordinary
	// background task's single bell is redundant once its completion
	// card exists, but a watch task's notifications ARE its event
	// history and the completion only means the stream ended.
	watchTask := launchIsWatchTask(launch)

	// Q11 (docs/specs/agent-visibility.md): notifications fire for
	// TOP-LEVEL nodes only; a nested completion updates its card
	// silently. This row IS the thread's bell — the frontend's
	// notification surface and the toast both hang off it — so a nested
	// launch simply does not get one. Everything else on this path still
	// runs for a nested launch: the stash drains, the output file is
	// read, and the completion sibling is enriched with the payload and
	// output state, which is what its card renders.
	//
	// A watch task is exempt regardless of depth. Its notification rows
	// are not a bell at all: they ARE its event history (claude-wire.md
	// §E7 — one row per observed output event, exempt from the frontend's
	// redundant-notification hide), and suppressing them would delete
	// content no other row carries.
	writeBell := strings.TrimSpace(launch.ParentID) == "" || watchTask

	notification := store.Item{
		ID:        nextTaskNotificationID(meta.TaskID, meta.UUID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindNotification,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsxFirst(evt.Content, backgroundTaskNotificationPlaceholderSummary),
		ParentID:  stringsxFirst(launch.ParentID, eventParentID(evt), meta.ParentToolUseID),
		ToolName:  launch.ToolName,
		CreatedAt: now,
		UpdatedAt: now,
		Meta:      backgroundNotificationItemMeta(meta, "ready", "", watchTask),
	}
	if writeBell {
		if persisted, ok, err := r.store.GetThreadItem(evt.ThreadID, notification.ID); err != nil {
			return fmt.Errorf("task notification existing lookup %s: %w", notification.ID, err)
		} else if ok && persisted.Kind == itemKindNotification {
			notification.CreatedAt = persisted.CreatedAt
			notification.TurnIndex = persisted.TurnIndex
			notification.ItemIndex = persisted.ItemIndex
			notification.PayloadID = persisted.PayloadID
			if notification.ParentID == "" {
				notification.ParentID = persisted.ParentID
			}
			if notification.ToolName == "" {
				notification.ToolName = persisted.ToolName
			}
		}
	}

	// persistBell is the one write point for the notification row, so
	// every state transition below (loading → loaded / error) reads the
	// same way whether or not this launch is entitled to a bell.
	persistBell := func(state, readError string, payload *store.Payload) error {
		if !writeBell {
			return nil
		}
		notification.Meta = backgroundNotificationItemMeta(meta, state, readError, watchTask)
		return r.maybeDeferOrPersist(evt.ThreadID, notification, payload)
	}

	var notificationPayload *store.Payload
	outputState := "ready"
	readErrorString := ""
	if meta.OutputFile != "" {
		if err := persistBell("loading", "", nil); err != nil {
			return err
		}
		if err := r.enrichExistingBackgroundCompletionFromNotification(evt, launch, meta, nil, "loading", ""); err != nil {
			return err
		}

		payload, readErr := buildBackgroundOutputFilePayload("tool-call-result:"+launch.ID, launch, meta.OutputFile, nil, now)
		if readErr != nil {
			outputState = "error"
			readErrorString = readErr.Error()
			log.Printf("triage: read Claude task output file %q: %v", meta.OutputFile, readErr)
		} else {
			outputState = "loaded"
			notificationPayload = payload
			// The transcript reconciliation runs only once the file has proved
			// readable and reuses the bounded payload bytes. A read failure has
			// already been reported. A reconciliation failure is its OWN report:
			// the file was
			// readable but could not be projected, which is the one
			// condition under which the payload loaded and the rows are
			// still missing.
			if backfillErr := r.maybeBackfillSubagentTranscript(evt.ThreadID, launch, meta, payload); backfillErr != nil {
				outputState = "error"
				readErrorString = backfillErr.Error()
			}
		}
		if err := persistBell(outputState, readErrorString, notificationPayload); err != nil {
			return err
		}
	} else if err := persistBell(outputState, "", nil); err != nil {
		return err
	}

	return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, meta, notificationPayload, outputState, readErrorString)
}

// maybeBackfillSubagentTranscript completes an agent's transcript from
// the notification's `output_file` (subagent_transcript.go). A no-op for
// anything whose output file is not a sidechain JSONL — a background
// Bash's output_file is captured command output, and the command_output
// payload path already owns it.
func (r *Router) maybeBackfillSubagentTranscript(threadID string, launch store.Item, meta backgroundTaskNotificationMeta, payload *store.Payload) error {
	if !isSubagentTranscriptLaunch(launch) {
		return nil
	}
	if payload == nil {
		return fmt.Errorf("subagent transcript payload is missing after a successful output_file read")
	}
	var payloadMeta struct {
		OriginalBytes int64 `json:"originalBytes"`
		Truncated     bool  `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(payload.Meta), &payloadMeta); err != nil {
		return fmt.Errorf("decode subagent transcript payload metadata: %w", err)
	}
	if payloadMeta.Truncated {
		return fmt.Errorf(
			"subagent transcript is %s, above the %s ceiling",
			formatByteCount(payloadMeta.OriginalBytes), formatByteCount(claudeTaskOutputFileMaxBytes))
	}
	written, err := r.backfillSubagentTranscript(threadID, launch, payload.Data)
	if err != nil {
		log.Printf("triage: backfill subagent transcript for %s from %q: %v", launch.ID, meta.OutputFile, err)
		return err
	}
	if written > 0 {
		log.Printf("triage: backfilled %d row(s) of subagent %s from its task_notification transcript", written, launch.ID)
	}
	return nil
}

// drainTaskNotificationStash drains the pending-background-terminal
// stash for the notification's task_id and, if one was waiting, writes
// the `tool_completion` sibling at the current write head. Extracted
// so both the backgrounded-tool path (pre notification-row write — the
// sibling must reach the frontend first, see
// handleBackgroundTaskNotification's ordering note) and the
// foreground-tool skip path (no row write) share the load-bearing
// drain logic. A foreground stash is pathological today but the drain
// is idempotent on absence — safe to invoke either way.
func (r *Router) drainTaskNotificationStash(evt provider.ProviderEvent, meta backgroundTaskNotificationMeta, launch store.Item) error {
	stash, stashFound, err := r.store.TakePendingBackgroundTerminal(evt.ThreadID, meta.TaskID)
	if err != nil {
		log.Printf("triage: drain stash on task_notification %s: %v", meta.TaskID, err)
		return nil
	}
	if !stashFound {
		return nil
	}

	terminalMeta := terminalMetaFromNotification(meta)
	// Carry the notification's own summary into the sibling's FIRST
	// write. This path creates the completion row before the
	// notification row exists, so leaving the caption to the enrich
	// call below would mount the card and then grow a line under it.
	//
	// Not for a watch task: its notification rows are exempt from the
	// redundant-notification hide (they ARE the event history), so a
	// caption would show the same text twice on adjacent rows.
	//
	// Not when the notification carries an output_file: that content
	// becomes the sibling's payload, and the "summary" for those tasks
	// is the agent's entire report — see captionForSiblingWrite.
	if !launchIsWatchTask(launch) && meta.OutputFile == "" {
		terminalMeta.NotificationSummary = notificationCaptionSummary(evt.Content)
	}
	mergeStashIntoTerminalMeta(&terminalMeta, stash)
	terminalMeta.Source = "task_notification"

	syntheticEvt := provider.ProviderEvent{
		ThreadID:        evt.ThreadID,
		ItemID:          stringsxFirst(evt.ItemID, terminalMeta.ToolUseID, launch.ID),
		Content:         evt.Content,
		ParentToolUseID: stringsxFirst(eventParentID(evt), meta.ParentToolUseID),
		Timestamp:       evt.Timestamp,
	}
	return r.writeBackgroundCompletionSibling(syntheticEvt, terminalMeta, true)
}

// terminalMetaFromNotification lifts the notification's meta into the
// terminal-event meta shape so writeBackgroundCompletionSibling can
// merge it with stash data uniformly.
func terminalMetaFromNotification(meta backgroundTaskNotificationMeta) backgroundTaskTerminalMeta {
	return backgroundTaskTerminalMeta{
		TaskID:          meta.TaskID,
		ToolUseID:       meta.ToolUseID,
		ParentToolUseID: meta.ParentToolUseID,
		Status:          meta.Status,
		OutputFile:      meta.OutputFile,
	}
}

// The caller guarantees launch is a resolved backgrounded tool_call
// (handleBackgroundTaskNotification's gates); a missing sibling row is
// the no-op path.
func (r *Router) enrichExistingBackgroundCompletionFromNotification(
	evt provider.ProviderEvent,
	launch store.Item,
	meta backgroundTaskNotificationMeta,
	payload *store.Payload,
	outputState string,
	readError string,
) error {
	completionID := ToolCompletionID(launch.ID)
	completion, ok, err := r.store.GetThreadItem(evt.ThreadID, completionID)
	if err != nil {
		return fmt.Errorf("task notification completion lookup %s: %w", completionID, err)
	}
	if !ok || completion.Kind != itemKindBackgroundDone {
		return nil
	}
	completion.UpdatedAt = eventTimestampMillis(evt)
	if payload != nil {
		completion.PayloadID = payload.ID
	}
	completion.Meta = mergeBackgroundCompletionItemMeta(
		completion.Meta,
		backgroundNotificationCompletionMeta(
			meta, payload != nil, outputState, readError,
			// NO caption on the enrich path. This function only ever
			// runs against an already-persisted (and likely mounted)
			// sibling, and a caption materialising here would grow the
			// card after first render, which the row contract forbids
			// (frontend chat AGENTS.md §row shell stability). A caption
			// that misses its one chance (the sibling's first write) is
			// simply absent — the hide is existence-based either way,
			// and for output_file tasks a caption is vetoed everywhere
			// (see captionForSiblingWrite).
			"",
		),
	)
	return r.maybeDeferOrPersist(evt.ThreadID, completion, payload)
}

func (r *Router) resolveBackgroundTaskLaunch(threadID, eventItemIDValue, toolUseID, taskID string) (store.Item, bool, error) {
	launchID := strings.TrimSpace(eventItemIDValue)
	if launchID == "" {
		launchID = strings.TrimSpace(toolUseID)
	}
	if launchID != "" {
		launch, found, err := r.store.GetThreadItem(threadID, launchID)
		if err != nil {
			return store.Item{}, false, fmt.Errorf("background task launch lookup %s: %w", launchID, err)
		}
		if found {
			return launch, true, nil
		}
	}
	if taskID == "" {
		return store.Item{}, false, nil
	}
	launch, found, err := r.findToolCallByTaskID(threadID, taskID)
	if err != nil {
		return store.Item{}, false, fmt.Errorf("background task launch task_id lookup %s: %w", taskID, err)
	}
	return launch, found, nil
}

func (r *Router) findTaskNotificationItem(threadID, taskID string) (store.Item, bool, error) {
	item, found, err := r.store.FindNotificationItemByTaskID(threadID, taskID)
	if err != nil || !found {
		return store.Item{}, false, err
	}
	return item, true, nil
}

// launchIsWatchTask reports whether a persisted launch row was marked as
// Claude's Monitor (`watch_task`, copied onto the launch by the
// keep-running flip in tool_lifecycle.go). Decoded through a one-field
// struct rather than ToolCompleteMeta so a launch carrying a large
// `input` echo isn't re-scanned on every notification.
func launchIsWatchTask(launch store.Item) bool {
	if launch.Meta == "" {
		return false
	}
	var m struct {
		WatchTask bool `json:"watch_task"`
	}
	if json.Unmarshal([]byte(launch.Meta), &m) != nil {
		return false
	}
	return m.WatchTask
}

// captionForSiblingWrite decides whether THIS write of the completion
// sibling may carry the notification caption. Three vetoes:
//
//   - an existing persisted sibling: the caption's one chance was the
//     first write; a mounted card must not grow a line (row contract).
//   - a watch task: its notification rows are the event history and
//     are never hidden, so the caption would duplicate them.
//   - an output_file on the notification: the file's content becomes
//     the sibling's own payload, and for those tasks (async agents,
//     Task subagents) the notification "summary" is the agent's ENTIRE
//     final report — captioning it dumps the full report inline as a
//     muted paragraph, duplicating the expandable payload
//     (2026-08-22, "Test nested agent spawning"). The caption exists to
//     preserve text that would otherwise render nowhere; a payload-
//     backed notification has a better home for it.
func captionForSiblingWrite(existing *store.Item, launch store.Item, outputFile, summary string) string {
	if existing != nil || launchIsWatchTask(launch) || outputFile != "" {
		return ""
	}
	return notificationCaptionSummary(summary)
}

// notificationCaptionSummary is the notification text worth carrying
// onto a completion sibling as a caption: the summary Claude itself saw
// ("Background command … completed (exit code 0)"), and nothing else.
// The placeholder the row falls back to when the envelope had no
// summary carries no information and is dropped.
func notificationCaptionSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == backgroundTaskNotificationPlaceholderSummary {
		return ""
	}
	return summary
}

func notificationOutputState(raw string) (string, string) {
	var meta map[string]any
	if json.Unmarshal([]byte(raw), &meta) != nil {
		return "", ""
	}
	state, _ := meta["output_file_state"].(string)
	readError, _ := meta["output_file_error"].(string)
	return state, readError
}

func (r *Router) backgroundOutputFilePayload(launch store.Item, outputFile string, exitCode *int, now int64) *store.Payload {
	payload, err := buildBackgroundOutputFilePayload("tool-call-result:"+launch.ID, launch, outputFile, exitCode, now)
	if err != nil {
		log.Printf("triage: read Claude background output file %q: %v", outputFile, err)
		return nil
	}
	return payload
}

func buildBackgroundOutputFilePayload(payloadID string, launch store.Item, outputFile string, exitCode *int, now int64) (*store.Payload, error) {
	data, meta, err := readClaudeTaskOutputFile(outputFile, claudeTaskOutputFileMaxBytes)
	if err != nil {
		return nil, err
	}
	meta["outputFile"] = outputFile
	meta["outputFileState"] = "loaded"

	if isCommandOutputLaunch(launch) {
		code := 0
		if exitCode != nil {
			code = *exitCode
		}
		commandMeta := ExtractCommandOutputMetaWithError(string(data), CommandFromLaunch(launch), code, "")
		commandMeta.OutputState = "loaded"
		commandMetaJSON, err := json.Marshal(commandMeta)
		if err != nil {
			return nil, fmt.Errorf("marshal command output payload meta: %w", err)
		}
		return &store.Payload{
			ID:        payloadID,
			Kind:      "command_output",
			Meta:      string(commandMetaJSON),
			Data:      data,
			CreatedAt: now,
		}, nil
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal output_file payload meta: %w", err)
	}
	return &store.Payload{
		ID:        payloadID,
		Kind:      payloadKindToolCallResult,
		Meta:      string(metaJSON),
		Data:      data,
		CreatedAt: now,
	}, nil
}

func isCommandOutputLaunch(launch store.Item) bool {
	return isCommandOutputToolName(launch.ToolName)
}

func CommandFromLaunch(launch store.Item) string {
	var meta struct {
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if json.Unmarshal([]byte(launch.Meta), &meta) == nil {
		if command := strings.TrimSpace(meta.Input.Command); command != "" {
			return command
		}
	}
	summary := strings.TrimSpace(launch.Summary)
	prefix := strings.TrimSpace(launch.ToolName) + ": "
	if strings.HasPrefix(summary, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(summary, prefix))
	}
	return summary
}

// resolveClaudeTaskOutputPath canonicalises an `output_file` path and
// enforces the containment guard, returning the resolved path and its
// stat. Two readers share it: the payload read below, and the subagent
// transcript backfill (subagent_transcript.go), which needs a vetted
// PATH rather than bytes because it streams the file through the
// importer's own reader. Splitting it is what keeps one containment
// rule instead of two.
func resolveClaudeTaskOutputPath(path string) (string, os.FileInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil, fmt.Errorf("empty output_file path")
	}
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("output_file path must be absolute")
	}
	// Resolve symlinks BEFORE the regular-file check. Claude wraps each
	// task output as `<task_id>.output → <subagent>.jsonl` inside
	// `~/.claude/projects/...`, so an unconditional symlink rejection
	// silently drops every Task subagent's payload. EvalSymlinks
	// canonicalises both the link itself and any parent directories
	// that are symlinks (common on macOS where /tmp → /private/tmp).
	// The allow-list check then runs against the resolved path so the
	// containment guard is enforced after symlink resolution rather
	// than circumvented by it.
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("output_file path is not a regular file")
	}
	if !isAllowedClaudeOutputPath(resolvedPath) {
		return "", nil, fmt.Errorf("output_file path is outside allowed roots")
	}
	return resolvedPath, info, nil
}

func readClaudeTaskOutputFile(path string, maxBytes int64) ([]byte, map[string]any, error) {
	resolvedPath, info, err := resolveClaudeTaskOutputPath(path)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	if maxBytes < 0 {
		maxBytes = 0
	}
	limit := maxBytes + 1
	if limit < 1 {
		limit = 1
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, nil, err
	}
	truncated := int64(len(raw)) > maxBytes
	data := raw
	if truncated {
		data = raw[:maxBytes]
		marker := fmt.Sprintf(
			"\n\n[output truncated at %s; original file is %s]",
			formatByteCount(maxBytes),
			formatByteCount(info.Size()),
		)
		data = append(data, []byte(marker)...)
	}
	meta := map[string]any{
		"originalBytes": info.Size(),
		"storedBytes":   len(data),
		"truncated":     truncated,
	}
	return data, meta, nil
}

func isAllowedClaudeOutputPath(path string) bool {
	for _, root := range allowedClaudeOutputRoots() {
		if pathWithinRoot(path, root) {
			return true
		}
	}
	return false
}

// allowedClaudeOutputRootsOnce memoises the resolved-roots list so we
// don't re-stat the candidate paths on every notification. The roots
// are stable for the process lifetime — `os.TempDir()` reads from the
// initial environment, `/tmp` doesn't move, and `~/.claude/projects/`
// is resolved against `os.UserHomeDir()` once at first use. Tests that
// need to bypass the memoised list can call
// `resetAllowedClaudeOutputRootsForTest` to force a recomputation.
var (
	allowedClaudeOutputRootsOnce  sync.Once
	allowedClaudeOutputRootsCache []string
)

func allowedClaudeOutputRoots() []string {
	allowedClaudeOutputRootsOnce.Do(func() {
		allowedClaudeOutputRootsCache = computeAllowedClaudeOutputRoots()
	})
	return allowedClaudeOutputRootsCache
}

// resetAllowedClaudeOutputRootsForTest clears the memoised roots so
// subsequent calls recompute them. Test-only — production code never
// calls this.
func resetAllowedClaudeOutputRootsForTest() {
	allowedClaudeOutputRootsOnce = sync.Once{}
	allowedClaudeOutputRootsCache = nil
}

// computeAllowedClaudeOutputRoots builds the resolved set of allowed
// roots for `output_file` payloads. Two surfaces are valid:
//
//  1. The standard temp roots (`os.TempDir()`, `/tmp`, `/private/tmp`
//     to handle the macOS `/tmp → /private/tmp` symlink). This is
//     where ad-hoc `Bash run_in_background:true` output lands.
//  2. `~/.claude/projects/`. Claude's Task subagent wraps each
//     `<task_id>.output` as a symlink into this directory, so the
//     allow-list MUST include it for the resolved-path check to
//     accept the canonicalised target. Without it, every Task
//     subagent's structured output would be silently dropped.
//
// Each candidate is symlink-resolved at startup (via EvalSymlinks) so
// the comparison in `pathWithinRoot` runs against canonical paths on
// both sides. A candidate that doesn't exist or isn't absolute after
// resolution falls back to `filepath.Clean(candidate)` so the root
// list stays stable even on machines where the directory hasn't been
// created yet (a fresh user with no Claude subagents — the path will
// exist by the time the first notification arrives).
func computeAllowedClaudeOutputRoots() []string {
	candidates := []string{
		os.TempDir(),
		"/tmp",
		"/private/tmp",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".claude", "projects"))
	}
	seen := map[string]struct{}{}
	roots := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			resolved = filepath.Clean(candidate)
		}
		if !filepath.IsAbs(resolved) {
			continue
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		roots = append(roots, resolved)
	}
	return roots
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// backgroundTaskNotificationPlaceholderSummary is what the notification
// row falls back to when the envelope carried no `summary`. It says
// nothing the row's own kind doesn't, so it is never propagated onto a
// completion sibling as a caption.
const backgroundTaskNotificationPlaceholderSummary = "Background task notification"

// nextTaskNotificationID is the notification row's identity, and it is
// per-EVENT rather than per-task.
//
// A persistent Monitor (claude-wire.md §E7) fires one
// `system/task_notification` for every output event of the stream it
// watches. All of them share one `task_id`, and Claude sees each as its
// own message — so a task-only row id made every event overwrite the
// last and left exactly one row: the newest. Mixing the envelope's own
// `uuid` in gives one row per event while keeping the id DETERMINISTIC,
// which is the exactly-once mechanism: a reconnect replaying the same
// envelope upserts the same row rather than appending a duplicate.
//
// An older CLI (and the claude-tui reconstruction, which synthesizes
// these envelopes and has no per-notification id to offer) carries no
// uuid; that falls back to the legacy task-only id, which is the
// pre-existing upsert-in-place behavior. For an ordinary background
// task — one notification, one task — both forms produce exactly one
// row, so nothing about that case changes.
func nextTaskNotificationID(taskID, uuid string) string {
	if uuid = strings.TrimSpace(uuid); uuid != "" {
		return "task-notification:" + taskID + ":" + uuid
	}
	return "task-notification:" + taskID
}

func backgroundNotificationItemMeta(meta backgroundTaskNotificationMeta, outputState, readError string, watchTask bool) string {
	fields := map[string]any{
		"task_id":           meta.TaskID,
		"source":            "task_notification",
		"output_file_state": outputState,
	}
	if watchTask {
		// Only ever written as `true`. An absent key means "not a watch
		// task", exactly as it does on the launch row this is copied
		// from, so a row persisted before this field existed reads the
		// same as one written today.
		fields["watch_task"] = true
	}
	if meta.UUID != "" {
		fields["uuid"] = meta.UUID
	}
	if meta.ToolUseID != "" {
		fields["tool_use_id"] = meta.ToolUseID
	}
	if meta.ParentToolUseID != "" {
		fields["parent_tool_use_id"] = meta.ParentToolUseID
	}
	if meta.Status != "" {
		fields["status"] = meta.Status
	}
	if meta.OutputFile != "" {
		fields["output_file"] = meta.OutputFile
	}
	if readError != "" {
		fields["output_file_error"] = readError
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// notificationSummary is the text Claude itself saw. The frontend hides
// the notification ROW for an ordinary background task (its completion
// card is the in-app signal), so without this the exit code and the
// CLI's own wording were simply lost. Empty when the envelope carried no
// summary; the merge below never overwrites a stored value with "".
func backgroundNotificationCompletionMeta(
	meta backgroundTaskNotificationMeta,
	payloadLoaded bool,
	outputState string,
	readError string,
	notificationSummary string,
) string {
	fields := map[string]any{
		"task_id":                     meta.TaskID,
		"notification_source":         "task_notification",
		"notification_output_loaded":  payloadLoaded,
		"notification_output_state":   outputState,
		"notification_output_file":    meta.OutputFile,
		"notification_terminal_state": meta.Status,
	}
	if notificationSummary != "" {
		fields["notification_summary"] = notificationSummary
	}
	if readError != "" {
		fields["notification_output_error"] = readError
	}
	if meta.ParentToolUseID != "" {
		fields["parent_tool_use_id"] = meta.ParentToolUseID
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func backgroundCompletionItemMeta(meta backgroundTaskTerminalMeta, rich bool) string {
	fields := map[string]any{
		"task_id":         meta.TaskID,
		"status_source":   meta.Source,
		"summary_is_rich": rich || backgroundTerminalHasRichSummary("", meta),
	}
	if meta.ToolUseID != "" {
		fields["tool_use_id"] = meta.ToolUseID
	}
	if meta.ParentToolUseID != "" {
		fields["parent_tool_use_id"] = meta.ParentToolUseID
	}
	if meta.OutputFile != "" {
		fields["output_file"] = meta.OutputFile
	}
	if meta.NotificationSummary != "" {
		// Same key the notification-first path writes through
		// backgroundNotificationCompletionMeta, so both arrival orders
		// leave one stamped meta.
		fields["notification_summary"] = meta.NotificationSummary
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func mergeBackgroundCompletionItemMeta(existing, incoming string) string {
	var merged map[string]any
	if json.Unmarshal([]byte(existing), &merged) != nil || merged == nil {
		merged = map[string]any{}
	}
	var next map[string]any
	if json.Unmarshal([]byte(incoming), &next) != nil {
		next = map[string]any{}
	}
	for key, value := range next {
		if key == "status_source" {
			if merged["status_source"] == "task_updated" && value != "task_updated" {
				continue
			}
		}
		if key == "summary_is_rich" {
			merged[key] = truthy(merged[key]) || truthy(value)
			continue
		}
		if value == "" {
			continue
		}
		merged[key] = value
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return existing
	}
	return string(data)
}

func shouldKeepExistingBackgroundStatus(existingMeta, incomingSource string) bool {
	var existing map[string]any
	if json.Unmarshal([]byte(existingMeta), &existing) != nil {
		return false
	}
	return existing["status_source"] == "task_updated" && incomingSource != "task_updated"
}

func shouldKeepExistingBackgroundSummary(existingMeta, content string, meta backgroundTaskTerminalMeta) bool {
	var existing map[string]any
	if json.Unmarshal([]byte(existingMeta), &existing) != nil {
		return false
	}
	return truthy(existing["summary_is_rich"]) && !backgroundTerminalHasRichSummary(content, meta)
}

func backgroundTerminalHasRichSummary(content string, meta backgroundTaskTerminalMeta) bool {
	return strings.TrimSpace(content) != "" || meta.ExitCode != nil || meta.OutputFile != ""
}

func truthy(value any) bool {
	v, ok := value.(bool)
	return ok && v
}

func stringsxFirst(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func formatByteCount(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}
