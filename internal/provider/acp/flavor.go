package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"agent-overflow/internal/provider"
)

// PermissionPolicy governs how the client handles session/request_permission
// callbacks for the active RuntimeMode.
type PermissionPolicy uint8

const (
	// PolicyPrompt forwards permission requests to AO's ApprovalRegistry.
	PolicyPrompt PermissionPolicy = iota
	// PolicyDenyMutating auto-denies mutating requests (RuntimeReadOnly).
	PolicyDenyMutating
	// PolicyAutoAllowNonQuestion auto-allows non-question requests (RuntimeFullAccess).
	PolicyAutoAllowNonQuestion
)

// PermissionAction is the flavor's classification of an incoming
// session/request_permission request.
type PermissionAction uint8

const (
	PermissionAskUser PermissionAction = iota
	PermissionAutoAllow
	PermissionAutoDeny
	PermissionAskUserInput
)

// PermissionClassification is returned by Flavor.ClassifyPermission.
type PermissionClassification struct {
	Action      PermissionAction
	OptionID    string // selected optionId when Action is AutoAllow or AutoDeny
	Approval    provider.ApprovalRequest
	UserInput   provider.UserInputRequest
	OptionByKey map[string]string // maps AO decision or answer label -> ACP optionId
}

// NormalizedToolStart holds normalized fields for EventToolStart.
type NormalizedToolStart struct {
	ToolName       string
	ItemType       string
	Input          json.RawMessage
	SubagentLaunch bool
	Description    string
	SubagentType   string
	IsBackground   bool
	ExtraMeta      map[string]any
}

// FileDiffChange describes one file change in the normalized shape triage's
// ExtractFileChangeToolResult consumes to build rich inline diffs on Edit/Write rows.
type FileDiffChange struct {
	Path string
	Kind string // "add", "update", "delete"
	Diff string
}

// NormalizedToolComplete holds normalized outputs for EventToolComplete and
// companion heavy payload events (EventDiff, EventCommandOutput, EventProposedPlan).
type NormalizedToolComplete struct {
	ToolName     string
	ItemType     string
	Content      string
	IsError      bool
	ExitCode     *int
	DurationMs   *int
	ItemStatus   string
	DiffPatch    string
	DiffFilePath string
	DiffChanges  []FileDiffChange
	CommandOut   string
	CommandLine  string
	ProposedPlan string
	ExtraMeta    map[string]any
}

// PromptOutcomeOverride lets a flavor override a prompt's stop outcome when a
// provider surfaces an upstream failure out-of-band (for example, agy writing
// step_type=17 error records into its conversation SQLite DB while returning
// stopReason="end_turn").
type PromptOutcomeOverride struct {
	ErrorMessage string
	Failure      *provider.FailureMeta
}

// Flavor isolates provider-specific ACP conventions (metadata extensions,
// usage math, mode names, tool shapes, and out-of-band error detection) from
// the spec-standard ACP v1 client.
type Flavor interface {
	ProviderKind() provider.ProviderKind
	BuildSessionMeta(cfg SessionConfig) map[string]any
	MapRuntimeMode(rt provider.RuntimeMode, im provider.InteractionMode, available []SessionMode) (modeID string, policy PermissionPolicy)
	ConvertUsage(u Usage, model string) provider.TokenUsage
	NormalizeToolStart(u *ToolCallUpdate) NormalizedToolStart
	NormalizeToolComplete(u *ToolCallUpdate, started NormalizedToolStart) NormalizedToolComplete
	ClassifyPermission(threadID, turnID, requestID string, req RequestPermissionParams, policy PermissionPolicy) PermissionClassification
	ExtractPendingQuestion(u *ToolCallUpdate) ([]provider.UserInputQuestion, bool)
	ToolOutlivesPrompt(u *ToolCallUpdate) bool
	BeforePrompt(ctx context.Context, sessionID string) any
	AfterPrompt(ctx context.Context, sessionID string, stopReason string, beforeToken any) *PromptOutcomeOverride
	ShouldHoldSpontaneousWakeClose(ctx context.Context, sessionID string) bool
}

// DefaultFlavor implements spec-standard ACP v1 behavior for all Flavor hooks.
// Provider flavors embed DefaultFlavor and override only what differs.
type DefaultFlavor struct {
	Kind provider.ProviderKind
}

func (f DefaultFlavor) ProviderKind() provider.ProviderKind {
	return f.Kind
}

func (DefaultFlavor) BuildSessionMeta(SessionConfig) map[string]any {
	return nil
}

