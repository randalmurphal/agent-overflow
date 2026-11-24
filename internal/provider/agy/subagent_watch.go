package agy

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

const (
	defaultSubagentPollInterval     = 200 * time.Millisecond
	defaultSubagentDiscoveryTimeout = 10 * time.Second
	maxSubagentMetaFileBytes        = 1 << 20 // 1 MiB cap per JSON metadata/message file
)

// isSafeConversationID ensures a conversation ID is a single safe path token
// that cannot escape `<appDataDir>/brain/`.
func isSafeConversationID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isAlpha && !isDigit && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// CanTrackSubagent reports whether this flavor can correlate Antigravity
// subagent launch records and inbox receipts under AppDataDir.
func (f *Flavor) CanTrackSubagent(sessionID string) bool {
	return strings.TrimSpace(f.AppDataDir) != "" && isSafeConversationID(sessionID)
}

// ParseAgyTranscriptModel splits an Antigravity model identifier
// (for example, "gemini-4-hydrogen-tiered-high" or "claude-sonnet-4-6-medium")
// into its base model slug and resolved reasoning effort.
func ParseAgyTranscriptModel(raw string) (model, effort string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	s = strings.ReplaceAll(s, "-tiered", "")
	// "gpt-oss-120b-medium" is a literal catalog model slug whose "-medium"
	// suffix is part of the model name, not a thought_level tier.
	if strings.EqualFold(s, "gpt-oss-120b-medium") {
		return s, ""
	}
	for _, suffix := range []string{"-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-none"} {
		if len(s) > len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
			base := strings.TrimRight(s[:len(s)-len(suffix)], "-")
			if base != "" {
				return base, strings.TrimPrefix(suffix, "-")
			}
		}
	}
	return s, ""
}

type subagentRecord struct {
	ConversationID string `json:"conversationId"`
	State          string `json:"state"`
}

