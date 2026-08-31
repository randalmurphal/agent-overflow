package app

import (
	"agent-overflow/internal/aocli"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/notify"
	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usageledger"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workflowapp"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
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

// Thread binding (decision D17).
//
// A run bound to a thread reports back into that thread: every resting
// transition composes a wake and injects it as a user message, so the agent (or
// human) that started the run is told the run finished where they were working,
// rather than having to watch an overlay.
//
// A binding is always made against a thread that already exists — the CLI's
// `run start` auto-bind to its origin thread, or an explicit bind here. D32
// removed the seed-a-new-thread-and-bind-it affordance; nothing on a workflow
// surface creates a conversation any more.
//
// Only a ROOT run carries a binding. A called run's results reach its caller
// through the call phase, and its parks surface at the root — giving it a
// binding of its own would put two runs' results into one conversation with no
// way to tell which one the human is answering.

// WorkflowBindThread makes an existing conversation thread the run's origin.
// The run's results are delivered there from then on, replacing any previous
// binding.
//
// LocalOnly: it associates a local run record with a local provider session.
func (a *App) WorkflowBindThread(itemID, threadID string) (store.WorkItem, error) {
	item, err := a.workflowBindableItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: thread id is required", item.ID)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: %w", item.ID, err)
	}
	if thread.Archived {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: thread %s is archived", item.ID, thread.ID)
	}
	if err := workflowapp.ValidateBindingThread(item, thread); err != nil {
		return store.WorkItem{}, fmt.Errorf("bind workflow run %s: %w", item.ID, err)
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

// WorkflowUnbindThread drops a run's origin binding. Its results go back to the
// workflows overlay and the OS notification.
//
// LocalOnly: same surface as WorkflowBindThread.
func (a *App) WorkflowUnbindThread(itemID string) (store.WorkItem, error) {
	item, err := a.workflowBindableItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, ""); err != nil {
		return store.WorkItem{}, err
	}
	return a.store.GetWorkItem(item.ID)
}

// workflowBindableItem loads a run and refuses the ones a binding cannot
// describe. Refusing a called run here is what keeps "children never notify as
// themselves" structural rather than conventional.
func (a *App) workflowBindableItem(itemID string) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("workflow thread binding: item id is required")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if item.ParentItemID != "" {
		return store.WorkItem{}, fmt.Errorf(
			"workflow thread binding %s: this run was called by %s; bind the run that called it",
			item.ID, item.ParentItemID,
		)
	}
	return item, nil
}

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

// WorkflowListItemCosts returns per-run TREE costs for overview rows: each
// entry is the run's own ledger rows plus every run it called, transitively —
// the same total the detail view's spend and the engine's budget check report
// for that run, and the number a root's overview row must show for a recursive
// campaign whose spend lands almost entirely on its call children. Composition
// goes through the one ledger pricing rule (internal/usageledger); a run whose
// phases ran on Codex has no wire-reported cost at all, so summing the
// `cost_usd` column alone would report those runs as free.
//
// The rollup folds each (run, model, cost_source) group into the run itself
// and every ancestor on its parent chain, resolved from one project-wide node
// read rather than a per-run tree CTE, so the overview stays constant-time in
// query count. A ledger row whose run record is gone keeps its own entry —
// ledger rows deliberately outlive the runs they attribute — and a chain is
// followed only as far as its records still exist.
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
	nodes, err := a.store.ListProjectWorkItemNodes(projectID)
	if err != nil {
		return nil, err
	}
	parents := make(map[string]string, len(nodes))
	for _, node := range nodes {
		parents[node.ID] = node.ParentItemID
	}
	spends := make(map[string]*usageledger.Spend, len(groups))
	for _, group := range groups {
		for _, itemID := range workItemAncestryChain(group.WorkItemID, parents) {
			spend, ok := spends[itemID]
			if !ok {
				spend = &usageledger.Spend{}
				spends[itemID] = spend
			}
			if err := spend.Add(group.UsageDetailRow); err != nil {
				return nil, fmt.Errorf("list workflow item costs for project %s: %w", projectID, err)
			}
		}
	}
	costs := make(map[string]float64, len(spends))
	for itemID, spend := range spends {
		costs[itemID] = spend.TotalUSD()
	}
	return costs, nil
}

