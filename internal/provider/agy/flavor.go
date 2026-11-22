package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

// Flavor implements acp.Flavor for Google Antigravity (`agy acp`).
type Flavor struct {
	acp.DefaultFlavor
	// AppDataDir is `<geminiDir>/antigravity-cli`, used for read-only
	// step_type=17 error detection and pending high-priority wake checks.
	AppDataDir string
}

// NewFlavor constructs an Antigravity ACP flavor bound to appDataDir.
func NewFlavor(appDataDir string) *Flavor {
	return &Flavor{
		DefaultFlavor: acp.DefaultFlavor{Kind: provider.Agy},
		AppDataDir:    strings.TrimSpace(appDataDir),
	}
}

func (f *Flavor) ProviderKind() provider.ProviderKind {
	return provider.Agy
}

func (f *Flavor) BuildSessionMeta(cfg acp.SessionConfig) map[string]any {
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(cfg.SystemPrompt); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(cfg.AppendSystemPrompt); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{
		"antigravity": map[string]any{
			"systemPrompt": map[string]any{
				"append": strings.Join(parts, "\n\n"),
			},
		},
	}
}

// ConvertUsage converts agy 1.5.x prompt usage to AO TokenUsage.
// Verified on agy 1.5.13: inputTokens is already non-cached input, and
// outputTokens already includes thoughtTokens.
func (f *Flavor) ConvertUsage(u acp.Usage, _ string) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:              max(u.InputTokens, 0),
		OutputTokens:             max(u.OutputTokens, 0),
		CacheReadInputTokens:     max(u.CachedReadTokens, 0),
		CacheCreationInputTokens: max(u.CachedWriteTokens, 0),
		ReasoningOutputTokens:    max(u.ThoughtTokens, 0),
	}
}

func (f *Flavor) NormalizeToolStart(u *acp.ToolCallUpdate) acp.NormalizedToolStart {
	norm := f.DefaultFlavor.NormalizeToolStart(u)
	rawIn := u.EffectiveInput()

	switch strings.ToLower(strings.TrimSpace(u.Name)) {
	case "read_file":
		norm.ToolName = "Read"
		norm.ItemType = "Read"
		path := extractLocationOrInputPath(u, rawIn, "Path", "file_path", "path")
		if path != "" {
			if reshaped, err := json.Marshal(map[string]any{"file_path": path}); err == nil {
				norm.Input = reshaped
			}
		}

	case "edit_file", "replace_file_content", "multi_replace_file_content":
		norm.ToolName = "Edit"
		norm.ItemType = "file_change"
		path := extractLocationOrInputPath(u, rawIn, "Path", "TargetFile", "file_path", "path")
		if path != "" {
			if reshaped, err := json.Marshal(map[string]any{"file_path": path}); err == nil {
				norm.Input = reshaped
			}
		}

	case "write_file":
		norm.ToolName = "Write"
		norm.ItemType = "file_change"
		path := extractLocationOrInputPath(u, rawIn, "Path", "TargetFile", "file_path", "path")
		if path != "" {
			if reshaped, err := json.Marshal(map[string]any{"file_path": path}); err == nil {
				norm.Input = reshaped
			}
		}

	case "run_command":
		norm.ToolName = "Bash"
		norm.ItemType = "command_execution"
		var params struct {
			CommandLine string `json:"CommandLine"`
			ToolSummary string `json:"toolSummary"`
			IsDaemon    bool   `json:"IsDaemon"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if params.CommandLine != "" {
				fields["command"] = params.CommandLine
			}
			if params.ToolSummary != "" {
				fields["description"] = params.ToolSummary
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "invoke_subagent":
		norm.ToolName = "Agent"
		norm.ItemType = "Agent"
		norm.SubagentLaunch = true
		var params struct {
			TypeName string `json:"TypeName"`
			Role     string `json:"Role"`
			Prompt   string `json:"Prompt"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			norm.SubagentType = strings.TrimSpace(params.TypeName)
			norm.Description = strings.TrimSpace(params.Role)
			if reshaped, err := json.Marshal(map[string]any{
				"subagent_type": norm.SubagentType,
				"description":   norm.Description,
				"prompt":        params.Prompt,
			}); err == nil {
				norm.Input = reshaped
			}
		}

	case "ask_question":
		norm.ToolName = "AskUserQuestion"
		norm.ItemType = "AskUserQuestion"
	}

	return norm
}