func (DefaultFlavor) MapRuntimeMode(rt provider.RuntimeMode, im provider.InteractionMode, available []SessionMode) (string, PermissionPolicy) {
	hasMode := func(id string) bool {
		for _, m := range available {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if im == provider.ModePlan && hasMode("plan") {
		return "plan", PolicyPrompt
	}
	switch rt {
	case provider.RuntimeReadOnly:
		return pickMode("default", available), PolicyDenyMutating
	case provider.RuntimeApprovalRequired:
		return pickMode("default", available), PolicyPrompt
	case provider.RuntimeAutoAcceptEdits:
		if hasMode("accept-edits") {
			return "accept-edits", PolicyPrompt
		}
		return pickMode("default", available), PolicyPrompt
	case provider.RuntimeAuto:
		if hasMode("auto") {
			return "auto", PolicyPrompt
		}
		return pickMode("default", available), PolicyPrompt
	case provider.RuntimeFullAccess:
		if hasMode("accept-edits") {
			return "accept-edits", PolicyAutoAllowNonQuestion
		}
		return pickMode("default", available), PolicyAutoAllowNonQuestion
	default:
		return pickMode("default", available), PolicyPrompt
	}
}

func pickMode(preferred string, available []SessionMode) string {
	for _, m := range available {
		if m.ID == preferred {
			return preferred
		}
	}
	if len(available) > 0 {
		return available[0].ID
	}
	return preferred
}

func (DefaultFlavor) ConvertUsage(u Usage, _ string) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:              max(u.InputTokens, 0),
		OutputTokens:             max(u.OutputTokens, 0),
		CacheReadInputTokens:     max(u.CachedReadTokens, 0),
		CacheCreationInputTokens: max(u.CachedWriteTokens, 0),
		ReasoningOutputTokens:    max(u.ThoughtTokens, 0),
	}
}

func (DefaultFlavor) NormalizeToolStart(u *ToolCallUpdate) NormalizedToolStart {
	toolName := strings.TrimSpace(u.Name)
	if toolName == "" {
		toolName = strings.TrimSpace(u.Title)
	}
	if toolName == "" {
		toolName = strings.TrimSpace(u.Kind)
	}
	if toolName == "" {
		toolName = "tool"
	}
	itemType := toolName
	if u.Kind == "execute" {
		itemType = "command_execution"
	} else if u.Kind == "edit" {
		itemType = "file_change"
	}
	return NormalizedToolStart{
		ToolName: toolName,
		ItemType: itemType,
		Input:    u.EffectiveInput(),
	}
}

func (DefaultFlavor) NormalizeToolComplete(u *ToolCallUpdate, started NormalizedToolStart) NormalizedToolComplete {
	out := NormalizedToolComplete{
		ToolName:   started.ToolName,
		ItemType:   started.ItemType,
		IsError:    u.Status == "failed",
		ItemStatus: u.Status,
	}
	if out.ToolName == "" {
		out.ToolName = strings.TrimSpace(u.Name)
	}
	if out.ItemType == "" {
		out.ItemType = out.ToolName
	}

	var textParts []string
	for _, item := range u.Content {
		switch item.Type {
		case "diff":
			patch := BuildUnifiedDiff(item.Path, item.OldText, item.NewText)
			if patch != "" {
				if out.DiffPatch != "" {
					out.DiffPatch += "\n"
				}
				out.DiffPatch += patch
				if out.DiffFilePath == "" {
					out.DiffFilePath = item.Path
				}
				if item.OldText == "" && item.NewText != "" {
					out.DiffChanges = append(out.DiffChanges, FileDiffChange{
						Path: item.Path,
						Kind: "add",
						Diff: item.NewText,
					})
				} else if item.OldText != "" && item.NewText == "" {
					out.DiffChanges = append(out.DiffChanges, FileDiffChange{
						Path: item.Path,
						Kind: "delete",
						Diff: item.OldText,
					})
				} else if hunks := BuildUnifiedDiffHunks(item.OldText, item.NewText); hunks != "" {
					out.DiffChanges = append(out.DiffChanges, FileDiffChange{
						Path: item.Path,
						Kind: "update",
						Diff: hunks,
					})
				}
			}
		case "content":
			if item.Content != nil && item.Content.Text != "" {
				textParts = append(textParts, item.Content.Text)
			}
		}
	}
	if len(textParts) > 0 {
		out.Content = strings.Join(textParts, "\n")
	}
	if len(u.RawOutput) > 0 && string(u.RawOutput) != "null" && string(u.RawOutput) != "{}" {
		var rawObj struct {
			Output        string `json:"output"`
			Stdout        string `json:"stdout"`
			Stderr        string `json:"stderr"`
			ExitCode      *int   `json:"exit_code"`
			ExitCodeCamel *int   `json:"exitCode"`
			DurationMs    *int   `json:"duration_ms"`
			DurationCamel *int   `json:"durationMs"`
		}
		if json.Unmarshal(u.RawOutput, &rawObj) == nil &&
			(rawObj.Output != "" || rawObj.Stdout != "" || rawObj.Stderr != "" ||
				rawObj.ExitCode != nil || rawObj.ExitCodeCamel != nil ||
				rawObj.DurationMs != nil || rawObj.DurationCamel != nil) {
			if rawObj.ExitCode != nil {
				out.ExitCode = rawObj.ExitCode
			} else if rawObj.ExitCodeCamel != nil {
				out.ExitCode = rawObj.ExitCodeCamel
			}
			if rawObj.DurationMs != nil {
				out.DurationMs = rawObj.DurationMs
			} else if rawObj.DurationCamel != nil {
				out.DurationMs = rawObj.DurationCamel
			}
			if out.Content == "" {
				switch {
				case rawObj.Output != "":
					out.Content = rawObj.Output
				case rawObj.Stdout != "" && rawObj.Stderr != "":
					out.Content = rawObj.Stdout + "\n" + rawObj.Stderr
				case rawObj.Stdout != "":
					out.Content = rawObj.Stdout
				case rawObj.Stderr != "":
					out.Content = rawObj.Stderr
				}
			}
		} else if out.Content == "" {
			var rawStr string
			if json.Unmarshal(u.RawOutput, &rawStr) == nil {
				out.Content = rawStr
			} else {
				out.Content = string(u.RawOutput)
			}
		}
	}
	if u.Kind == "execute" || started.ItemType == "command_execution" {
		out.CommandOut = out.Content
		out.CommandLine = extractCommandLine(u.EffectiveInput(), started.Input)
	}
	return out
}