// workItemAncestryChain returns the run itself followed by its ancestors,
// nearest first, walking the parent map as far as records exist. The linkage
// is acyclic by construction (§3a), but the walk is bounded by a visited set
// anyway: corrupt data must terminate with a short chain, not hang the
// overview.
func workItemAncestryChain(itemID string, parents map[string]string) []string {
	chain := make([]string, 0, 4)
	visited := make(map[string]bool, 4)
	for itemID != "" && !visited[itemID] {
		chain = append(chain, itemID)
		visited[itemID] = true
		itemID = parents[itemID]
	}
	return chain
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

// Discard with loss preview (decision D23).
//
// Discard is the only flow in the app that deletes branches, so it is the only
// one that can destroy work no other surface would ever show again. The preview
// is the consent: it walks the whole run tree, reports every worktree the
// discard would remove along with what is in it, and mutates nothing. The
// discard itself then removes exactly what the preview described.
//
// Project deletion (D25) walks the same trees and removes the same checkouts,
// but it is cleanup: it deletes no branch and takes no consent, because nothing
// it does is unrecoverable. Keep it that way — "discard is the only flow that
// deletes a branch" is what makes this preview worth reading.

// WorkflowDiscardWorktree is one checkout the discard would remove, plus the
// work that lives in it and nowhere else.
type WorkflowDiscardWorktree struct {
	ItemID string `json:"itemId"`
	// UnitID is set when this is a fan-out unit's sub-worktree.
	UnitID string `json:"unitId,omitempty"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Base is the ref the unmerged commits are measured against: the run's base
	// branch for a run worktree, the owning run's branch for a unit worktree
	// (so a unit's landed commits are not counted twice).
	Base string `json:"base"`
	// Present reports whether the checkout is still on disk. A registered
	// worktree whose directory is gone carries no dirty files but its branch
	// still exists and is still deleted.
	Present bool `json:"present"`
	// Registered reports whether git still knows this path as a worktree of the
	// project. An unregistered path is reported, not removed.
	Registered          bool             `json:"registered"`
	DirtyFiles          []string         `json:"dirtyFiles"`
	DirtyFileCount      int              `json:"dirtyFileCount"`
	UnmergedCommits     []gitdiff.Commit `json:"unmergedCommits"`
	UnmergedCommitCount int              `json:"unmergedCommitCount"`
	// Error carries a per-worktree inspection failure. The preview reports it
	// rather than failing outright: a human deciding whether to discard is
	// better served by "this one could not be inspected" than by no preview.
	Error string `json:"error,omitempty"`
}

// WorkflowDiscardPreview is what a discard of one run tree would destroy.
type WorkflowDiscardPreview struct {
	ItemID string `json:"itemId"`
	// Members is the run tree, root first.
	Members []string `json:"members"`
	// LiveMembers is the subset still in flight. Discarding cancels them first;
	// they are called out because that is work the human is stopping, not just
	// work they are throwing away. It is narrower than what the discard settles
	// (workflowDiscardStops): a parked member is also cancelled, but naming it
	// here as "still working" would be a lie about what it is doing.
	LiveMembers []string                  `json:"liveMembers"`
	Worktrees   []WorkflowDiscardWorktree `json:"worktrees"`
}

// WorkflowDiscardResult is what a discard actually destroyed. It rides the
// disposition receipt into the durable run record, because a discard is the one
// disposition whose effects cannot be recovered by looking at git afterwards:
// the branches it deleted are exactly the ones nothing else references.
type WorkflowDiscardResult struct {
	// Members is the run tree the discard covered, root first.
	Members []string `json:"members"`
	// Cancelled is the subset that was still in flight and was stopped.
	Cancelled []string `json:"cancelled"`
	// RemovedWorktrees and DeletedBranches are what git was actually asked to
	// destroy — a subset of the preview, since a checkout can be released
	// between the preview and the discard.
	RemovedWorktrees []string `json:"removedWorktrees"`
	DeletedBranches  []string `json:"deletedBranches"`
}

// WorkflowDiscardPreview reports what discarding a run tree would destroy. It
// runs read-only git queries and mutates nothing.
//
// LocalOnly: it reads local checkouts and repository history.
func (a *App) WorkflowDiscardPreview(itemID string) (WorkflowDiscardPreview, error) {
	preview, err := a.workflowApplication().DiscardPreview(itemID)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	return projectWorkflowDiscardPreview(preview), nil
}

type workflowTreeLoss = workflowapp.TreeLoss
type workflowWorktreeTarget = workflowapp.WorktreeTarget
type projectWorktreeRegistry = workflowapp.WorktreeRegistry

func (a *App) workflowTreeLoss(rootID, projectPath string) (workflowTreeLoss, error) {
	return a.workflowApplication().TreeLoss(rootID, projectPath)
}

func (a *App) workflowRunTree(rootID string) ([]store.WorkItem, error) {
	return a.workflowApplication().RunTree(rootID)
}

func (a *App) workflowRunTreeNodes(rootID string) ([]store.WorkItemNode, error) {
	return a.workflowApplication().RunTreeNodes(rootID)
}

func (a *App) cancelWorkflowTreeMembers(members []store.WorkItem) ([]string, error) {
	return a.workflowApplication().CancelTreeMembers(members)
}

func (a *App) readProjectWorktrees(projectPath, label string) (projectWorktreeRegistry, error) {
	return a.workflowApplication().ReadWorktrees(projectPath, label)
}

func projectWorkflowDiscardPreview(value workflowapp.DiscardPreview) WorkflowDiscardPreview {
	preview := WorkflowDiscardPreview{
		ItemID: value.ItemID, Members: value.Members, LiveMembers: value.LiveMembers,
		Worktrees: make([]WorkflowDiscardWorktree, 0, len(value.Worktrees)),
	}
	for _, worktree := range value.Worktrees {
		preview.Worktrees = append(preview.Worktrees, WorkflowDiscardWorktree{
			ItemID: worktree.ItemID, UnitID: worktree.UnitID, Path: worktree.Path,
			Branch: worktree.Branch, Base: worktree.Base, Present: worktree.Present,
			Registered: worktree.Registered, DirtyFiles: worktree.DirtyFiles,
			DirtyFileCount: worktree.DirtyFileCount, UnmergedCommits: worktree.UnmergedCommits,
			UnmergedCommitCount: worktree.UnmergedCommitCount, Error: worktree.Error,
		})
	}
	return preview
}

func projectWorkflowDiscardResult(value workflowapp.DiscardResult) WorkflowDiscardResult {
	return WorkflowDiscardResult{
		Members: value.Members, Cancelled: value.Cancelled,
		RemovedWorktrees: value.RemovedWorktrees, DeletedBranches: value.DeletedBranches,
	}
}

type WorkflowDispositionReceipt struct {
	Action        string `json:"action"`
	Mode          string `json:"mode,omitempty"`
	SHA           string `json:"sha,omitempty"`
	PRRef         string `json:"prRef,omitempty"`
	Base          string `json:"base,omitempty"`
	CleanupFailed bool   `json:"cleanupFailed,omitempty"`
	// Discarded is set for a discard and records the tree it covered and the
	// checkouts and branches it destroyed (D23). Merge and PR leave the work
	// reachable; discard does not, so its receipt is the only account of it.
	Discarded *WorkflowDiscardResult `json:"discarded,omitempty"`
	Policy    string                 `json:"policy"`
	At        int64                  `json:"at"`
}

// Kept as the root wire vocabulary used by existing integration fixtures.
const workflowDispositionPR = "pr"

// WorkflowMergeItem cleanly lands a done item's branch on the live profile's
// base branch. Refusals park the run for human disposition.
func (a *App) WorkflowMergeItem(itemID string) (WorkflowDispositionReceipt, error) {
	receipt, err := a.workflowApplication().MergeItem(itemID)
	return projectWorkflowDispositionReceipt(receipt), err
}

// WorkflowCreateItemPR pushes a done item's branch and creates a PR/MR through
// the repository's existing forge integration.
func (a *App) WorkflowCreateItemPR(itemID string) (WorkflowDispositionReceipt, error) {
	receipt, err := a.workflowApplication().CreateItemPR(itemID)
	return projectWorkflowDispositionReceipt(receipt), err
}

// WorkflowDiscardItem removes an eligible item's worktree through the existing
// guarded removal path and keeps the durable run record.
func (a *App) WorkflowDiscardItem(itemID string) (WorkflowDispositionReceipt, error) {
	receipt, err := a.workflowApplication().DiscardItem(itemID)
	return projectWorkflowDispositionReceipt(receipt), err
}

func projectWorkflowDispositionReceipt(value workflowapp.DispositionReceipt) WorkflowDispositionReceipt {
	receipt := WorkflowDispositionReceipt{
		Action: value.Action, Mode: value.Mode, SHA: value.SHA, PRRef: value.PRRef,
		Base: value.Base, CleanupFailed: value.CleanupFailed, Policy: value.Policy, At: value.At,
	}
	if value.Discarded != nil {
		discarded := projectWorkflowDiscardResult(*value.Discarded)
		receipt.Discarded = &discarded
	}
	return receipt
}

func (a *App) workflowProjectProfile(projectID string) (*profile.Profile, error) {
	source := a.workflowProfiles()
	return source.Profile(a.lifeCtx(), projectID)
}

func (a *App) autoDisposeWorkflowItem(itemID string) {
	if err := a.workflowApplication().AutoDispose(itemID); err != nil {
		log.Printf("workflow auto-disposition %s: %v", itemID, err)
		a.emit(eventchan.WorkflowError, engine.ErrorEvent{
			ItemID: itemID,
			Error:  "automatic workflow disposition failed; inspect the item's disposition state",
		})
	}
}

// Per-run pause and its graceful-quit counterpart (decision D23).

// WorkflowPauseItem parks a run tree `needs-human(paused)`: in-flight turns are
// interrupted now, resources are released, and every live member of the tree
// comes down through the engine's one teardown path. Resuming continues on the
// provider sessions the runs parked on.
//
// LocalOnly: pausing interrupts local provider processes and releases the
// worktrees they hold.
func (a *App) WorkflowPauseItem(ctx context.Context, itemID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "pause workflow run"); err != nil {
		return err
	}
	return workflowEngine.PauseItem(itemID)
}

// WorkflowRequestSoftStop arms or clears a run tree's request to stop at its
// next call boundary (D36). Nothing is interrupted and nothing starts: the run
// keeps going and, the next time it would invoke a call, parks
// `needs-human(checkpoint)` instead. Resuming takes the call it skipped.
//
// It is one method with a flag rather than a pair, because the two directions
// are one piece of state: a clear has to be able to undo an arm through exactly
// the path that set it, and a caller that can only ever arm would have no way to
// change its mind.
//
// LocalOnly: the request decides whether the next wave of autonomous provider
// sessions runs, which is the same control plane as pause.
func (a *App) WorkflowRequestSoftStop(ctx context.Context, itemID string, armed bool) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "request a workflow soft stop"); err != nil {
		return err
	}
	return workflowEngine.SetSoftStop(itemID, armed)
}

// pauseWorkflowRunsForShutdown is the graceful-quit half: every active root run
// is paused before the process tears its provider sessions down, so a restart
// finds resumable `needs-human(paused)` runs rather than crash-parked ones.
//
// It is bounded rather than awaited. PauseAllActive itself never waits for a
// turn to finish — it interrupts — but it does serialize behind whatever the
// engine's command loop is already doing, and that could be a phase entry that
// is provisioning a worktree. Quit must not hang on it. Timing out is a
// degradation, not a failure: the runs that missed the window are exactly the
// ones the startup sweep parks `interrupted`, which resume handles identically.
func (a *App) pauseWorkflowRunsForShutdown() error {
	return a.workflowApplication().PauseActiveForShutdown(workflowPauseAllTimeout)
}

type WorkflowPRReviewComments struct {
	Count   int                   `json:"count"`
	Threads []gitops.ReviewThread `json:"threads"`
}

// WorkflowFetchPRReviewComments returns the PR's review conversations that
// have not been explicitly resolved. Conversation comments without a forge
// resolution state remain visible.
func (a *App) WorkflowFetchPRReviewComments(itemID string) (WorkflowPRReviewComments, error) {
	if a.shuttingDown.Load() {
		return WorkflowPRReviewComments{}, ErrShuttingDown
	}
	comments, err := a.workflowApplication().FetchPRReviewComments(itemID)
	if err != nil {
		return WorkflowPRReviewComments{}, err
	}
	return WorkflowPRReviewComments{Count: comments.Count, Threads: comments.Threads}, nil
}

// WorkflowSendPRReviewCommentsToThread opens or reuses the run's linked
// thread, then sends the current unresolved review comments through the
// normal user-message path.
func (a *App) WorkflowSendPRReviewCommentsToThread(itemID string) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	return a.workflowApplication().SendPRReviewCommentsToThread(itemID)
}

// WorkflowDiscussPR opens or reuses the run's linked thread and sends a
// diff-free snapshot of the PR and run intent for discussion preparation.
func (a *App) WorkflowDiscussPR(itemID string) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	return a.workflowApplication().DiscussPR(itemID)
}

// The run map's one read: a whole run TREE as metadata, in one call.
//
// The detail view (`WorkflowGetItem`) answers for one run and carries its
// evidence — envelopes, outputs, artifacts. This answers for a campaign: the
// root and every run it called, each with the skeleton of the definition it
// FROZE plus the records of what has actually happened. Nothing an agent wrote
// crosses this wire (no envelopes, no narratives, no diffs), so a forty-wave
// campaign is a few hundred small rows however much its models produced.
//
// TWO round trips answer it, whatever the tree's size. The first resolves the
// tree's root from the run the caller named (`WorkItemTreeRoot`, §5.9). The
// second is `store.ReadWorkItemTree`, which runs SIX statements — the tree's
// runs, its attempts, its units, its armed auto-resumes, and its ledger summed
// and split — inside ONE read transaction, so every number on the answer is a
// fact about one WAL snapshot rather than about six. Every statement resolves
// membership through the store's recursive CTE
// (`internal/store/work_item_tree.go`) rather than walking the linkage a round
// trip at a time in Go, and rather than round-tripping a campaign's id list
// back into a bind array.
//
// The runs STREAM through a visitor rather than arriving as a slice, and that
// is a retention decision: each run carries the definition it froze, capped at
// 4MiB apiece, so a materialised 4096-member tree would hold gigabytes for the
// length of one fetch. The projection below reads each snapshot, keeps the few
// hundred bytes it draws, and drops the blob before the next row is scanned.
//
// Each fetch decodes every member's frozen snapshot to project its skeleton,
// which is real work repeated on every repaint. That amplification is ACCEPTED
// rather than memoised: `engine/refresh.go` re-freezes a run's snapshot on
// rerun and on a definition refresh, so a skeleton cache keyed by run id would
// serve the definition the run USED to have — a map that quietly disagrees with
// the run it is drawing is worse than one that costs a decode.

// WorkflowRunMapView is the whole tree, parent-linked. Runs are ordered root
// first, then by distance from the root and creation order within a level, so a
// consumer can build the tree in one pass without sorting and a parent is
// always seen before its children. The distance is derived from the LINKAGE the
// read walked, not from the persisted `call_depth`, so the promise does not
// rest on a column a corrupt row could make lie.
type WorkflowRunMapView struct {
	// RootItemID is the tree root this map is FOR, which need not be the id the
	// caller asked about: any run in the tree resolves to the same map, so a
	// stale nav entry or a deep link to a child normalises instead of erroring.
	RootItemID string              `json:"rootItemId"`
	Runs       []WorkflowRunMapRun `json:"runs"`
	// Refusal is set — with Runs empty — when the map cannot be drawn for a
	// reason RETRYING CANNOT FIX. See WorkflowRunMapRefusal.
	Refusal *WorkflowRunMapRefusal `json:"refusal,omitempty"`
}

// WorkflowRunMapRefusal is a map this backend will never answer, said in a
// shape a client can act on: a code to branch on and a sentence to render.
//
// It rides the RESULT rather than the method's error for two reasons. The
// transport strips a method error's text for any non-loopback caller (one
// correlation id, no prose — `internal/transport/dispatcher.go`), so an error
// return literally cannot carry a user-facing sentence to a remote client;
// and every code here is PERMANENT, while the entity store's answer to a
// thrown error is a backoff ladder that re-asks the same unanswerable question
// forever. An unexpected failure — a store that will not read, a ledger group
// that will not price — stays an error, because retrying IS the right response
// to those.
type WorkflowRunMapRefusal struct {
	// Code is the machine-readable class. Every one of them is permanent: the
	// client must stop re-sourcing the key until something else changes.
	Code string `json:"code"`
	// Message is the sentence to show. It names the run, never a path or an
	// internal type.
	Message string `json:"message"`
}

// The refusal codes. They are a closed set; a new one is a wire change the
// frontend has to learn, which is the point of not spelling them as prose.
const (
	// WorkflowRunMapRefusalNotFound: the named run has no row. Ordinary rather
	// than exceptional — a stale nav entry, a deleted project's records, a deep
	// link into a run somebody discarded.
	WorkflowRunMapRefusalNotFound = "not-found"
	// WorkflowRunMapRefusalTooLarge: the tree has more members than
	// `maxWorkflowRunMapMembers`. A truncated map is the worst of the three
	// outcomes — the reader cannot see that the part they were looking for is
	// missing — so the read refuses and this says so.
	WorkflowRunMapRefusalTooLarge = "too-large"
	// WorkflowRunMapRefusalCorruptLinkage: the call linkage is deeper than
	// `engine.MaxCallDepth` or closes a cycle. The schema's CHECKs make a parent
	// reference all-or-nothing, not acyclic, so both are writable — and neither
	// can be ordered parent-before-child, which is the promise every consumer of
	// this view builds its tree on.
	WorkflowRunMapRefusalCorruptLinkage = "corrupt-linkage"
)

// WorkflowRunMapRun is one run of the tree: who it is, where it sits, what its
// frozen definition says will happen, and what has.
type WorkflowRunMapRun struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId"`
	// Parent linkage is present only on a called run (§3a): the caller, its call
	// phase, and the attempt of it that invoked this run. ParentUnitID narrows
	// that to one fan-out unit when the call was declared on a unit.
	//
	// The ROOT can carry one too, and that is how an ORPHAN says so: a run whose
	// named parent's row is gone resolves to itself as the tree root, with the
	// dangling reference left on the answer. A parent id naming no run in `runs`
	// is exactly that state and needs no field of its own.
	ParentItemID  string `json:"parentItemId,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
	CallDepth     int    `json:"callDepth,omitempty"`

	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	SoftStop     bool   `json:"softStop"`
	StartedAt    int64  `json:"startedAt,omitempty"`
	EndedAt      int64  `json:"endedAt,omitempty"`
	AutoResumeAt int64  `json:"autoResumeAt,omitempty"`

	// Skeleton is this run's OWN frozen definition, projected to what the map
	// draws. It is never the root's: a definition refresh between waves is
	// reachable, and the waves then legitimately differ.
	Skeleton []WorkflowRunMapSkeletonPhase `json:"skeleton"`
	// SkeletonMissing marks records-only mode: the frozen definition could not
	// supply phases. The map then renders this run's recorded attempts with no
	// ghosts and no loop affordance — degrading is the contract, since a run's
	// history is readable whatever its snapshot says.
	SkeletonMissing bool `json:"skeletonMissing"`
	// SkeletonError is why, when the reason was CORRUPTION rather than absence:
	// the column held bytes that would not decode as a snapshot. An absent
	// snapshot is ordinary (the run failed before its first phase entry, so it
	// never froze one) and leaves this empty. The two are one flag and one
	// sentence rather than one flag alone because a reader who cannot tell them
	// apart is told a corrupt record is normal history.
	SkeletonError string `json:"skeletonError,omitempty"`
	// TailSelfCall reports that the last skeleton phase calls this run's own
	// workflow — the edge a recursive campaign iterates on, and the one call
	// shape the map flattens into waves rather than nesting as composition.
	TailSelfCall bool `json:"tailSelfCall"`

	Phases []WorkflowRunMapPhaseAttempt `json:"phases"`
	Units  []WorkflowRunMapUnit         `json:"units"`

	// Spend and Budget are the ROOT's alone and absent on every called run: a
	// budget is enforced against the tree (§12), so one pair per tree is the only
	// one that means anything.
	//
	// Spend is the tree's composed dollars, halves apart, through the one ledger
	// pricing rule every dollar surface folds through — so the map's number and
	// `WorkflowGetItem`'s are the same number, and a total carrying unpriced rows
	// says so instead of presenting a lower bound as exact.
	Spend *WorkflowRunSpend `json:"spend,omitempty"`
	// Budget is the ceiling in force, nil when there is none — which is most
	// runs. It is `engine.ResolveBudget`'s answer, the enforcement's own, so it
	// includes the ceiling a run inherits from its project profile
	// (`reliability.per_item_budget`) rather than only the one it declared. A
	// map that read the column alone rendered a profile-defaulted campaign as
	// unbounded right up to the moment the engine parked it at its ceiling.
	Budget *WorkflowAgentRunBudget `json:"budget,omitempty"`
}

// WorkflowRunMapSkeletonPhase is one phase of a frozen definition, projected to
// what the map needs to draw a node that has not happened yet. It is a
// projection rather than the raw snapshot deliberately: prompts, schemas, and
// envelopes are the largest thing a run persists and none of it is drawable.
type WorkflowRunMapSkeletonPhase struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Shape is the resolved shape — single, fan-out, or call — never the blank
	// an unannotated phase is authored with.
	Shape string `json:"shape"`
	// CallTarget is the workflow a call phase invokes, empty for every other
	// shape.
	CallTarget string `json:"callTarget,omitempty"`
	IsCheck    bool   `json:"isCheck"`
	// MaxDepth is the call edge's authored recursion bound, 0 when it declares
	// none — which is a real state, not a missing one: the run's budget is then
	// the only thing bounding the chain.
	MaxDepth int `json:"maxDepth,omitempty"`
}

// WorkflowRunMapPhaseAttempt is one recorded phase attempt. Cause is the
// ENGINE's diagnosis of a park (the workspace that would not cut, the budget
// that ran out); InterventionKind is the `kind` of the intervention persisted on
// the attempt, which today is `taken-over` or nothing — a human gate decision
// records its decision and note rather than a kind, so it surfaces through the
// run's own reason instead.
type WorkflowRunMapPhaseAttempt struct {
	PhaseID          string `json:"phaseId"`
	Attempt          int    `json:"attempt"`
	Status           string `json:"status"`
	Cause            string `json:"cause,omitempty"`
	InterventionKind string `json:"interventionKind,omitempty"`
	ThreadID         string `json:"threadId,omitempty"`
	StartedAt        int64  `json:"startedAt"`
	EndedAt          int64  `json:"endedAt,omitempty"`
}

// WorkflowRunMapUnit is one fan-out unit (or join) of one phase attempt. A unit
// row exists from the moment its attempt expands, so a queued branch is a real
// record rather than something the map has to guess at.
type WorkflowRunMapUnit struct {
	PhaseID   string `json:"phaseId"`
	Attempt   int    `json:"attempt"`
	UnitID    string `json:"unitId"`
	UnitIndex int    `json:"unitIndex"`
	Kind      string `json:"kind"`
	// Provider names the resource a pending unit is waiting capacity on.
	Provider    string `json:"provider,omitempty"`
	Status      string `json:"status"`
	UnitAttempt int    `json:"unitAttempt"`
	ThreadID    string `json:"threadId,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	EndedAt     int64  `json:"endedAt,omitempty"`
}

// maxWorkflowRunMapMembers is the most runs one map will answer for. Depth is
// already bounded by `engine.MaxCallDepth` (256), so this bounds the other
// dimension: 4096 is that depth at a realistic mean fan of sixteen calls per
// run — comfortably above any campaign this codebase has run, and low enough
// that the answer stays a few hundred kilobytes rather than a heap spike.
//
// Exceeding it REFUSES, as `WorkflowRunMapRefusalTooLarge`.
const maxWorkflowRunMapMembers = 4096

// WorkflowGetRunMap answers for the whole tree the supplied run belongs to. Any
// run in the tree is a valid argument — the root is resolved server-side (§5.9).
//
// It has a CLASSIFIED error contract. The three refusals a corrupt or oversized
// or simply absent tree produces come back as `view.Refusal` with a nil error,
// because each is an expected, permanent, user-facing state (see
// WorkflowRunMapRefusal). Everything else — a store that will not read, a
// ledger group with an unknown cost source — is an error, and the caller is
// right to retry those.
func (a *App) WorkflowGetRunMap(ctx context.Context, itemID string) (WorkflowRunMapView, error) {
	if a.store == nil {
		return WorkflowRunMapView{}, fmt.Errorf("workflow store unavailable")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return WorkflowRunMapView{}, fmt.Errorf("workflow run map: item id is required")
	}
	rootID, err := a.workflowRunTreeRoot(itemID)
	if err != nil {
		return workflowRunMapRefusalFor(itemID, err)
	}
	tree, err := a.gatherWorkflowRunTree(ctx, rootID)
	if err != nil {
		return workflowRunMapRefusalFor(itemID, err)
	}
	// The scan anchors on the root and refuses an empty tree, so this cannot
	// fire — and it is checked rather than assumed because the alternative is
	// resolving a ceiling for a run with no id, which reads as "no budget".
	if tree.root.ItemID != rootID {
		return WorkflowRunMapView{}, fmt.Errorf("workflow run map: tree read for %s did not carry its root", rootID)
	}
	spend, budget, err := a.workflowRunMapMoney(ctx, tree.root, tree.usage, tree.usageDetail)
	if err != nil {
		return WorkflowRunMapView{}, err
	}
	view := WorkflowRunMapView{RootItemID: rootID, Runs: tree.runs}
	for index := range view.Runs {
		run := &view.Runs[index]
		run.AutoResumeAt = tree.resumeAt[run.ItemID]
		run.Phases = slicesx.OrEmpty(tree.phasesByItem[run.ItemID])
		run.Units = slicesx.OrEmpty(tree.unitsByItem[run.ItemID])
		if run.ItemID == rootID {
			run.Spend, run.Budget = &spend, budget
		}
	}
	return view, nil
}

// workflowRunMapRefusalFor classifies one read failure. A refusal the wire has
// a code for becomes an answer; anything else stays an error, so a transient
// failure keeps the retry a permanent one must not get.
func workflowRunMapRefusalFor(itemID string, err error) (WorkflowRunMapView, error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code:    WorkflowRunMapRefusalNotFound,
			Message: fmt.Sprintf("Run %s no longer exists.", itemID),
		}}, nil
	case errors.Is(err, store.ErrWorkItemTreeTooLarge):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code: WorkflowRunMapRefusalTooLarge,
			Message: fmt.Sprintf(
				"This campaign has more than %d runs, which is more than the map can draw at once.",
				maxWorkflowRunMapMembers),
		}}, nil
	case errors.Is(err, store.ErrWorkItemTreeTooDeep), errors.Is(err, store.ErrWorkItemTreeCyclicLinkage):
		return WorkflowRunMapView{Runs: []WorkflowRunMapRun{}, Refusal: &WorkflowRunMapRefusal{
			Code: WorkflowRunMapRefusalCorruptLinkage,
			Message: fmt.Sprintf(
				"The call linkage around run %s does not describe a tree, so its map cannot be drawn.",
				itemID),
		}}, nil
	}
	return WorkflowRunMapView{}, err
}