func (f *Flavor) NormalizeToolComplete(u *acp.ToolCallUpdate, started acp.NormalizedToolStart) acp.NormalizedToolComplete {
	norm := f.DefaultFlavor.NormalizeToolComplete(u, started)

	// Plan-mode artifact write_file carries ArtifactMetadata.RequestFeedback=true.
	if strings.EqualFold(strings.TrimSpace(u.Name), "write_file") {
		var params struct {
			Content          string `json:"Content"`
			ArtifactMetadata *struct {
				RequestFeedback bool `json:"RequestFeedback"`
				UserFacing      bool `json:"UserFacing"`
			} `json:"ArtifactMetadata"`
		}
		if len(u.EffectiveInput()) > 0 && json.Unmarshal(u.EffectiveInput(), &params) == nil {
			if params.ArtifactMetadata != nil && params.ArtifactMetadata.RequestFeedback && strings.TrimSpace(params.Content) != "" {
				norm.ProposedPlan = params.Content
			}
		}
	}

	return norm
}

func (f *Flavor) ClassifyPermission(threadID, turnID, requestID string, req acp.RequestPermissionParams, policy acp.PermissionPolicy) acp.PermissionClassification {
	cls := f.DefaultFlavor.ClassifyPermission(threadID, turnID, requestID, req, policy)
	if cls.Action == acp.PermissionAskUser {
		// Surface option._meta["agy.security.warning"] if present on any allow option.
		for _, opt := range req.Options {
			if len(opt.Meta) == 0 {
				continue
			}
			var meta map[string]json.RawMessage
			if json.Unmarshal(opt.Meta, &meta) != nil {
				continue
			}
			warnRaw, ok := meta["agy.security.warning"]
			if !ok || len(warnRaw) == 0 {
				continue
			}
			var warn struct {
				Title   string `json:"title"`
				Message string `json:"message"`
			}
			if json.Unmarshal(warnRaw, &warn) == nil && (warn.Title != "" || warn.Message != "") {
				note := strings.TrimSpace(strings.TrimSpace(warn.Title) + ": " + strings.TrimSpace(warn.Message))
				if cls.Approval.Description != "" {
					cls.Approval.Description = cls.Approval.Description + " (" + note + ")"
				} else {
					cls.Approval.Description = note
				}
			}
		}
	}
	return cls
}

