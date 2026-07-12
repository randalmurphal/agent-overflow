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
	item.waiting = false
	return errors.Join(errs...)
}

func (e *Engine) startWaiting() error {
	waiting := make([]*runtimeItem, 0)
	for _, item := range e.items {
		if item.waiting && State(item.item.State) == StateRunning {
			waiting = append(waiting, item)
		}
	}
	sort.Slice(waiting, func(i, j int) bool {
		left, right := waiting[i].item, waiting[j].item
		if left.SortPosition != right.SortPosition {
			return left.SortPosition < right.SortPosition
		}
		if left.CreatedAt != right.CreatedAt {
			return left.CreatedAt < right.CreatedAt
		}
		return left.ID < right.ID
	})

	var errs []error
	for _, item := range waiting {
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
			continue
		}
		item.acquired = canonicalResources(phase.Resources)
		item.waiting = false
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
