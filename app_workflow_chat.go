package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"agent-overflow/internal/aocli"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"

	"github.com/google/uuid"
)

const (
	workflowChatMCPServerName = "agent-overflow-workflows"
	workflowProposalItemKind  = "workflow_proposal"
)

type workflowChatToolArguments struct {
	Project    string         `json:"project"`
	Workflow   string         `json:"workflow"`
	Goal       string         `json:"goal"`
	Seeds      map[string]any `json:"seeds,omitempty"`
	BaseBranch string         `json:"base_branch,omitempty"`
}

type workflowChatProposalMeta struct {
	State         string          `json:"state"`
	ProjectID     string          `json:"projectId"`
	ProjectName   string          `json:"projectName"`
	ProjectColor  string          `json:"projectColor,omitempty"`
	WorkflowID    string          `json:"workflowId"`
	WorkflowName  string          `json:"workflowName"`
	WorkflowScope string          `json:"workflowScope"`
	Goal          string          `json:"goal"`
	Seeds         json.RawMessage `json:"seeds"`
	BaseBranch    string          `json:"baseBranch"`
	StepMode      bool            `json:"stepMode"`
	WorkItemID    string          `json:"workItemId,omitempty"`
}

// workflowChatEnqueueEligible admits the interactive surfaces: chat, plan
// (the same thread — the agent-mode toggle flips chat↔plan without a session
// restart, so tool availability cannot track the flip), and workflow triage.
func workflowChatEnqueueEligible(thread store.Thread) bool {
	if thread.Provider != string(provider.Claude) && thread.Provider != string(provider.Codex) {
		return false
	}
	switch thread.Mode {
	case threadmode.ModeChat, threadmode.ModePlan, threadmode.ModeWorkflowTriage:
		return true
	default:
		return false
	}
}

// workflowChatMCPServerConfig returns the provider-specific config for a newly
// started eligible session. The setting is intentionally sampled at startup;
// changing it does not mutate an already-running provider process.
func (a *App) workflowChatMCPServerConfig(thread store.Thread) (map[string]any, bool, error) {
	if !workflowChatEnqueueEligible(thread) || !a.currentSettings().WorkflowChatEnqueue {
		return nil, false, nil
	}
	// Unit-level App instances and non-transport embeddings legitimately omit
	// the HTTP server. Production boot installs it before any session starts.
	if a.transportServer.Load() == nil {
		return nil, false, nil
	}
	spec, err := a.workflowChatMCPServerSpec(thread)
	return spec, err == nil, err
}

func (a *App) workflowChatMCPServerSpec(thread store.Thread) (map[string]any, error) {
	srv := a.transportServer.Load()
	if srv == nil {
		return nil, fmt.Errorf("workflow chat MCP: transport server unavailable")
	}
	endpoint, err := url.Parse(srv.AppURL())
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("workflow chat MCP: invalid transport URL")
	}
	endpoint.Path = "/mcp/workflows"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	headers := map[string]string{
		"Authorization":             "Bearer " + srv.Token(),
		transport.MCPThreadIDHeader: thread.ID,
	}
	switch thread.Provider {
	case string(provider.Claude):
		return claude.HTTPMCPServer(endpoint.String(), headers), nil
	case string(provider.Codex):
		return codex.HTTPMCPServer(endpoint.String(), headers), nil
	default:
		return nil, fmt.Errorf("workflow chat MCP: unsupported provider %q", thread.Provider)
	}
}

