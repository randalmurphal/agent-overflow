package main

import (
	"fmt"
	"strings"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// The `/workflow` composer context (spec §5, D15/D31). Sending a message that
// starts with `/workflow` appends a text block telling the agent that the
// `agent-overflow` command exists, that its credentials are already in the
// environment, which workflows this project has, and what is already running.
//
// The block's format is owned by internal/aocli (pure, unit-tested); this
// resolver produces the live data behind it. It is NOT a bound method: the
// block never reaches the frontend — the send path
// (app_composer_commands.go) is its only caller, and it appends the block to
// the provider payload only.

// workflowComposerBlock renders the `/workflow` block for one thread.
func (a *App) workflowComposerBlock(threadID string) (string, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "", fmt.Errorf("workflow composer context: thread id is required")
	}
	if a.store == nil {
		return "", fmt.Errorf("workflow composer context: store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", err
	}
	context := aocli.ComposerContext{
		SessionReady: len(a.sessionAOEnv(threadID)) > 0,
		// Boot publishes the command under its canonical name and hands the
		// directory to every session's PATH (D30); an empty cliBinDir means
		// that failed and the block has to say so.
		CommandOnPath: a.cliBinDir != "",
	}
	slug := ""
	if strings.TrimSpace(thread.ProjectID) != "" {
		projectRow, err := a.store.GetProject(thread.ProjectID)
		if err != nil {
			return "", err
		}
		context.ProjectName = projectRow.Name
		slug = projectRow.Slug
	}
	context.SharedDir, context.ProjectDir = aocli.WorkflowSourceDirs(a.workflowDataRoot(), slug)

	resolved, err := aocli.ResolveConfigured(a.workflowDataRoot(), slug)
	if err != nil {
		return "", fmt.Errorf("workflow composer context: %w", err)
	}
	context.Workflows = make([]aocli.ComposerWorkflow, 0, len(resolved))
	for _, workflow := range resolved {
		context.Workflows = append(context.Workflows, aocli.ComposerWorkflow{
			ID: workflow.Workflow.ID, Name: workflow.Workflow.Name, Scope: string(workflow.Scope),
		})
	}

	if thread.ProjectID != "" {
		runs, err := a.store.ListWorkItemSummaries(store.WorkItemListFilter{
			ProjectID: thread.ProjectID,
			States:    []string{string(engine.StateRunning), string(engine.StateNeedsHuman)},
		})
		if err != nil {
			return "", err
		}
		// Newest first: a composer block is read top-down, and the run someone is
		// about to ask about is almost always the most recent one.
		context.Runs = make([]aocli.ComposerRun, 0, len(runs))
		for i := len(runs) - 1; i >= 0; i-- {
			context.Runs = append(context.Runs, aocli.ComposerRun{
				ItemID: runs[i].ID, WorkflowID: runs[i].WorkflowID, State: runs[i].State,
				Reason: runs[i].Reason, PhaseID: runs[i].CurrentPhaseID,
			})
		}
	}
	return aocli.RenderComposerContext(context), nil
}
