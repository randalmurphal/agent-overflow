package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func permissionDeniedEvent(threadID, toolUseID string, fields map[string]any) provider.ProviderEvent {
	base := map[string]any{
		"kind":               permissionDeniedNotificationKind,
		"toolName":           "Bash",
		"toolUseId":          toolUseID,
		"decisionReasonType": "rule",
		"decisionReason":     "Denied by alwaysDenyRules: Bash(rm:*)",
	}
	for k, v := range fields {
		base[k] = v
	}
	meta, _ := json.Marshal(base)
	itemID := ""
	if toolUseID != "" {
		itemID = "permission-denied:" + toolUseID
	}
	return provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  threadID,
		ItemID:    itemID,
		Content:   "Bash was denied by a permission rule",
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func seedRunningToolCall(t *testing.T, r *Router, threadID, toolUseID string) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"toolName": "Bash", "input": map[string]any{"command": "rm -rf /"}})
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    toolUseID,
		ItemType:  "Bash",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
	}
}

func TestPermissionDeniedPersistsNoticeAndAnnotatesTheToolCall(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedRunningToolCall(t, router, "t1", "toolu_01")

	if err := router.Handle(permissionDeniedEvent("t1", "toolu_01", nil)); err != nil {
		t.Fatalf("handle permission_denied: %v", err)
	}

	notice, found, err := st.GetThreadItem("t1", "permission-denied:toolu_01")
	if err != nil || !found {
		t.Fatalf("notice row: found=%v err=%v", found, err)
	}
	if notice.Kind != itemKindNotification || notice.ToolName != permissionDeniedNotificationKind {
		t.Fatalf("notice row = %+v", notice)
	}
	if notice.Summary != "Bash was denied by a permission rule" {
		t.Fatalf("notice summary = %q", notice.Summary)
	}
	var noticeMeta map[string]any
	if err := json.Unmarshal([]byte(notice.Meta), &noticeMeta); err != nil {
		t.Fatalf("notice meta: %v", err)
	}
	// The reason is the whole point of the row — the sanitizer must not
	// drop it back to {kind,title}.
	if noticeMeta["decisionReason"] != "Denied by alwaysDenyRules: Bash(rm:*)" {
		t.Fatalf("notice meta = %+v", noticeMeta)
	}
	if noticeMeta["decisionReasonType"] != "rule" || noticeMeta["toolName"] != "Bash" {
		t.Fatalf("notice meta = %+v", noticeMeta)
	}

	tool, found, err := st.GetThreadItem("t1", "toolu_01")
	if err != nil || !found {
		t.Fatalf("tool row: found=%v err=%v", found, err)
	}
	if tool.Decision != "declined" {
		t.Fatalf("tool decision = %q, want declined", tool.Decision)
	}
	// Status must stay `running`: the denied tool still gets a real
	// tool_result, and persistToolCallCompletion drops a completion whose
	// row already left running.
	if tool.Status != statusRunning {
		t.Fatalf("tool status = %q, want it left alone at %q", tool.Status, statusRunning)
	}
	var toolMeta map[string]any
	if err := json.Unmarshal([]byte(tool.Meta), &toolMeta); err != nil {
		t.Fatalf("tool meta: %v", err)
	}
	denied, _ := toolMeta["permissionDenied"].(map[string]any)
	if denied["reason"] != "Denied by alwaysDenyRules: Bash(rm:*)" || denied["reasonType"] != "rule" {
		t.Fatalf("tool meta.permissionDenied = %+v", toolMeta["permissionDenied"])
	}
	// The launch meta survives the merge.
	if toolMeta["toolName"] != "Bash" {
		t.Fatalf("annotation clobbered the launch meta: %+v", toolMeta)
	}
}

