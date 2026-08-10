package engine

import (
	"context"
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
			e.teardown(item, teardownRequest{
				cause: err, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: ReasonSetupFailed,
			}),
			err,
		)
	}
	if exceeded != "" {
		return true, e.teardown(item, teardownRequest{
			cause: errors.New(exceeded), phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonBudgetExhausted,
		})
	}
	return false, nil
}

// checkBudget reports which ceiling the tree crossed, or "" when it is still
// inside every one it declared. It answers with the numbers rather than a bare
// bool because the park it produces is the one place the record can say which
// ceiling stopped the run and by how much — `budget-exhausted` alone leaves a
// reader guessing between tokens, dollars, and the clock.
//
// The decision is `ResolveBudget`'s, not this method's: the same call answers
// the reserved `{{budget}}` read an element renders and the `run status` line a
// human reads, so what a budget SAYS and what it DOES cannot drift apart.
func (e *Engine) checkBudget(item *runtimeItem) (string, error) {
	view, err := e.budgetView(item)
	if err != nil || view == nil {
		return "", err
	}
	// The one caller that must REFUSE an unjudgeable ceiling rather than report
	// it: continuing here would spend past a bound nobody can evaluate.
	if view.Unjudged != "" {
		return "", errors.New(view.Unjudged)
	}
	if view.Exceeded != "" {
		e.emitBudgetExceeded(item.item.ID, view.Spend, view.Elapsed())
	}
	return view.Exceeded, nil
}

// budgetView resolves the ceiling in force for an item's run tree. A nil view
// with a nil error is a run under no ceiling at all, which is most runs.
func (e *Engine) budgetView(item *runtimeItem) (*BudgetView, error) {
	root, err := e.treeRoot(item)
	if err != nil {
		return nil, err
	}
	return ResolveBudget(e.ctx, e.profiles, e.spend, root, e.now())
}

// BudgetView is one run tree's ceiling and the spend measured against it — the
// numbers the enforcement compares, so every surface that shows a budget shows
// what would actually park the run. Exactly one ceiling is ever in force
// (`profile.ValidateBudget`), named by Kind.
//
// Spend is populated for a token or dollar ceiling only. A wall-clock ceiling
// is measured against the run tree's start alone, so pricing the tree there
// would be two aggregate queries answering a question the ceiling does not ask.
type BudgetView struct {
	// RootItemID is the run the ceiling belongs to and the tree the spend covers
	// — not necessarily the run this view was resolved for (§12).
	RootItemID string `json:"rootItemId"`
	Kind       string `json:"kind"`
	// Exactly one Ceiling* field is set, per Kind.
	CeilingTokens int64   `json:"ceilingTokens,omitempty"`
	CeilingUSD    float64 `json:"ceilingUsd,omitempty"`
	CeilingMillis int64   `json:"ceilingMillis,omitempty"`
	Spend         Spend   `json:"spend"`
	ElapsedMillis int64   `json:"elapsedMillis"`
	// Exceeded is the sentence naming the ceiling the tree crossed and by how
	// much, empty while the run is still inside it. It is what the park's cause
	// carries, so the record says which ceiling stopped the run.
	Exceeded string `json:"exceeded,omitempty"`
	// Unjudged is the sentence naming why a NON-breach verdict cannot be
	// trusted: some rows have no price at all, so the dollar total is a lower
	// bound and the headroom this view shows may not exist.
	//
	// It is a field rather than an error because the two callers need opposite
	// things from it. Enforcement must refuse — running on under a ceiling
	// nobody can evaluate is exactly what a budget exists to prevent — while a
	// READ must still answer: taking `run status` and the `{{budget}}` binding
	// away from an operator is the worst possible response to a model the rate
	// table has not learned yet, and those surfaces are how the operator finds
	// out. `checkBudget` is the one place it becomes an error.
	Unjudged string `json:"unjudged,omitempty"`
}

// Budget kinds. One string vocabulary, shared by the prompt binding, the CLI
// line, and anything else that has to say which ceiling is in force.
const (
	BudgetKindTokens    = "tokens"
	BudgetKindUSD       = "usd"
	BudgetKindWallClock = "wall_clock"
)

// Elapsed is how long the run TREE has been going, from its root's persisted
// start.
func (v BudgetView) Elapsed() time.Duration {
	return time.Duration(v.ElapsedMillis) * time.Millisecond
}

// Fraction is spend as a share of the ceiling, for a caller rendering a percent.
// It is not clamped: a run parks the first time it is over 1, and rounding a
// breach down to "100%" would hide it.
func (v BudgetView) Fraction() float64 {
	switch v.Kind {
	case BudgetKindTokens:
		if v.CeilingTokens > 0 {
			return float64(v.Spend.Tokens) / float64(v.CeilingTokens)
		}
	case BudgetKindUSD:
		if v.CeilingUSD > 0 {
			return v.Spend.USD / v.CeilingUSD
		}
	case BudgetKindWallClock:
		if v.CeilingMillis > 0 {
			return float64(v.ElapsedMillis) / float64(v.CeilingMillis)
		}
	}
	return 0
}

