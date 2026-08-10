package main

import (
	"context"
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
	HumanGateCount       int                       `json:"humanGateCount"`
	Phases               []WorkflowDefinitionPhase `json:"phases"`
	Inputs               []WorkflowDefinitionInput `json:"inputs"`
	DefaultStepMode      bool                      `json:"defaultStepMode"`
	Valid                bool                      `json:"valid"`
	FirstValidationError string                    `json:"firstValidationError,omitempty"`
	AllBindingsAvailable bool                      `json:"allBindingsAvailable"`
}

type WorkflowDefinitionCatalog struct {
	BaseBranch string                      `json:"baseBranch"`
	Workflows  []WorkflowDefinitionListing `json:"workflows"`
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
	calls := aocli.CallResolverFor(resolved)
	listings := make([]WorkflowDefinitionListing, 0, len(resolved))
	for _, workflow := range resolved {
		validation := def.Validate(workflow, &bindings, calls)
		listing := WorkflowDefinitionListing{
			ID: workflow.Workflow.ID, Name: workflow.Workflow.Name,
			Scope: string(workflow.Scope), PhaseCount: len(workflow.Workflow.Phases),
			HumanGateCount:  workflow.HumanGateCount,
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
				Multiline: input.Schema.Multiline,
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
	return WorkflowDefinitionCatalog{BaseBranch: bindings.BaseBranch, Workflows: listings}, nil
}

// WorkflowListItemCosts returns per-run costs for overview rows, composed
// through the one ledger pricing rule (app_usage_pricing.go). A run whose
// phases ran on Codex has no wire-reported cost at all — every one of its rows
// carries tokens and a zero `cost_usd` — so summing the column alone reported
// those runs as free.
func (a *App) WorkflowListItemCosts(projectID string) (map[string]float64, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("list workflow item costs: project id is required")
	}
	groups, err := a.store.QueryWorkItemCosts(projectID)
	if err != nil {
		return nil, err
	}
	spends := make(map[string]*ledgerSpend, len(groups))
	for _, group := range groups {
		spend, ok := spends[group.WorkItemID]
		if !ok {
			spend = &ledgerSpend{}
			spends[group.WorkItemID] = spend
		}
		if err := spend.add(group.UsageDetailRow); err != nil {
			return nil, fmt.Errorf("list workflow item costs for project %s: %w", projectID, err)
		}
	}
	costs := make(map[string]float64, len(spends))
	for itemID, spend := range spends {
		costs[itemID] = spend.TotalUSD()
	}
	return costs, nil
}

// WorkflowRerunItem starts a failed run's last phase again immediately,
// carrying its latest diagnosis, plus the caller's optional guidance, into the
// new attempt.
//
// refreshDefinition re-reads the workflow and its prompt files from disk for
// that attempt instead of rendering the definition the run froze at start.
func (a *App) WorkflowRerunItem(ctx context.Context, itemID, guidance string, refreshDefinition bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("rerun workflow item: item id is required")
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "rerun workflow run"); err != nil {
		return err
	}
	return workflowEngine.RerunFailed(itemID, guidance, refreshDefinition)
}