// The denied tool's real tool_result must still land after the notice —
// the annotation is why this is asserted rather than assumed.
func TestPermissionDeniedLeavesTheToolResultRoutable(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedRunningToolCall(t, router, "t1", "toolu_01")
	if err := router.Handle(permissionDeniedEvent("t1", "toolu_01", nil)); err != nil {
		t.Fatalf("handle permission_denied: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"toolName": "Bash", "isError": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "toolu_01",
		ItemType:  "Bash",
		Content:   "Permission to use Bash has been denied.",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}

	tool, found, err := st.GetThreadItem("t1", "toolu_01")
	if err != nil || !found {
		t.Fatalf("tool row: found=%v err=%v", found, err)
	}
	if tool.Status == statusRunning {
		t.Fatal("the tool_result completion was dropped — the annotation must not settle the row")
	}
	if tool.Decision != "declined" {
		t.Fatalf("completion clobbered the decision: %q", tool.Decision)
	}
}

// No tool_call row (fresh session, dropped launch): the notice is the
// honest record and nothing is fabricated.
func TestPermissionDeniedWithoutAToolRowFabricatesNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(permissionDeniedEvent("t1", "toolu_ghost", nil)); err != nil {
		t.Fatalf("handle permission_denied: %v", err)
	}
	if _, found, err := st.GetThreadItem("t1", "toolu_ghost"); err != nil || found {
		t.Fatalf("ghost tool row created: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetThreadItem("t1", "permission-denied:toolu_ghost"); err != nil || !found {
		t.Fatalf("notice row missing: found=%v err=%v", found, err)
	}
}

// A re-delivered envelope upserts the same row rather than minting a
// second notice — the deterministic id is what makes that true.
func TestPermissionDeniedIsIdempotentOnRedelivery(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedRunningToolCall(t, router, "t1", "toolu_01")

	for i := 0; i < 3; i++ {
		if err := router.Handle(permissionDeniedEvent("t1", "toolu_01", nil)); err != nil {
			t.Fatalf("handle permission_denied #%d: %v", i, err)
		}
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	notices := 0
	for _, item := range items {
		if item.Kind == itemKindNotification {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("notification rows = %d, want 1", notices)
	}
}

// The workspace-boundary flag has to survive to the persisted meta: it is
// what swaps the remedy copy from "a rule denied this" to "add the
// directory to this session".
func TestPermissionDeniedWorkspaceBoundaryReachesTheRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedRunningToolCall(t, router, "t1", "toolu_02")

	evt := permissionDeniedEvent("t1", "toolu_02", map[string]any{
		"toolName":           "Read",
		"decisionReasonType": "workingDir",
		"decisionReason":     "Path is outside allowed working directories",
		"workspaceBoundary":  true,
	})
	evt.Content = "Read was denied — the path is outside this workspace's allowed directories"
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle permission_denied: %v", err)
	}

	notice, _, err := st.GetThreadItem("t1", "permission-denied:toolu_02")
	if err != nil {
		t.Fatalf("notice row: %v", err)
	}
	var noticeMeta map[string]any
	if err := json.Unmarshal([]byte(notice.Meta), &noticeMeta); err != nil {
		t.Fatalf("notice meta: %v", err)
	}
	if boundary, _ := noticeMeta["workspaceBoundary"].(bool); !boundary {
		t.Fatalf("notice meta = %+v", noticeMeta)
	}
	tool, _, err := st.GetThreadItem("t1", "toolu_02")
	if err != nil {
		t.Fatalf("tool row: %v", err)
	}
	var toolMeta map[string]any
	if err := json.Unmarshal([]byte(tool.Meta), &toolMeta); err != nil {
		t.Fatalf("tool meta: %v", err)
	}
	denied, _ := toolMeta["permissionDenied"].(map[string]any)
	if boundary, _ := denied["workspaceBoundary"].(bool); !boundary {
		t.Fatalf("tool meta.permissionDenied = %+v", denied)
	}
}

// permission_retry carries no tool_use_id at all — it is per command
// NAME. It must persist as a plain notice and annotate nothing.
func TestPermissionRetryPersistsAPlainNotice(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedRunningToolCall(t, router, "t1", "toolu_01")

	meta, _ := json.Marshal(map[string]any{
		"kind":     permissionRetryNotificationKind,
		"commands": []string{"git status", "ls"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  "t1",
		Content:   "Allowed git status, ls",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle permission_retry: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var notice store.Item
	for _, item := range items {
		if item.Kind == itemKindNotification {
			notice = item
		}
	}
	if notice.ID == "" || !strings.HasPrefix(notice.ID, "notification:") {
		t.Fatalf("retry notice id = %q, want the counter-allocated form", notice.ID)
	}
	if notice.ToolName != permissionRetryNotificationKind {
		t.Fatalf("retry notice toolName = %q", notice.ToolName)
	}
	var noticeMeta map[string]any
	if err := json.Unmarshal([]byte(notice.Meta), &noticeMeta); err != nil {
		t.Fatalf("notice meta: %v", err)
	}
	commands, _ := noticeMeta["commands"].([]any)
	if len(commands) != 2 || commands[0] != "git status" {
		t.Fatalf("notice meta.commands = %+v", noticeMeta["commands"])
	}
	tool, _, err := st.GetThreadItem("t1", "toolu_01")
	if err != nil {
		t.Fatalf("tool row: %v", err)
	}
	if tool.Decision != "" {
		t.Fatalf("permission_retry annotated a tool call: decision=%q", tool.Decision)
	}
}

// A notice whose producer sent no content still gets a sentence.
func TestPermissionNoticeSummaryFallback(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, _ := json.Marshal(map[string]any{
		"kind":     permissionDeniedNotificationKind,
		"toolName": "Bash",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  "t1",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if item.Kind != itemKindNotification {
			continue
		}
		if item.Summary != "Bash was denied by the permission system" {
			t.Fatalf("summary = %q, want the composed fallback (never the generic placeholder)", item.Summary)
		}
		return
	}
	t.Fatal("no notification row persisted")
}

// A subagent's denial belongs to the subagent. `system/permission_denied`
// is a top-level envelope (no parent_tool_use_id), so without inheriting
// the denied tool row's scope the notice rendered as a main-timeline row
// between agent cards (incident 2026-08-22, read-only review thread).
func TestPermissionDeniedInheritsTheSubagentScopeOfTheDeniedTool(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	launchMeta, _ := json.Marshal(map[string]any{"toolName": "Agent", "input": map[string]any{"description": "L2 review"}})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "toolu_launch",
		ItemType:  "Agent",
		Meta:      launchMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
	bashMeta, _ := json.Marshal(map[string]any{"toolName": "Bash", "input": map[string]any{"command": "go test ./..."}})
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        "t1",
		ItemID:          "toolu_side",
		ItemType:        "Bash",
		ParentToolUseID: "toolu_launch",
		Meta:            bashMeta,
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("seed sidechain tool_call: %v", err)
	}

	if err := router.Handle(permissionDeniedEvent("t1", "toolu_side", map[string]any{"agentId": "a1"})); err != nil {
		t.Fatalf("handle permission_denied: %v", err)
	}

	notice, found, err := st.GetThreadItem("t1", "permission-denied:toolu_side")
	if err != nil || !found {
		t.Fatalf("notice row: found=%v err=%v", found, err)
	}
	if notice.ParentID != "toolu_launch" {
		t.Fatalf("notice parent_id = %q, want the launch toolu_launch (subagent scope)", notice.ParentID)
	}
	tool, _, _ := st.GetThreadItem("t1", "toolu_side")
	if tool.Decision != "declined" {
		t.Fatalf("tool decision = %q, want declined", tool.Decision)
	}

	// A top-level tool's denial stays top-level.
	seedRunningToolCall(t, router, "t1", "toolu_top")
	if err := router.Handle(permissionDeniedEvent("t1", "toolu_top", nil)); err != nil {
		t.Fatalf("handle top-level denial: %v", err)
	}
	top, _, _ := st.GetThreadItem("t1", "permission-denied:toolu_top")
	if top.ParentID != "" {
		t.Fatalf("top-level notice parent_id = %q, want empty", top.ParentID)
	}
}