// ExtractPendingQuestion extracts structured ask_question prompts when the tool
// transitions to status="pending".
func (f *Flavor) ExtractPendingQuestion(u *acp.ToolCallUpdate) ([]provider.UserInputQuestion, bool) {
	if !strings.EqualFold(strings.TrimSpace(u.Name), "ask_question") {
		return nil, false
	}
	raw := u.EffectiveInput()
	if len(raw) == 0 {
		return nil, false
	}
	var payload struct {
		Questions []struct {
			Question      string   `json:"question"`
			Options       []string `json:"options"`
			IsMultiSelect bool     `json:"is_multi_select"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Questions) == 0 {
		return nil, false
	}
	questions := make([]provider.UserInputQuestion, 0, len(payload.Questions))
	for _, q := range payload.Questions {
		if strings.TrimSpace(q.Question) == "" {
			continue
		}
		opts := make([]provider.UserInputQuestionOption, 0, len(q.Options))
		for _, opt := range q.Options {
			opts = append(opts, provider.UserInputQuestionOption{Label: opt})
		}
		questions = append(questions, provider.UserInputQuestion{
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.IsMultiSelect,
		})
	}
	if len(questions) == 0 {
		return nil, false
	}
	return questions, true
}

func (f *Flavor) ToolOutlivesPrompt(u *acp.ToolCallUpdate) bool {
	switch strings.ToLower(strings.TrimSpace(u.Name)) {
	case "schedule":
		return true
	case "run_command":
		var params struct {
			IsDaemon bool `json:"IsDaemon"`
		}
		if len(u.EffectiveInput()) > 0 && json.Unmarshal(u.EffectiveInput(), &params) == nil && params.IsDaemon {
			return false
		}
		return true
	default:
		return false
	}
}

func (f *Flavor) BeforePrompt(_ context.Context, sessionID string) any {
	if f.AppDataDir == "" || sessionID == "" {
		return nil
	}
	maxIdx, ok := readMaxStepIndex(f.AppDataDir, sessionID)
	if !ok {
		return nil
	}
	return maxIdx
}

func (f *Flavor) AfterPrompt(_ context.Context, sessionID string, stopReason string, beforeToken any) *acp.PromptOutcomeOverride {
	if f.AppDataDir == "" || sessionID == "" || stopReason == "cancelled" {
		return nil
	}
	sinceIdx := -1
	if v, ok := beforeToken.(int); ok {
		sinceIdx = v
	}
	return inspectTurnErrorStep(f.AppDataDir, sessionID, sinceIdx)
}

// ShouldHoldSpontaneousWakeClose checks `<appDataDir>/brain/<sessionId>/.system_generated/messages/`
// for unread MESSAGE_PRIORITY_HIGH wake messages queued behind the current wake.
func (f *Flavor) ShouldHoldSpontaneousWakeClose(_ context.Context, sessionID string) bool {
	if f.AppDataDir == "" || sessionID == "" {
		return false
	}
	msgDir := filepath.Join(f.AppDataDir, "brain", sessionID, ".system_generated", "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil || len(entries) == 0 {
		return false
	}
	readSet := map[string]bool{}
	if raw, err := os.ReadFile(filepath.Join(msgDir, "read.json")); err == nil {
		var ids []string
		if json.Unmarshal(raw, &ids) == nil {
			for _, id := range ids {
				readSet[id] = true
			}
		} else {
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				for k := range obj {
					readSet[k] = true
				}
			}
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "read.json" || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if readSet[id] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(msgDir, name))
		if err != nil {
			continue
		}
		var msg struct {
			Priority string `json:"priority"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Priority == "MESSAGE_PRIORITY_HIGH" {
			return true
		}
	}
	return false
}

// TranslateMCPServers converts AO's provider-neutral HTTP MCP server map into
// the ACP v1 mcpServers array format.
func TranslateMCPServers(servers map[string]any) []acp.MCPServerConfig {
	if len(servers) == 0 {
		return []acp.MCPServerConfig{}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]acp.MCPServerConfig, 0, len(names))
	for _, name := range names {
		raw, ok := servers[name].(map[string]any)
		if !ok || raw == nil {
			continue
		}
		urlVal, _ := raw["url"].(string)
		if strings.TrimSpace(urlVal) != "" {
			cfg := acp.MCPServerConfig{
				Type: "http",
				Name: name,
				URL:  strings.TrimSpace(urlVal),
			}
			if hdrs := extractStringMap(raw["headers"]); len(hdrs) > 0 {
				keys := make([]string, 0, len(hdrs))
				for k := range hdrs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					cfg.Headers = append(cfg.Headers, acp.HTTPHeader{Name: k, Value: hdrs[k]})
				}
			}
			out = append(out, cfg)
			continue
		}
		if cmd, _ := raw["command"].(string); strings.TrimSpace(cmd) != "" {
			cfg := acp.MCPServerConfig{
				Name:    name,
				Command: strings.TrimSpace(cmd),
			}
			if argsRaw, ok := raw["args"].([]string); ok {
				cfg.Args = append([]string(nil), argsRaw...)
			} else if argsAny, ok := raw["args"].([]any); ok {
				for _, a := range argsAny {
					if s, ok := a.(string); ok {
						cfg.Args = append(cfg.Args, s)
					}
				}
			}
			if envMap := extractStringMap(raw["env"]); len(envMap) > 0 {
				keys := make([]string, 0, len(envMap))
				for k := range envMap {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					cfg.Env = append(cfg.Env, acp.EnvVar{Name: k, Value: envMap[k]})
				}
			}
			out = append(out, cfg)
		}
	}
	return out
}

func extractStringMap(raw any) map[string]string {
	switch m := raw.(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
		return out
	default:
		return nil
	}
}

func extractLocationOrInputPath(u *acp.ToolCallUpdate, rawIn json.RawMessage, keys ...string) string {
	if len(u.Locations) > 0 && strings.TrimSpace(u.Locations[0].Path) != "" {
		return strings.TrimSpace(u.Locations[0].Path)
	}
	if len(rawIn) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(rawIn, &obj) != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
