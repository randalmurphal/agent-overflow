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
	// PolicyAutoAcceptEdits auto-allows file edit requests (RuntimeAutoAcceptEdits).
	PolicyAutoAcceptEdits
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
	ToolName         string
	ItemType         string
	Content          string
	IsError          bool
	IsBackground     bool
	BackgroundTaskID string
	OutputFile       string
	ExitCode         *int
	DurationMs       *int
	ItemStatus       string
	DiffPatch        string
	DiffFilePath     string
	DiffChanges      []FileDiffChange
	CommandOut       string
	CommandLine      string
	ProposedPlan     string
	ExtraMeta        map[string]any
}

// PromptOutcomeOverride lets a flavor override a prompt's stop outcome when a
// provider surfaces an upstream failure out-of-band (for example, agy writing
// step_type=17 error records into its conversation SQLite DB while returning
// stopReason="end_turn").
type PromptOutcomeOverride struct {
	ErrorMessage string
	Failure      *provider.FailureMeta
}

// CompactionEvent describes a detected context compaction boundary.
type CompactionEvent struct {
	ItemID    string
	Trigger   string // "auto" or "manual"
	PreTokens int
	Summary   string
}

// SubagentLaunchContext carries the parent session context for a flavor that
// tracks an asynchronous subagent out of band.
type SubagentLaunchContext struct {
	SessionID         string
	ThreadID          string
	TurnID            string
	ToolCallID        string
	RawInput          json.RawMessage
	Started           NormalizedToolStart
	ParentModel       string
	ParentEffort      string
	Emit              func(provider.ProviderEvent)
	OnChildDiscovered func(childConversationID string)
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
	BeforePrompt(ctx context.Context, sessionID string) any
	AfterPrompt(ctx context.Context, sessionID string, stopReason string, beforeToken any) *PromptOutcomeOverride
	ShouldHoldSpontaneousWakeClose(ctx context.Context, sessionID string) bool
	CanTrackSubagent(sessionID string) bool
	WatchSubagent(ctx context.Context, launch SubagentLaunchContext)
	ResolveSubagentLaunch(ctx context.Context, sessionID, childConversationID string) (string, bool)
	InitCompactionCursor(ctx context.Context, sessionID string) int
	DetectCompaction(ctx context.Context, sessionID string, afterIdx int) (nextIdx int, ev *CompactionEvent)
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
	if im == provider.ModePlan {
		if !hasMode("plan") {
			if rt == provider.RuntimeReadOnly {
				return "", PolicyDenyMutating
			}
			return "", PolicyPrompt
		}
		if rt == provider.RuntimeReadOnly {
			return "plan", PolicyDenyMutating
		}
		return "plan", PolicyPrompt
	}
	switch rt {
	case provider.RuntimeReadOnly:
		return pickMode(available, "default", "ask"), PolicyDenyMutating
	case provider.RuntimeApprovalRequired:
		return pickMode(available, "default", "ask"), PolicyPrompt
	case provider.RuntimeAutoAcceptEdits:
		return pickMode(available, "accept-edits", "default", "ask"), PolicyAutoAcceptEdits
	case provider.RuntimeAuto:
		return pickMode(available, "auto", "default", "ask"), PolicyPrompt
	case provider.RuntimeFullAccess:
		return pickMode(available, "accept-edits", "default", "ask"), PolicyAutoAllowNonQuestion
	default:
		return pickMode(available, "default", "ask"), PolicyPrompt
	}
}

