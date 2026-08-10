package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// WorkflowAgentRunBudget is the ceiling in force for one run and where its run
// TREE stands against it. Present only on a run that has one — most runs
// declare no budget, and a field saying "no ceiling" on every one of them would
// be noise on the surface a reader scans for what a run needs.
//
// The numbers are the enforcement's own (`engine.ResolveBudget`), not a second
// aggregation: a status line that could disagree with the check that parks the
// run would be worse than no line, because a reader would trust it.
type WorkflowAgentRunBudget struct {
	// Kind names which ceiling is in force and therefore which pair of fields
	// below carries it: tokens, usd, or wall_clock.
	Kind          string  `json:"kind"`
	CeilingTokens int64   `json:"ceilingTokens,omitempty"`
	CeilingUSD    float64 `json:"ceilingUsd,omitempty"`
	CeilingMillis int64   `json:"ceilingMillis,omitempty"`
	SpentTokens   int64   `json:"spentTokens,omitempty"`
	SpentUSD      float64 `json:"spentUsd,omitempty"`
	ElapsedMillis int64   `json:"elapsedMillis,omitempty"`
	// Percent is spend as a share of the ceiling, rounded. It is NOT clamped:
	// a run parks the first time it goes over, and rounding a breach down to
	// 100 would hide the one state the field exists to make visible.
	Percent int `json:"percent"`
	// Estimated says the dollar figure is not exactly what the providers
	// reported — the rate table priced part of it (Codex reports tokens only),
	// or some rows could not be priced at all. Never set for a token or
	// wall-clock ceiling: both are exact.
	Estimated bool `json:"estimated,omitempty"`
	// UnpricedRows counts ledger rows whose model resolves to no rate, which
	// makes SpentUSD a LOWER BOUND rather than an estimate. The run cannot be
	// judged against a dollar ceiling it has not already crossed and will park
	// at its next phase boundary, so the status surface has to name the reason.
	UnpricedRows int64 `json:"unpricedRows,omitempty"`
	// Exhausted is the ceiling already crossed — true only in the window
	// between the breach and the park it produces, and on a run parked for it.
	Exhausted bool `json:"exhausted,omitempty"`
	// RootItemID is set only when the ceiling belongs to an ANCESTOR: §12
	// enforces the root's budget across the whole tree, so a called run's
	// status has to say whose ceiling it is spending.
	RootItemID string `json:"rootItemId,omitempty"`
}

// workflowRunBudget resolves what a run's budget line says. It answers nil for
// a run under no ceiling, which is not an error — it is the ordinary case.
func (a *App) workflowRunBudget(ctx context.Context, item store.WorkItem) (*WorkflowAgentRunBudget, error) {
	if a.store == nil {
		return nil, fmt.Errorf("workflow run budget: store unavailable")
	}
	root, err := engine.TreeRoot(a.store, item)
	if err != nil {
		return nil, err
	}
	view, err := engine.ResolveBudget(
		ctx,
		workflowProfileSource{store: a.store, configRoot: a.workflowDataRoot()},
		workflowSpendSource{store: a.store},
		root, time.Now(),
	)
	if err != nil || view == nil {
		return nil, err
	}
	budget := &WorkflowAgentRunBudget{
		Kind:      view.Kind,
		Percent:   int(math.Round(view.Fraction() * 100)),
		Exhausted: view.Exceeded != "",
	}
	if root.ID != item.ID {
		budget.RootItemID = root.ID
	}
	switch view.Kind {
	case engine.BudgetKindTokens:
		budget.CeilingTokens = view.CeilingTokens
		budget.SpentTokens = view.Spend.Tokens
	case engine.BudgetKindUSD:
		budget.CeilingUSD = view.CeilingUSD
		budget.SpentUSD = view.Spend.USD
		budget.Estimated = view.Spend.Estimated
		budget.UnpricedRows = view.Spend.Unpriced
	case engine.BudgetKindWallClock:
		budget.CeilingMillis = view.CeilingMillis
		budget.ElapsedMillis = view.ElapsedMillis
	}
	return budget, nil
}
