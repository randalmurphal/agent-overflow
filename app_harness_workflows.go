package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	projectconfig "agent-overflow/internal/project"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// HarnessSeedWorkflows is the project-local workflow fixture surface. Files
// use the same project.ConfigDir layout as the production definition/profile
// sources; item rows are never written directly.
type HarnessSeedWorkflows struct {
	Definitions []HarnessSeedWorkflowDefinition `json:"definitions,omitempty"`
	Profile     string                          `json:"profile,omitempty"`
	Items       []HarnessSeedWorkflowItem       `json:"items,omitempty"`
}

// HarnessSeedWorkflowDefinition writes one YAML definition plus sibling prompt
// files. Name is the filename stem; the workflow id remains authoritative in
// YAML and may differ from Name.
type HarnessSeedWorkflowDefinition struct {
	Name    string            `json:"name"`
	YAML    string            `json:"yaml"`
	Prompts map[string]string `json:"prompts,omitempty"`
}

// HarnessSeedWorkflowItem creates Count copies through WorkflowStartRun. Every
// run starts immediately; Target is the state the seed waits for — running
// (the default: start it and return), needs-human, or done. A fixture that
// must not execute is seeded with the global pause set, which holds its first
// phase without parking it.
type HarnessSeedWorkflowItem struct {
	Workflow string          `json:"workflow"`
	Goal     string          `json:"goal"`
	Seeds    json.RawMessage `json:"seeds,omitempty"`
	StepMode bool            `json:"stepMode,omitempty"`
	Count    int             `json:"count,omitempty"`
	Target   string          `json:"target,omitempty"`
}

const maxHarnessSeedWorkflowItems = 100

// workflowSeedTargets are the states a seeded run may be driven to. `running`
// is the default and returns as soon as the run has started.
var workflowSeedTargets = []engine.State{engine.StateRunning, engine.StateNeedsHuman, engine.StateDone}

func supportedWorkflowSeedTarget(target string) bool {
	for _, supported := range workflowSeedTargets {
		if engine.State(target) == supported {
			return true
		}
	}
	return false
}

func workflowSeedTargetList() string {
	names := make([]string, 0, len(workflowSeedTargets))
	for _, target := range workflowSeedTargets {
		names = append(names, string(target))
	}
	return strings.Join(names, ", ")
}

func validateWorkflowSeedPlan(spec HarnessSeedSpec) error {
	total := 0
	for projectIndex, project := range spec.Projects {
		if project.Workflows == nil {
			continue
		}
		for itemIndex, item := range project.Workflows.Items {
			target := workflowTarget(item)
			if !supportedWorkflowSeedTarget(target) {
				return fmt.Errorf("seed project %d (%s): workflows: item %d (%s) target %q: unsupported target; supported targets are %s", projectIndex+1, project.Name, itemIndex+1, item.Workflow, target, workflowSeedTargetList())
			}
			if strings.TrimSpace(item.Workflow) == "" || strings.TrimSpace(item.Goal) == "" {
				return fmt.Errorf("seed project %d (%s): workflows: item %d target %q: workflow and goal must be non-empty", projectIndex+1, project.Name, itemIndex+1, target)
			}
			count := item.Count
			if count == 0 {
				count = 1
			}
			if count < 0 {
				return fmt.Errorf("seed project %d (%s): workflows: item %d (%s) target %q: count must be greater than zero", projectIndex+1, project.Name, itemIndex+1, item.Workflow, target)
			}
			if count > maxHarnessSeedWorkflowItems-total {
				return fmt.Errorf("seed workflows: expanded item count exceeds %d", maxHarnessSeedWorkflowItems)
			}
			total += count
		}
	}
	return nil
}

func specHasWorkflowSeed(spec HarnessSeedSpec) bool {
	for _, project := range spec.Projects {
		if project.Workflows != nil {
			return true
		}
	}
	return false
}

func (h *Harness) seedProjectWorkflowFiles(projectRow store.Project, spec HarnessSeedWorkflows) error {
	configDir := projectconfig.ConfigDir(h.paths.DataDir, projectRow.Slug)
	workflowDir := filepath.Join(configDir, "workflows")
	if len(spec.Definitions) > 0 {
		if err := os.MkdirAll(workflowDir, 0o700); err != nil {
			return fmt.Errorf("workflows: create definition directory: %w", err)
		}
	}

	claimed := make(map[string]string)
	workflowIDs := make([]string, 0, len(spec.Definitions))
	for i, definition := range spec.Definitions {
		workflowID, err := h.writeSeedWorkflowDefinition(workflowDir, definition, claimed)
		if err != nil {
			return fmt.Errorf("workflows: definition %d (%s): %w", i+1, definition.Name, err)
		}
		workflowIDs = append(workflowIDs, workflowID)
	}
	if spec.Profile != "" {
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			return fmt.Errorf("workflows: create profile directory: %w", err)
		}
		if err := writeSeedFile(filepath.Join(configDir, "profile.yaml"), []byte(spec.Profile)); err != nil {
			return fmt.Errorf("workflows: write profile: %w", err)
		}
	}

	profiles := workflowProfileSource{store: h.app.store, configRoot: h.paths.DataDir}
	definitions := workflowDefinitionSource{store: h.app.store, configRoot: h.paths.DataDir, profiles: profiles}
	for _, workflowID := range workflowIDs {
		if _, err := definitions.Resolve(context.Background(), store.WorkItem{
			ProjectID: projectRow.ID, WorkflowID: workflowID, WorkflowScope: string(def.ScopeProject),
		}); err != nil {
			return fmt.Errorf("workflows: definition %q: %w", workflowID, err)
		}
	}
	return nil
}

