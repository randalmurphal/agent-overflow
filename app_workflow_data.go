package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

type WorkflowDefinitionInput struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Required  bool            `json:"required"`
	Enum      []any           `json:"enum,omitempty"`
	Format    string          `json:"format,omitempty"`
	Default   json.RawMessage `json:"default,omitempty"`
	Multiline bool            `json:"multiline,omitempty"`
}

type WorkflowDefinitionListing struct {
	ID                   string                    `json:"id"`
	Name                 string                    `json:"name"`
	Scope                string                    `json:"scope"`
	PhaseCount           int                       `json:"phaseCount"`
	Phases               []WorkflowDefinitionPhase `json:"phases"`
	Inputs               []WorkflowDefinitionInput `json:"inputs"`
	DefaultStepMode      bool                      `json:"defaultStepMode"`
	Valid                bool                      `json:"valid"`
	FirstValidationError string                    `json:"firstValidationError,omitempty"`
	AllBindingsAvailable bool                      `json:"allBindingsAvailable"`
}

type WorkflowDefinitionCatalog struct {
	BaseBranch             string                      `json:"baseBranch"`
	PredictedQueuePosition int                         `json:"predictedQueuePosition"`
	Workflows              []WorkflowDefinitionListing `json:"workflows"`
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
func (a *App) WorkflowListDefinitions(projectID string) (WorkflowDefinitionCatalog, error) {
	if a.store == nil {
		return WorkflowDefinitionCatalog{}, fmt.Errorf("workflow store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return WorkflowDefinitionCatalog{}, fmt.Errorf("list workflow definitions: project id is required")
	}
	projectRow, err := a.store.GetProject(projectID)
	if err != nil {
		return WorkflowDefinitionCatalog{}, err
	}
	bindings, _, err := profile.Load(filepath.Join(
		project.ConfigDir(a.workflowDataRoot(), projectRow.Slug), "profile.yaml",
	))
	if err != nil {
		return WorkflowDefinitionCatalog{}, err
	}
	resolved, err := aocli.ResolveConfigured(a.workflowDataRoot(), projectRow.Slug)
	if err != nil {
		return WorkflowDefinitionCatalog{}, err
	}
	position, err := a.store.PredictWorkItemQueuePosition(projectID, time.Now().UnixMilli())
	if err != nil {
		return WorkflowDefinitionCatalog{}, err
	}
	listings := make([]WorkflowDefinitionListing, 0, len(resolved))
	for _, workflow := range resolved {
		validation := def.Validate(workflow, &bindings)
		listing := WorkflowDefinitionListing{
			ID: workflow.Workflow.ID, Name: workflow.Workflow.Name,
			Scope: string(workflow.Scope), PhaseCount: len(workflow.Workflow.Phases),
			Phases:          make([]WorkflowDefinitionPhase, 0, len(workflow.Workflow.Phases)),
			Inputs:          make([]WorkflowDefinitionInput, 0, len(workflow.Workflow.Inputs)),
			DefaultStepMode: workflow.Workflow.DefaultStepMode,
			Valid:           validation.Valid(), AllBindingsAvailable: true,
		}
		for _, phase := range workflow.Workflow.Phases {
			listing.Phases = append(listing.Phases, WorkflowDefinitionPhase{
				ID: phase.ID, Provider: phase.Provider, Model: phase.Model,
			})
		}
		inputNames := make([]string, 0, len(workflow.Workflow.Inputs))
		for name := range workflow.Workflow.Inputs {
			inputNames = append(inputNames, name)
		}
		sort.Strings(inputNames)
		for _, name := range inputNames {
			input := workflow.Workflow.Inputs[name]
			listing.Inputs = append(listing.Inputs, WorkflowDefinitionInput{
				Name: name, Type: input.Schema.Type, Required: !input.Optional,
				Enum: append([]any(nil), input.Schema.Enum...), Format: input.Schema.Format,
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
	return WorkflowDefinitionCatalog{
		BaseBranch: bindings.BaseBranch, PredictedQueuePosition: position, Workflows: listings,
	}, nil
}

// WorkflowListItemCosts returns grouped per-run costs for overview rows.
func (a *App) WorkflowListItemCosts(projectID string) (map[string]float64, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("list workflow item costs: project id is required")
	}
	return a.store.QueryWorkItemCosts(projectID)
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
