package triage

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

const claudeTaskOutputFileMaxBytes = 8 * 1024 * 1024

type backgroundTaskNotificationMeta struct {
	TaskID     string `json:"task_id"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Source     string `json:"source,omitempty"`
	OutputFile string `json:"output_file,omitempty"`
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

	now := eventTimestampMillis(evt)
	turnIndex, err := r.notificationTurnIndex(evt.ThreadID, launch, found)
	if err != nil {
		log.Printf("triage: task notification turn index %s: %v", meta.TaskID, err)
	}

	notification := store.Item{
		ID:        nextTaskNotificationID(meta.TaskID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      itemKindNotification,
		Role:      "system",
		Status:    statusCompleted,
		Summary:   stringsxFirst(evt.Content, "Background task notification"),
		CreatedAt: now,
		UpdatedAt: now,
		Meta:      backgroundNotificationItemMeta(meta, "ready", ""),
	}
	if found {
		notification.ParentID = launch.ParentID
		notification.ToolName = launch.ToolName
	}
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

	if meta.OutputFile == "" {
		if err := r.maybeDeferOrPersist(evt.ThreadID, notification, nil); err != nil {
			return err
		}
		return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, found, meta, nil, "ready", "")
	}

	notification.Meta = backgroundNotificationItemMeta(meta, "loading", "")
	if err := r.maybeDeferOrPersist(evt.ThreadID, notification, nil); err != nil {
		return err
	}
	if err := r.enrichExistingBackgroundCompletionFromNotification(evt, launch, found, meta, nil, "loading", ""); err != nil {
		return err
	}

	payload, readErr := buildBackgroundOutputFilePayload(payloadIDForBackgroundOutput(launch, found, meta), meta.OutputFile, now)
	if readErr != nil {
		notification.Meta = backgroundNotificationItemMeta(meta, "error", readErr.Error())
		if err := r.maybeDeferOrPersist(evt.ThreadID, notification, nil); err != nil {
			return err
		}
		log.Printf("triage: read Claude task output file %q: %v", meta.OutputFile, readErr)
		return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, found, meta, nil, "error", readErr.Error())
	}

	notification.Meta = backgroundNotificationItemMeta(meta, "loaded", "")
	if err := r.maybeDeferOrPersist(evt.ThreadID, notification, payload); err != nil {
		return err
	}
	return r.enrichExistingBackgroundCompletionFromNotification(evt, launch, found, meta, payload, "loaded", "")
}

func (r *Router) enrichExistingBackgroundCompletionFromNotification(
	evt provider.ProviderEvent,
	launch store.Item,
	found bool,
	meta backgroundTaskNotificationMeta,
	payload *store.Payload,
	outputState string,
	readError string,
) error {
	if !found || launch.Kind != itemKindToolCall {
		return nil
	}
	completionID := nextToolCompletionID(launch.ID)
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
		backgroundNotificationCompletionMeta(meta, payload != nil, outputState, readError),
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

func (r *Router) notificationTurnIndex(threadID string, launch store.Item, found bool) (int, error) {
	if found {
		return r.backgroundCompletionTurnIndex(threadID, launch.TurnIndex)
	}
	if turnIndex, ok := r.openTurnIndex(threadID); ok {
		return turnIndex, nil
	}
	return r.store.LastTurnIndex(threadID)
}

func (r *Router) findTaskNotificationItem(threadID, taskID string) (store.Item, bool, error) {
	item, found, err := r.store.FindNotificationItemByTaskID(threadID, taskID)
	if err != nil || !found {
		return store.Item{}, false, err
	}
	return item, true, nil
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

func (r *Router) backgroundOutputFilePayload(launchID, outputFile string, now int64) *store.Payload {
	payload, err := buildBackgroundOutputFilePayload("tool-call-result:"+launchID, outputFile, now)
	if err != nil {
		log.Printf("triage: read Claude background output file %q: %v", outputFile, err)
		return nil
	}
	return payload
}

func buildBackgroundOutputFilePayload(payloadID, outputFile string, now int64) (*store.Payload, error) {
	data, meta, err := readClaudeTaskOutputFile(outputFile, claudeTaskOutputFileMaxBytes)
	if err != nil {
		return nil, err
	}
	meta["outputFile"] = outputFile
	meta["outputFileState"] = "loaded"
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

func readClaudeTaskOutputFile(path string, maxBytes int64) ([]byte, map[string]any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil, fmt.Errorf("empty output_file path")
	}
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("output_file path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("output_file path is a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("output_file path is not a regular file")
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, nil, err
	}
	if !isAllowedClaudeOutputPath(resolvedPath) {
		return nil, nil, fmt.Errorf("output_file path is outside allowed temp roots")
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

func allowedClaudeOutputRoots() []string {
	candidates := []string{
		os.TempDir(),
		"/tmp",
		"/private/tmp",
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

func payloadIDForBackgroundOutput(launch store.Item, found bool, meta backgroundTaskNotificationMeta) string {
	if found && launch.ID != "" {
		return "tool-call-result:" + launch.ID
	}
	if meta.ToolUseID != "" {
		return "tool-call-result:" + meta.ToolUseID
	}
	return "task-output-file:" + meta.TaskID
}

func nextTaskNotificationID(taskID string) string {
	return "task-notification:" + taskID
}

func backgroundNotificationItemMeta(meta backgroundTaskNotificationMeta, outputState, readError string) string {
	fields := map[string]any{
		"task_id":           meta.TaskID,
		"source":            "task_notification",
		"output_file_state": outputState,
	}
	if meta.ToolUseID != "" {
		fields["tool_use_id"] = meta.ToolUseID
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

func backgroundNotificationCompletionMeta(
	meta backgroundTaskNotificationMeta,
	payloadLoaded bool,
	outputState string,
	readError string,
) string {
	fields := map[string]any{
		"task_id":                     meta.TaskID,
		"notification_source":         "task_notification",
		"notification_output_loaded":  payloadLoaded,
		"notification_output_state":   outputState,
		"notification_output_file":    meta.OutputFile,
		"notification_terminal_state": meta.Status,
	}
	if readError != "" {
		fields["notification_output_error"] = readError
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
	if meta.OutputFile != "" {
		fields["output_file"] = meta.OutputFile
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
