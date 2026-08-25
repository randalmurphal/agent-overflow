package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/scheduler"
)

// maxAutomationNameBytes bounds the name that ends up in every started run's
// goal and on the automation's row.
const maxAutomationNameBytes = 200

// WorkflowAutomationInput is the editable definition of an automation: a
// trigger, an optional run-if condition, and the seeds its runs start with.
// There is no notes field — continuity notes are written through
// WorkflowSetJobNotes, because a phase rewrites them too (§5 update-job-notes)
// and a definition edit must not clobber what the last run learned.
type WorkflowAutomationInput struct {
	ProjectID     string          `json:"projectId"`
	WorkflowID    string          `json:"workflowId"`
	WorkflowScope string          `json:"workflowScope"`
	Name          string          `json:"name"`
	Enabled       bool            `json:"enabled"`
	Trigger       json.RawMessage `json:"trigger"`
	Condition     json.RawMessage `json:"condition,omitempty"`
	Seeds         json.RawMessage `json:"seeds,omitempty"`
}

// WorkflowAutomationView is one automation row as the UI reads it: the stored
// definition, the parsed trigger's rendering, the next fire a cron trigger has
// coming, and the fire record — including the last skip, which is how a
// starving overlap or a wrong condition becomes visible.
//
// TriggerError is the standing answer to "why is this doing nothing": a stored
// trigger that no longer parses is reported on its own row rather than silently
// dropped from the schedule.
type WorkflowAutomationView struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"projectId"`
	WorkflowID     string          `json:"workflowId"`
	WorkflowScope  string          `json:"workflowScope"`
	Name           string          `json:"name"`
	Enabled        bool            `json:"enabled"`
	Trigger        json.RawMessage `json:"trigger"`
	TriggerKind    string          `json:"triggerKind,omitempty"`
	TriggerSummary string          `json:"triggerSummary,omitempty"`
	TriggerError   string          `json:"triggerError,omitempty"`
	// NextFireAt is set only for an enabled cron trigger that parses. A disabled
	// automation has no next fire, and saying otherwise would be a lie.
	NextFireAt     int64           `json:"nextFireAt,omitempty"`
	Condition      json.RawMessage `json:"condition,omitempty"`
	Seeds          json.RawMessage `json:"seeds,omitempty"`
	Notes          string          `json:"notes"`
	LastFiredAt    int64           `json:"lastFiredAt,omitempty"`
	LastRunItemID  string          `json:"lastRunItemId,omitempty"`
	SkipCount      int64           `json:"skipCount"`
	LastSkipAt     int64           `json:"lastSkipAt,omitempty"`
	LastSkipReason string          `json:"lastSkipReason,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
}

// WorkflowCreateAutomation validates and persists a new automation, then
// recomputes the schedule so its first fire is armed without a restart.
func (a *App) WorkflowCreateAutomation(input WorkflowAutomationInput) (WorkflowAutomationView, error) {
	workflowScheduler, err := a.requireWorkflowScheduler()
	if err != nil {
		return WorkflowAutomationView{}, err
	}
	normalized, err := a.validateAutomationInput(input)
	if err != nil {
		return WorkflowAutomationView{}, fmt.Errorf("create automation: %w", err)
	}
	now := time.Now().UnixMilli()
	normalized.ID = uuid.NewString()
	normalized.CreatedAt = now
	normalized.UpdatedAt = now
	if err := a.store.CreateAutomation(normalized); err != nil {
		return WorkflowAutomationView{}, err
	}
	if err := workflowScheduler.Refresh(); err != nil {
		return WorkflowAutomationView{}, fmt.Errorf("create automation %s: refresh schedule: %w", normalized.ID, err)
	}
	return automationView(normalized, time.Now()), nil
}