// ResolveBudget answers what ceiling a run tree is under and where it stands
// against it. A nil view with a nil error means no ceiling is in force — the
// item declared none and the project profile declares none either.
//
// `root` must be the tree's ROOT (`TreeRoot`): §12 enforces the root's ceiling
// across every run it called, so resolving against a child would read a budget
// that is not the one in force and a start time that is not the one measured.
func ResolveBudget(
	ctx context.Context, profiles ProfileSource, spend SpendSource,
	root store.WorkItem, now time.Time,
) (*BudgetView, error) {
	budget, err := EffectiveBudget(ctx, profiles, root)
	if err != nil || budget == nil {
		return nil, err
	}

	// Wall clock runs from the ROOT's persisted start, not this run's: a child
	// started an hour into its tree has already spent that hour of the ceiling.
	elapsed := now.Sub(time.UnixMilli(root.StartedAt))
	if elapsed < 0 {
		elapsed = 0
	}
	view := &BudgetView{RootItemID: root.ID, ElapsedMillis: elapsed.Milliseconds()}

	if budget.WallClock != nil {
		ceiling, err := time.ParseDuration(string(*budget.WallClock))
		if err != nil || ceiling <= 0 {
			return nil, fmt.Errorf("check item %q budget: invalid wall_clock %q", root.ID, *budget.WallClock)
		}
		view.Kind = BudgetKindWallClock
		view.CeilingMillis = ceiling.Milliseconds()
		if elapsed > ceiling {
			view.Exceeded = fmt.Sprintf(
				"run tree rooted at %s has been going %s, past its wall_clock budget of %s",
				root.ID, elapsed.Round(time.Second), ceiling,
			)
		}
		return view, nil
	}

	view.Spend, err = spend.TreeSpend(ctx, root.ID)
	if err != nil {
		return nil, fmt.Errorf("check item %q budget spend: %w", root.ID, err)
	}
	switch {
	case budget.Tokens != nil:
		view.Kind = BudgetKindTokens
		view.CeilingTokens = *budget.Tokens
		if view.Spend.Tokens > *budget.Tokens {
			view.Exceeded = fmt.Sprintf(
				"run tree rooted at %s has spent %d tokens, past its budget of %d",
				root.ID, view.Spend.Tokens, *budget.Tokens,
			)
		}
	case budget.USD != nil:
		view.Kind = BudgetKindUSD
		view.CeilingUSD = *budget.USD
		if view.Spend.USD > *budget.USD {
			view.Exceeded = fmt.Sprintf(
				"run tree rooted at %s has spent $%.2f, past its budget of $%.2f",
				root.ID, view.Spend.USD, *budget.USD,
			)
		}
		// A dollar ceiling the tree is INSIDE is only trustworthy if every row
		// could be priced. Tokens are exact whatever the rate table knows, so a
		// token ceiling is unaffected — which is why this sits here and not in
		// the spend source, where it would break token budgets over a model
		// nobody has a rate for yet. A breach already proven by the priced lower
		// bound needs no caveat: unpriced rows can only add to the total.
		if view.Exceeded == "" && view.Spend.Unpriced > 0 {
			view.Unjudged = fmt.Sprintf(
				"check item %q budget: %d usage rows have no USD rate, so spend against a $%.2f ceiling cannot be judged",
				root.ID, view.Spend.Unpriced, *budget.USD,
			)
		}
	default:
		// ValidateBudget requires exactly one ceiling, and both the item column
		// and the profile go through it — so this is a budget that passed
		// validation while declaring nothing, which is a bug in the validator
		// rather than an authored state to tolerate silently.
		return nil, fmt.Errorf("check item %q budget: no ceiling is declared", root.ID)
	}
	return view, nil
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
		root, err := TreeRoot(e.store, item.item)
		if err != nil {
			return store.WorkItem{}, err
		}
		item.rootID = root.ID
	}
	root, err := e.store.GetWorkItem(item.rootID)
	if err != nil {
		return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: %w", item.item.ID, err)
	}
	return root, nil
}

// WorkItemReader is the one read the tree-root walk needs. It is narrower than
// the engine's own persistence so a caller outside this package — the app
// rendering a run's budget — can walk the same linkage without the engine.
type WorkItemReader interface {
	GetWorkItem(id string) (store.WorkItem, error)
}

// TreeRoot walks parent linkage to the run tree's root. It is the one walk:
// every tree-wide fact (the budget envelope, the start a wall clock is measured
// against, the soft-stop request) is read off the row it returns, and a second
// implementation would eventually disagree about which run is the root.
func TreeRoot(items WorkItemReader, item store.WorkItem) (store.WorkItem, error) {
	current := item
	for depth := 0; current.ParentItemID != ""; depth++ {
		if depth > MaxCallDepth {
			return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: tree is deeper than %d", item.ID, MaxCallDepth)
		}
		parent, err := items.GetWorkItem(current.ParentItemID)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("resolve tree root of item %q: %w", item.ID, err)
		}
		current = parent
	}
	return current, nil
}

// EffectiveBudget is the ceiling in force for one run: its own envelope when it
// declares one, the live project profile's per-item default otherwise. Nil with
// a nil error means no ceiling, which is most runs.
func EffectiveBudget(ctx context.Context, profiles ProfileSource, item store.WorkItem) (*profile.Budget, error) {
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

	projectProfile, err := profiles.Profile(ctx, item.ProjectID)
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
