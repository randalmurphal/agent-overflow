package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/engine"
)

func setupWorkflowChatProposalTest(t *testing.T) (*App, store.Project, store.Thread) {
	t.Helper()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeWorkflowFixture(t, configRoot)
	if _, err := app.settings.Update(map[string]any{"workflowPaused": true}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowEngine != nil {
			_ = app.workflowEngine.Close()
		}
	})
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	projectRow, err := app.store.GetProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := app.CreateThread(CreateThreadOptions{
		ProjectID: projectRow.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, projectRow, thread
}

func recordWorkflowChatProposal(t *testing.T, app *App, thread store.Thread, project store.Project, goal string) store.Item {
	t.Helper()
	result, err := app.handleWorkflowMCPToolCall(context.Background(), thread.ID, json.RawMessage(`{
		"project": `+quoted(project.Slug)+`, "workflow": "packet-flow", "goal": `+quoted(goal)+`,
		"seeds": {"goal": `+quoted(goal)+`}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "awaiting user confirmation") || !strings.Contains(result, "Nothing has started") {
		t.Fatalf("tool result = %q", result)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == workflowProposalItemKind && item.Summary == goal {
			return item
		}
	}
	t.Fatalf("persisted proposal %q not found after reload: %+v", goal, items)
	return store.Item{}
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestWorkflowChatToolPersistsProposalAndSurfacesValidation(t *testing.T) {
	app, projectRow, thread := setupWorkflowChatProposalTest(t)
	proposal := recordWorkflowChatProposal(t, app, thread, projectRow, "Ship chat enqueue")
	var meta workflowChatProposalMeta
	if err := json.Unmarshal([]byte(proposal.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.State != "pending" || meta.ProjectID != projectRow.ID || meta.WorkflowScope != "shared" {
		t.Fatalf("proposal meta = %+v", meta)
	}

	_, err := app.handleWorkflowMCPToolCall(context.Background(), thread.ID, json.RawMessage(`{
		"project": `+quoted(projectRow.ID)+`, "workflow": "packet-flow", "goal": "bad seeds",
		"seeds": {"goal": false}
	}`))
	if err == nil || !strings.Contains(err.Error(), "$.seeds.goal must be a string") {
		t.Fatalf("invalid seed error = %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("invalid tool call persisted another item: %+v", items)
	}
}

func TestWorkflowChatProposalQueueAndDismissPersistResolvedState(t *testing.T) {
	app, projectRow, thread := setupWorkflowChatProposalTest(t)
	queuedProposal := recordWorkflowChatProposal(t, app, thread, projectRow, "Queue me")
	workItem, err := app.WorkflowQueueChatProposal(
		thread.ID, queuedProposal.ID, projectRow.ID, "packet-flow", "shared", "Queue me edited",
		json.RawMessage(`{"goal":"Queue me edited"}`), "", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if workItem.Source != "agent" || workItem.SourceRef != queuedProposal.ID || workItem.State != string(engine.StateRunning) {
		t.Fatalf("queued work item = %+v", workItem)
	}
	resolved, found, err := app.store.GetThreadItem(thread.ID, queuedProposal.ID)
	if err != nil || !found {
		t.Fatalf("reload queued proposal: found=%t err=%v", found, err)
	}
	var queuedMeta workflowChatProposalMeta
	if err := json.Unmarshal([]byte(resolved.Meta), &queuedMeta); err != nil {
		t.Fatal(err)
	}
	if queuedMeta.State != "started" || queuedMeta.WorkItemID != workItem.ID || queuedMeta.Goal != "Queue me edited" {
		t.Fatalf("queued proposal meta = %+v", queuedMeta)
	}
	if _, err := app.WorkflowQueueChatProposal(thread.ID, queuedProposal.ID, projectRow.ID, "packet-flow", "shared", "again", json.RawMessage(`{"goal":"again"}`), "", false); err == nil {
		t.Fatal("resolved proposal queued twice")
	}
	// Simulate a crash after the work item commit but before the proposal
	// receipt commit. A retry must reconcile to the provenance row, not create
	// a second run.
	queuedMeta.State, queuedMeta.WorkItemID = "pending", ""
	pendingBytes, err := json.Marshal(queuedMeta)
	if err != nil {
		t.Fatal(err)
	}
	pendingString := string(pendingBytes)
	if err := app.store.UpdateItemFields(thread.ID, queuedProposal.ID, store.ItemPartialUpdate{Meta: &pendingString}); err != nil {
		t.Fatal(err)
	}
	recovered, err := app.WorkflowQueueChatProposal(
		thread.ID, queuedProposal.ID, projectRow.ID, "packet-flow", "shared", "different retry",
		json.RawMessage(`{"goal":"different retry"}`), "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != workItem.ID {
		t.Fatalf("crash recovery work item = %q, want existing %q", recovered.ID, workItem.ID)
	}
	workItems, err := app.store.ListWorkItems(store.WorkItemListFilter{ProjectID: projectRow.ID})
	if err != nil || len(workItems) != 1 {
		t.Fatalf("crash recovery work items = %+v, err=%v", workItems, err)
	}

	dismissedProposal := recordWorkflowChatProposal(t, app, thread, projectRow, "Dismiss me")
	if err := app.WorkflowDismissChatProposal(thread.ID, dismissedProposal.ID); err != nil {
		t.Fatal(err)
	}
	dismissed, found, err := app.store.GetThreadItem(thread.ID, dismissedProposal.ID)
	if err != nil || !found {
		t.Fatalf("reload dismissed proposal: found=%t err=%v", found, err)
	}
	var dismissedMeta workflowChatProposalMeta
	if err := json.Unmarshal([]byte(dismissed.Meta), &dismissedMeta); err != nil {
		t.Fatal(err)
	}
	if dismissedMeta.State != "dismissed" {
		t.Fatalf("dismissed proposal state = %q", dismissedMeta.State)
	}
}

func TestWorkflowChatMCPConfigGatesNewProviderSessions(t *testing.T) {
	app, _ := setupE2EApp(t)
	server, err := transport.New(transport.Config{
		Token: "workflow-token", Dispatcher: transport.NewDispatcher(), EventBus: transport.NewEventBus(0),
		MCPToolCall: app.handleWorkflowMCPToolCall,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	app.SetTransportServer(server)

	for _, providerName := range []string{string(provider.Claude), string(provider.Codex)} {
		thread := store.Thread{ID: "thread-" + providerName, Provider: providerName, Mode: "chat"}
		spec, enabled, err := app.workflowChatMCPServerConfig(thread)
		if err != nil || !enabled {
			t.Fatalf("%s enabled config: enabled=%t err=%v", providerName, enabled, err)
		}
		headerKey := "headers"
		if providerName == string(provider.Codex) {
			headerKey = "http_headers"
		}
		headers := spec[headerKey].(map[string]string)
		if headers["Authorization"] != "Bearer workflow-token" || headers[transport.MCPThreadIDHeader] != thread.ID {
			t.Fatalf("%s headers = %#v", providerName, headers)
		}
		if !strings.Contains(spec["url"].(string), "/mcp/workflows") {
			t.Fatalf("%s url = %v", providerName, spec["url"])
		}
	}
	triage := store.Thread{ID: "triage", Provider: string(provider.Claude), Mode: "workflow-triage"}
	if _, enabled, err := app.workflowChatMCPServerConfig(triage); err != nil || !enabled {
		t.Fatalf("triage config: enabled=%t err=%v", enabled, err)
	}
	// Plan is the same interactive thread as chat (the agent-mode toggle flips
	// chat↔plan without a session restart), so it must stay eligible.
	plan := store.Thread{ID: "plan", Provider: string(provider.Claude), Mode: "plan"}
	if _, enabled, err := app.workflowChatMCPServerConfig(plan); err != nil || !enabled {
		t.Fatalf("plan config: enabled=%t err=%v", enabled, err)
	}
	studio := store.Thread{ID: "studio", Provider: string(provider.Claude), Mode: "workflow-studio"}
	if _, enabled, err := app.workflowChatMCPServerConfig(studio); err != nil || enabled {
		t.Fatalf("studio config: enabled=%t err=%v", enabled, err)
	}
	if _, err := app.settings.Update(map[string]any{"workflowChatEnqueue": false}); err != nil {
		t.Fatal(err)
	}
	chat := store.Thread{ID: "off", Provider: string(provider.Codex), Mode: "chat"}
	if _, enabled, err := app.workflowChatMCPServerConfig(chat); err != nil || enabled {
		t.Fatalf("disabled setting config: enabled=%t err=%v", enabled, err)
	}
}
