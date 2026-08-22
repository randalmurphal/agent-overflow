package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// Claude's permission-notice family. Both subtypes arrive as
// EventNotification and are told apart by `meta.kind`, mirroring the
// model-fallback family. See parse_system.go for the wire shapes.
const (
	permissionDeniedNotificationKind = "permission_denied"
	permissionRetryNotificationKind  = "permission_retry"
)

// permissionNoticeMeta is the decoded shape of a permission notice's
// wire meta. Fields not present on a given subtype stay zero:
// permission_retry carries only Commands, permission_denied carries
// everything else.
type permissionNoticeMeta struct {
	Kind               string   `json:"kind"`
	ToolName           string   `json:"toolName"`
	ToolUseID          string   `json:"toolUseId"`
	AgentID            string   `json:"agentId"`
	DecisionReasonType string   `json:"decisionReasonType"`
	DecisionReason     string   `json:"decisionReason"`
	WorkspaceBoundary  bool     `json:"workspaceBoundary"`
	Commands           []string `json:"commands"`
}

func isPermissionNoticeKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case permissionDeniedNotificationKind, permissionRetryNotificationKind:
		return true
	default:
		return false
	}
}

// addPermissionNoticeMeta forwards the notice's own fields onto the
// persisted notification meta. Without this the sanitizer would keep
// only `kind` + `title`, and the reason — the entire point of the row —
// would be dropped on the floor.
//
// It is a WHITELIST, not a bound: every field here is already truncated by
// claude/parse_system.go's `maxClaudePermission*` family at the point it is
// read off the wire, and a second copy of those five numbers on this side was
// only ever a way for the two to disagree. What this function decides is WHICH
// fields become persisted meta.
func addPermissionNoticeMeta(meta map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var notice permissionNoticeMeta
	if json.Unmarshal(raw, &notice) != nil {
		return
	}
	if value := strings.TrimSpace(notice.ToolName); value != "" {
		meta["toolName"] = value
	}
	if value := strings.TrimSpace(notice.ToolUseID); value != "" {
		meta["toolUseId"] = value
	}
	if value := strings.TrimSpace(notice.AgentID); value != "" {
		meta["agentId"] = value
	}
	if value := strings.TrimSpace(notice.DecisionReasonType); value != "" {
		meta["decisionReasonType"] = value
	}
	if value := strings.TrimSpace(notice.DecisionReason); value != "" {
		meta["decisionReason"] = value
	}
	if notice.WorkspaceBoundary {
		// Only ever recorded true — see parse_system.go. The renderer
		// reads it as "swap the remedy copy", so a false would be a claim
		// the wire never made.
		meta["workspaceBoundary"] = true
	}
	if commands := commandNames(notice.Commands); len(commands) > 0 {
		meta["commands"] = commands
	}
}

// commandNames drops the empties out of the notice's command list. Length and
// per-entry size are the parser's job (maxClaudePermissionCommands /
// maxClaudePermissionCommandRunes).
func commandNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// lookupDeniedToolCall fetches the tool_call row a permission_denied
// notice explains. found is false when the envelope carried no
// tool_use_id, the row does not exist yet (fresh session, dropped
// launch), or the id names something other than a tool call. A lookup
// error is logged and reads as not found: the notice row is the durable
// record either way and must not fail on the cross-reference.
func (r *Router) lookupDeniedToolCall(threadID string, notice permissionNoticeMeta) (store.Item, bool) {
	if r == nil || r.store == nil {
		return store.Item{}, false
	}
	toolUseID := strings.TrimSpace(notice.ToolUseID)
	if toolUseID == "" {
		return store.Item{}, false
	}
	item, found, err := r.store.GetThreadItem(threadID, toolUseID)
	if err != nil {
		log.Printf("triage: permission denial tool lookup %s: %v", toolUseID, err)
		return store.Item{}, false
	}
	if !found || item.Kind != itemKindToolCall {
		return store.Item{}, false
	}
	return item, true
}

