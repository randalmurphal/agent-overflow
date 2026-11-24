package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	maxTranscriptReadChunkBytes     = 1 << 20 // 1 MiB per poll chunk
	maxTranscriptLineBytes          = 2 << 20 // 2 MiB max single JSONL line
	maxFinalTranscriptDrainChunks   = 8
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
// subagent launch records, inbox receipts, and child transcript logs under
// AppDataDir.
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

type transcriptStep struct {
	StepIndex       int                  `json:"step_index"`
	Source          string               `json:"source"`
	Type            string               `json:"type"`
	Status          string               `json:"status"`
	CreatedAt       string               `json:"created_at"`
	Model           string               `json:"model"`
	InputTokens     int                  `json:"input_tokens"`
	CacheReadTokens int                  `json:"cache_read_tokens"`
	OutputTokens    int                  `json:"output_tokens"`
	Thinking        string               `json:"thinking"`
	Content         string               `json:"content"`
	Error           string               `json:"error"`
	ToolCalls       []transcriptToolCall `json:"tool_calls"`
}

type transcriptToolCall struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

type pendingChildTool struct {
	id       string
	name     string
	rawInput json.RawMessage
	started  acp.NormalizedToolStart
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
// ID via agy's `.system_generated/subagents/` + child inbox launch record,
// tails the child's `transcript_full.jsonl` (falling back to `transcript.jsonl`)
// forward-only with an os.Stat size gate to populate the agent pane and stamp
// the resolved model/effort, and settles strictly from agy's parent-inbox
// signals or SUBAGENT_STATE_KILLED.
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

	fallbackModel, fallbackEffort, isTierAlias := resolveSubagentModelAndEffort(launch.RawInput, launch.ParentModel, launch.ParentEffort)
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

	// Stamp child task_id/agent_id immediately. For tier aliases ("flash",
	// "flash_lite", "pro"), defer stamping subagent_model/run_effort until the
	// first PLANNER_RESPONSE reveals the actual resolved model and effort so
	// keepFirstLaunchStamps preserves the real values instead of an empty tier.
	var modelStamped bool
	if isTierAlias {
		f.emitSubagentIdentityMeta(launch, childID, "", "")
	} else {
		f.emitSubagentIdentityMeta(launch, childID, fallbackModel, fallbackEffort)
		if fallbackModel != "" && fallbackEffort != "" {
			modelStamped = true
		}
	}

	brainDir := filepath.Join(f.AppDataDir, "brain")
	fullPath := filepath.Join(brainDir, childID, ".system_generated", "logs", "transcript_full.jsonl")
	compactPath := filepath.Join(brainDir, childID, ".system_generated", "logs", "transcript.jsonl")
	var activeTranscriptPath string
	var transcriptOffset int64
	var transcriptRemainder string

	startedAt := launchTime
	haveStepStart := false
	var toolUses int
	var totalTokens int64
	var lastToolName, lastActivity, lastAssistantText string
	var pendingTools []pendingChildTool

	buildProgress := func(now time.Time) provider.SubagentProgressMeta {
		dur := now.Sub(startedAt).Milliseconds()
		if dur < 1 {
			dur = 1
		}
		return provider.SubagentProgressMeta{
			TaskID:       childID,
			ToolUses:     toolUses,
			TotalTokens:  totalTokens,
			DurationMs:   dur,
			Activity:     lastActivity,
			LastToolName: lastToolName,
			AgentType:    launch.Started.SubagentType,
		}
	}

	drainTranscriptOnce := func() bool {
		if activeTranscriptPath == "" {
			if st, err := os.Stat(fullPath); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				activeTranscriptPath = fullPath
			} else if st, err := os.Stat(compactPath); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
				activeTranscriptPath = compactPath
			} else {
				return false
			}
		}
		lines := f.readNewJSONLLinesContained(brainDir, childID, activeTranscriptPath, &transcriptOffset, &transcriptRemainder)
		if len(lines) == 0 {
			return false
		}
		progressDirty := false
		for _, line := range lines {
			var step transcriptStep
			if json.Unmarshal([]byte(line), &step) != nil {
				continue
			}
			stepTime := parseStepTime(step.CreatedAt)
			if !haveStepStart && !stepTime.IsZero() {
				startedAt = stepTime
				haveStepStart = true
			}
			if stepTime.IsZero() {
				stepTime = time.Now()
			}

			switch strings.ToUpper(strings.TrimSpace(step.Type)) {
			case "PLANNER_RESPONSE":
				if !modelStamped && strings.TrimSpace(step.Model) != "" {
					baseModel, effort := ParseAgyTranscriptModel(step.Model)
					if baseModel != "" {
						f.emitSubagentIdentityMeta(launch, childID, baseModel, effort)
						modelStamped = true
					}
				}
				stepSpend := int64(max(step.InputTokens, 0) + max(step.OutputTokens, 0))
				if stepSpend > 0 {
					totalTokens += stepSpend
					progressDirty = true
				}
				if thinking := strings.TrimSpace(step.Thinking); thinking != "" {
					thinkMeta, _ := json.Marshal(map[string]any{
						provider.MetaTranscriptSnapshotKey: true,
					})
					launch.Emit(provider.ProviderEvent{
						Kind:            provider.EventThinking,
						ThreadID:        launch.ThreadID,
						TurnID:          launch.TurnID,
						ItemID:          fmt.Sprintf("agy-sub:%s:think:%d", childID, step.StepIndex),
						ParentToolUseID: launch.ToolCallID,
						Content:         thinking,
						Meta:            thinkMeta,
						Timestamp:       stepTime,
					})
				}
				if text := strings.TrimSpace(step.Content); text != "" {
					emitChildAssistantText(launch, fmt.Sprintf("agy-sub:%s:text:%d", childID, step.StepIndex), text, stepTime)
					lastAssistantText = text
				}
				for idx, tc := range step.ToolCalls {
					rawArgs := decodeTranscriptToolArgs(tc.Args)
					tcID := fmt.Sprintf("agy-sub:%s:tool:%d:%d", childID, step.StepIndex, idx)
					u := acp.ToolCallUpdate{
						ToolCallID: tcID,
						Name:       tc.Name,
						Status:     "in_progress",
						RawInput:   rawArgs,
					}
					norm := f.NormalizeToolStart(&u)
					launch.Emit(provider.ProviderEvent{
						Kind:            provider.EventToolStart,
						ThreadID:        launch.ThreadID,
						TurnID:          launch.TurnID,
						ItemID:          tcID,
						ItemType:        norm.ItemType,
						ParentToolUseID: launch.ToolCallID,
						Meta:            acp.BuildToolStartMeta(norm),
						Timestamp:       stepTime,
					})
					pendingTools = append(pendingTools, pendingChildTool{
						id:       tcID,
						name:     tc.Name,
						rawInput: rawArgs,
						started:  norm,
					})
					toolUses++
					lastToolName = norm.ToolName
					if act := formatChildToolActivity(norm); act != "" {
						lastActivity = act
					}
					progressDirty = true
				}

			case "GENERIC":
				if len(pendingTools) > 0 {
					pt := pendingTools[0]
					pendingTools = pendingTools[1:]
					status := "completed"
					content := step.Content
					if strings.EqualFold(strings.TrimSpace(step.Status), "ERROR") || strings.TrimSpace(step.Error) != "" {
						status = "failed"
						if strings.TrimSpace(content) == "" {
							content = step.Error
						}
					}
					acpUpdate := acp.ToolCallUpdate{
						ToolCallID: pt.id,
						Name:       pt.name,
						Status:     status,
						RawInput:   pt.rawInput,
					}
					if content != "" {
						acpUpdate.Content = []acp.ToolContentItem{{
							Type:    "content",
							Content: &acp.ContentBlock{Type: "text", Text: content},
						}}
					}
					doneNorm := f.NormalizeToolComplete(&acpUpdate, pt.started)
					for _, evt := range acp.BuildToolCompleteEvents(launch.ThreadID, launch.TurnID, pt.id, doneNorm, stepTime) {
						evt.ParentToolUseID = launch.ToolCallID
						launch.Emit(evt)
					}
					progressDirty = true
				}
			}
		}
		if progressDirty {
			f.emitSubagentProgress(launch, buildProgress(time.Now()))
		}
		return true
	}

	settle := func(status, report string) {
		// Drain any final lines appended concurrently with the parent inbox signal.
		for i := 0; i < maxFinalTranscriptDrainChunks; i++ {
			if !drainTranscriptOnce() {
				break
			}
		}
		if !modelStamped && (fallbackModel != "" || fallbackEffort != "") {
			f.emitSubagentIdentityMeta(launch, childID, fallbackModel, fallbackEffort)
			modelStamped = true
		}
		// Close any in-flight child tool call rows if the subagent ended abruptly.
		now := time.Now()
		for _, pt := range pendingTools {
			termStatus := "failed"
			if status == "killed" {
				termStatus = "cancelled"
			}
			acpUpdate := acp.ToolCallUpdate{
				ToolCallID: pt.id,
				Name:       pt.name,
				Status:     termStatus,
				RawInput:   pt.rawInput,
			}
			doneNorm := f.NormalizeToolComplete(&acpUpdate, pt.started)
			for _, evt := range acp.BuildToolCompleteEvents(launch.ThreadID, launch.TurnID, pt.id, doneNorm, now) {
				evt.ParentToolUseID = launch.ToolCallID
				launch.Emit(evt)
			}
		}
		pendingTools = nil

		// If the subagent reported back without writing assistant text into its
		// transcript, surface the returned report inside the agent pane so the
		// pane never appears blank with only the opening prompt.
		if status == "completed" && strings.TrimSpace(report) != "" && strings.TrimSpace(lastAssistantText) == "" {
			emitChildAssistantText(launch, fmt.Sprintf("agy-sub:%s:report", childID), strings.TrimSpace(report), now)
		}

		finalProgress := buildProgress(now)
		f.emitSubagentProgress(launch, finalProgress)
		f.emitSubagentSettlement(launch, childID, status, report, finalProgress)
	}

	idleTicks := 0
	for {
		if drainTranscriptOnce() {
			if idleTicks > 0 {
				idleTicks = 0
				ticker.Reset(pollInterval)
			}
		}
		if status, report, ok := f.scanParentInbox(launch.SessionID, childID, launchTime); ok {
			settle(status, report)
			return
		}
		if rec, ok := f.readSubagentRecord(launch.SessionID, childID); ok && rec.State == "SUBAGENT_STATE_KILLED" {
			settle("killed", "")
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

func emitChildAssistantText(launch acp.SubagentLaunchContext, itemID, text string, ts time.Time) {
	launch.Emit(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        launch.ThreadID,
		TurnID:          launch.TurnID,
		ItemID:          itemID,
		ParentToolUseID: launch.ToolCallID,
		Content:         text,
		Timestamp:       ts,
	})
	stopMeta, _ := json.Marshal(map[string]any{"blockType": "text"})
	launch.Emit(provider.ProviderEvent{
		Kind:            provider.EventContentBlockStop,
		ThreadID:        launch.ThreadID,
		TurnID:          launch.TurnID,
		ItemID:          itemID,
		ParentToolUseID: launch.ToolCallID,
		Meta:            stopMeta,
		Timestamp:       ts,
	})
}

func isAgyModelTierAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "flash", "flash_lite", "flash-lite", "pro":
		return true
	default:
		return false
	}
}