// WorkflowUpdateAutomation replaces an automation's definition. Continuity
// notes and the fire record are untouched: neither is part of the definition.
func (a *App) WorkflowUpdateAutomation(automationID string, input WorkflowAutomationInput) (WorkflowAutomationView, error) {
	workflowScheduler, err := a.requireWorkflowScheduler()
	if err != nil {
		return WorkflowAutomationView{}, err
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return WorkflowAutomationView{}, fmt.Errorf("update automation: automation id is required")
	}
	existing, err := a.store.GetAutomation(automationID)
	if err != nil {
		return WorkflowAutomationView{}, err
	}
	normalized, err := a.validateAutomationInput(input)
	if err != nil {
		return WorkflowAutomationView{}, fmt.Errorf("update automation %s: %w", automationID, err)
	}
	normalized.ID = automationID
	normalized.Notes = existing.Notes
	normalized.CreatedAt = existing.CreatedAt
	normalized.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateAutomation(normalized); err != nil {
		return WorkflowAutomationView{}, err
	}
	if err := workflowScheduler.Refresh(); err != nil {
		return WorkflowAutomationView{}, fmt.Errorf("update automation %s: refresh schedule: %w", automationID, err)
	}
	// The fire record is definition-independent, so it is carried into the view
	// rather than re-read: an edit does not reset what the automation has done.
	normalized.LastFiredAt = existing.LastFiredAt
	normalized.LastRunItemID = existing.LastRunItemID
	normalized.SkipCount = existing.SkipCount
	normalized.LastSkipAt = existing.LastSkipAt
	normalized.LastSkipReason = existing.LastSkipReason
	return automationView(normalized, time.Now()), nil
}

// WorkflowDeleteAutomation removes a trigger. Runs it already started are
// untouched — they are ordinary runs whose provenance happens to name a row
// that no longer exists.
func (a *App) WorkflowDeleteAutomation(automationID string) error {
	workflowScheduler, err := a.requireWorkflowScheduler()
	if err != nil {
		return err
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return fmt.Errorf("delete automation: automation id is required")
	}
	if err := a.store.DeleteAutomation(automationID); err != nil {
		return err
	}
	if err := workflowScheduler.Refresh(); err != nil {
		return fmt.Errorf("delete automation %s: refresh schedule: %w", automationID, err)
	}
	return nil
}

// WorkflowSetAutomationEnabled is the trigger's on/off switch. Disabling stops
// future fires; it never touches a run already in flight.
func (a *App) WorkflowSetAutomationEnabled(automationID string, enabled bool) error {
	workflowScheduler, err := a.requireWorkflowScheduler()
	if err != nil {
		return err
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return fmt.Errorf("set automation enabled: automation id is required")
	}
	if err := a.store.SetAutomationEnabled(automationID, enabled, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := workflowScheduler.Refresh(); err != nil {
		return fmt.Errorf("set automation %s enabled: refresh schedule: %w", automationID, err)
	}
	return nil
}

// WorkflowListAutomations returns one project's automations, each enriched with
// what its stored trigger actually means right now.
func (a *App) WorkflowListAutomations(projectID string) ([]WorkflowAutomationView, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("list automations: project id is required")
	}
	automations, err := a.store.ListAutomations(projectID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]WorkflowAutomationView, 0, len(automations))
	for _, automation := range automations {
		views = append(views, automationView(automation, now))
	}
	return views, nil
}

// WorkflowRunAutomationNow fires an automation on a human's behalf: it skips the
// run-if condition (pressing the button is the decision) but still refuses to
// overlap the automation's own previous run, loudly, because there is someone
// present to read the refusal.
func (a *App) WorkflowRunAutomationNow(automationID string) (store.WorkItem, error) {
	workflowScheduler, err := a.requireWorkflowScheduler()
	if err != nil {
		return store.WorkItem{}, err
	}
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return store.WorkItem{}, fmt.Errorf("run automation: automation id is required")
	}
	itemID, err := workflowScheduler.RunNow(automationID)
	if err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(itemID)
}