// workflowRunTree is one tree's rows, already grouped by the run they belong
// to. It exists so the bound method reads resolve → gather → assemble instead
// of interleaving the reads with the regroup each one needs.
type workflowRunTree struct {
	// runs is root-first and parent-before-child, already projected to the wire:
	// the store's rows carry the frozen snapshots, and nothing here keeps one.
	runs []WorkflowRunMapRun
	// root is what the tree's budget is resolved from — the root row narrowed to
	// the four facts a ceiling is decided by.
	root         engine.BudgetSubject
	resumeAt     map[string]int64
	phasesByItem map[string][]WorkflowRunMapPhaseAttempt
	unitsByItem  map[string][]WorkflowRunMapUnit
	usage        store.WorkItemUsage
	usageDetail  []store.UsageDetailRow
}

// gatherWorkflowRunTree reads the tree and buckets what it returns by the run
// it belongs to. Every read runs inside `ReadWorkItemTree`'s single
// transaction, so a run created mid-read cannot contribute attempt rows the
// assembly would silently discard — the runs and their records are one WAL
// snapshot. The caps are checked by the run scan, which is what keeps the other
// five statements off a tree this codebase refuses to answer for.
//
// The visitor is where snapshot retention is bounded: it projects each run to
// the wire shape and lets the blob go before the next row is scanned.
func (a *App) gatherWorkflowRunTree(ctx context.Context, rootID string) (workflowRunTree, error) {
	tree := workflowRunTree{
		runs:         make([]WorkflowRunMapRun, 0, 16),
		resumeAt:     make(map[string]int64),
		phasesByItem: make(map[string][]WorkflowRunMapPhaseAttempt),
		unitsByItem:  make(map[string][]WorkflowRunMapUnit),
	}
	read, err := a.store.ReadWorkItemTree(
		ctx, rootID, engine.MaxCallDepth, maxWorkflowRunMapMembers,
		func(item store.WorkItemTreeRun) error {
			run := WorkflowRunMapRun{
				ItemID: item.ID, WorkflowID: item.WorkflowID,
				ParentItemID: item.ParentItemID, ParentPhaseID: item.ParentPhaseID,
				ParentUnitID: item.ParentUnitID, ParentAttempt: item.ParentAttempt,
				CallDepth: item.CallDepth,
				State:     item.State, Reason: item.Reason, SoftStop: item.SoftStop,
				StartedAt: item.StartedAt, EndedAt: item.EndedAt,
			}
			run.Skeleton, run.TailSelfCall, run.SkeletonError = workflowRunMapSkeleton(item)
			run.SkeletonMissing = len(run.Skeleton) == 0
			if item.ID == rootID {
				tree.root = engine.BudgetSubject{
					ItemID: item.ID, ProjectID: item.ProjectID,
					Budget: item.Budget, StartedAt: item.StartedAt,
				}
			}
			tree.runs = append(tree.runs, run)
			return nil
		},
	)
	if err != nil {
		return workflowRunTree{}, fmt.Errorf("workflow run map: %w", err)
	}
	for _, resume := range read.AutoResumes {
		tree.resumeAt[resume.ItemID] = resume.At
	}
	for _, phase := range read.PhaseStatuses {
		tree.phasesByItem[phase.ItemID] = append(tree.phasesByItem[phase.ItemID], WorkflowRunMapPhaseAttempt{
			PhaseID: phase.PhaseID, Attempt: phase.Attempt, Status: phase.Status,
			Cause: phase.ParkCause, InterventionKind: phase.InterventionKind,
			ThreadID: phase.ThreadID, StartedAt: phase.StartedAt, EndedAt: phase.EndedAt,
		})
	}
	for _, unit := range read.UnitStatuses {
		tree.unitsByItem[unit.ItemID] = append(tree.unitsByItem[unit.ItemID], WorkflowRunMapUnit{
			PhaseID: unit.PhaseID, Attempt: unit.Attempt, UnitID: unit.UnitID,
			UnitIndex: unit.UnitIndex, Kind: unit.Kind, Provider: unit.Provider,
			Status: unit.Status, UnitAttempt: unit.UnitAttempt, ThreadID: unit.ThreadID,
			StartedAt: unit.StartedAt, EndedAt: unit.EndedAt,
		})
	}
	tree.usage, tree.usageDetail = read.Usage, read.UsageDetail
	return tree, nil
}