func resolveSubagentModelAndEffort(rawInput json.RawMessage, parentModel, parentEffort string) (model, effort string, isTierAlias bool) {
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
		return base, parsedEffort, false
	}
	base, parsedEffort := ParseAgyTranscriptModel(requested)
	return base, parsedEffort, isAgyModelTierAlias(requested)
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

func (f *Flavor) emitSubagentProgress(launch acp.SubagentLaunchContext, progress provider.SubagentProgressMeta) {
	meta, err := json.Marshal(progress)
	if err != nil {
		return
	}
	launch.Emit(provider.ProviderEvent{
		Kind:            provider.EventSubagentProgress,
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

// decodeTranscriptToolArgs unwraps agy's JSON-encoded string argument map
// (e.g. `{"Path":"\"/a/b.go\"","StartLine":"1"}`) into a standard JSON object.
func decodeTranscriptToolArgs(args map[string]string) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, rawVal := range args {
		var parsed any
		if json.Unmarshal([]byte(rawVal), &parsed) == nil {
			out[k] = parsed
		} else {
			out[k] = rawVal
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return encoded
}

func formatChildToolActivity(norm acp.NormalizedToolStart) string {
	name := strings.TrimSpace(norm.ToolName)
	if name == "" {
		return ""
	}
	if len(norm.Input) == 0 {
		return name
	}
	var obj map[string]any
	if json.Unmarshal(norm.Input, &obj) != nil {
		return name
	}
	for _, key := range []string{"description", "command", "file_path", "pattern", "query", "url", "message"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			val := strings.TrimSpace(v)
			if key == "file_path" {
				val = filepath.Base(val)
			}
			if len(val) > 64 {
				val = val[:64] + "..."
			}
			return name + ": " + val
		}
	}
	return name
}

func parseStepTime(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// openContainedTranscript traverses brainDir -> sessionID -> relFile strictly
// through nested os.OpenRoot handles (kernel-enforced symlink containment with
// no directory-replacement TOCTOU) and calls Lstat before Open so a Unix FIFO
// or device node can never block open(2) during session cancellation.
func openContainedTranscript(brainDir, sessionID, path string) (*os.File, error) {
	if strings.TrimSpace(brainDir) == "" || !isSafeConversationID(sessionID) || strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	brainReal, err := filepath.EvalSymlinks(brainDir)
	if err != nil {
		return nil, err
	}
	brainRoot, err := os.OpenRoot(brainReal)
	if err != nil {
		return nil, err
	}
	defer func() { _ = brainRoot.Close() }()

	dirInfo, err := brainRoot.Lstat(sessionID)
	if err != nil {
		return nil, err
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, os.ErrPermission
	}
	sessionRoot, err := brainRoot.OpenRoot(sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sessionRoot.Close() }()
	openedDirInfo, err := sessionRoot.Stat(".")
	if err != nil || !openedDirInfo.IsDir() || !os.SameFile(dirInfo, openedDirInfo) {
		if err != nil {
			return nil, err
		}
		return nil, os.ErrPermission
	}

	expectedSessionDir := filepath.Join(brainDir, sessionID)
	relFile, err := filepath.Rel(expectedSessionDir, path)
	if err != nil || relFile == "" || relFile == "." || relFile == ".." || strings.HasPrefix(relFile, ".."+string(os.PathSeparator)) {
		if relReal, relErr := filepath.Rel(filepath.Join(brainReal, sessionID), path); relErr == nil &&
			relReal != "" && relReal != "." && relReal != ".." && !strings.HasPrefix(relReal, ".."+string(os.PathSeparator)) {
			relFile = relReal
		} else {
			return nil, os.ErrPermission
		}
	}
	linfo, err := sessionRoot.Lstat(relFile)
	if err != nil {
		return nil, err
	}
	if !linfo.Mode().IsRegular() {
		return nil, os.ErrPermission
	}
	f, err := sessionRoot.OpenFile(relFile, os.O_RDONLY|openNonBlockFlag(), 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(linfo, info) {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, os.ErrPermission
	}
	return f, nil
}

// readNewJSONLLinesContained checks the file size with os.Stat first so idle
// ticks at EOF avoid opening any file descriptors, then reads up to
// maxTranscriptReadChunkBytes of newly appended newline-terminated JSONL lines
// through openContainedTranscript.
func (f *Flavor) readNewJSONLLinesContained(brainDir, sessionID, path string, offset *int64, remainder *string) []string {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Size() == *offset {
		return nil
	}
	if f.subagentTranscriptOpenHook != nil {
		f.subagentTranscriptOpenHook()
	}
	file, err := openContainedTranscript(brainDir, sessionID, path)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	if size == *offset {
		return nil
	}
	if size < *offset {
		*offset = 0
		*remainder = ""
	}
	if *offset > 0 {
		if _, err := file.Seek(*offset, io.SeekStart); err != nil {
			return nil
		}
	}
	toRead := size - *offset
	if toRead > maxTranscriptReadChunkBytes {
		toRead = maxTranscriptReadChunkBytes
	}
	buf := make([]byte, toRead)
	n, err := file.Read(buf)
	if n <= 0 || (err != nil && err != io.EOF) {
		return nil
	}
	*offset += int64(n)

	chunk := *remainder + string(buf[:n])
	var lines []string
	for {
		nl := strings.IndexByte(chunk, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(chunk[:nl])
		chunk = chunk[nl+1:]
		if line != "" && len(line) <= maxTranscriptLineBytes && bytes.HasPrefix([]byte(line), []byte("{")) {
			lines = append(lines, line)
		}
	}
	if len(chunk) > maxTranscriptLineBytes {
		chunk = ""
	}
	*remainder = chunk
	return lines
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
