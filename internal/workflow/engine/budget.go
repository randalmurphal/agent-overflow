package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/workflow/profile"
)

// enforceBudget routes every budget decision through the engine's existing
// teardown path. The bool reports that the item was parked, including when a
// spend/profile error made the budget impossible to evaluate.
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
	budget, err := e.effectiveBudget(item)
	if err != nil || budget == nil {
		return false, err
	}

	elapsed := e.now().Sub(time.UnixMilli(item.item.StartedAt))
	if elapsed < 0 {
		elapsed = 0
	}
	if budget.WallClock != nil {
		ceiling, err := time.ParseDuration(string(*budget.WallClock))
		if err != nil || ceiling <= 0 {
			return false, fmt.Errorf("check item %q budget: invalid wall_clock %q", item.item.ID, *budget.WallClock)
		}
		if elapsed > ceiling {
			e.emitBudgetExceeded(item.item.ID, Spend{}, elapsed)
			return true, nil
		}
		return false, nil
	}

	spend, err := e.spend.ItemSpend(e.ctx, item.item.ID)
	if err != nil {
		return false, fmt.Errorf("check item %q budget spend: %w", item.item.ID, err)
	}
	exceeded := budget.Tokens != nil && spend.Tokens > *budget.Tokens ||
		budget.USD != nil && spend.USD > *budget.USD
	if exceeded {
		e.emitBudgetExceeded(item.item.ID, spend, elapsed)
	}
	return exceeded, nil
}

func (e *Engine) effectiveBudget(item *runtimeItem) (*profile.Budget, error) {
	if len(item.item.Budget) > 0 {
		var budget profile.Budget
		if err := decodeJSON(item.item.Budget, &budget); err != nil {
			return nil, fmt.Errorf("decode item %q budget: %w", item.item.ID, err)
		}
		if validation := profile.ValidateBudget(&budget); !validation.Valid() {
			return nil, fmt.Errorf("decode item %q budget: %s", item.item.ID, joinProfileFindings(validation.Findings))
		}
		return &budget, nil
	}

	projectProfile, err := e.profiles.Profile(e.ctx, item.item.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load budget profile for project %q: %w", item.item.ProjectID, err)
	}
	if projectProfile == nil {
		return nil, fmt.Errorf("load budget profile for project %q: nil profile", item.item.ProjectID)
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