// workflowRunTreeRoot resolves the run every tree-wide fact belongs to. The
// linkage is acyclic by construction (§3a) and bounded by the engine's absolute
// call depth; the store's recursive walk carries that bound and REFUSES past
// it, which is what makes corrupt linkage terminate a read instead of hanging
// it — the same posture as `workflowAncestry` and `rootWorkItem`.
//
// A parent id whose row is GONE resolves to the last run that exists, and the
// dangling reference rides that run's `ParentItemID` on the wire: a root naming
// a parent no member of the answer carries IS the orphan state, which is why
// this neither logs it nor invents a field for it. The run the caller named is
// the exception: a request for a run that does not exist is
// `WorkflowRunMapRefusalNotFound`, because there is no tree to answer for.
//
// Membership DOWNWARD from this root is the store CTE's alone
// (`ReadWorkItemTree` and the reads inside it), which is what keeps the map's
// runs and its rows describing one tree. The discard preview's
// `walkWorkflowRunTree` (app_workflow_discard.go) is a separate walk on
// purpose: it drives per-run filesystem inspection, so it wants the rows one at
// a time rather than a set.
func (a *App) workflowRunTreeRoot(itemID string) (string, error) {
	node, err := a.store.WorkItemTreeRoot(itemID, engine.MaxCallDepth)
	if err != nil {
		return "", fmt.Errorf("workflow run map: %w", err)
	}
	return node.ID, nil
}