func (DefaultFlavor) ClassifyPermission(threadID, turnID, requestID string, req RequestPermissionParams, policy PermissionPolicy) PermissionClassification {
	allowOpt := findPermissionOption(req.Options, "allow_once", "allow_always")
	allowAlwaysOpt := findPermissionOption(req.Options, "allow_always", "")
	denyOpt := findPermissionOption(req.Options, "reject_once", "reject_always")

	// interaction_* requests are structured user questions, never silently auto-approved.
	if strings.HasPrefix(req.ToolCall.ToolCallID, "interaction_") && len(req.Options) > 0 {
		opts := make([]provider.UserInputQuestionOption, 0, len(req.Options))
		byKey := make(map[string]string, len(req.Options)*2)
		for _, opt := range req.Options {
			opts = append(opts, provider.UserInputQuestionOption{
				Label: opt.Name,
			})
			byKey[opt.Name] = opt.OptionID
			byKey[opt.OptionID] = opt.OptionID
		}
		questionText := strings.TrimSpace(req.ToolCall.Title)
		if questionText == "" {
			questionText = "Choose an option"
		}
		q := provider.UserInputQuestion{
			ID:       req.ToolCall.ToolCallID,
			Header:   "Question",
			Question: questionText,
			Options:  opts,
		}
		return PermissionClassification{
			Action: PermissionAskUserInput,
			UserInput: provider.UserInputRequest{
				RequestID: requestID,
				ThreadID:  threadID,
				TurnID:    turnID,
				ToolUseID: req.ToolCall.ToolCallID,
				ToolName:  "user_input",
				Title:     questionText,
				Questions: provider.NormalizeUserInputQuestions([]provider.UserInputQuestion{q}),
			},
			OptionByKey: byKey,
		}
	}

	switch policy {
	case PolicyAutoAllowNonQuestion:
		if allowOpt != "" {
			return PermissionClassification{Action: PermissionAutoAllow, OptionID: allowOpt}
		}
	case PolicyDenyMutating:
		if isMutatingToolKind(req.ToolCall.Kind, req.ToolCall.Name) {
			return PermissionClassification{Action: PermissionAutoDeny, OptionID: denyOpt}
		}
		if allowOpt != "" {
			return PermissionClassification{Action: PermissionAutoAllow, OptionID: allowOpt}
		}
	}

	toolName := strings.TrimSpace(req.ToolCall.Name)
	if toolName == "" {
		toolName = strings.TrimSpace(req.ToolCall.Title)
	}
	if toolName == "" {
		toolName = strings.TrimSpace(req.ToolCall.Kind)
	}
	title := strings.TrimSpace(req.ToolCall.Title)
	if title == "" {
		title = toolName
	}
	kind := approvalKindFromACP(req.ToolCall.Kind)
	byKey := map[string]string{}
	if allowOpt != "" {
		byKey["allow"] = allowOpt
		byKey["accept"] = allowOpt
	}
	if allowAlwaysOpt != "" {
		byKey["allow_session"] = allowAlwaysOpt
		byKey["acceptForSession"] = allowAlwaysOpt
	} else if allowOpt != "" {
		byKey["allow_session"] = allowOpt
		byKey["acceptForSession"] = allowOpt
	}
	if denyOpt != "" {
		byKey["deny"] = denyOpt
		byKey["decline"] = denyOpt
		byKey["cancel"] = denyOpt
	}
	for _, opt := range req.Options {
		byKey[opt.OptionID] = opt.OptionID
	}

	return PermissionClassification{
		Action: PermissionAskUser,
		Approval: provider.ApprovalRequest{
			RequestID:   requestID,
			ThreadID:    threadID,
			TurnID:      turnID,
			ToolUseID:   req.ToolCall.ToolCallID,
			ToolName:    toolName,
			Title:       title,
			Description: title,
			Input:       req.ToolCall.EffectiveInput(),
			Kind:        kind,
		},
		OptionByKey: byKey,
	}
}