func (a *App) handleWorkflowMCPToolCall(_ context.Context, threadID string, arguments json.RawMessage) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("workflow proposal: calling thread is required")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("workflow proposal: load calling thread: %w", err)
	}
	if !workflowChatEnqueueEligible(thread) {
		return "", fmt.Errorf("workflow proposal: thread mode %q is not eligible", thread.Mode)
	}
	var input workflowChatToolArguments
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", fmt.Errorf("workflow proposal: invalid arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("workflow proposal: invalid arguments: multiple JSON values")
	}
	input.Project = strings.TrimSpace(input.Project)
	input.Workflow = strings.TrimSpace(input.Workflow)
	input.Goal = strings.TrimSpace(input.Goal)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	if input.Seeds == nil {
		input.Seeds = map[string]any{}
	}
	if input.Project == "" || input.Workflow == "" || input.Goal == "" {
		return "", fmt.Errorf("workflow proposal: project, workflow, and goal are required")
	}
	projectRow, resolved, baseBranch, seeds, err := a.validateWorkflowChatProposal(input)
	if err != nil {
		return "", err
	}
	meta := workflowChatProposalMeta{
		State: "pending", ProjectID: projectRow.ID, ProjectName: projectRow.Name,
		ProjectColor: projectRow.Color, WorkflowID: resolved.Workflow.ID,
		WorkflowName: resolved.Workflow.Name, WorkflowScope: string(resolved.Scope),
		Goal: input.Goal, Seeds: seeds, BaseBranch: baseBranch,
		StepMode: resolved.Workflow.DefaultStepMode,
	}
	item, err := a.persistWorkflowChatProposal(threadID, meta)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Workflow proposal %s was recorded and is awaiting user confirmation. Nothing has been enqueued.", item.ID), nil
}

func (a *App) validateWorkflowChatProposal(input workflowChatToolArguments) (store.Project, def.ResolvedWorkflow, string, json.RawMessage, error) {
	projectRow, err := a.store.GetProject(input.Project)
	if err != nil && errors.Is(err, sql.ErrNoRows) {
		projectRow, err = a.store.GetProjectBySlug(input.Project)
	}
	if err != nil {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: project %q was not found", input.Project)
	}
	if projectRow.Archived {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: project %q is archived", projectRow.Name)
	}
	resolvedList, err := aocli.ResolveConfigured(a.workflowDataRoot(), projectRow.Slug)
	if err != nil {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: resolve definitions: %w", err)
	}
	var resolved *def.ResolvedWorkflow
	for i := range resolvedList {
		if resolvedList[i].Workflow.ID == input.Workflow {
			resolved = &resolvedList[i]
			break
		}
	}
	if resolved == nil {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: workflow %q was not found for project %q", input.Workflow, projectRow.Name)
	}
	profiles := workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()}
	bindings, err := profiles.Profile(a.lifeCtx(), projectRow.ID)
	if err != nil {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: load project profile: %w", err)
	}
	validation := def.Validate(*resolved, bindings)
	if !validation.Valid() {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: workflow %q is invalid: %s", input.Workflow, validation.Findings[0].Error())
	}
	if validationErrors := def.ValidateInputs(resolved.Workflow, input.Seeds); len(validationErrors) > 0 {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: %s", strings.Join(validationErrors, "; "))
	}
	baseBranch := input.BaseBranch
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(bindings.BaseBranch)
	}
	if baseBranch != "" {
		if err := gitops.ValidateBranchName(baseBranch); err != nil {
			return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: invalid base branch: %w", err)
		}
	}
	seeds, err := json.Marshal(input.Seeds)
	if err != nil {
		return store.Project{}, def.ResolvedWorkflow{}, "", nil, fmt.Errorf("workflow proposal: encode seeds: %w", err)
	}
	return projectRow, *resolved, baseBranch, seeds, nil
}

func (a *App) persistWorkflowChatProposal(threadID string, meta workflowChatProposalMeta) (store.Item, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return store.Item{}, fmt.Errorf("workflow proposal: encode timeline item: %w", err)
	}
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("workflow proposal: resolve timeline position: %w", err)
	}
	now := time.Now().UnixMilli()
	item := store.Item{
		ID: "workflow-proposal:" + uuid.NewString(), ThreadID: threadID,
		TurnIndex: turnIndex, Kind: workflowProposalItemKind, Role: "assistant",
		Status: "completed", Summary: meta.Goal, Meta: string(encoded),
		CreatedAt: now, UpdatedAt: now,
	}
	item.ItemIndex, err = a.store.AppendItem(item)
	if err != nil {
		return store.Item{}, fmt.Errorf("workflow proposal: persist timeline item: %w", err)
	}
	a.emit("provider:item_event", triage.NewItemStreamUpsert(item))
	return item, nil
}