// workflowRunMapSkeleton projects one run's frozen definition and reports
// whether it tail-self-calls and why it has no skeleton when it has none.
//
// A snapshot that will not decode degrades to no skeleton plus the decode
// failure as user-facing state: the run's records are readable regardless, and
// refusing the whole map over one unreadable definition would take the tree
// away from the person trying to see what happened in it. It is deliberately
// NOT logged — this read runs on every debounced repaint, so a log line here is
// one corrupt row writing to disk forever, and the fact is on the wire where
// somebody can act on it.
//
// Tailness is judged against the RUN ROW's workflow id rather than the frozen
// definition's own, because that is the id a child run carries — which is what
// makes the chain edge the consumer follows and this flag the same statement.
func workflowRunMapSkeleton(item store.WorkItemTreeRun) (phases []WorkflowRunMapSkeletonPhase, tailSelfCall bool, decodeErr string) {
	snapshotPhases, err := workflowSnapshotPhases(item.Snapshot)
	if err != nil {
		return []WorkflowRunMapSkeletonPhase{}, false, err.Error()
	}
	skeleton := make([]WorkflowRunMapSkeletonPhase, 0, len(snapshotPhases))
	for _, phase := range snapshotPhases {
		node := WorkflowRunMapSkeletonPhase{
			ID: phase.ID, Name: phase.Name, Shape: string(phase.EffectiveShape()),
			IsCheck: workflowPhaseIsCheck(phase), MaxDepth: phase.MaxDepth,
		}
		if phase.IsCall() {
			node.CallTarget = phase.CallTarget()
		}
		skeleton = append(skeleton, node)
	}
	if len(skeleton) == 0 {
		return skeleton, false, ""
	}
	tail := skeleton[len(skeleton)-1]
	return skeleton, tail.CallTarget != "" && tail.CallTarget == item.WorkflowID, ""
}

