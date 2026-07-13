package engine

import (
	"errors"
	"fmt"
	"sort"
)

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

func (e *Engine) acquireResources(projectID string, resources []string) (bool, error) {
	names := canonicalResources(resources)
	if len(names) == 0 {
		return true, nil
	}
	projectProfile, err := e.profiles.Profile(e.ctx, projectID)
	if err != nil {
		return false, fmt.Errorf("load live profile for project %q: %w", projectID, err)
	}
	if projectProfile == nil {
		return false, fmt.Errorf("load live profile for project %q: nil profile", projectID)
	}
	for _, name := range names {
		capacity, ok := projectProfile.Capacities[name]
		if !ok || capacity < 1 {
			return false, fmt.Errorf("project %q resource %q has no positive capacity", projectID, name)
		}
		if e.holders[resourceKey{projectID: projectID, name: name}] >= capacity {
			return false, nil
		}
	}
	for _, name := range names {
		e.holders[resourceKey{projectID: projectID, name: name}]++
	}
	return true, nil
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

func (e *Engine) addWaiting(item *runtimeItem) {
	if _, exists := e.waitingByID[item.item.ID]; exists {
		item.waiting = true
		return
	}
	index := sort.Search(len(e.waiting), func(index int) bool {
		return !queueLess(e.waiting[index].item, item.item)
	})
	e.waiting = append(e.waiting, nil)
	copy(e.waiting[index+1:], e.waiting[index:])
	e.waiting[index] = item
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

func (e *Engine) startWaiting() error {
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
		acquired, err := e.acquireResources(item.item.ProjectID, phase.Resources)
		if err != nil {
			combined := errors.Join(err, e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}))
			e.emitError(item.item.ID, combined)
			errs = append(errs, combined)
			continue
		}
		if !acquired {
			index++
			continue
		}
		item.acquired = canonicalResources(phase.Resources)
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