func pickMode(available []SessionMode, preferred ...string) string {
	for _, want := range preferred {
		for _, m := range available {
			if m.ID == want {
				return want
			}
		}
	}
	if len(available) > 0 {
		// Never silently fall back to an arbitrary mode (such as yolo) when
		// none of the explicitly safe modes are advertised.
		return ""
	}
	if len(preferred) > 0 {
		return preferred[0]
	}
	return ""
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
	itemStatus := u.Status
	if itemStatus == "cancelled" {
		// Map ACP "cancelled" to AO's "killed" status so triage renders a
		// stopped badge instead of falling through to completed.
		itemStatus = "killed"
	}
	out := NormalizedToolComplete{
		ToolName:   started.ToolName,
		ItemType:   started.ItemType,
		IsError:    u.Status == "failed",
		ItemStatus: itemStatus,
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
				} else if hunks := stripUnifiedDiffHeader(patch); hunks != "" {
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
	allowOnceOpt := findPermissionOption(req.Options, "allow_once", "")
	allowAlwaysOpt := findPermissionOption(req.Options, "allow_always", "")
	allowAnyOpt := findPermissionOption(req.Options, "allow_once", "allow_always")
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
		if allowAnyOpt != "" {
			return PermissionClassification{Action: PermissionAutoAllow, OptionID: allowAnyOpt}
		}
	case PolicyAutoAcceptEdits:
		if isEditToolKind(req.ToolCall.Kind, req.ToolCall.Name) && allowAnyOpt != "" {
			return PermissionClassification{Action: PermissionAutoAllow, OptionID: allowAnyOpt}
		}
	case PolicyDenyMutating:
		// Fail closed in read-only mode: only explicitly read-only tool kinds
		// and names are auto-allowed; unknown or "other" tools are denied.
		if isReadOnlyToolKind(req.ToolCall.Kind, req.ToolCall.Name) && allowAnyOpt != "" {
			return PermissionClassification{Action: PermissionAutoAllow, OptionID: allowAnyOpt}
		}
		return PermissionClassification{Action: PermissionAutoDeny, OptionID: denyOpt}
	case PolicyPrompt:
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
	avail := make([]json.RawMessage, 0, 4)
	avail = append(avail, json.RawMessage(`"cancel"`))
	if denyOpt != "" {
		byKey["deny"] = denyOpt
		byKey["decline"] = denyOpt
		avail = append(avail, json.RawMessage(`"decline"`))
	}
	if allowAlwaysOpt != "" {
		byKey["allow_session"] = allowAlwaysOpt
		byKey["acceptForSession"] = allowAlwaysOpt
		avail = append(avail, json.RawMessage(`"acceptForSession"`))
	}
	if allowOnceOpt != "" {
		byKey["allow"] = allowOnceOpt
		byKey["accept"] = allowOnceOpt
		avail = append(avail, json.RawMessage(`"accept"`))
	}
	for _, opt := range req.Options {
		byKey[opt.OptionID] = opt.OptionID
	}

	approvalInput, approvalDesc := buildApprovalPreview(&req.ToolCall, title)

	return PermissionClassification{
		Action: PermissionAskUser,
		Approval: provider.ApprovalRequest{
			RequestID:          requestID,
			ThreadID:           threadID,
			TurnID:             turnID,
			ToolUseID:          req.ToolCall.ToolCallID,
			ToolName:           toolName,
			Title:              title,
			Description:        approvalDesc,
			Input:              approvalInput,
			Kind:               kind,
			AvailableDecisions: &avail,
		},
		OptionByKey: byKey,
	}
}

const maxApprovalDiffPreviewBytes = 16 * 1024

// MergeApprovalPreview copies any already-computed "path" and "diff" preview
// fields from enriched into base without recomputing unified diffs.
func MergeApprovalPreview(base, enriched json.RawMessage) json.RawMessage {
	if len(enriched) == 0 {
		return base
	}
	var src map[string]any
	if json.Unmarshal(enriched, &src) != nil {
		return base
	}
	diffPath, _ := src["path"].(string)
	diffText, _ := src["diff"].(string)
	return mergeApprovalPreview(base, diffPath, diffText)
}

func mergeApprovalPreview(base json.RawMessage, diffPath, combinedDiff string) json.RawMessage {
	if diffPath == "" && combinedDiff == "" {
		return base
	}
	obj := map[string]any{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &obj); err != nil {
			return base
		}
	}
	changed := false
	if diffPath != "" {
		hasPath := false
		for _, k := range []string{"file_path", "path", "filePath", "TargetFile", "AbsolutePath", "Path"} {
			if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
				hasPath = true
				break
			}
		}
		if !hasPath {
			obj["path"] = diffPath
			changed = true
		}
	}
	if combinedDiff != "" {
		if existing, ok := obj["diff"].(string); !ok || strings.TrimSpace(existing) == "" {
			obj["diff"] = combinedDiff
			changed = true
		}
	}
	if !changed {
		return base
	}
	enriched, err := json.Marshal(obj)
	if err != nil {
		return base
	}
	return enriched
}