// seedWorkflowItems creates every project's runs in authored order. Direct
// start removes the old queue-ordering dance: runs start as they are created
// and are bounded by provider resource capacity, not by a shared drain budget.
func (h *Harness) seedWorkflowItemsForProjects(spec HarnessSeedSpec, result *HarnessSeedResult) error {
	for projectIndex, project := range spec.Projects {
		if project.Workflows == nil {
			continue
		}
		for itemIndex, item := range project.Workflows.Items {
			created, err := h.seedWorkflowItems(result.Projects[projectIndex].ProjectID, item)
			result.Projects[projectIndex].WorkItemIDs = append(result.Projects[projectIndex].WorkItemIDs, created...)
			if err != nil {
				return fmt.Errorf("seed project %d (%s): workflows: item %d (%s) target %q: %w", projectIndex+1, project.Name, itemIndex+1, item.Workflow, workflowTarget(item), err)
			}
		}
	}
	return nil
}

func (h *Harness) writeSeedWorkflowDefinition(workflowDir string, spec HarnessSeedWorkflowDefinition, claimed map[string]string) (string, error) {
	if spec.Name == "" || spec.Name != filepath.Base(spec.Name) || filepath.Ext(spec.Name) != "" || strings.ContainsAny(spec.Name, `/\\`) {
		return "", fmt.Errorf("name %q must be a plain filename stem", spec.Name)
	}
	if strings.TrimSpace(spec.YAML) == "" {
		return "", fmt.Errorf("yaml must be non-empty")
	}
	workflow, err := def.ParseBytes([]byte(spec.YAML))
	if err != nil {
		return "", err
	}
	definitionName := spec.Name + ".yaml"
	if err := claimSeedPath(claimed, definitionName, "definition "+spec.Name); err != nil {
		return "", err
	}
	if err := writeSeedFile(filepath.Join(workflowDir, definitionName), []byte(spec.YAML)); err != nil {
		return "", err
	}
	promptNames := make([]string, 0, len(spec.Prompts))
	for name := range spec.Prompts {
		promptNames = append(promptNames, name)
	}
	sort.Strings(promptNames)
	for _, name := range promptNames {
		body := spec.Prompts[name]
		rel, err := confinedSeedRelativePath(name)
		if err != nil {
			return "", fmt.Errorf("prompt %q: %w", name, err)
		}
		if err := claimSeedPath(claimed, rel, "definition "+spec.Name+" prompt "+name); err != nil {
			return "", err
		}
		path := filepath.Join(workflowDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("create prompt directory: %w", err)
		}
		if err := writeSeedFile(path, []byte(body)); err != nil {
			return "", fmt.Errorf("write prompt %q: %w", name, err)
		}
	}
	return workflow.ID, nil
}

func confinedSeedRelativePath(name string) (string, error) {
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("must be a non-empty relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the workflow directory")
	}
	return clean, nil
}

func claimSeedPath(claimed map[string]string, path, owner string) error {
	if prior := claimed[path]; prior != "" {
		return fmt.Errorf("path %q is claimed by both %s and %s", path, prior, owner)
	}
	claimed[path] = owner
	return nil
}

func writeSeedFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func workflowTarget(spec HarnessSeedWorkflowItem) string {
	if spec.Target == "" {
		return string(engine.StateRunning)
	}
	return spec.Target
}