// validateAutomationInput refuses every automation that could not produce a
// run: an unparseable trigger, a workflow that does not resolve in the project,
// seeds that are not an object or that claim a reserved name, a malformed
// condition. The workflow is resolved through the same path a start uses, so a
// definition this project cannot run is refused when the automation is written
// rather than at 3am.
func (a *App) validateAutomationInput(input WorkflowAutomationInput) (store.Automation, error) {
	if a.store == nil {
		return store.Automation{}, fmt.Errorf("workflow store unavailable")
	}
	projectID := strings.TrimSpace(input.ProjectID)
	workflowID := strings.TrimSpace(input.WorkflowID)
	name := strings.TrimSpace(input.Name)
	scope := def.Scope(strings.TrimSpace(input.WorkflowScope))
	if projectID == "" || workflowID == "" || name == "" {
		return store.Automation{}, fmt.Errorf("project id, workflow id, and name are required")
	}
	if len(name) > maxAutomationNameBytes {
		return store.Automation{}, fmt.Errorf("name exceeds %d bytes", maxAutomationNameBytes)
	}
	if scope != def.ScopeProject && scope != def.ScopeShared {
		return store.Automation{}, fmt.Errorf("scope must be project or shared")
	}
	if _, err := scheduler.ParseTrigger(input.Trigger); err != nil {
		return store.Automation{}, err
	}
	if _, _, err := scheduler.ParseCondition(input.Condition); err != nil {
		return store.Automation{}, err
	}
	seeds, err := normalizeAutomationSeeds(input.Seeds)
	if err != nil {
		return store.Automation{}, err
	}
	if _, err := a.store.GetProject(projectID); err != nil {
		return store.Automation{}, err
	}
	_, definitions := a.workflowSources()
	if _, err := definitions.Resolve(a.lifeCtx(), store.WorkItem{
		ProjectID: projectID, WorkflowID: workflowID, WorkflowScope: string(scope),
	}); err != nil {
		return store.Automation{}, err
	}
	condition := json.RawMessage(bytes.TrimSpace(input.Condition))
	if string(condition) == "null" {
		condition = nil
	}
	return store.Automation{
		ProjectID: projectID, WorkflowID: workflowID, WorkflowScope: string(scope),
		Name: name, Enabled: input.Enabled,
		Trigger: json.RawMessage(bytes.TrimSpace(input.Trigger)), Condition: condition, Seeds: seeds,
	}, nil
}

// normalizeAutomationSeeds decodes the stored seeds and refuses the two names
// the scheduler injects. Reserved means unavailable: a stored `trigger` seed
// could never win (reserved names bind last) and storing one would only make
// the row lie about what its runs receive.
func normalizeAutomationSeeds(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("seeds must be an object: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("seeds must contain one JSON object")
	}
	if values == nil {
		return nil, fmt.Errorf("seeds must be an object")
	}
	for name := range values {
		if scheduler.ReservedSeed(name) {
			return nil, fmt.Errorf(
				"seed %q is reserved: the scheduler supplies %q and %q to every run it starts",
				name, scheduler.TriggerVariable, scheduler.JobNotesVariable,
			)
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode seeds: %w", err)
	}
	return encoded, nil
}

func automationView(automation store.Automation, now time.Time) WorkflowAutomationView {
	view := WorkflowAutomationView{
		ID: automation.ID, ProjectID: automation.ProjectID, WorkflowID: automation.WorkflowID,
		WorkflowScope: automation.WorkflowScope, Name: automation.Name, Enabled: automation.Enabled,
		Trigger: automation.Trigger, Condition: automation.Condition, Seeds: automation.Seeds,
		Notes: automation.Notes, LastFiredAt: automation.LastFiredAt,
		LastRunItemID: automation.LastRunItemID, SkipCount: automation.SkipCount,
		LastSkipAt: automation.LastSkipAt, LastSkipReason: automation.LastSkipReason,
		CreatedAt: automation.CreatedAt, UpdatedAt: automation.UpdatedAt,
	}
	trigger, err := scheduler.ParseTrigger(automation.Trigger)
	if err != nil {
		view.TriggerError = err.Error()
		return view
	}
	view.TriggerKind = string(trigger.Kind)
	view.TriggerSummary = trigger.Summary()
	if automation.Enabled {
		if next, ok := trigger.Next(now); ok {
			view.NextFireAt = next.UnixMilli()
		}
	}
	return view
}
