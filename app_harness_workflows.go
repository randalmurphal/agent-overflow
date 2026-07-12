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

// HarnessSeedWorkflowItem creates Count copies through WorkflowEnqueueItem.
// Target is queued (the default), needs-human, or done.
type HarnessSeedWorkflowItem struct {
	Workflow string          `json:"workflow"`
	Goal     string          `json:"goal"`
	Seeds    json.RawMessage `json:"seeds,omitempty"`
	StepMode bool            `json:"stepMode,omitempty"`
	Count    int             `json:"count,omitempty"`
	Target   string          `json:"target,omitempty"`
}

const maxHarnessSeedWorkflowItems = 100

type harnessWorkflowQueueSnapshot struct {
	active      bool
	concurrency int
	maxStarts   int
}

// workflowQueueSnapshot captures the effective queue state. maxStarts is
// process-local rather than persisted, but workflow:queue-state exposes a
// positive remaining budget. An exhausted bounded drain is the one state that
// cannot be reconstructed through WorkflowSetQueue, so refuse instead of
// silently converting it to an unbounded active queue.
func (h *Harness) workflowQueueSnapshot() (harnessWorkflowQueueSnapshot, error) {
	settings := h.app.currentSettings()
	snapshot := harnessWorkflowQueueSnapshot{
		active: settings.WorkflowQueueActive, concurrency: settings.WorkflowConcurrency,
	}
	bus := h.app.eventBus.Load()
	if bus == nil {
		return snapshot, nil
	}
	events := bus.Replay(map[string]uint64{"workflow:queue-state": 0})
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Channel != "workflow:queue-state" {
			continue
		}
		if event.Gap {
			return harnessWorkflowQueueSnapshot{}, fmt.Errorf("workflow queue state fell outside the event replay window; cannot restore it exactly")
		}
		var queue engine.QueueEvent
		if err := json.Unmarshal(event.Data, &queue); err != nil {
			return harnessWorkflowQueueSnapshot{}, fmt.Errorf("decode workflow:queue-state snapshot: %w", err)
		}
		if settings.WorkflowQueueActive && !queue.Active && queue.StartsRemaining == 0 {
			return harnessWorkflowQueueSnapshot{}, fmt.Errorf("workflow queue has an exhausted transient maxStarts budget that cannot be restored exactly")
		}
		snapshot.active = queue.Active
		snapshot.concurrency = queue.GlobalConcurrency
		snapshot.maxStarts = queue.StartsRemaining
		return snapshot, nil
	}
	return snapshot, nil
}

