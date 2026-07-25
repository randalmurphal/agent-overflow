package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/workflow/def"
)

// phaseResources is the canonical, sorted set a phase acquires: everything it
// declared plus the implicit `provider:<name>` resource every agent-driver
// phase takes. Tool phases never acquire provider capacity. A frozen agent
// phase without a provider is unrunnable, so it is refused here rather than
// silently escaping the bound.
func phaseResources(phase def.Phase) ([]string, error) {
	names := phase.Resources
	if phase.Driver == def.DriverAgent {
		provider := strings.TrimSpace(phase.Provider)
		if provider == "" {
			return nil, fmt.Errorf("agent phase %q declares no provider", phase.ID)
		}
		names = append(append([]string(nil), names...), ProviderResource(provider))
	}
	return canonicalResources(names), nil
}

func canonicalResources(resources []string) []string {
	set := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if resource != "" {
			set[resource] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for resource := range set {
		result = append(result, resource)
	}
	sort.Strings(result)
	return result
}

// resourceCapacity reads the live project profile at every acquisition, so
// editing profile.yaml takes effect on the next phase start. A `provider:<name>`
// resource the profile does not declare falls back to DefaultProviderCapacity;
// any other undeclared resource is a wiring error.
func resourceCapacity(capacities map[string]int, projectID, name string) (int, error) {
	capacity, declared := capacities[name]
	if declared {
		if capacity < 1 {
			return 0, fmt.Errorf("project %q resource %q has no positive capacity", projectID, name)
		}
		return capacity, nil
	}
	if isProviderResource(name) {
		return DefaultProviderCapacity, nil
	}
	return 0, fmt.Errorf("project %q resource %q has no positive capacity", projectID, name)
}

const providerResourcePrefix = "provider:"

func isProviderResource(name string) bool {
	return len(name) > len(providerResourcePrefix) && strings.HasPrefix(name, providerResourcePrefix)
}

// acquirePhaseResources takes the phase's whole resource set all-or-nothing in
// canonical order and returns exactly what it took, so a caller can never
// record a different set than it holds.
func (e *Engine) acquirePhaseResources(projectID string, phase def.Phase) ([]string, bool, error) {
	names, err := phaseResources(phase)
	if err != nil {
		return nil, false, err
	}
	if len(names) == 0 {
		return nil, true, nil
	}
	projectProfile, err := e.profiles.Profile(e.ctx, projectID)
	if err != nil {
		return nil, false, fmt.Errorf("load live profile for project %q: %w", projectID, err)
	}
	if projectProfile == nil {
		return nil, false, fmt.Errorf("load live profile for project %q: nil profile", projectID)
	}
	for _, name := range names {
		capacity, err := resourceCapacity(projectProfile.Capacities, projectID, name)
		if err != nil {
			return nil, false, err
		}
		if e.holders[resourceKey{projectID: projectID, name: name}] >= capacity {
			return nil, false, nil
		}
	}
	for _, name := range names {
		e.holders[resourceKey{projectID: projectID, name: name}]++
	}
	return names, true, nil
}

// releaseResources is called only by teardown.
func (e *Engine) releaseResources(item *runtimeItem) error {
	var errs []error
	for _, name := range item.acquired {
		key := resourceKey{projectID: item.item.ProjectID, name: name}
		if e.holders[key] < 1 {
			errs = append(errs, fmt.Errorf("release unheld project resource %s/%s", item.item.ProjectID, name))
			continue
		}
		if e.holders[key] == 1 {
			delete(e.holders, key)
		} else {
			e.holders[key]--
		}
	}
	item.acquired = nil
	e.removeWaiting(item)
	return errors.Join(errs...)
}

// addWaiting appends to a FIFO list, so freed capacity goes to the
// longest-waiting phase.
func (e *Engine) addWaiting(item *runtimeItem) {
	if _, exists := e.waitingByID[item.item.ID]; exists {
		item.waiting = true
		return
	}
	e.waiting = append(e.waiting, item)
	e.waitingByID[item.item.ID] = struct{}{}
	item.waiting = true
}

func (e *Engine) removeWaiting(item *runtimeItem) {
	if _, exists := e.waitingByID[item.item.ID]; !exists {
		item.waiting = false
		return
	}
	delete(e.waitingByID, item.item.ID)
	for index, waiting := range e.waiting {
		if waiting != item {
			continue
		}
		copy(e.waiting[index:], e.waiting[index+1:])
		e.waiting[len(e.waiting)-1] = nil
		e.waiting = e.waiting[:len(e.waiting)-1]
		break
	}
	item.waiting = false
}

// startWaiting releases held phase starts in wait order. It is the one place
// freed capacity — or an unpause — turns into work.
func (e *Engine) startWaiting() error {
	if e.paused {
		return nil
	}
	var errs []error
	for index := 0; index < len(e.waiting); {
		item := e.waiting[index]
		phase, ok := findPhase(item.workflow, item.phaseID)
		if !ok {
			err := errors.Join(
				e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
				fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID),
			)
			e.emitError(item.item.ID, err)
			errs = append(errs, err)
			continue
		}
		acquired, ok, err := e.acquirePhaseResources(item.item.ProjectID, phase)
		if err != nil {
			combined := errors.Join(err, e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}))
			e.emitError(item.item.ID, combined)
			errs = append(errs, combined)
			continue
		}
		if !ok {
			index++
			continue
		}
		item.acquired = acquired
		e.removeWaiting(item)
		halted, err := e.enforceBudget(item)
		if halted {
			if err != nil {
				e.emitError(item.item.ID, err)
				errs = append(errs, err)
			}
			continue
		}
		vars, _, err := e.variableContext(item, nil)
		if err != nil {
			combined := errors.Join(err, e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}))
			e.emitError(item.item.ID, combined)
			errs = append(errs, combined)
			continue
		}
		if err := e.startRunner(item, phase, vars); err != nil {
			e.emitError(item.item.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
