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
	"sync"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

const (
	defaultSubagentPollInterval      = 150 * time.Millisecond
	defaultSubagentDiscoveryTimeout  = 15 * time.Second
	defaultSubagentSettleGrace       = 400 * time.Millisecond
	defaultSubagentParentReportGrace = 1200 * time.Millisecond
)

// CanTrackSubagent reports whether this flavor can watch Antigravity's
// out-of-band subagent transcript.jsonl files for sessionID.
func (f *Flavor) CanTrackSubagent(sessionID string) bool {
	return strings.TrimSpace(f.AppDataDir) != "" && strings.TrimSpace(sessionID) != ""
}

// ParseAgyTranscriptModel splits an Antigravity transcript model identifier
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

type pendingParentSpawn struct {
	role     string
	typeName string
	prompt   string
}

type discoveredSubagent struct {
	conversationID string
	logPath        string
	role           string
	typeName       string
	prompt         string
	claimed        bool
}

type parentSubagentCoordinator struct {
	mu           sync.Mutex
	path         string
	offset       int64
	remainder    string
	pendingSpawn []pendingParentSpawn
	discovered   []*discoveredSubagent
	seenConvID   map[string]bool
	reports      map[string]string
}

func (f *Flavor) parentCoordinator(sessionID string) *parentSubagentCoordinator {
	f.subagentMu.Lock()
	defer f.subagentMu.Unlock()
	if f.subagentParents == nil {
		f.subagentParents = make(map[string]*parentSubagentCoordinator)
	}
	c, ok := f.subagentParents[sessionID]
	if !ok {
		c = &parentSubagentCoordinator{
			path:       transcriptPathForSession(f.AppDataDir, sessionID),
			seenConvID: make(map[string]bool),
			reports:    make(map[string]string),
		}
		f.subagentParents[sessionID] = c
	}
	return c
}

func transcriptPathForSession(appDataDir, sessionID string) string {
	return filepath.Join(appDataDir, "brain", sessionID, ".system_generated", "logs", "transcript.jsonl")
}

