package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/aocli"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/project"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

type WorkflowDefinitionPhase struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

type WorkflowDefinitionListing struct {
	ID                     string                    `json:"id"`
	Name                   string                    `json:"name"`
	Scope                  string                    `json:"scope"`
	PhaseCount             int                       `json:"phaseCount"`
	Phases                 []WorkflowDefinitionPhase `json:"phases"`
	DefaultStepMode        bool                      `json:"defaultStepMode"`
	Valid                  bool                      `json:"valid"`
	FirstValidationError   string                    `json:"firstValidationError,omitempty"`
	AllBindingsAvailable   bool                      `json:"allBindingsAvailable"`
	PredictedQueuePosition int                       `json:"predictedQueuePosition"`
}

// WorkflowGetJobNotes reads the continuity notes stored on an automation.
func (a *App) WorkflowGetJobNotes(automationID string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("workflow store unavailable")
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return "", fmt.Errorf("workflow job notes: automation id is required")
	}
	return a.store.GetAutomationNotes(automationID)
}

// WorkflowSetJobNotes replaces one automation's bounded continuity notes.
func (a *App) WorkflowSetJobNotes(automationID, notes string) error {
	if a.store == nil {
		return fmt.Errorf("workflow store unavailable")
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return fmt.Errorf("workflow job notes: automation id is required")
	}
	if len(notes) > notify.MaxBodyBytes {
		return fmt.Errorf("workflow job notes exceed %d bytes", notify.MaxBodyBytes)
	}
	return a.store.SetAutomationNotes(automationID, notes, time.Now().UnixMilli())
}

// WorkflowListDefinitions returns resolved project/shared definitions with the
// existing dry-run validator's first finding and binding cross-check.
func (a *App) WorkflowListDefinitions(projectID string) ([]WorkflowDefinitionListing, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("list workflow definitions: project id is required")
	}
	projectRow, err := a.store.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	bindings, _, err := profile.Load(filepath.Join(
		project.ConfigDir(a.workflowDataRoot(), projectRow.Slug), "profile.yaml",
	))
	if err != nil {
		return nil, err
	}
	resolved, err := aocli.ResolveConfigured(a.workflowDataRoot(), projectRow.Slug)
	if err != nil {
		return nil, err
	}
	position, err := a.store.PredictWorkItemQueuePosition(projectID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	listings := make([]WorkflowDefinitionListing, 0, len(resolved))
	for _, workflow := range resolved {
		validation := def.Validate(workflow, &bindings)
		listing := WorkflowDefinitionListing{
			ID: workflow.Workflow.ID, Name: workflow.Workflow.Name,
			Scope: string(workflow.Scope), PhaseCount: len(workflow.Workflow.Phases),
			Phases:          make([]WorkflowDefinitionPhase, 0, len(workflow.Workflow.Phases)),
			DefaultStepMode: workflow.Workflow.DefaultStepMode,
			Valid:           validation.Valid(), AllBindingsAvailable: true,
			PredictedQueuePosition: position,
		}
		for _, phase := range workflow.Workflow.Phases {
			listing.Phases = append(listing.Phases, WorkflowDefinitionPhase{
				ID: phase.ID, Provider: phase.Provider, Model: phase.Model,
			})
		}
		if len(validation.Findings) > 0 {
			listing.FirstValidationError = validation.Findings[0].Error()
		}
		for _, finding := range validation.Findings {
			if strings.HasPrefix(finding.Code, "binding.") {
				listing.AllBindingsAvailable = false
				break
			}
		}
		listings = append(listings, listing)
	}
	return listings, nil
}

// WorkflowRemoveQueuedItem keeps the record but prevents a not-yet-started run
// from being provisioned.
func (a *App) WorkflowRemoveQueuedItem(itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("remove queued workflow item: item id is required")
	}
	return workflowEngine.RemoveQueued(itemID)
}