// workflowRunMapMoney prices the root's whole tree and resolves the ceiling in
// force over it.
//
// Both go through the paths the ENFORCEMENT uses: the spend is
// `usageledger.PriceGroups` — the one ledger pricing rule every dollar surface
// folds through — and the ceiling is `engine.ResolveBudget`, the call the engine's
// own budget check is built on, so a profile-supplied default is the map's
// number exactly as it is the park's. The tree is priced ONCE and the resolved
// spend is handed to `ResolveBudget` rather than letting it re-read the ledger,
// which is what keeps a repainting map at the six statements it advertises.
func (a *App) workflowRunMapMoney(
	ctx context.Context, root engine.BudgetSubject,
	usage store.WorkItemUsage, detail []store.UsageDetailRow,
) (WorkflowRunSpend, *WorkflowAgentRunBudget, error) {
	priced, err := usageledger.PriceGroups(detail)
	if err != nil {
		return WorkflowRunSpend{}, nil, fmt.Errorf("workflow run map: run %s spend: %w", root.ItemID, err)
	}
	spend := WorkflowRunSpend{
		CostUSD: priced.TotalUSD(), WireCostUSD: priced.WireUSD,
		EstimatedCostUSD: priced.EstimatedUSD, UnpricedRows: priced.UnpricedRows,
	}
	view, err := engine.ResolveBudget(
		ctx,
		a.workflowProfiles(),
		resolvedTreeSpend{rootItemID: root.ItemID, spend: engine.Spend{
			Tokens:    usage.TotalTokens,
			USD:       priced.TotalUSD(),
			Estimated: priced.Estimated(),
			Unpriced:  priced.UnpricedRows,
		}},
		root, time.Now(),
	)
	if err != nil {
		return WorkflowRunSpend{}, nil, fmt.Errorf("workflow run map: run %s budget: %w", root.ItemID, err)
	}
	return spend, workflowBudgetLine(view, root.ItemID), nil
}

