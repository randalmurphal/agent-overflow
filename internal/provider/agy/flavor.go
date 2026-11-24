package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/agyconfig"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflowtools"
)

// Flavor implements acp.Flavor for Google Antigravity (`agy acp`).
type Flavor struct {
	acp.DefaultFlavor
	// AppDataDir is `<geminiDir>/antigravity-cli`, used for read-only
	// step_type=17 error detection, pending high-priority wake checks, and
	// out-of-band subagent transcript tracking.
	AppDataDir string

	subagentMu            sync.Mutex
	subagentLaunchByChild map[string]string
	subagentChildByLaunch map[string]string

	// mcpTrustMu guards session-registered AO server membership and whether
	// any colliding third-party MCP server name exists (in session config or
	// user mcp_config.json), which disables ambiguous single-underscore MCP
	// auto-approval so a third-party server can never spoof an AO tool.
	mcpTrustMu             sync.Mutex
	mcpConfigured          bool
	registeredAOMCP        map[string]bool
	collidingThirdPartyMCP bool

	// Optional overrides for deterministic unit tests (zero uses defaults).
	subagentPollInterval     time.Duration
	subagentDiscoveryTimeout time.Duration
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
	f.recordSessionMCPTrust(cfg.MCPServers)
	parts := make([]string, 0, 3)
	if s := strings.TrimSpace(cfg.SystemPrompt); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(cfg.AppendSystemPrompt); s != "" {
		parts = append(parts, s)
	}
	if len(cfg.DisabledTools) > 0 {
		parts = append(parts, "The following native tools are disabled for this session and must not be called: "+strings.Join(cfg.DisabledTools, ", ")+".")
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
	rawName := strings.TrimSpace(u.Name)

	if server, tool, ok := parseAgyMCPToolName(rawName); ok {
		norm.ToolName = "MCP/" + tool
		norm.ItemType = "MCP/" + tool
		if norm.ExtraMeta == nil {
			norm.ExtraMeta = map[string]any{}
		}
		norm.ExtraMeta["mcp"] = map[string]string{
			"server": server,
			"tool":   tool,
		}
		return norm
	}

	switch strings.ToLower(rawName) {
	case "read_file", "view_file":
		norm.ToolName = "Read"
		norm.ItemType = "Read"
		path := extractLocationOrInputPath(u, rawIn, "Path", "AbsolutePath", "file_path", "path")
		fields := map[string]any{}
		if path != "" {
			fields["file_path"] = path
		}
		var params struct {
			StartLine int `json:"StartLine"`
			EndLine   int `json:"EndLine"`
			Offset    int `json:"offset"`
			Limit     int `json:"limit"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			offset := params.StartLine
			if offset <= 0 {
				offset = params.Offset
			}
			if offset <= 0 && len(u.Locations) > 0 && u.Locations[0].Line > 0 {
				offset = u.Locations[0].Line
			}
			if offset > 0 {
				fields["offset"] = offset
			}
			limit := params.Limit
			if limit <= 0 && params.StartLine > 0 && params.EndLine >= params.StartLine {
				limit = params.EndLine - params.StartLine + 1
			}
			if limit > 0 {
				fields["limit"] = limit
			}
		}
		if len(fields) > 0 {
			if reshaped, err := json.Marshal(fields); err == nil {
				norm.Input = reshaped
			}
		}

	case "edit_file", "replace_file_content", "multi_replace_file_content":
		norm.ToolName = "Edit"
		norm.ItemType = "file_change"
		path := extractLocationOrInputPath(u, rawIn, "Path", "TargetFile", "file_path", "path")
		fields := map[string]any{}
		if path != "" {
			fields["file_path"] = path
		}
		var params struct {
			Target             string `json:"Target"`
			TargetContent      string `json:"TargetContent"`
			Replacement        string `json:"Replacement"`
			ReplacementContent string `json:"ReplacementContent"`
			ReplacementChunks  []struct {
				TargetContent      string `json:"TargetContent"`
				ReplacementContent string `json:"ReplacementContent"`
			} `json:"ReplacementChunks"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			oldStr := firstNonEmpty(params.Target, params.TargetContent)
			newStr := firstNonEmpty(params.Replacement, params.ReplacementContent)
			if oldStr == "" && newStr == "" && len(params.ReplacementChunks) > 0 {
				var oldParts, newParts []string
				for _, ch := range params.ReplacementChunks {
					if ch.TargetContent != "" {
						oldParts = append(oldParts, ch.TargetContent)
					}
					if ch.ReplacementContent != "" {
						newParts = append(newParts, ch.ReplacementContent)
					}
				}
				oldStr = strings.Join(oldParts, "\n")
				newStr = strings.Join(newParts, "\n")
			}
			if oldStr != "" {
				fields["old_string"] = oldStr
			}
			if newStr != "" {
				fields["new_string"] = newStr
			}
		}
		if len(fields) > 0 {
			if reshaped, err := json.Marshal(fields); err == nil {
				norm.Input = reshaped
			}
		}

	case "write_file", "write_to_file":
		norm.ToolName = "Write"
		norm.ItemType = "file_change"
		path := extractLocationOrInputPath(u, rawIn, "Path", "TargetFile", "file_path", "path")
		fields := map[string]any{}
		if path != "" {
			fields["file_path"] = path
		}
		var params struct {
			Content     string `json:"Content"`
			CodeContent string `json:"CodeContent"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			if content := firstNonEmpty(params.Content, params.CodeContent); content != "" {
				fields["content"] = content
			}
		}
		if len(fields) > 0 {
			if reshaped, err := json.Marshal(fields); err == nil {
				norm.Input = reshaped
			}
		}

	case "run_command":
		norm.ToolName = "Bash"
		norm.ItemType = "command_execution"
		var params struct {
			CommandLine       string `json:"CommandLine"`
			ToolSummary       string `json:"toolSummary"`
			Cwd               string `json:"Cwd"`
			IsDaemon          bool   `json:"IsDaemon"`
			WaitMsBeforeAsync int    `json:"WaitMsBeforeAsync"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if params.CommandLine != "" {
				fields["command"] = params.CommandLine
				if norm.ExtraMeta == nil {
					norm.ExtraMeta = map[string]any{}
				}
				norm.ExtraMeta["command"] = params.CommandLine
			}
			if params.ToolSummary != "" {
				fields["description"] = params.ToolSummary
			}
			if params.Cwd != "" {
				fields["cwd"] = params.Cwd
				if norm.ExtraMeta == nil {
					norm.ExtraMeta = map[string]any{}
				}
				norm.ExtraMeta["cwd"] = params.Cwd
			}
			if params.IsDaemon {
				fields["run_in_background"] = true
				norm.IsBackground = true
			}
			if params.WaitMsBeforeAsync > 0 {
				fields["timeout"] = params.WaitMsBeforeAsync
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "grep_search", "ripgrep", "search_files":
		norm.ToolName = "Grep"
		norm.ItemType = "Grep"
		var params struct {
			Query           string   `json:"Query"`
			Pattern         string   `json:"Pattern"`
			PatternLower    string   `json:"pattern"`
			SearchPath      string   `json:"SearchPath"`
			Path            string   `json:"Path"`
			PathLower       string   `json:"path"`
			Includes        []string `json:"Includes"`
			Glob            string   `json:"glob"`
			CaseInsensitive bool     `json:"CaseInsensitive"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if pat := firstNonEmpty(params.Query, params.Pattern, params.PatternLower); pat != "" {
				fields["pattern"] = pat
			}
			if path := firstNonEmpty(params.SearchPath, params.Path, params.PathLower); path != "" {
				fields["path"] = path
			}
			if params.Glob != "" {
				fields["glob"] = params.Glob
			} else if len(params.Includes) > 0 {
				fields["glob"] = strings.Join(params.Includes, ",")
			}
			if params.CaseInsensitive {
				fields["-i"] = true
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "find_by_name", "glob", "list_dir":
		norm.ToolName = "Glob"
		norm.ItemType = "Glob"
		var params struct {
			Pattern         string `json:"Pattern"`
			PatternLower    string `json:"pattern"`
			SearchDirectory string `json:"SearchDirectory"`
			DirectoryPath   string `json:"DirectoryPath"`
			Path            string `json:"Path"`
			PathLower       string `json:"path"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if pat := firstNonEmpty(params.Pattern, params.PatternLower); pat != "" {
				fields["pattern"] = pat
			}
			if path := firstNonEmpty(params.SearchDirectory, params.DirectoryPath, params.Path, params.PathLower); path != "" {
				fields["path"] = path
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "search_web", "web_search":
		norm.ToolName = "WebSearch"
		norm.ItemType = "WebSearch"
		var params struct {
			Query      string `json:"query"`
			QueryUpper string `json:"Query"`
			Domain     string `json:"domain"`
			DomainUp   string `json:"Domain"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if q := firstNonEmpty(params.Query, params.QueryUpper); q != "" {
				fields["query"] = q
			}
			if d := firstNonEmpty(params.Domain, params.DomainUp); d != "" {
				fields["allowed_domains"] = []string{d}
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "read_url_content", "fetch_url", "web_fetch":
		norm.ToolName = "WebFetch"
		norm.ItemType = "WebFetch"
		var params struct {
			URL      string `json:"Url"`
			URLLower string `json:"url"`
			Prompt   string `json:"prompt"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if targetURL := firstNonEmpty(params.URL, params.URLLower); targetURL != "" {
				fields["url"] = targetURL
			}
			if params.Prompt != "" {
				fields["prompt"] = params.Prompt
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
		if norm.ExtraMeta == nil {
			norm.ExtraMeta = map[string]any{}
		}
		norm.ExtraMeta["provider"] = string(provider.Agy)
		var params struct {
			TypeName string `json:"TypeName"`
			Role     string `json:"Role"`
			Prompt   string `json:"Prompt"`
			Model    string `json:"Model"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			norm.SubagentType = strings.TrimSpace(params.TypeName)
			norm.Description = strings.TrimSpace(params.Role)
			fields := map[string]any{
				"subagent_type":     norm.SubagentType,
				"description":       norm.Description,
				"prompt":            params.Prompt,
				"run_in_background": true,
			}
			if m := strings.TrimSpace(params.Model); m != "" && m != "inherit" {
				fields["model"] = m
			}
			if reshaped, err := json.Marshal(fields); err == nil {
				norm.Input = reshaped
			}
		}

	case "send_message":
		norm.ToolName = "SendMessage"
		norm.ItemType = "SendMessage"
		var params struct {
			Recipient string `json:"Recipient"`
			Message   string `json:"Message"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if params.Recipient != "" {
				fields["to"] = params.Recipient
			}
			if params.Message != "" {
				fields["message"] = params.Message
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "ask_question":
		norm.ToolName = "AskUserQuestion"
		norm.ItemType = "AskUserQuestion"
		if questions, ok := f.ExtractPendingQuestion(u); ok {
			normalized := provider.NormalizeUserInputQuestions(questions)
			if reshaped, err := json.Marshal(map[string]any{"questions": normalized}); err == nil {
				norm.Input = reshaped
			}
		}

	case "generate_image":
		norm.ToolName = "ImageGeneration"
		norm.ItemType = "ImageGeneration"
		var params struct {
			Prompt    string `json:"Prompt"`
			ImageName string `json:"ImageName"`
			Path      string `json:"Path"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			fields := map[string]any{}
			if params.Prompt != "" {
				fields["prompt"] = params.Prompt
			}
			if p := firstNonEmpty(params.Path, params.ImageName); p != "" {
				fields["path"] = p
			}
			if len(fields) > 0 {
				if reshaped, err := json.Marshal(fields); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "manage_task":
		var params struct {
			Action string `json:"Action"`
			TaskID string `json:"TaskId"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			desc := strings.TrimSpace(params.Action)
			if params.TaskID != "" {
				desc = strings.TrimSpace(desc + " " + params.TaskID)
			}
			if desc != "" {
				if reshaped, err := json.Marshal(map[string]any{"description": desc, "action": params.Action, "task_id": params.TaskID}); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "schedule":
		var params struct {
			CronExpression  string `json:"CronExpression"`
			DurationSeconds int    `json:"DurationSeconds"`
			Prompt          string `json:"Prompt"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			desc := strings.TrimSpace(params.Prompt)
			if params.CronExpression != "" {
				desc = strings.TrimSpace(params.CronExpression + " " + desc)
			} else if params.DurationSeconds > 0 {
				desc = strings.TrimSpace(fmt.Sprintf("%ds %s", params.DurationSeconds, desc))
			}
			if desc != "" {
				if reshaped, err := json.Marshal(map[string]any{"description": desc, "prompt": params.Prompt}); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "define_subagent":
		var params struct {
			Name        string `json:"Name"`
			Description string `json:"Description"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			desc := firstNonEmpty(params.Name, params.Description)
			if desc != "" {
				if reshaped, err := json.Marshal(map[string]any{"description": desc, "name": params.Name}); err == nil {
					norm.Input = reshaped
				}
			}
		}

	case "manage_subagents":
		var params struct {
			Action string `json:"Action"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil && strings.TrimSpace(params.Action) != "" {
			if reshaped, err := json.Marshal(map[string]any{"description": strings.TrimSpace(params.Action), "action": params.Action}); err == nil {
				norm.Input = reshaped
			}
		}

	case "run_workflow":
		var params struct {
			Description string `json:"Description"`
			ScriptPath  string `json:"ScriptPath"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			if desc := firstNonEmpty(params.Description, params.ScriptPath); desc != "" {
				if reshaped, err := json.Marshal(map[string]any{"description": desc, "script_path": params.ScriptPath}); err == nil {
					norm.Input = reshaped
				}
			}
		}
	}

	return norm
}

func (f *Flavor) NormalizeToolComplete(u *acp.ToolCallUpdate, started acp.NormalizedToolStart) acp.NormalizedToolComplete {
	norm := f.DefaultFlavor.NormalizeToolComplete(u, started)
	rawName := strings.ToLower(strings.TrimSpace(u.Name))
	if rawName == "" {
		rawName = strings.ToLower(strings.TrimSpace(started.ToolName))
	}
	rawIn := u.EffectiveInput()
	if len(rawIn) == 0 {
		rawIn = started.Input
	}

	// Plan-mode artifact write_file/edit_file carries ArtifactMetadata.RequestFeedback=true,
	// and edit/write/replace tools get a fallback inline diff when ACP omitted content[{type:"diff"}].
	if rawName == "write_file" || rawName == "write_to_file" || rawName == "edit_file" ||
		rawName == "replace_file_content" || rawName == "multi_replace_file_content" ||
		started.ItemType == "file_change" {
		var params struct {
			Path               string `json:"Path"`
			TargetFile         string `json:"TargetFile"`
			FilePath           string `json:"file_path"`
			Content            string `json:"Content"`
			CodeContent        string `json:"CodeContent"`
			Target             string `json:"Target"`
			TargetContent      string `json:"TargetContent"`
			Replacement        string `json:"Replacement"`
			ReplacementContent string `json:"ReplacementContent"`
			ReplacementChunks  []struct {
				TargetContent      string `json:"TargetContent"`
				ReplacementContent string `json:"ReplacementContent"`
			} `json:"ReplacementChunks"`
			ArtifactMetadata *struct {
				RequestFeedback bool `json:"RequestFeedback"`
				UserFacing      bool `json:"UserFacing"`
			} `json:"ArtifactMetadata"`
		}
		if len(rawIn) > 0 && json.Unmarshal(rawIn, &params) == nil {
			path := extractLocationOrInputPath(u, rawIn, "Path", "TargetFile", "file_path", "path")
			if params.ArtifactMetadata != nil && params.ArtifactMetadata.RequestFeedback {
				body := firstNonEmpty(params.Content, params.CodeContent)
				// For edit_file / replace_file_content / multi_replace_file_content,
				// read the full updated artifact file from disk so ProposedPlan is
				// never truncated to a single hunk or dropped on multi-chunk edits.
				if body == "" && path != "" && filepath.IsAbs(path) {
					if data, err := os.ReadFile(path); err == nil && len(data) > 0 && len(data) <= 512*1024 {
						body = string(data)
					}
				}
				if body == "" {
					body = firstNonEmpty(params.Replacement, params.ReplacementContent)
				}
				if strings.TrimSpace(body) != "" {
					norm.ProposedPlan = body
				}
			}
			// Fallback inline diff when an ACP update omitted content[{type:"diff"}].
			if len(norm.DiffChanges) == 0 && norm.ItemType == "file_change" {
				if path != "" {
					switch rawName {
					case "write_file", "write_to_file", "write":
						if content := firstNonEmpty(params.Content, params.CodeContent); content != "" {
							if patch := acp.BuildUnifiedDiff(path, "", content); patch != "" {
								norm.DiffPatch = patch
								norm.DiffFilePath = path
								norm.DiffChanges = []acp.FileDiffChange{{Path: path, Kind: "add", Diff: content}}
							}
						}
					case "edit_file", "replace_file_content", "edit":
						oldStr := firstNonEmpty(params.Target, params.TargetContent)
						newStr := firstNonEmpty(params.Replacement, params.ReplacementContent)
						if oldStr != "" || newStr != "" {
							if patch := acp.BuildUnifiedDiff(path, oldStr, newStr); patch != "" {
								norm.DiffPatch = patch
								norm.DiffFilePath = path
								if hunks := acp.BuildUnifiedDiffHunks(oldStr, newStr); hunks != "" {
									norm.DiffChanges = []acp.FileDiffChange{{Path: path, Kind: "update", Diff: hunks}}
								}
							}
						}
					case "multi_replace_file_content":
						var patchParts, hunkParts []string
						for _, ch := range params.ReplacementChunks {
							if ch.TargetContent == "" && ch.ReplacementContent == "" {
								continue
							}
							if p := acp.BuildUnifiedDiff(path, ch.TargetContent, ch.ReplacementContent); p != "" {
								patchParts = append(patchParts, p)
							}
							if h := acp.BuildUnifiedDiffHunks(ch.TargetContent, ch.ReplacementContent); h != "" {
								hunkParts = append(hunkParts, h)
							}
						}
						if len(patchParts) > 0 {
							norm.DiffPatch = strings.Join(patchParts, "\n")
							norm.DiffFilePath = path
							if len(hunkParts) > 0 {
								norm.DiffChanges = []acp.FileDiffChange{{Path: path, Kind: "update", Diff: strings.Join(hunkParts, "\n")}}
							}
						}
					}
				}
			}
		}
	}

	// Preserve generated image path and prompt on EventToolComplete so triage's
	// generated-image importer can attach the picture inline beside assistant prose.
	if rawName == "generate_image" || started.ToolName == "ImageGeneration" {
		imgPath, imgPrompt := extractGeneratedImageFields(rawIn, started.Input, u.RawOutput, norm.Content)
		if imgPath != "" || imgPrompt != "" {
			if norm.ExtraMeta == nil {
				norm.ExtraMeta = map[string]any{}
			}
			inputFields := map[string]any{}
			if imgPath != "" {
				inputFields["path"] = imgPath
			}
			if imgPrompt != "" {
				inputFields["prompt"] = imgPrompt
			}
			norm.ExtraMeta["input"] = inputFields
		}
	}

	// Preserve subagent conversationId on Agent completion for lineage linking.
	if rawName == "invoke_subagent" || started.SubagentLaunch {
		if convID := extractConversationID(u.RawOutput, norm.Content); convID != "" {
			if norm.ExtraMeta == nil {
				norm.ExtraMeta = map[string]any{}
			}
			norm.ExtraMeta["agent_id"] = convID
		}
	}

	// Detect run_command transitioning to an asynchronous background task after
	// WaitMsBeforeAsync expires (or IsDaemon: true) so AO adds it to the
	// background tray and tails its task log file while it runs. Only mark
	// in_progress frames as backgrounded; marking a completed frame would leave
	// the launch row permanently running because run_command settles via
	// tool_call_update(completed) rather than a separate terminal event.
	if u.Status == "in_progress" && (rawName == "run_command" || started.ItemType == "command_execution") {
		if taskID, logPath, ok := parseAgyBackgroundTaskOutput(firstNonEmpty(norm.CommandOut, norm.Content)); ok {
			norm.IsBackground = true
			norm.BackgroundTaskID = taskID
			norm.OutputFile = logPath
		}
	}

	return norm
}

func parseAgyBackgroundTaskOutput(text string) (taskID, logPath string, ok bool) {
	const taskPrefix = "Tool is running as a background task with task id:"
	idx := strings.Index(text, taskPrefix)
	if idx < 0 {
		return "", "", false
	}
	rest := text[idx+len(taskPrefix):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	taskID = strings.TrimSpace(rest)

	const logPrefix = "Task logs are available at:"
	if logIdx := strings.Index(text, logPrefix); logIdx >= 0 {
		rawLog := text[logIdx+len(logPrefix):]
		if nl := strings.IndexAny(rawLog, "\r\n"); nl >= 0 {
			rawLog = rawLog[:nl]
		}
		rawLog = strings.TrimSpace(rawLog)
		if strings.HasPrefix(rawLog, "file://") {
			rawLog = resolveTranscriptURI(rawLog, "")
		}
		if filepath.IsAbs(rawLog) {
			logPath = filepath.Clean(rawLog)
		}
	}
	return taskID, logPath, true
}

func normalizeMCPPrefix(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

func isAOMCPNameOrPrefix(name string) bool {
	norm := normalizeMCPPrefix(name)
	if norm == "" {
		return false
	}
	return strings.HasPrefix(norm, "ao_thread_tools") ||
		strings.HasPrefix("ao_thread_tools", norm) ||
		strings.HasPrefix(norm, "ao_workflow_tools") ||
		strings.HasPrefix("ao_workflow_tools", norm)
}

func (f *Flavor) recordSessionMCPTrust(servers []acp.MCPServerConfig) {
	reg := make(map[string]bool, 2)
	colliding := false
	for _, s := range servers {
		trimmed := strings.TrimSpace(s.Name)
		switch trimmed {
		case "ao-thread-tools":
			if reg["ao-thread-tools"] {
				colliding = true
			}
			reg["ao-thread-tools"] = true
		case "ao-workflow-tools":
			if reg["ao-workflow-tools"] {
				colliding = true
			}
			reg["ao-workflow-tools"] = true
		default:
			if isAOMCPNameOrPrefix(s.Name) {
				colliding = true
			}
		}
	}
	if !colliding && f.AppDataDir != "" {
		// Check user's global mcp_config.json for ANY third-party server whose
		// normalized name matches or prefixes ao_thread_tools / ao_workflow_tools
		// (including exact underscore/hyphen spellings, since AO injects its own
		// loopback servers via ACP session/new, never via global mcp_config.json).
		// Fail closed on read/parse errors (missing file returns nil, nil).
		geminiDir := filepath.Dir(f.AppDataDir)
		homeDir := filepath.Dir(geminiDir)
		userServers, err := agyconfig.New(agyconfig.PathForHome(homeDir)).ListServers()
		if err != nil {
			colliding = true
		} else {
			for _, us := range userServers {
				if isAOMCPNameOrPrefix(us.Name) {
					colliding = true
					break
				}
			}
		}
	}
	f.mcpTrustMu.Lock()
	f.mcpConfigured = true
	f.registeredAOMCP = reg
	f.collidingThirdPartyMCP = colliding
	f.mcpTrustMu.Unlock()
}

func (f *Flavor) isTrustedAOMCPTool(rawName string) bool {
	trimmed := strings.TrimSpace(rawName)
	server, tool, ok := parseAgyMCPToolName(trimmed)
	if !ok {
		return false
	}
	f.mcpTrustMu.Lock()
	configured := f.mcpConfigured
	registered := f.registeredAOMCP[server]
	colliding := f.collidingThirdPartyMCP
	f.mcpTrustMu.Unlock()
	if configured && !registered {
		return false
	}
	if colliding {
		// Any colliding third-party server (either prefix-overlapping on
		// single-underscore or exact-name colliding in global mcp_config.json)
		// disables automatic approval; require explicit user approval instead.
		return false
	}
	switch server {
	case "ao-thread-tools":
		for _, allowed := range threadtools.ToolNames {
			if tool == allowed {
				return true
			}
		}
	case "ao-workflow-tools":
		for _, allowed := range workflowtools.ToolNames() {
			if tool == allowed {
				return true
			}
		}
	}
	return false
}

func (f *Flavor) ClassifyPermission(threadID, turnID, requestID string, req acp.RequestPermissionParams, policy acp.PermissionPolicy) acp.PermissionClassification {
	// Never bypass read-only enforcement for any MCP tool; only auto-approve
	// exact known AO coordination tools when the session is not read-only and
	// no third-party server name collides with the AO MCP prefix.
	if policy != acp.PolicyDenyMutating && f.isTrustedAOMCPTool(req.ToolCall.Name) {
		for _, opt := range req.Options {
			if opt.Kind == "allow_once" || opt.Kind == "allow_always" {
				return acp.PermissionClassification{
					Action:   acp.PermissionAutoAllow,
					OptionID: opt.OptionID,
				}
			}
		}
	}
	effectivePolicy := policy
	if effectivePolicy == acp.PolicyAutoAcceptEdits && hasAgySecurityWarning(req.Options) {
		// Security-warned edits (e.g. paths outside the workspace) must never
		// be silently auto-approved in Auto-accept edits mode.
		effectivePolicy = acp.PolicyPrompt
	}
	cls := f.DefaultFlavor.ClassifyPermission(threadID, turnID, requestID, req, effectivePolicy)
	if cls.Action == acp.PermissionAskUser {
		// Normalize tool name and input into AO's canonical shapes (Bash/Edit/Write/MCP)
		// so approval cards display command previews and file paths cleanly.
		norm := f.NormalizeToolStart(&req.ToolCall)
		if norm.ToolName != "" {
			cls.Approval.ToolName = norm.ToolName
		}
		if len(norm.Input) > 0 {
			cls.Approval.Input = acp.MergeApprovalPreview(norm.Input, cls.Approval.Input)
		}
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

// hasAgySecurityWarning reports whether any permission option carries
// `_meta["agy.security.warning"]`.
func hasAgySecurityWarning(options []acp.PermissionOption) bool {
	for _, opt := range options {
		if len(opt.Meta) == 0 {
			continue
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(opt.Meta, &meta) != nil {
			continue
		}
		if warnRaw, ok := meta["agy.security.warning"]; ok && len(warnRaw) > 0 && string(warnRaw) != "null" {
			return true
		}
	}
	return false
}

// ExtractPendingQuestion extracts structured ask_question prompts when the tool
// transitions to status="pending". Accepts both raw ACP `ask_question` payloads
// (options: []string) and already-normalized `AskUserQuestion` payloads
// (options: [{"label": ...}]) from two-step tool_call -> tool_call_update flows.
func (f *Flavor) ExtractPendingQuestion(u *acp.ToolCallUpdate) ([]provider.UserInputQuestion, bool) {
	name := strings.TrimSpace(u.Name)
	if !strings.EqualFold(name, "ask_question") && !strings.EqualFold(name, "AskUserQuestion") {
		return nil, false
	}
	raw := u.EffectiveInput()
	if len(raw) == 0 {
		return nil, false
	}
	var payload struct {
		Questions []struct {
			ID            string            `json:"id"`
			Header        string            `json:"header"`
			Question      string            `json:"question"`
			Options       []json.RawMessage `json:"options"`
			IsMultiSelect bool              `json:"is_multi_select"`
			MultiSelect   bool              `json:"multiSelect"`
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
		for _, rawOpt := range q.Options {
			var label string
			if json.Unmarshal(rawOpt, &label) == nil {
				if strings.TrimSpace(label) != "" {
					opts = append(opts, provider.UserInputQuestionOption{Label: label})
				}
				continue
			}
			var obj struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			}
			if json.Unmarshal(rawOpt, &obj) == nil && strings.TrimSpace(obj.Label) != "" {
				opts = append(opts, provider.UserInputQuestionOption{
					Label:       obj.Label,
					Description: obj.Description,
				})
			}
		}
		questions = append(questions, provider.UserInputQuestion{
			ID:          q.ID,
			Header:      q.Header,
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.IsMultiSelect || q.MultiSelect,
		})
	}
	if len(questions) == 0 {
		return nil, false
	}
	return questions, true
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
	sinceIdx, ok := beforeToken.(int)
	if !ok {
		// Baseline read failed before the prompt; skip post-prompt error scan
		// rather than risking misattributing an older turn's error step.
		return nil
	}
	return inspectTurnErrorStep(f.AppDataDir, sessionID, sinceIdx)
}

func (f *Flavor) InitCompactionCursor(_ context.Context, sessionID string) int {
	if f.AppDataDir == "" || sessionID == "" {
		return -1
	}
	return readMaxCheckpointIndex(f.AppDataDir, sessionID)
}

func (f *Flavor) DetectCompaction(_ context.Context, sessionID string, afterIdx int) (int, *acp.CompactionEvent) {
	if f.AppDataDir == "" || sessionID == "" {
		return afterIdx, nil
	}
	return detectNewCheckpoint(f.AppDataDir, sessionID, afterIdx)
}

// ShouldHoldSpontaneousWakeClose checks `<appDataDir>/brain/<sessionId>/.system_generated/messages/`
// for unread MESSAGE_PRIORITY_HIGH wake messages queued behind the current wake.
func (f *Flavor) ShouldHoldSpontaneousWakeClose(_ context.Context, sessionID string) bool {
	if f.AppDataDir == "" || validateConversationSessionID(sessionID) != nil {
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
				for k, v := range obj {
					if b, isBool := v.(bool); isBool {
						if b {
							readSet[k] = true
						}
					} else if v != nil {
						readSet[k] = true
					}
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// knownAOMCPServers lists AO's own loopback MCP servers so single-underscore
// `mcp_<server>_<tool>` names from agy split cleanly even when server names
// contain hyphens or underscores.
var knownAOMCPServers = []struct {
	wire   string
	server string
}{
	{"ao-thread-tools", "ao-thread-tools"},
	{"ao_thread_tools", "ao-thread-tools"},
	{"ao-browser-tools", "ao-browser-tools"},
	{"ao_browser_tools", "ao-browser-tools"},
	{"ao-remote-tools", "ao-remote-tools"},
	{"ao_remote_tools", "ao-remote-tools"},
	{"ao-workflow-tools", "ao-workflow-tools"},
	{"ao_workflow_tools", "ao-workflow-tools"},
}

// parseAgyMCPToolName parses both `mcp__<server>__<tool>` and agy's
// single-underscore `mcp_<server>_<tool>` tool names.
func parseAgyMCPToolName(name string) (server, tool string, ok bool) {
	if strings.HasPrefix(name, "mcp__") {
		rest := name[len("mcp__"):]
		if sep := strings.Index(rest, "__"); sep > 0 && sep+2 < len(rest) {
			return rest[:sep], rest[sep+2:], true
		}
		return "", "", false
	}
	if !strings.HasPrefix(name, "mcp_") {
		return "", "", false
	}
	rest := name[len("mcp_"):]
	for _, known := range knownAOMCPServers {
		prefix := known.wire + "_"
		if strings.HasPrefix(rest, prefix) && len(rest) > len(prefix) {
			return known.server, rest[len(prefix):], true
		}
	}
	sep := strings.IndexByte(rest, '_')
	if sep <= 0 || sep+1 >= len(rest) {
		return "", "", false
	}
	return rest[:sep], rest[sep+1:], true
}

func extractConversationID(rawOut json.RawMessage, content string) string {
	if len(rawOut) > 0 {
		var obj struct {
			ConversationID string `json:"conversationId"`
			ID             string `json:"conversation_id"`
		}
		if json.Unmarshal(rawOut, &obj) == nil {
			if id := firstNonEmpty(obj.ConversationID, obj.ID); id != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "conversationId"); idx >= 0 {
			rest := line[idx+len("conversationId"):]
			rest = strings.TrimLeft(rest, `":= `)
			if end := strings.IndexAny(rest, `" ,}`); end > 0 {
				return rest[:end]
			}
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

var (
	imageFilePathQuotedRe = regexp.MustCompile(`(?i)["'` + "`" + `](?:file://)?((?:/|[A-Za-z]:[\\/])[^"'` + "`" + `\r\n<>]+\.(?:png|jpe?g|webp|gif))["'` + "`" + `]`)
	imageFilePathRe       = regexp.MustCompile(`(?i)(?:file://)?((?:/|[A-Za-z]:[\\/])[^\s"'` + "`" + `<>)\]]+\.(?:png|jpe?g|webp|gif))`)
)

func normalizeImagePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "file://") {
		return resolveTranscriptURI(raw, "")
	}
	return raw
}

func extractGeneratedImageFields(rawIn, startedIn, rawOut json.RawMessage, content string) (path, prompt string) {
	readInput := func(raw json.RawMessage) (p, pr string) {
		if len(raw) == 0 {
			return "", ""
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return "", ""
		}
		for _, k := range []string{"Prompt", "prompt"} {
			if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
				pr = strings.TrimSpace(s)
				break
			}
		}
		for _, k := range []string{"Path", "path"} {
			if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" && filepath.IsAbs(strings.TrimSpace(s)) {
				p = strings.TrimSpace(s)
				break
			}
		}
		return p, pr
	}

	inPath, inPrompt := readInput(rawIn)
	stPath, stPrompt := readInput(startedIn)
	prompt = firstNonEmpty(inPrompt, stPrompt)

	if len(rawOut) > 0 {
		var obj map[string]any
		if json.Unmarshal(rawOut, &obj) == nil {
			for _, k := range []string{
				"path", "Path", "saved_path", "savedPath",
				"file_path", "filePath", "output_path", "outputPath",
				"image_path", "imagePath",
			} {
				if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
					path = normalizeImagePath(s)
					break
				}
			}
			if path == "" {
				for _, k := range []string{"paths", "saved_paths", "savedPaths"} {
					if arr, ok := obj[k].([]any); ok && len(arr) > 0 {
						if s, ok := arr[0].(string); ok && strings.TrimSpace(s) != "" {
							path = normalizeImagePath(s)
							break
						}
					}
				}
			}
		}
	}
	if path == "" && content != "" {
		if m := imageFilePathQuotedRe.FindStringSubmatch(content); len(m) > 1 {
			path = normalizeImagePath(m[1])
		} else if m := imageFilePathRe.FindStringSubmatch(content); len(m) > 1 {
			path = normalizeImagePath(m[1])
		}
	}
	if path == "" {
		path = normalizeImagePath(firstNonEmpty(inPath, stPath))
	}
	return path, prompt
}