func (h *Harness) seedWorkflowItems(projectID string, spec HarnessSeedWorkflowItem) ([]string, error) {
	target := workflowTarget(spec)
	if !supportedWorkflowSeedTarget(target) {
		return nil, fmt.Errorf("unsupported target %q; supported targets are %s", target, workflowSeedTargetList())
	}
	if strings.TrimSpace(spec.Workflow) == "" || strings.TrimSpace(spec.Goal) == "" {
		return nil, fmt.Errorf("workflow and goal must be non-empty")
	}
	count := spec.Count
	if count == 0 {
		count = 1
	}
	if count < 0 {
		return nil, fmt.Errorf("count must be greater than zero")
	}
	if count > maxHarnessSeedWorkflowItems {
		return nil, fmt.Errorf("count exceeds %d", maxHarnessSeedWorkflowItems)
	}
	seeds := spec.Seeds
	if len(seeds) == 0 {
		seeds = json.RawMessage(`{}`)
	}
	created := make([]string, 0, count)
	for i := 0; i < count; i++ {
		itemID, err := h.startSeedWorkflowItem(projectID, spec, seeds, engine.State(target))
		if itemID != "" {
			created = append(created, itemID)
		}
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

// startSeedWorkflowItem subscribes before starting so a run that reaches its
// target inside the start call cannot be missed.
func (h *Harness) startSeedWorkflowItem(projectID string, spec HarnessSeedWorkflowItem, seeds json.RawMessage, target engine.State) (string, error) {
	if target == engine.StateRunning {
		item, err := h.app.WorkflowStartRun(projectID, spec.Workflow, string(def.ScopeProject), spec.Goal, seeds, nil, "", spec.StepMode)
		if err != nil {
			return "", err
		}
		return item.ID, nil
	}
	bus := h.app.eventBus.Load()
	if bus == nil {
		return "", fmt.Errorf("cannot drive target %q without the harness event bus", target)
	}
	subscriber := bus.Subscribe()
	defer subscriber.Close()
	item, err := h.app.WorkflowStartRun(projectID, spec.Workflow, string(def.ScopeProject), spec.Goal, seeds, nil, "", spec.StepMode)
	if err != nil {
		return "", err
	}
	return item.ID, h.awaitSeedWorkflowTarget(subscriber, item.ID, target)
}

const harnessWorkflowTargetTimeout = 30 * time.Second

func (h *Harness) awaitSeedWorkflowTarget(subscriber *transport.Subscriber, itemID string, target engine.State) error {
	timer := time.NewTimer(harnessWorkflowTargetTimeout)
	defer timer.Stop()
	for {
		select {
		case event := <-subscriber.Events():
			if event.Channel != "workflow:item-state" {
				continue
			}
			var state engine.StateEvent
			if err := json.Unmarshal(event.Data, &state); err != nil {
				return fmt.Errorf("decode workflow:item-state while waiting for item %s target %q: %w", itemID, target, err)
			}
			if state.ItemID != itemID {
				continue
			}
			if state.To == target {
				return nil
			}
			if state.To == engine.StateDone || state.To == engine.StateFailed || state.To == engine.StateCancelled || state.To == engine.StateNeedsHuman {
				return fmt.Errorf("reached terminal/resting state %q (%s) before target %q", state.To, state.Reason, target)
			}
		case <-subscriber.Done():
			return fmt.Errorf("event bus closed before item reached target %q", target)
		case <-timer.C:
			item, getErr := h.app.store.GetWorkItem(itemID)
			if getErr != nil {
				return fmt.Errorf("timed out waiting for target %q and reloading the item failed: %w", target, getErr)
			}
			return fmt.Errorf("timed out waiting for target %q; item rests at %s (%s)", target, item.State, item.Reason)
		}
	}
}

// prepareWorkflowReset pauses the engine and cancels every live run through
// the production cancel path before project deletion removes its SQLite row.
// Pausing first means a phase completing mid-sweep cannot start its successor.
// Parked and terminal items are already evicted from engine memory; the
// project cascade removes their durable rows later in HarnessReset.
//
// The returned closure clears the pause flag rather than restoring whatever it
// was: reset is a blank slate, and a spec that leaves the engine paused (the
// documented way to seed a held fixture) must not silently hold every later
// spec's runs in the same worker.
func (h *Harness) prepareWorkflowReset() (func() error, error) {
	if h.app.workflowEngine == nil {
		return nil, nil
	}
	if err := h.app.WorkflowSetGlobalPause(true); err != nil {
		return nil, fmt.Errorf("reset workflows: pause engine: %w", err)
	}
	resume := func() error {
		if err := h.app.WorkflowSetGlobalPause(false); err != nil {
			return fmt.Errorf("reset workflows: clear pause state: %w", err)
		}
		return nil
	}

	running, err := h.app.store.ListWorkItemSummaries(store.WorkItemListFilter{
		States: []string{string(engine.StateRunning)},
	})
	if err != nil {
		return resume, fmt.Errorf("reset workflows: list running items: %w", err)
	}
	for _, item := range running {
		if err := h.app.WorkflowCancelItem(context.Background(), item.ID); err != nil {
			return resume, fmt.Errorf("reset workflows: cancel running item %s: %w", item.ID, err)
		}
	}
	// Start RPCs intentionally detach runner provisioning. Reset must still
	// wait for those workers before enumerating/deleting threads, or a late
	// phase-thread create can race the project deletion snapshot.
	if err := h.app.workflowEngine.Sync(); err != nil {
		log.Printf("harness: reset: detached workflow startup settled with error: %v", err)
	}
	return resume, nil
}
