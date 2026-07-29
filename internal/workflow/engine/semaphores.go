package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/profile"
)

// phaseResources is the canonical, sorted set a phase acquires: everything it
// declared plus the implicit `provider:<name>` resource an agent-driver phase
// takes for its own turn. Tool phases never acquire provider capacity. A frozen
// agent phase without a provider is unrunnable, so it is refused here rather
// than silently escaping the bound.
//
// A fan-out phase runs no turn of its own — its units and its join do, each
// acquiring provider capacity for itself (unitResources). If the phase also
// held a slot it would compete with the very units it is waiting for, and at
// capacity 1 it would deadlock outright: the phase would hold the only slot
// forever while its first unit waited for it.
func phaseResources(phase def.Phase) ([]string, error) {
	names := phase.Resources
	if phase.Driver == def.DriverAgent && phase.EffectiveShape() != def.ShapeFanOut {
		provider := strings.TrimSpace(phase.Provider)
		if provider == "" {
			return nil, fmt.Errorf("agent phase %q declares no provider", phase.ID)
		}
		names = append(append([]string(nil), names...), ProviderResource(provider))
	}
	return canonicalResources(names), nil
}

// unitResources is what one fan-out unit or join acquires: its own provider
// capacity, and nothing else. The phase's declared resources stay phase-scoped
// — acquired once at phase entry and held for the whole attempt — so a
// `live-stack` mutex is taken once by the attempt, not once per unit.
//
// A call unit takes nothing, for the same reason a call phase takes nothing: it
// runs no turn. Its child run's phases acquire what they need through the same
// project semaphores while the unit rests, so the tree's provider bound is
// respected without the resting unit holding a slot its own child would then
// have to wait for.
func unitResources(unit def.Unit) ([]string, error) {
	driver, runsWork := unit.EffectiveDriver()
	if !runsWork || driver != def.DriverAgent {
		return nil, nil
	}
	provider := strings.TrimSpace(unit.Provider)
	if provider == "" {
		return nil, fmt.Errorf("agent unit %q declares no provider", unit.ID)
	}
	return []string{ProviderResource(provider)}, nil
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
// editing profile.yaml takes effect on the next phase or unit start. A
// `provider:<name>` resource the profile does not declare falls back to
// DefaultProviderCapacity; any other undeclared resource is a wiring error.
func resourceCapacity(capacities map[string]int, projectID, name string) (int, error) {
	capacity, declared := capacities[name]
	if declared {
		if capacity < 1 {
			return 0, fmt.Errorf("project %q resource %q has no positive capacity", projectID, name)
		}
		return capacity, nil
	}
	if def.IsProviderResource(name) {
		return DefaultProviderCapacity, nil
	}
	return 0, fmt.Errorf("project %q resource %q has no positive capacity", projectID, name)
}

// acquirePhaseResources takes the phase's whole resource set all-or-nothing in
// canonical order and returns exactly what it took, so a caller can never
// record a different set than it holds.
func (e *Engine) acquirePhaseResources(projectID string, phase def.Phase) ([]string, bool, error) {
	names, err := phaseResources(phase)
	if err != nil {
		return nil, false, err
	}
	return e.acquireResources(projectID, names)
}

// acquireUnitResources takes one unit's capacity through the same semaphores,
// with the same all-or-nothing rule, so units and phases contend on one bound.
func (e *Engine) acquireUnitResources(projectID string, unit def.Unit) ([]string, bool, error) {
	names, err := unitResources(unit)
	if err != nil {
		return nil, false, err
	}
	return e.acquireResources(projectID, names)
}

// liveProfile is the one read of a project's profile the scheduler makes at
// decision time (spec §6: bounds are read live, so editing profile.yaml takes
// effect on the next start or expansion — no restart, no re-run). A source that
// answers nil without an error is a broken source, not an unbounded project.
func (e *Engine) liveProfile(projectID string) (*profile.Profile, error) {
	projectProfile, err := e.profiles.Profile(e.ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("load live profile for project %q: %w", projectID, err)
	}
	if projectProfile == nil {
		return nil, fmt.Errorf("load live profile for project %q: nil profile", projectID)
	}
	return projectProfile, nil
}

func (e *Engine) acquireResources(projectID string, names []string) ([]string, bool, error) {
	if len(names) == 0 {
		return nil, true, nil
	}
	projectProfile, err := e.liveProfile(projectID)
	if err != nil {
		return nil, false, err
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
	errs := []error{e.releaseHeld(item.item.ProjectID, item.acquired)}
	item.acquired = nil
	e.removeAllWaiting(item)
	return errors.Join(errs...)
}

// releaseUnitResources is called only by teardownUnit.
func (e *Engine) releaseUnitResources(item *runtimeItem, unit *unitRun) error {
	err := e.releaseHeld(item.item.ProjectID, unit.acquired)
	unit.acquired = nil
	e.removeWaiting(item, unit)
	return err
}

func (e *Engine) releaseHeld(projectID string, names []string) error {
	var errs []error
	for _, name := range names {
		key := resourceKey{projectID: projectID, name: name}
		if e.holders[key] < 1 {
			errs = append(errs, fmt.Errorf("release unheld project resource %s/%s", projectID, name))
			continue
		}
		if e.holders[key] == 1 {
			delete(e.holders, key)
		} else {
			e.holders[key]--
		}
	}
	return errors.Join(errs...)
}

// waiter is one held start: a phase attempt, or one unit of a fan-out attempt.
// Both wait in the same FIFO, so freed capacity always goes to the
// longest-waiting piece of work regardless of which kind it is.
type waiter struct {
	item *runtimeItem
	unit *unitRun
}

// itemID identifies the run a held start belongs to, whichever kind it is.
func (w waiter) itemID() string { return w.item.item.ID }

type waitKey struct {
	itemID string
	unitID string
}

func waiterKey(item *runtimeItem, unit *unitRun) waitKey {
	key := waitKey{itemID: item.item.ID}
	if unit != nil {
		key.unitID = unit.id
	}
	return key
}

// addWaiting appends to a FIFO list, so freed capacity goes to the
// longest-waiting phase or unit.
func (e *Engine) addWaiting(item *runtimeItem, unit *unitRun) {
	key := waiterKey(item, unit)
	if _, exists := e.waitingKeys[key]; exists {
		markWaiting(item, unit, true)
		return
	}
	e.waiting = append(e.waiting, waiter{item: item, unit: unit})
	e.waitingKeys[key] = struct{}{}
	markWaiting(item, unit, true)
}

func (e *Engine) removeWaiting(item *runtimeItem, unit *unitRun) {
	key := waiterKey(item, unit)
	if _, exists := e.waitingKeys[key]; !exists {
		markWaiting(item, unit, false)
		return
	}
	delete(e.waitingKeys, key)
	for index, held := range e.waiting {
		if held.item != item || held.unit != unit {
			continue
		}
		copy(e.waiting[index:], e.waiting[index+1:])
		e.waiting[len(e.waiting)-1] = waiter{}
		e.waiting = e.waiting[:len(e.waiting)-1]
		break
	}
	markWaiting(item, unit, false)
}

// removeAllWaiting drops every held start belonging to an item — its phase
// start and each of its units. Teardown releases the whole attempt, so leaving
// a unit waiter behind would hand capacity to work that no longer exists.
func (e *Engine) removeAllWaiting(item *runtimeItem) {
	for index := len(e.waiting) - 1; index >= 0; index-- {
		if e.waiting[index].item == item {
			e.removeWaiting(item, e.waiting[index].unit)
		}
	}
}

func markWaiting(item *runtimeItem, unit *unitRun, waiting bool) {
	if unit != nil {
		unit.waiting = waiting
		return
	}
	item.waiting = waiting
}

// startWaiting releases held starts in wait order. It is the one place freed
// capacity — or an unpause — turns into work, for phases and units alike.
func (e *Engine) startWaiting() error {
	if e.paused {
		return nil
	}
	var errs []error
	for index := 0; index < len(e.waiting); {
		held := e.waiting[index]
		if held.unit != nil {
			started, err := e.releaseWaitingUnit(held.item, held.unit)
			if err != nil {
				e.emitError(held.item.item.ID, err)
				errs = append(errs, err)
			}
			if !started && err == nil {
				index++
			}
			continue
		}
		item := held.item
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
		e.removeWaiting(item, nil)
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
		if err := e.startPhaseWork(item, phase, vars); err != nil {
			e.emitError(item.item.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// releaseWaitingUnit starts one held unit if its capacity is now free. It
// reports whether the waiter left the list, which is how startWaiting knows
// whether its cursor still points at an unexamined entry.
func (e *Engine) releaseWaitingUnit(item *runtimeItem, unit *unitRun) (bool, error) {
	acquired, ok, err := e.acquireUnitResources(item.item.ProjectID, unit.definition)
	if err != nil {
		return true, errors.Join(err, e.teardown(item, teardownRequest{
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed,
		}))
	}
	if !ok {
		return false, nil
	}
	unit.acquired = acquired
	e.removeWaiting(item, unit)
	return true, e.startUnitWork(item, unit)
}