// resolvedTreeSpend answers `ResolveBudget` with a tree spend its caller has
// already read, so one fetch prices the ledger once. It refuses any other run's
// id rather than answering with numbers that are not that run's: the interface
// takes an id, and a memo that ignored it would be a silent wrong answer the
// day a caller resolves two budgets from one fold.
type resolvedTreeSpend struct {
	rootItemID string
	spend      engine.Spend
}

func (s resolvedTreeSpend) TreeSpend(_ context.Context, rootItemID string) (engine.Spend, error) {
	if rootItemID != s.rootItemID {
		return engine.Spend{}, fmt.Errorf(
			"workflow run map spend: asked for run %s, priced run %s", rootItemID, s.rootItemID)
	}
	return s.spend, nil
}

// Human recovery of one fan-out unit. A parked fan-out attempt is repaired unit
// by unit rather than replaced: its finished units keep their results, and the
// run returns to `running` once nothing is left blocking it. The engine owns
// those FSM edges; this file owns the app-side surface and the runner
// bookkeeping a taken-over unit thread needs.

// WorkflowRetryUnit re-runs one failed or taken-over unit of a parked fan-out
// attempt, the attempt's join included. The note explains the retry in the run
// record and reaches the unit's next try as feedback.
func (a *App) WorkflowRetryUnit(ctx context.Context, itemID, unitID, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "retry workflow unit"); err != nil {
		return err
	}
	// Read the row before the engine reopens it: a retry supersedes any steering
	// registration on the previous try's thread, and after the call the row has
	// already moved on.
	abandoned := a.workflowUnitThreadUnderTakeover(itemID, unitID)
	if err := workflowEngine.RetryUnit(itemID, unitID, note); err != nil {
		return err
	}
	a.releaseWorkflowUnitTakeover(abandoned)
	return nil
}