type inboxMessageRecord struct {
	Sender         string `json:"sender"`
	Timestamp      string `json:"timestamp"`
	Content        string `json:"content"`
	SourceMetadata *struct {
		Tool *struct {
			ConversationID string `json:"conversationId"`
			ToolCall       *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"toolCall"`
		} `json:"tool"`
	} `json:"sourceMetadata"`
}

// ResolveSubagentLaunch maps a child conversation ID (from a live forwarded ACP
// `<childConversationId>:<callId>` tool call) back to its launching parent
// `invoke_subagent` toolCallId using the child's inbox launch record. Results
// are cached in memory so repeated child wire events avoid disk I/O.
func (f *Flavor) ResolveSubagentLaunch(_ context.Context, sessionID, childConversationID string) (string, bool) {
	if !f.CanTrackSubagent(sessionID) || !isSafeConversationID(childConversationID) {
		return "", false
	}
	cacheKey := sessionID + ":" + childConversationID
	f.subagentMu.Lock()
	if cached, ok := f.subagentLaunchByChild[cacheKey]; ok && cached != "" {
		f.subagentMu.Unlock()
		return cached, true
	}
	f.subagentMu.Unlock()

	launchID, ok := f.findLaunchToolCallIDForChild(sessionID, childConversationID)
	if !ok || launchID == "" {
		return "", false
	}
	f.rememberChildLaunch(sessionID, childConversationID, launchID)
	return launchID, true
}

func (f *Flavor) rememberChildLaunch(sessionID, childID, launchToolCallID string) {
	if sessionID == "" || childID == "" || launchToolCallID == "" {
		return
	}
	f.subagentMu.Lock()
	if f.subagentLaunchByChild == nil {
		f.subagentLaunchByChild = make(map[string]string)
	}
	if f.subagentChildByLaunch == nil {
		f.subagentChildByLaunch = make(map[string]string)
	}
	f.subagentLaunchByChild[sessionID+":"+childID] = launchToolCallID
	f.subagentChildByLaunch[sessionID+":"+launchToolCallID] = childID
	f.subagentMu.Unlock()
}

// WatchSubagent correlates an invoke_subagent launch to its child conversation
// ID via agy's `.system_generated/subagents/` + child inbox launch record (never
// polling `transcript.jsonl`), stamps model/effort and child ID onto the launch
// row so live forwarded wire tool calls nest under it, and settles the tray row
// when the child's report reaches the parent inbox or its state becomes killed.
func (f *Flavor) WatchSubagent(ctx context.Context, launch acp.SubagentLaunchContext) {
	if launch.Emit == nil || !f.CanTrackSubagent(launch.SessionID) || strings.TrimSpace(launch.ToolCallID) == "" {
		return
	}
	pollInterval := f.subagentPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultSubagentPollInterval
	}
	discoveryTimeout := f.subagentDiscoveryTimeout
	if discoveryTimeout <= 0 {
		discoveryTimeout = defaultSubagentDiscoveryTimeout
	}

	baseModel, effort := resolveSubagentModelAndEffort(launch.RawInput, launch.ParentModel, launch.ParentEffort)
	launchTime := time.Now()
	discoveryDeadline := launchTime.Add(discoveryTimeout)

	var childID string
	if wireID, _ := launch.Started.ExtraMeta["agent_id"].(string); isSafeConversationID(wireID) {
		childID = wireID
	}

	matchCallID := strings.TrimSpace(launch.RawLaunchToolCallID)
	if matchCallID == "" {
		matchCallID = launch.ToolCallID
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for childID == "" {
		if id, ok := f.findChildIDForLaunch(launch.SessionID, matchCallID); ok {
			childID = id
			break
		}
		if time.Now().After(discoveryDeadline) {
			// Do not invent fake "Agent failed" errors when a server/mock does
			// not write detached subagent metadata; leave no phantom error.
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}

	f.rememberChildLaunch(launch.SessionID, childID, matchCallID)
	if launch.OnChildDiscovered != nil {
		launch.OnChildDiscovered(childID)
	}
	f.emitSubagentIdentityMeta(launch, childID, baseModel, effort)

	idleTicks := 0
	for {
		if status, report, ok := f.scanParentInbox(launch.SessionID, childID, launchTime); ok {
			f.emitSubagentSettlement(launch, childID, status, report, provider.SubagentProgressMeta{
				TaskID:     childID,
				DurationMs: max(time.Since(launchTime).Milliseconds(), 1),
				AgentType:  launch.Started.SubagentType,
			})
			return
		}
		if rec, ok := f.readSubagentRecord(launch.SessionID, childID); ok && rec.State == "SUBAGENT_STATE_KILLED" {
			f.emitSubagentSettlement(launch, childID, "killed", "", provider.SubagentProgressMeta{
				TaskID:     childID,
				DurationMs: max(time.Since(launchTime).Milliseconds(), 1),
				AgentType:  launch.Started.SubagentType,
			})
			return
		}

		idleTicks++
		if idleTicks == 5 {
			ticker.Reset(pollInterval * 2)
		} else if idleTicks == 15 {
			ticker.Reset(pollInterval * 4)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func resolveSubagentModelAndEffort(rawInput json.RawMessage, parentModel, parentEffort string) (model, effort string) {
	var requested string
	if len(rawInput) > 0 {
		var params struct {
			Model string `json:"Model"`
		}
		if json.Unmarshal(rawInput, &params) == nil {
			requested = strings.TrimSpace(params.Model)
		}
	}
	if requested == "" || strings.EqualFold(requested, "inherit") {
		base, parsedEffort := ParseAgyTranscriptModel(parentModel)
		if parsedEffort == "" {
			parsedEffort = strings.TrimSpace(parentEffort)
		}
		return base, parsedEffort
	}
	return ParseAgyTranscriptModel(requested)
}

func (f *Flavor) findChildIDForLaunch(parentSessionID, launchToolCallID string) (string, bool) {
	f.subagentMu.Lock()
	if cached, ok := f.subagentChildByLaunch[parentSessionID+":"+launchToolCallID]; ok && cached != "" {
		f.subagentMu.Unlock()
		return cached, true
	}
	f.subagentMu.Unlock()

	brainDir := filepath.Join(f.AppDataDir, "brain")
	subDir := filepath.Join(brainDir, parentSessionID, ".system_generated", "subagents")
	entries, err := os.ReadDir(subDir)
	if err != nil || len(entries) == 0 {
		return "", false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		childID := strings.TrimSuffix(name, ".json")
		if !isSafeConversationID(childID) {
			continue
		}
		if foundLaunchID, ok := f.findLaunchToolCallIDForChild(parentSessionID, childID); ok {
			f.rememberChildLaunch(parentSessionID, childID, foundLaunchID)
			if foundLaunchID == launchToolCallID {
				return childID, true
			}
		}
	}
	return "", false
}

func (f *Flavor) findLaunchToolCallIDForChild(parentSessionID, childID string) (string, bool) {
	brainDir := filepath.Join(f.AppDataDir, "brain")
	msgDir := filepath.Join(brainDir, childID, ".system_generated", "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil || len(entries) == 0 {
		return "", false
	}
	// Scan in reverse so newly written messages are inspected first in long sessions.
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		name := entry.Name()
		if entry.IsDir() || name == "read.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, ok := readContainedSmallFile(brainDir, childID, filepath.Join(msgDir, name))
		if !ok {
			continue
		}
		var msg inboxMessageRecord
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.Sender != parentSessionID || msg.SourceMetadata == nil || msg.SourceMetadata.Tool == nil || msg.SourceMetadata.Tool.ToolCall == nil {
			continue
		}
		tool := msg.SourceMetadata.Tool
		if tool.ConversationID != "" && tool.ConversationID != parentSessionID {
			continue
		}
		if tool.ToolCall.Name == "invoke_subagent" && strings.TrimSpace(tool.ToolCall.ID) != "" {
			return strings.TrimSpace(tool.ToolCall.ID), true
		}
	}
	return "", false
}

func (f *Flavor) readSubagentRecord(parentSessionID, childID string) (subagentRecord, bool) {
	brainDir := filepath.Join(f.AppDataDir, "brain")
	path := filepath.Join(brainDir, parentSessionID, ".system_generated", "subagents", childID+".json")
	data, ok := readContainedSmallFile(brainDir, parentSessionID, path)
	if !ok {
		return subagentRecord{}, false
	}
	var rec subagentRecord
	if json.Unmarshal(data, &rec) != nil {
		return subagentRecord{}, false
	}
	return rec, true
}

// scanParentInbox inspects `<parent>/.system_generated/messages/` in a single
// pass for agy's own parent-inbox signals for childID:
//   - a direct report from childID (`sender == childID`) -> "completed"
//   - agy's subagent execution error notice (`sender == "system"`,
//     `with id "<childID>" encountered an error`) -> "failed"
//   - agy's idle-subagent guard notice (`sender == "system"`, mentioning
//     childID going idle) -> "completed"
//   - a post-launch broadcast stop notice (`sender == "system"`,
//     "All your subagents and background tasks have been stopped") -> "killed"
func (f *Flavor) scanParentInbox(parentSessionID, childID string, since time.Time) (status string, report string, found bool) {
	brainDir := filepath.Join(f.AppDataDir, "brain")
	msgDir := filepath.Join(brainDir, parentSessionID, ".system_generated", "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil || len(entries) == 0 {
		return "", "", false
	}
	childErrNeedle := `with id "` + childID + `" encountered an error`
	childIdleNeedle1 := "Subagent " + childID + " has gone idle"
	childIdleNeedle2 := "Subagent " + childID + " went idle without reporting"

	var systemStopped bool
	// Scan in reverse so newly written messages are inspected first in long sessions.
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		name := entry.Name()
		if entry.IsDir() || name == "read.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, ok := readContainedSmallFile(brainDir, parentSessionID, filepath.Join(msgDir, name))
		if !ok {
			continue
		}
		var msg inboxMessageRecord
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.Sender == childID {
			return "completed", strings.TrimSpace(msg.Content), true
		}
		if msg.Sender == "system" {
			content := strings.TrimSpace(msg.Content)
			if strings.Contains(content, childErrNeedle) {
				return "failed", content, true
			}
			if strings.Contains(content, childIdleNeedle1) || strings.Contains(content, childIdleNeedle2) {
				return "completed", content, true
			}
			if !systemStopped && strings.Contains(content, "All your subagents and background tasks have been stopped") {
				if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(msg.Timestamp)); err == nil {
					if !ts.Before(since.Add(-1 * time.Second)) {
						systemStopped = true
					}
				}
			}
		}
	}
	if systemStopped {
		return "killed", "", true
	}
	return "", "", false
}

func (f *Flavor) emitSubagentIdentityMeta(launch acp.SubagentLaunchContext, childID, baseModel, effort string) {
	fields := map[string]any{
		"task_id":  childID,
		"agent_id": childID,
	}
	if baseModel != "" {
		fields["subagent_model"] = baseModel
	}
	if effort != "" {
		fields["run_effort"] = effort
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		return
	}
	launch.Emit(provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        launch.ThreadID,
		TurnID:          launch.TurnID,
		ItemID:          launch.ToolCallID,
		ParentToolUseID: launch.ParentToolUseID,
		Meta:            meta,
		Timestamp:       time.Now(),
	})
}

func (f *Flavor) emitSubagentSettlement(launch acp.SubagentLaunchContext, taskID, status, report string, usage provider.SubagentProgressMeta) {
	metaFields := map[string]any{
		"task_id":     taskID,
		"tool_use_id": launch.ToolCallID,
		"status":      status,
		"source":      "agy_inbox",
		"usage":       usage,
	}
	if launch.ParentToolUseID != "" {
		metaFields["parent_tool_use_id"] = launch.ParentToolUseID
	}
	meta, err := json.Marshal(metaFields)
	if err != nil {
		return
	}
	launch.Emit(provider.ProviderEvent{
		Kind:            provider.EventBackgroundTaskNotification,
		ThreadID:        launch.ThreadID,
		TurnID:          launch.TurnID,
		ItemID:          launch.ToolCallID,
		ParentToolUseID: launch.ParentToolUseID,
		Content:         report,
		Meta:            meta,
		Timestamp:       time.Now(),
	})
}

func readContainedSmallFile(brainDir, convID, path string) ([]byte, bool) {
	if !isSafeConversationID(convID) {
		return nil, false
	}
	resolvedBrain, err := filepath.EvalSymlinks(brainDir)
	if err != nil {
		return nil, false
	}
	expectedDir := filepath.Join(resolvedBrain, convID)
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, false
	}
	rel, err := filepath.Rel(expectedDir, resolvedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, false
	}
	f, err := os.OpenFile(resolvedPath, os.O_RDONLY|openNonBlockFlag(), 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSubagentMetaFileBytes {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSubagentMetaFileBytes+1))
	if err != nil || len(data) > maxSubagentMetaFileBytes {
		return nil, false
	}
	return data, true
}

func resolveTranscriptURI(uri, fallback string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return fallback
	}
	if !strings.HasPrefix(uri, "file://") {
		if filepath.IsAbs(uri) {
			return filepath.Clean(uri)
		}
		return fallback
	}
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return fallback
	}
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return fallback
	}
	p := parsed.Path
	if len(p) >= 3 && p[0] == '/' && ((p[1] >= 'A' && p[1] <= 'Z') || (p[1] >= 'a' && p[1] <= 'z')) && p[2] == ':' {
		p = p[1:]
	}
	if p == "" {
		return fallback
	}
	return filepath.Clean(filepath.FromSlash(p))
}