func (DefaultFlavor) ExtractPendingQuestion(*ToolCallUpdate) ([]provider.UserInputQuestion, bool) {
	return nil, false
}

func (DefaultFlavor) ToolOutlivesPrompt(*ToolCallUpdate) bool {
	return false
}

func (DefaultFlavor) BeforePrompt(context.Context, string) any {
	return nil
}

func (DefaultFlavor) AfterPrompt(context.Context, string, string, any) *PromptOutcomeOverride {
	return nil
}

func (DefaultFlavor) ShouldHoldSpontaneousWakeClose(context.Context, string) bool {
	return false
}

func findPermissionOption(options []PermissionOption, preferredKind, fallbackKind string) string {
	for _, opt := range options {
		if opt.Kind == preferredKind {
			return opt.OptionID
		}
	}
	if fallbackKind != "" {
		for _, opt := range options {
			if opt.Kind == fallbackKind {
				return opt.OptionID
			}
		}
	}
	return ""
}

func isMutatingToolKind(kind, name string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "edit", "delete", "execute", "move":
		return true
	case "read", "search", "think", "fetch":
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(lower, "write") ||
		strings.Contains(lower, "edit") ||
		strings.Contains(lower, "delete") ||
		strings.Contains(lower, "run_command") ||
		strings.Contains(lower, "bash")
}

func approvalKindFromACP(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "execute":
		return "command"
	case "edit", "delete", "move":
		return "file-change"
	case "read":
		return "file-read"
	default:
		return "permission"
	}
}

func extractCommandLine(inputs ...json.RawMessage) string {
	for _, raw := range inputs {
		if len(raw) == 0 {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		for _, key := range []string{"CommandLine", "commandLine", "command", "cmd"} {
			if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	return ""
}

// BuildUnifiedDiff produces a unified diff patch from ACP's full oldText and
// newText strings for a file path.
func BuildUnifiedDiff(path, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		cleanPath = "file"
	}
	fromFile := "a/" + cleanPath
	toFile := "b/" + cleanPath
	var prefix string
	if oldText == "" && newText != "" {
		fromFile = "/dev/null"
		prefix = fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n", cleanPath, cleanPath)
	} else if newText == "" && oldText != "" {
		toFile = "/dev/null"
		prefix = fmt.Sprintf("diff --git a/%s b/%s\ndeleted file mode 100644\n", cleanPath, cleanPath)
	} else {
		prefix = fmt.Sprintf("diff --git a/%s b/%s\n", cleanPath, cleanPath)
	}
	ud := difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldText),
		B:        difflib.SplitLines(newText),
		FromFile: fromFile,
		ToFile:   toFile,
		Context:  3,
	}
	body, err := difflib.GetUnifiedDiffString(ud)
	if err != nil || body == "" {
		return ""
	}
	return prefix + body
}

// BuildUnifiedDiffHunks returns only the `@@ ... @@` unified diff hunks without
// `diff --git` / `---` / `+++` file headers, matching the shape
// triage.ExtractFileChangeToolResult expects on `update` FileDiffChange rows so
// triage can attach workspace-relative headers without rejecting absolute paths.
func BuildUnifiedDiffHunks(oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	ud := difflib.UnifiedDiff{
		A:       difflib.SplitLines(oldText),
		B:       difflib.SplitLines(newText),
		Context: 3,
	}
	body, err := difflib.GetUnifiedDiffString(ud)
	if err != nil || body == "" {
		return ""
	}
	// Strip any empty header lines difflib may emit before the first @@ hunk.
	if idx := strings.Index(body, "@@"); idx >= 0 {
		return body[idx:]
	}
	return body
}
