package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/profile"
)

// enforceBudget routes every budget decision through the engine's existing
// teardown path. The bool reports that the item was parked, including when a
// spend/profile error made the budget impossible to evaluate.
//
// The budget checked is always the *root* run's, and the spend is the whole
// tree's (§12): a child that would otherwise run under no ceiling at all is
// bounded by the ceiling its root was started with, and a runaway recursion
// hits it however the depth bounds are set. The item parked is the one the
// check ran on — the run that was about to spend past the ceiling.
func (e *Engine) enforceBudget(item *runtimeItem) (bool, error) {
	exceeded, err := e.checkBudget(item)
	if err != nil {
		return true, errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
			err,
		)
	}
	if exceeded {
		return true, e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonBudgetExhausted})
	}
	return false, nil
}

func (e *Engine) checkBudget(item *runtimeItem) (bool, error) {
	root, err := e.treeRoot(item)
	if err != nil {
		return false, err
	}
	budget, err := e.effectiveBudget(root)
	if err != nil || budget == nil {
		return false, err
	}

	// Wall clock runs from the *root's* persisted start, not this run's: a child
	// started an hour into its tree has already spent that hour of the ceiling.
	elapsed := e.now().Sub(time.UnixMilli(root.StartedAt))
	if elapsed < 0 {
		elapsed = 0
	}
	if budget.WallClock != nil {
		ceiling, err := time.ParseDuration(string(*budget.WallClock))
		if err != nil || ceiling <= 0 {
			return false, fmt.Errorf("check item %q budget: invalid wall_clock %q", root.ID, *budget.WallClock)
		}
		if elapsed > ceiling {
			e.emitBudgetExceeded(item.item.ID, Spend{}, elapsed)
			return true, nil
		}
		return false, nil
	}

	spend, err := e.spend.TreeSpend(e.ctx, root.ID)
	if err != nil {
		return false, fmt.Errorf("check item %q budget spend: %w", root.ID, err)
	}
	exceeded := budget.Tokens != nil && spend.Tokens > *budget.Tokens ||
		budget.USD != nil && spend.USD > *budget.USD
	if exceeded {
		e.emitBudgetExceeded(item.item.ID, spend, elapsed)
	}
	return exceeded, nil
}

// treeRoot resolves the run tree's root — the item every tree-wide fact is read
// off: the budget envelope and start time the whole tree is measured against
// (§12), and the soft-stop request the next call boundary consults (D36).
// Parent linkage is immutable, so the root id is cached on the resident item;
// the row itself is re-read every time, because both of those facts change
// under the tree (a rerun re-stamps the start, a human arms the stop mid-run).
//
// The row is read even when the caller IS the root, because the resident item's
// copy is not authoritative for either field: both are written from outside the
// command loop's view of the item, and a stale read here would silently ignore
// a stop a human armed a second ago.
func (e *Engine) treeRoot(item *runtimeItem) (store.WorkItem, error) {
	if item.rootID == "" {
		current := item.item
		for depth := 0; current.ParentItemID != ""; depth++ {
			if depth > MaxCallDepth {
				return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: tree is deeper than %d", item.item.ID, MaxCallDepth)
			}
			parent, err := e.store.GetWorkItem(current.ParentItemID)
			if err != nil {
				return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: %w", item.item.ID, err)
			}
			current = parent
		}
		item.rootID = current.ID
	}
	root, err := e.store.GetWorkItem(item.rootID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: %w", item.item.ID, err)
	}
	return root, nil
}

func (e *Engine) effectiveBudget(item store.WorkItem) (*profile.Budget, error) {
	if len(item.Budget) > 0 {
		var budget profile.Budget
		if err := decodeJSON(item.Budget, &budget); err != nil {
			return nil, fmt.Errorf("decode item %q budget: %w", item.ID, err)
		}
		if validation := profile.ValidateBudget(&budget); !validation.Valid() {
			return nil, fmt.Errorf("decode item %q budget: %s", item.ID, joinProfileFindings(validation.Findings))
		}
		return &budget, nil
	}

	projectProfile, err := e.profiles.Profile(e.ctx, item.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load budget profile for project %q: %w", item.ProjectID, err)
	}
	if projectProfile == nil {
		return nil, fmt.Errorf("load budget profile for project %q: nil profile", item.ProjectID)
	}
	return projectProfile.Reliability.PerItemBudget, nil
}

func joinProfileFindings(findings []profile.Finding) string {
	messages := make([]string, 0, len(findings))
	for _, finding := range findings {
		messages = append(messages, finding.Error())
	}
	return strings.Join(messages, "; ")
}

func (e *Engine) emitBudgetExceeded(itemID string, spend Spend, elapsed time.Duration) {
	spendCopy := spend
	e.emitter.Emit("workflow:error", ErrorEvent{
		ItemID:          itemID,
		Error:           "workflow item budget exhausted",
		Spend:           &spendCopy,
		WallClockMillis: elapsed.Milliseconds(),
	})
}