// deniedToolCallScope is the scope a permission_denied notice persists
// under. `system/permission_denied` is a top-level envelope — it never
// carries `parent_tool_use_id`, only the denied `tool_use_id` — so a
// subagent's denial would otherwise land as a main-timeline row between
// agent cards while the tool it explains sits inside one (incident
// 2026-08-22: a read-only thread's review agents' Bash denials rendered
// as the main agent's). The denied tool row's own parent_id IS that
// attribution, for Agent launches and forked skills alike, so the notice
// inherits it. A scope the event already carries wins; a missing row
// leaves the notice top-level, which is the honest record.
func deniedToolCallScope(evt provider.ProviderEvent, tool store.Item, found bool) string {
	if scope := eventParentID(evt); scope != "" {
		return scope
	}
	if !found {
		return ""
	}
	return strings.TrimSpace(tool.ParentID)
}

// annotateDeniedToolCall attaches the denial to the tool_call row it
// explains, so the card itself says "Declined" instead of showing a
// tool that quietly produced a rejection sentence. The standalone
// notice row still carries the full reason; this is the cross-reference
// the eye lands on first.
//
// Three deliberate narrowings:
//
//   - Only `permission_denied` (permission_retry carries no tool_use_id
//     at all — it is per command NAME, not per tool call).
//   - Status is NEVER touched. The denied tool still gets a real
//     tool_result (the CLI hands the rejection message to the model as
//     one), and persistToolCallCompletion DROPS a completion whose row
//     already left `running`. Writing a terminal status here would make
//     the tool card lose its actual result.
//   - A missing row is a no-op, not a fabricated one. The denial lands
//     after the assistant tool_use in wire order, so the row normally
//     exists; when it does not (fresh session, dropped launch) the
//     notice row alone is the honest record.
//
// Failures are logged, never propagated: the notice row is already
// persisted and visible, and losing the chip must not fail the event.
//
// The row is fetched ONCE by lookupDeniedToolCall, because the caller
// needs it before the notice is persisted: the notice row inherits the
// denied tool's scope (see deniedToolCallScope).
func (r *Router) annotateDeniedToolCall(evt provider.ProviderEvent, notice permissionNoticeMeta, item store.Item, found bool) {
	if r == nil || r.store == nil || !found {
		return
	}
	toolUseID := strings.TrimSpace(notice.ToolUseID)
	if toolUseID == "" {
		return
	}

	fields := map[string]any{
		"permissionDenied": permissionDeniedToolMeta(notice),
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		log.Printf("triage: encode permission denial meta for %s: %v", toolUseID, err)
		return
	}
	mergedMeta := mergeItemMetaJSON(item.Meta, encoded)
	if item.Decision == decisionDeclined && mergedMeta == item.Meta {
		return
	}
	item.Meta = mergedMeta
	// `declined` is the existing vocabulary for "this tool call was
	// refused" (ToolDecisionChip renders it), and a pre-ask auto-deny is
	// the same outcome as a user decline minus the dialog. Recording it
	// under a second name would fork the chip for no user-visible gain.
	item.Decision = decisionDeclined
	item.UpdatedAt = eventTimestampMillis(evt)
	if err := r.persistItem(item, nil); err != nil {
		log.Printf("triage: annotate denied tool call %s: %v", toolUseID, err)
	}
}

func permissionDeniedToolMeta(notice permissionNoticeMeta) map[string]any {
	out := map[string]any{}
	if value := strings.TrimSpace(notice.DecisionReason); value != "" {
		out["reason"] = value
	}
	if value := strings.TrimSpace(notice.DecisionReasonType); value != "" {
		out["reasonType"] = value
	}
	if notice.WorkspaceBoundary {
		out["workspaceBoundary"] = true
	}
	return out
}

// permissionNoticeSummaryFallback composes a sentence for a notice whose
// producer sent no content. Only reachable from a malformed envelope —
// the parser always composes one — but a notice with no sentence is
// worse than a generic one.
func permissionNoticeSummaryFallback(notice permissionNoticeMeta) string {
	if notice.Kind == permissionRetryNotificationKind {
		return "Retrying previously-denied commands"
	}
	tool := strings.TrimSpace(notice.ToolName)
	if tool == "" {
		tool = "A tool call"
	}
	return fmt.Sprintf("%s was denied by the permission system", tool)
}

// decisionDeclined is the `items.decision` value ToolDecisionChip renders as
// "Declined". It is the same word as statusDeclined and deliberately so —
// decision and status are different columns describing the same refusal.
const decisionDeclined = statusDeclined