// WorkflowRetryFailedUnits re-runs every failed unit of a parked fan-out
// attempt in one action. It is the recovery for a cause that hit many units at
// once — a provider usage limit stopping most of a wide fan-out — where
// repairing unit by unit is the same action typed N times. The note explains
// the retry in the run record and reaches every repaired unit's next try as
// feedback.
func (a *App) WorkflowRetryFailedUnits(ctx context.Context, itemID, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "retry failed workflow units"); err != nil {
		return err
	}
	// Nothing to release here, unlike the single-unit retry: this repairs units
	// resting `failed`, and a unit under human steering rests `taken-over`. Its
	// registration stays because the unit does — the human is still driving it.
	return workflowEngine.RetryFailedUnits(itemID, note)
}

// WorkflowDropUnit accepts a failed or taken-over WORK unit's absence. The unit
// is recorded `dropped`, its join sees it as such, and the attempt resumes. The
// join itself is refused: it is what consolidates the units, so its absence
// leaves nothing to accept.
func (a *App) WorkflowDropUnit(itemID, unitID, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	abandoned := a.workflowUnitThreadUnderTakeover(itemID, unitID)
	if err := workflowEngine.DropUnit(itemID, unitID, note); err != nil {
		return err
	}
	a.releaseWorkflowUnitTakeover(abandoned)
	return nil
}

// WorkflowTakeOverUnit detaches one live fan-out unit from engine control so a
// human can steer its thread directly. The unit's session stays alive and is
// re-registered schema-less, exactly as a taken-over phase thread is; its
// siblings keep running and the attempt parks once they rest.
func (a *App) WorkflowTakeOverUnit(itemID, unitID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if !a.workflowApplication().HasRunner() {
		return fmt.Errorf("take over workflow unit %q of item %s: runner unavailable", unitID, itemID)
	}
	unit, err := a.currentWorkflowUnit(itemID, unitID)
	if err != nil {
		return fmt.Errorf("take over workflow unit: %w", err)
	}
	if unit.ThreadID == "" {
		return fmt.Errorf(
			"take over workflow unit %q of item %s: the unit runs a deterministic command and has no session to steer",
			unitID, itemID,
		)
	}
	if err := workflowEngine.TakeOverUnit(itemID, unitID); err != nil {
		return err
	}
	if err := a.workflowApplication().RegisterTakeover(context.Background(), itemID, unit.ThreadID); err != nil {
		return fmt.Errorf(
			"take over workflow unit %q of item %s: register schema-less steering: %w",
			unitID, itemID, err,
		)
	}
	return nil
}

// workflowFailedUnits lists the units resting `failed` in a run's CURRENT phase
// attempt — exactly the set WorkflowRetryFailedUnits acts on, and exactly the
// ids WorkflowRetryUnit accepts. The join is one of them: its failure is the
// failure of a unit of the attempt, and a human repairs it by name like any
// other. The rule lives here so the surfaces that REPORT the set (the wake's
// failed-unit references, `agent-overflow run status`) cannot disagree with the
// verbs that repair it.
//
// Earlier attempts are excluded for the same reason: their rows are history, and
// the engine's repair only ever addresses the attempt the run is parked on.
func (a *App) workflowFailedUnits(itemID string) ([]store.WorkItemUnit, error) {
	return a.workflowApplication().FailedUnits(itemID)
}

// currentWorkflowUnit resolves one unit of a run's current phase attempt. Unit
// ids are unique inside an attempt, not inside a run, so the attempt is what
// makes the lookup unambiguous.
func (a *App) currentWorkflowUnit(itemID, unitID string) (store.WorkItemUnit, error) {
	return a.workflowApplication().CurrentUnit(itemID, unitID)
}

// workflowUnitThreadUnderTakeover returns the thread a unit is currently being
// steered on, or "" when it is not under human control. Failing to resolve it is
// deliberately not an error: the recovery action is what the human asked for,
// and the engine validates the unit itself.
func (a *App) workflowUnitThreadUnderTakeover(itemID, unitID string) string {
	return a.workflowApplication().UnitThreadUnderTakeover(itemID, unitID)
}

// releaseWorkflowUnitTakeover drops the steering registration of a unit thread
// the human has just retried or dropped. A retry starts a fresh try on a fresh
// thread and a drop ends the unit outright, so leaving the old thread registered
// would keep an abandoned session claiming to be a live takeover — and a send
// into it would be accepted as steering work nothing consumes.
func (a *App) releaseWorkflowUnitTakeover(threadID string) {
	if threadID == "" || !a.workflowApplication().HasRunner() {
		return
	}
	a.workflowApplication().ClearTakeoverThread(threadID)
}
