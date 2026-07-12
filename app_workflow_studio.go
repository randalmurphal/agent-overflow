package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// WorkflowOpenStudioThread opens or returns the project/workflow-specific
// studio shell. Studio tooling and kickoff prompts land in M4; this RPC only
// creates the correctly scoped thread and title.
func (a *App) WorkflowOpenStudioThread(projectID, workflowID string) (store.Thread, error) {
	if _, err := a.requireWorkflowEngine(); err != nil {
		return store.Thread{}, err
	}
	projectID = strings.TrimSpace(projectID)
	workflowID = strings.TrimSpace(workflowID)
	if projectID == "" {
		return store.Thread{}, fmt.Errorf("open workflow studio: project id is required")
	}

	title := workflowStudioTitle(workflowID)
	threadID := workflowStudioThreadID(projectID, workflowID)
	unlock := a.threadLocks().Lock("workflow-studio:" + projectID + ":" + workflowID)
	defer unlock()
	thread, err := a.store.GetThread(threadID)
	if err == nil {
		if thread.ProjectID != projectID || thread.Mode != threadmode.ModeWorkflowStudio {
			return store.Thread{}, fmt.Errorf("open workflow studio: reserved thread identity has incompatible metadata")
		}
		return thread, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.Thread{}, err
	}

	projectRow, err := a.store.GetProject(projectID)
	if err != nil {
		return store.Thread{}, err
	}
	seed := a.seedChatModelProfile("", "")
	now := time.Now().UnixMilli()
	thread = store.Thread{
		ID: threadID, ProjectID: projectRow.ID, ProjectPath: projectRow.Path,
		Title: title, Provider: seed.Provider, Model: seed.Model,
		WorkspacePath: projectRow.Path, Mode: threadmode.ModeWorkflowStudio,
		ReasoningEffort: seed.ReasoningEffort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow, RuntimeMode: seed.RuntimeMode,
		CreatedAt: now, UpdatedAt: now,
	}
	thread = a.sanitizeThreadModelSettings(thread)
	thread.DisabledMcpServers = a.snapshotDisabledMcpServers(thread.Provider, thread.WorkspacePath)
	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("create workflow studio thread: %w", err)
	}
	return a.store.GetThread(thread.ID)
}

func workflowStudioThreadID(projectID, workflowID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-overflow:workflow-studio:"+projectID+":"+workflowID)).String()
}

func workflowStudioTitle(workflowID string) string {
	if workflowID == "" {
		return "Workflow studio — new workflow"
	}
	return "Workflow studio — " + workflowID
}