// WorkflowQueueChatProposal resolves a pending card by enqueueing through the
// normal workflow path with agent provenance. Edited intake values are passed
// here so the persisted receipt matches the run the user approved.
func (a *App) WorkflowQueueChatProposal(threadID, proposalID, projectID, workflowID, workflowScope, goal string, seeds json.RawMessage, baseBranch string, stepMode bool) (store.WorkItem, error) {
	a.workflowChatProposalMu.Lock()
	defer a.workflowChatProposalMu.Unlock()
	item, meta, err := a.loadPendingWorkflowChatProposal(threadID, proposalID)
	if err != nil {
		return store.WorkItem{}, err
	}
	workItem, found, err := a.store.GetWorkItemBySourceRef("agent", proposalID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if !found {
		workItem, err = a.enqueueWorkflowItem(projectID, workflowID, workflowScope, goal, seeds, (*profile.Budget)(nil), baseBranch, stepMode, "agent", proposalID)
		if err != nil {
			return store.WorkItem{}, err
		}
	}
	projectRow, err := a.store.GetProject(workItem.ProjectID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("queue workflow proposal: reload project: %w", err)
	}
	resolved, err := aocli.ResolveWorkflow(a.workflowDataRoot(), projectRow.Slug, workItem.WorkflowID, def.Scope(workItem.WorkflowScope))
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("queue workflow proposal: reload workflow: %w", err)
	}
	meta.State = "queued"
	meta.ProjectID, meta.ProjectName, meta.ProjectColor = projectRow.ID, projectRow.Name, projectRow.Color
	meta.WorkflowID, meta.WorkflowName, meta.WorkflowScope = workItem.WorkflowID, resolved.Workflow.Name, workItem.WorkflowScope
	meta.Goal, meta.Seeds, meta.BaseBranch, meta.StepMode = workItem.Goal, workItem.Seeds, workItem.BaseBranch, workItem.StepMode
	meta.WorkItemID = workItem.ID
	if err := a.updateWorkflowChatProposal(item, meta); err != nil {
		return store.WorkItem{}, err
	}
	return workItem, nil
}

func (a *App) WorkflowDismissChatProposal(threadID, proposalID string) error {
	a.workflowChatProposalMu.Lock()
	defer a.workflowChatProposalMu.Unlock()
	item, meta, err := a.loadPendingWorkflowChatProposal(threadID, proposalID)
	if err != nil {
		return err
	}
	meta.State = "dismissed"
	return a.updateWorkflowChatProposal(item, meta)
}

func (a *App) loadPendingWorkflowChatProposal(threadID, proposalID string) (store.Item, workflowChatProposalMeta, error) {
	item, found, err := a.store.GetThreadItem(strings.TrimSpace(threadID), strings.TrimSpace(proposalID))
	if err != nil {
		return store.Item{}, workflowChatProposalMeta{}, err
	}
	if !found || item.Kind != workflowProposalItemKind {
		return store.Item{}, workflowChatProposalMeta{}, fmt.Errorf("workflow proposal %q was not found on this thread", proposalID)
	}
	var meta workflowChatProposalMeta
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		return store.Item{}, workflowChatProposalMeta{}, fmt.Errorf("workflow proposal %q has invalid persisted state: %w", proposalID, err)
	}
	if meta.State != "pending" {
		return store.Item{}, workflowChatProposalMeta{}, fmt.Errorf("workflow proposal %q is already %s", proposalID, meta.State)
	}
	return item, meta, nil
}

func (a *App) updateWorkflowChatProposal(item store.Item, meta workflowChatProposalMeta) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("workflow proposal: encode resolved state: %w", err)
	}
	now := time.Now().UnixMilli()
	encodedString := string(encoded)
	if err := a.store.UpdateItemFields(item.ThreadID, item.ID, store.ItemPartialUpdate{Summary: &meta.Goal, Meta: &encodedString, UpdatedAt: &now}); err != nil {
		return fmt.Errorf("workflow proposal: persist resolved state: %w", err)
	}
	item.Summary, item.Meta, item.UpdatedAt = meta.Goal, encodedString, now
	a.emit("provider:item_event", triage.NewItemStreamUpsert(item))
	return nil
}