func validateWorkflowSeedPlan(spec HarnessSeedSpec) error {
	total := 0
	for projectIndex, project := range spec.Projects {
		if project.Workflows == nil {
			continue
		}
		for itemIndex, item := range project.Workflows.Items {
			target := workflowTarget(item)
			switch engine.State(target) {
			case engine.StateQueued, engine.StateNeedsHuman, engine.StateDone:
			default:
				return fmt.Errorf("seed project %d (%s): workflows: item %d (%s) target %q: unsupported target; supported targets are queued, needs-human, and done", projectIndex+1, project.Name, itemIndex+1, item.Workflow, target)
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

func specHasQueuedWorkflowTarget(spec HarnessSeedSpec) bool {
	for _, project := range spec.Projects {
		if project.Workflows == nil {
			continue
		}
		for _, item := range project.Workflows.Items {
			if workflowTarget(item) == string(engine.StateQueued) {
				return true
			}
		}
	}
	return false
}

func specHasDrivenWorkflowTarget(spec HarnessSeedSpec) bool {
	for _, project := range spec.Projects {
		if project.Workflows == nil {
			continue
		}
		for _, item := range project.Workflows.Items {
			if workflowTarget(item) != string(engine.StateQueued) {
				return true
			}
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

func (h *Harness) seedWorkflowItemsInTargetOrder(spec HarnessSeedSpec, result *HarnessSeedResult) error {
	createdByProject := make([][][]string, len(spec.Projects))
	for projectIndex, project := range spec.Projects {
		if project.Workflows != nil {
			createdByProject[projectIndex] = make([][]string, len(project.Workflows.Items))
		}
	}
	// Drive every non-queued target before creating any queued target. The
	// production scheduler is global, so this keeps maxStarts=1 pinned to the
	// intended item while the result is reconstructed in authored order.
	for _, queuedPass := range []bool{false, true} {
		for projectIndex, project := range spec.Projects {
			if project.Workflows == nil {
				continue
			}
			for itemIndex, item := range project.Workflows.Items {
				isQueued := workflowTarget(item) == string(engine.StateQueued)
				if isQueued != queuedPass {
					continue
				}
				created, err := h.seedWorkflowItems(result.Projects[projectIndex].ProjectID, item)
				if err != nil {
					return fmt.Errorf("seed project %d (%s): workflows: item %d (%s) target %q: %w", projectIndex+1, project.Name, itemIndex+1, item.Workflow, workflowTarget(item), err)
				}
				createdByProject[projectIndex][itemIndex] = created
			}
		}
	}
	for projectIndex := range result.Projects {
		for _, itemIDs := range createdByProject[projectIndex] {
			result.Projects[projectIndex].WorkItemIDs = append(result.Projects[projectIndex].WorkItemIDs, itemIDs...)
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
		return string(engine.StateQueued)
	}
	return spec.Target
}

func (h *Harness) seedWorkflowItems(projectID string, spec HarnessSeedWorkflowItem) ([]string, error) {
	target := workflowTarget(spec)
	switch engine.State(target) {
	case engine.StateQueued, engine.StateNeedsHuman, engine.StateDone:
	default:
		return nil, fmt.Errorf("unsupported target %q; supported targets are queued, needs-human, and done", target)
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
		item, err := h.app.WorkflowEnqueueItem(projectID, spec.Workflow, string(def.ScopeProject), spec.Goal, seeds, nil, spec.StepMode)
		if err != nil {
			return created, err
		}
		created = append(created, item.ID)
		if engine.State(target) == engine.StateQueued {
			continue
		}
		if err := h.driveSeedWorkflowItem(item.ID, engine.State(target)); err != nil {
			return created, err
		}
	}
	return created, nil
}

const harnessWorkflowTargetTimeout = 30 * time.Second

func (h *Harness) driveSeedWorkflowItem(itemID string, target engine.State) (err error) {
	bus := h.app.eventBus.Load()
	if bus == nil {
		return fmt.Errorf("cannot drive target %q without the harness event bus", target)
	}
	subscriber := bus.Subscribe()
	defer subscriber.Close()
	if err := h.app.WorkflowSetQueue(true, 1, 1); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, h.app.WorkflowSetQueue(false, 0, 1))
	}()

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

// prepareWorkflowReset pauses the queue and evicts every queued runtime item
// through the production scheduler/cancel path before project deletion removes
// its SQLite row. Parked and terminal items are already evicted by the engine;
// the project cascade removes their durable rows later in HarnessReset.
func (h *Harness) prepareWorkflowReset() (func() error, error) {
	if h.app.workflowEngine == nil {
		return nil, nil
	}
	snapshot, err := h.workflowQueueSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reset workflows: snapshot queue: %w", err)
	}
	if err := h.app.WorkflowSetQueue(false, 0, snapshot.concurrency); err != nil {
		return nil, fmt.Errorf("reset workflows: pause queue: %w", err)
	}
	restore := func() error {
		if err := h.app.WorkflowSetQueue(snapshot.active, snapshot.maxStarts, snapshot.concurrency); err != nil {
			return fmt.Errorf("reset workflows: restore queue: %w", err)
		}
		return nil
	}

	projects, err := h.app.store.ListProjects()
	if err != nil {
		return restore, fmt.Errorf("reset workflows: list projects: %w", err)
	}
	for _, projectRow := range projects {
		running, err := h.app.store.ListWorkItemSummaries(store.WorkItemListFilter{
			ProjectID: projectRow.ID,
			States:    []string{string(engine.StateRunning)},
		})
		if err != nil {
			return restore, fmt.Errorf("reset workflows: list running items for project %s: %w", projectRow.ID, err)
		}
		for _, item := range running {
			if err := h.app.WorkflowCancelItem(item.ID); err != nil {
				return restore, fmt.Errorf("reset workflows: cancel running item %s: %w", item.ID, err)
			}
		}
	}
	for {
		queued, err := h.queuedWorkflowItems(projects)
		if err != nil {
			return restore, err
		}
		if len(queued) == 0 {
			break
		}
		startErr := h.app.WorkflowSetQueue(true, 1, 1)
		pauseErr := h.app.WorkflowSetQueue(false, 0, 1)
		var transitioned []store.WorkItem
		for _, queuedItem := range queued {
			item, getErr := h.app.store.GetWorkItem(queuedItem.ID)
			if getErr != nil {
				return restore, errors.Join(fmt.Errorf("reset workflows: reload queued item %s: %w", queuedItem.ID, getErr), startErr, pauseErr)
			}
			if item.State != string(engine.StateQueued) {
				transitioned = append(transitioned, item)
			}
		}
		if len(transitioned) == 0 {
			return restore, errors.Join(fmt.Errorf("reset workflows: no queued item left the queue"), startErr, pauseErr)
		}
		if startErr != nil {
			item := transitioned[0]
			log.Printf("harness: reset: starting queued workflow item %s settled as %s (%s): %v", item.ID, item.State, item.Reason, startErr)
		}
		if pauseErr != nil {
			return restore, fmt.Errorf("reset workflows: pause after starting queued item: %w", pauseErr)
		}
		for _, item := range transitioned {
			if item.State == string(engine.StateRunning) {
				if err := h.app.WorkflowCancelItem(item.ID); err != nil {
					return restore, fmt.Errorf("reset workflows: cancel item %s: %w", item.ID, err)
				}
			}
		}
	}
	// Queue mutation RPCs intentionally detach runner provisioning. Reset must
	// still wait for those workers before enumerating/deleting threads, or a
	// late phase-thread create can race the project deletion snapshot.
	if err := h.app.workflowEngine.Sync(); err != nil {
		log.Printf("harness: reset: detached workflow startup settled with error: %v", err)
	}
	return restore, nil
}

func (h *Harness) queuedWorkflowItems(projects []store.Project) ([]store.WorkItem, error) {
	var queued []store.WorkItem
	for _, projectRow := range projects {
		items, err := h.app.store.ListWorkItemSummaries(store.WorkItemListFilter{
			ProjectID: projectRow.ID,
			States:    []string{string(engine.StateQueued)},
		})
		if err != nil {
			return nil, fmt.Errorf("reset workflows: list queued items for project %s: %w", projectRow.ID, err)
		}
		queued = append(queued, items...)
	}
	return queued, nil
}