func extractApprovalDiffAndPath(tc *ToolCallUpdate) (string, string) {
	if tc == nil {
		return "", ""
	}
	var diffPath string
	var diffBuf strings.Builder
	truncated := false
	for _, item := range tc.Content {
		if item.Type != "diff" {
			continue
		}
		p := strings.TrimSpace(item.Path)
		if diffPath == "" && p != "" {
			diffPath = p
		}
		if truncated {
			continue
		}
		// Avoid running O(N*M) diff generation on giant payloads on the shared
		// ACP reader goroutine: cap individual diff inputs and stop once the
		// 16 KiB preview budget is full.
		if len(item.OldText)+len(item.NewText) > maxApprovalDiffPreviewBytes*4 {
			if diffBuf.Len() > 0 {
				diffBuf.WriteByte('\n')
			}
			diffBuf.WriteString("... (diff omitted: file exceeds approval preview limit)")
			truncated = true
			continue
		}
		patch := BuildUnifiedDiff(p, item.OldText, item.NewText)
		if patch == "" {
			continue
		}
		if diffBuf.Len() > 0 {
			diffBuf.WriteByte('\n')
		}
		if diffBuf.Len()+len(patch) > maxApprovalDiffPreviewBytes {
			rem := maxApprovalDiffPreviewBytes - diffBuf.Len()
			if rem > 0 {
				diffBuf.WriteString(patch[:rem])
			}
			diffBuf.WriteString("\n... (diff truncated)")
			truncated = true
			continue
		}
		diffBuf.WriteString(patch)
	}
	if diffPath == "" && len(tc.Locations) > 0 {
		diffPath = strings.TrimSpace(tc.Locations[0].Path)
	}
	return diffPath, diffBuf.String()
}

func buildApprovalPreview(tc *ToolCallUpdate, fallbackDesc string) (json.RawMessage, string) {
	diffPath, combinedDiff := extractApprovalDiffAndPath(tc)
	desc := fallbackDesc
	if diffPath != "" && !strings.Contains(desc, diffPath) {
		if desc == "" {
			desc = diffPath
		} else {
			desc = desc + ": " + diffPath
		}
	}
	return mergeApprovalPreview(tc.EffectiveInput(), diffPath, combinedDiff), desc
}

func (DefaultFlavor) ExtractPendingQuestion(*ToolCallUpdate) ([]provider.UserInputQuestion, bool) {
	return nil, false
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

func (DefaultFlavor) CanTrackSubagent(string) bool {
	return false
}

func (DefaultFlavor) WatchSubagent(context.Context, SubagentLaunchContext) {}

func (DefaultFlavor) ResolveSubagentLaunch(context.Context, string, string) (string, bool) {
	return "", false
}

func (DefaultFlavor) InitCompactionCursor(context.Context, string) int {
	return -1
}

func (DefaultFlavor) DetectCompaction(context.Context, string, int) (int, *CompactionEvent) {
	return -1, nil
}

func isEditToolKind(kind, name string) bool {
	if strings.EqualFold(strings.TrimSpace(kind), "edit") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "edit", "write", "edit_file", "write_file", "write_to_file",
		"replace_file_content", "multi_replace_file_content", "notebook_edit":
		return true
	default:
		return false
	}
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

// isReadOnlyToolKind reports whether a permission request is positively known
// to be read-only. Unknown kinds (including ACP's "other") return false so
// PolicyDenyMutating fails closed.
func isReadOnlyToolKind(kind, name string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "read", "search", "think", "fetch":
		return true
	case "edit", "delete", "execute", "move", "other":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read_file", "view_file", "read",
		"grep_search", "ripgrep", "search_files", "grep",
		"find_by_name", "glob", "list_dir",
		"search_web", "web_search", "read_url_content", "fetch_url", "web_fetch":
		return true
	default:
		return false
	}
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

// stripUnifiedDiffHeader removes the leading `--- ...` and `+++ ...` lines from
// a patch produced by BuildUnifiedDiff so callers do not compute the diff twice.
func stripUnifiedDiffHeader(patch string) string {
	idx := strings.Index(patch, "@@")
	if idx < 0 {
		return ""
	}
	return patch[idx:]
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