// WatchSubagent discovers the child subagent conversation created by an
// invoke_subagent tool call and tails its transcript.jsonl live, emitting
// resolved model/effort stamps, child transcript items, live subagent progress,
// and the settling EventBackgroundTaskNotification.
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
	settleGrace := f.subagentSettleGrace
	if settleGrace <= 0 {
		settleGrace = defaultSubagentSettleGrace
	}

	wantRole, wantType, wantPrompt := parseInvokeSubagentInput(launch.RawInput)
	coord := f.parentCoordinator(launch.SessionID)
	launchTime := time.Now()
	discoveryDeadline := launchTime.Add(discoveryTimeout)

	var child *discoveredSubagent
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for child == nil {
		coord.refresh(f.AppDataDir)
		child = coord.claim(wantRole, wantType, wantPrompt)
		if child != nil {
			break
		}
		if time.Now().After(discoveryDeadline) {
			// Fallback settle so a launch whose transcript never materialized
			// never stays a running zombie in the tray.
			f.emitSubagentSettlement(launch, launch.ToolCallID, "completed", "", provider.SubagentProgressMeta{
				TaskID:     launch.ToolCallID,
				DurationMs: max(time.Since(launchTime).Milliseconds(), 1),
				AgentType:  launch.Started.SubagentType,
			})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}

	childID := child.conversationID
	childPath := child.logPath
	if childPath == "" {
		childPath = transcriptPathForSession(f.AppDataDir, childID)
	}

	// Stamp task_id and agent_id onto the launch row immediately so the
	// background tray and Agent Pane correlate right away.
	f.emitSubagentIdentityMeta(launch, childID, "", "")

	type pendingChildTool struct {
		id       string
		name     string
		rawInput json.RawMessage
		started  acp.NormalizedToolStart
	}

	var (
		childOffset        int64
		childRemainder     string
		startedAt          = launchTime
		haveStepStart      bool
		modelStamped       bool
		toolUses           int
		totalTokens        int64
		lastToolName       string
		lastActivity       string
		lastAssistantText  string
		lastSentMessage    string
		lastError          string
		pendingTools       []pendingChildTool
		childDoneSeenAt    time.Time
		parentReport       string
		parentReportSeenAt time.Time
	)

	buildProgress := func(now time.Time) provider.SubagentProgressMeta {
		dur := now.Sub(startedAt).Milliseconds()
		if dur < 0 {
			dur = 0
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

	for {
		lines := readNewJSONLLines(childPath, &childOffset, &childRemainder)
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
					textID := fmt.Sprintf("agy-sub:%s:text:%d", childID, step.StepIndex)
					launch.Emit(provider.ProviderEvent{
						Kind:            provider.EventTextDelta,
						ThreadID:        launch.ThreadID,
						TurnID:          launch.TurnID,
						ItemID:          textID,
						ParentToolUseID: launch.ToolCallID,
						Content:         text,
						Timestamp:       stepTime,
					})
					stopMeta, _ := json.Marshal(map[string]any{"blockType": "text"})
					launch.Emit(provider.ProviderEvent{
						Kind:            provider.EventContentBlockStop,
						ThreadID:        launch.ThreadID,
						TurnID:          launch.TurnID,
						ItemID:          textID,
						ParentToolUseID: launch.ToolCallID,
						Meta:            stopMeta,
						Timestamp:       stepTime,
					})
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
					if strings.EqualFold(tc.Name, "send_message") {
						if msg := extractSentMessageBody(rawArgs); msg != "" {
							lastSentMessage = msg
						}
					}
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
				if len(step.ToolCalls) == 0 && len(pendingTools) == 0 && strings.EqualFold(strings.TrimSpace(step.Status), "DONE") {
					if childDoneSeenAt.IsZero() {
						childDoneSeenAt = time.Now()
					}
				} else if len(step.ToolCalls) > 0 {
					childDoneSeenAt = time.Time{}
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
				} else if strings.EqualFold(strings.TrimSpace(step.Status), "ERROR") || strings.TrimSpace(step.Error) != "" {
					lastError = firstNonEmpty(step.Error, step.Content)
				}

			default:
				if strings.EqualFold(strings.TrimSpace(step.Status), "ERROR") || strings.TrimSpace(step.Error) != "" {
					lastError = firstNonEmpty(step.Error, step.Content)
				}
			}
		}

		if progressDirty {
			f.emitSubagentProgress(launch, buildProgress(time.Now()))
		}

		coord.refresh(f.AppDataDir)
		if parentReport == "" {
			if r := coord.reportFor(childID); r != "" {
				parentReport = r
				parentReportSeenAt = time.Now()
			}
		}

		parentReportGrace := defaultSubagentParentReportGrace
		if f.subagentSettleGrace > 0 {
			parentReportGrace = settleGrace
		}
		now := time.Now()
		settledByChildDone := !childDoneSeenAt.IsZero() && (parentReport != "" || now.Sub(childDoneSeenAt) >= settleGrace)
		settledByParentReport := parentReport != "" && len(pendingTools) == 0 && now.Sub(parentReportSeenAt) >= parentReportGrace
		if settledByChildDone || settledByParentReport {
			finalProgress := buildProgress(now)
			f.emitSubagentProgress(launch, finalProgress)
			report := firstNonEmpty(parentReport, lastSentMessage, lastAssistantText)
			finalStatus := "completed"
			if report == "" && lastError != "" {
				finalStatus = "failed"
				report = lastError
			}
			f.emitSubagentSettlement(launch, childID, finalStatus, report, finalProgress)
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *Flavor) seedExistingSubagents(sessionID string) {
	if !f.CanTrackSubagent(sessionID) {
		return
	}
	coord := f.parentCoordinator(sessionID)
	coord.mu.Lock()
	defer coord.mu.Unlock()
	if coord.offset > 0 {
		// Already initialized for this session; do not discard in-flight
		// discoveries from earlier turns in the same live session.
		return
	}
	lines := readNewJSONLLines(coord.path, &coord.offset, &coord.remainder)
	for _, line := range lines {
		var step transcriptStep
		if json.Unmarshal([]byte(line), &step) != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(step.Type), "GENERIC") {
			if convID, _ := parseCreatedSubagentResult(step.Content, f.AppDataDir); convID != "" {
				coord.seenConvID[convID] = true
			}
		}
	}
}

func (f *Flavor) emitSubagentIdentityMeta(launch acp.SubagentLaunchContext, childID, baseModel, effort string) {
	// Omit toolName and input while carrying task_id/subagent_model so triage's
	// ToolStartMeta.isMetaUpdateOnly() routes this as a correlation update
	// without stamping an extraneous meta_update_only flag onto persisted meta.
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
		Kind:      provider.EventToolStart,
		ThreadID:  launch.ThreadID,
		TurnID:    launch.TurnID,
		ItemID:    launch.ToolCallID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (f *Flavor) emitSubagentProgress(launch acp.SubagentLaunchContext, progress provider.SubagentProgressMeta) {
	meta, err := json.Marshal(progress)
	if err != nil {
		return
	}
	launch.Emit(provider.ProviderEvent{
		Kind:      provider.EventSubagentProgress,
		ThreadID:  launch.ThreadID,
		TurnID:    launch.TurnID,
		ItemID:    launch.ToolCallID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (f *Flavor) emitSubagentSettlement(launch acp.SubagentLaunchContext, taskID, status, report string, usage provider.SubagentProgressMeta) {
	meta, err := json.Marshal(map[string]any{
		"task_id":     taskID,
		"tool_use_id": launch.ToolCallID,
		"status":      status,
		"source":      "agy_transcript",
		"usage":       usage,
	})
	if err != nil {
		return
	}
	launch.Emit(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  launch.ThreadID,
		TurnID:    launch.TurnID,
		ItemID:    launch.ToolCallID,
		Content:   report,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (c *parentSubagentCoordinator) refresh(appDataDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := readNewJSONLLines(c.path, &c.offset, &c.remainder)
	for _, line := range lines {
		var step transcriptStep
		if json.Unmarshal([]byte(line), &step) != nil {
			continue
		}
		stepType := strings.ToUpper(strings.TrimSpace(step.Type))
		switch stepType {
		case "PLANNER_RESPONSE":
			for _, tc := range step.ToolCalls {
				if strings.EqualFold(strings.TrimSpace(tc.Name), "invoke_subagent") {
					rawArgs := decodeTranscriptToolArgs(tc.Args)
					role, typeName, prompt := parseInvokeSubagentInput(rawArgs)
					c.pendingSpawn = append(c.pendingSpawn, pendingParentSpawn{
						role:     role,
						typeName: typeName,
						prompt:   prompt,
					})
				}
			}
		case "GENERIC":
			convID, logPath := parseCreatedSubagentResult(step.Content, appDataDir)
			if convID == "" || c.seenConvID[convID] {
				continue
			}
			c.seenConvID[convID] = true
			entry := &discoveredSubagent{
				conversationID: convID,
				logPath:        logPath,
			}
			if len(c.pendingSpawn) > 0 {
				sp := c.pendingSpawn[0]
				c.pendingSpawn = c.pendingSpawn[1:]
				entry.role = sp.role
				entry.typeName = sp.typeName
				entry.prompt = sp.prompt
			}
			c.discovered = append(c.discovered, entry)
		case "SYSTEM_MESSAGE":
			if sender, body, ok := parseParentSystemMessage(step.Content); ok {
				c.reports[sender] = body
			}
		}
	}
}

func (c *parentSubagentCoordinator) claim(wantRole, wantType, wantPrompt string) *discoveredSubagent {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Exact match on role + typeName + prompt.
	for _, d := range c.discovered {
		if d.claimed {
			continue
		}
		if wantRole != "" && d.role == wantRole &&
			(wantType == "" || d.typeName == wantType) &&
			(wantPrompt == "" || d.prompt == wantPrompt || strings.HasPrefix(wantPrompt, d.prompt)) {
			d.claimed = true
			return d
		}
	}
	// Match on role + typeName (in case prompt was compacted in transcript.jsonl).
	for _, d := range c.discovered {
		if d.claimed {
			continue
		}
		if wantRole != "" && d.role == wantRole && (wantType == "" || d.typeName == wantType) {
			d.claimed = true
			return d
		}
	}
	// Positional fallback: first unclaimed discovered subagent.
	for _, d := range c.discovered {
		if !d.claimed {
			d.claimed = true
			return d
		}
	}
	return nil
}

func (c *parentSubagentCoordinator) reportFor(childID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reports[childID]
}

func parseInvokeSubagentInput(raw json.RawMessage) (role, typeName, prompt string) {
	if len(raw) == 0 {
		return "", "", ""
	}
	var params struct {
		Role     string `json:"Role"`
		TypeName string `json:"TypeName"`
		Prompt   string `json:"Prompt"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return "", "", ""
	}
	return strings.TrimSpace(params.Role), strings.TrimSpace(params.TypeName), strings.TrimSpace(params.Prompt)
}

func parseCreatedSubagentResult(content, appDataDir string) (conversationID, logPath string) {
	if !strings.Contains(content, "conversationId") {
		return "", ""
	}
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start >= 0 && end > start {
		var obj struct {
			ConversationID string `json:"conversationId"`
			LogAbsoluteURI string `json:"logAbsoluteUri"`
		}
		if json.Unmarshal([]byte(content[start:end+1]), &obj) == nil && strings.TrimSpace(obj.ConversationID) != "" {
			conversationID = strings.TrimSpace(obj.ConversationID)
			if uri := strings.TrimSpace(obj.LogAbsoluteURI); uri != "" {
				logPath = resolveTranscriptURI(uri, appDataDir)
			}
			return conversationID, logPath
		}
	}
	return extractConversationID(nil, content), ""
}

func resolveTranscriptURI(rawURI, appDataDir string) string {
	var p string
	if strings.HasPrefix(rawURI, "file://") {
		if parsed, err := url.Parse(rawURI); err == nil && parsed.Path != "" {
			p = parsed.Path
		} else {
			p = strings.TrimPrefix(rawURI, "file://")
		}
		// Strip leading slash on Windows drive-letter URIs like file:///C:/...
		if len(p) >= 3 && p[0] == '/' && ((p[1] >= 'A' && p[1] <= 'Z') || (p[1] >= 'a' && p[1] <= 'z')) && p[2] == ':' {
			p = p[1:]
		}
	} else {
		p = rawURI
	}
	p = filepath.Clean(p)
	if appDataDir != "" {
		base := filepath.Clean(appDataDir)
		rel, err := filepath.Rel(base, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return ""
		}
	}
	return p
}

// parseParentSystemMessage extracts sender conversationId and message content
// from agy's parent transcript SYSTEM_MESSAGE:
//
//	[Message] timestamp=... sender=<uuid> priority=... content=<message>
func parseParentSystemMessage(content string) (sender, body string, ok bool) {
	idx := strings.Index(content, "sender=")
	if idx < 0 {
		return "", "", false
	}
	rest := content[idx+len("sender="):]
	endSender := strings.IndexAny(rest, " \t\r\n")
	if endSender <= 0 {
		return "", "", false
	}
	sender = strings.TrimSpace(rest[:endSender])
	if sender == "" {
		return "", "", false
	}
	afterSender := rest[endSender:]
	contentIdx := strings.Index(afterSender, "content=")
	if contentIdx < 0 {
		return sender, "", true
	}
	body = afterSender[contentIdx+len("content="):]
	if closeTag := strings.Index(body, "</SYSTEM_MESSAGE>"); closeTag >= 0 {
		body = body[:closeTag]
	}
	return sender, strings.TrimSpace(body), true
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

func extractSentMessageBody(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Message string `json:"Message"`
		Msg     string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(obj.Message, obj.Msg))
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

// readNewJSONLLines reads newly appended complete newline-terminated JSONL
// lines from path starting at *offset, preserving any trailing partial line in
// *remainder for the next poll.
func readNewJSONLLines(path string, offset *int64, remainder *string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	if *offset > 0 {
		if _, err := f.Seek(*offset, io.SeekStart); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil
	}
	*offset += int64(len(data))

	buf := *remainder + string(data)
	var lines []string
	for {
		nl := strings.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(buf[:nl])
		buf = buf[nl+1:]
		if line != "" && bytes.HasPrefix([]byte(line), []byte("{")) {
			lines = append(lines, line)
		}
	}
	*remainder = buf
	return lines
}
