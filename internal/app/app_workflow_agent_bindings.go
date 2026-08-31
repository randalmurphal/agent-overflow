package app

import (
	"agent-overflow/internal/aocli"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/memory"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflow/scheduler"
	"agent-overflow/internal/workflowapp"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
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
		a.workflowProfiles(),
		workflowSpendSource{store: a.store},
		engine.BudgetSubjectOf(root), time.Now(),
	)
	if err != nil {
		return nil, err
	}
	return workflowBudgetLine(view, item.ID), nil
}

// workflowBudgetLine projects a resolved view onto the wire shape, for the run
// the caller is reporting ON — which is not necessarily the run the ceiling
// belongs to (§12). A nil view is a run under no ceiling and answers nil, which
// is the ordinary case rather than an error.
//
// It is shared by the CLI/status surface and the run map so those two cannot
// render the same ceiling differently; the resolution itself is
// `engine.ResolveBudget`'s in both, which is what keeps either from disagreeing
// with the check that actually parks the run.
func workflowBudgetLine(view *engine.BudgetView, forItemID string) *WorkflowAgentRunBudget {
	if view == nil {
		return nil
	}
	budget := &WorkflowAgentRunBudget{
		Kind:      view.Kind,
		Percent:   int(math.Round(view.Fraction() * 100)),
		Exhausted: view.Exceeded != "",
	}
	if view.RootItemID != forItemID {
		budget.RootItemID = view.RootItemID
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
	return budget
}

// Server side of the `ao` execution surface (spec §5, D15/D17).
//
// Every method the CLI can reach is authorized twice. The transport refuses a
// method the token's scope may not name at all (grants → method, see
// internal/transport/scopedtoken.go); this file enforces the part a method name
// cannot express — WHICH runs, automations, and projects the caller may touch,
// and the surface-and-skip ledger that makes a re-entered phase's side effects
// fire once.

// workflowSourceAgent is the run provenance every CLI-started run carries. The
// two kinds are told apart by the source ref, not by a second source value:
// a run an interactive thread asked for has none (its provenance is the thread
// it is bound to), and a run a granted phase started carries its effect key.
const workflowSourceAgent = "agent"

// Effect tool names recorded in work_item_effects. They name the CLI operation,
// not the RPC, because the ledger is what the human reads when a re-entered
// phase reports a skip.
const (
	workflowEffectStartRun = "run-start"
	workflowEffectSchedule = "schedule"
	workflowEffectSetNotes = "notes-set"
)

// phaseSourceRefPrefix is what every run a given (item, phase) started shares.
// It names the phase, deliberately not the attempt: a phase re-entered by
// loop-back or crash recovery must recognise what its earlier attempt started.
// Neither component can contain a slash — an item id is a uuid and a phase id is
// a `def` identifier — so the prefix is unambiguous.
func phaseSourceRefPrefix(scope transport.CallerScope) string {
	return scope.ItemID + "/" + scope.PhaseID + "/"
}

// phaseSourceRef is the full provenance key one phase-started run carries: the
// phase plus the hash of the arguments it was started with. Persisting the
// effect key on the row itself is what makes surface-and-skip survive a crash
// between the run committing and its ledger entry landing — and because
// `idx_work_items_agent_source_ref` is UNIQUE, the database itself refuses a
// second run for the same key rather than trusting the check below to have run.
func phaseSourceRef(scope transport.CallerScope, payloadHash string) string {
	return phaseSourceRefPrefix(scope) + payloadHash
}

// requireCallerScope returns the authenticated scope for a CLI-only method. The
// absence of a scope is not an authorization edge case — it means the method was
// reached from the webview, which has no business calling the agent surface.
func requireCallerScope(ctx context.Context) (transport.CallerScope, error) {
	scope, ok := transport.CallerScopeFrom(ctx)
	if !ok {
		return transport.CallerScope{}, fmt.Errorf(
			"this method is part of the ao CLI surface and requires a session-scoped token")
	}
	if strings.TrimSpace(scope.ProjectID) == "" {
		return transport.CallerScope{}, fmt.Errorf("this session is not attached to a project")
	}
	return scope, nil
}

// authorizeScopedRunAction gates a run-control RPC that both the overlay and
// the CLI reach. A call carrying no caller scope is the user's own, through the
// UI, and passes untouched; a call carrying one is an agent's and is confined
// to what its scope allows.
//
// Phase scope may act only on the runs that (item, phase) started: a phase that
// can start work can also stop it, but nothing else in the project — including
// the run it is itself a phase of.
func (a *App) authorizeScopedRunAction(ctx context.Context, itemID, action string) error {
	scope, ok := transport.CallerScopeFrom(ctx)
	if !ok {
		return nil
	}
	_, err := a.scopedRun(scope, itemID, action, false)
	return err
}

// scopedRun loads a run and refuses it when the scope may not see it. `readOnly`
// widens the phase case to the whole project when the phase holds `introspect`:
// reading run state across the project is what introspection is, while acting on
// a run stays limited to the ones this phase started.
//
// It reads the SUMMARY projection. Authorization itself needs four columns —
// project, source, source ref, id — and every verb on this surface calls this
// first, several of them per transition of a run a supervising agent is
// watching; the full row would decode one frozen workflow snapshot each time to
// answer a question no snapshot bears on. The two verbs that DO need a blob
// (`run status`/`run inspect` for seeds and the run's own budget envelope,
// `run output` for the declared-output map) read the full row for themselves,
// so the cost lands on the callers that incur it rather than on all of them.
func (a *App) scopedRun(scope transport.CallerScope, itemID, action string, readOnly bool) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("%s: run id is required", action)
	}
	item, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if item.ProjectID != scope.ProjectID {
		return store.WorkItem{}, fmt.Errorf("%s: run %s belongs to another project", action, itemID)
	}
	if !scope.IsPhase() {
		return item, nil
	}
	if readOnly && scope.HasGrant(string(def.GrantIntrospect)) {
		return item, nil
	}
	if item.Source == workflowSourceAgent && strings.HasPrefix(item.SourceRef, phaseSourceRefPrefix(scope)) {
		return item, nil
	}
	return store.WorkItem{}, fmt.Errorf(
		"%s: run %s was not started by phase %q of run %s; this phase may only act on the runs it started",
		action, itemID, scope.PhaseID, scope.ItemID)
}

// scopedAutomation loads an automation and refuses one outside the scope's
// project. Both kinds are project-confined: an interactive thread drives the
// project it is open in, and a phase the project its run belongs to.
func (a *App) scopedAutomation(scope transport.CallerScope, automationID, action string) (store.Automation, error) {
	automationID = strings.TrimSpace(automationID)
	if automationID == "" {
		return store.Automation{}, fmt.Errorf("%s: automation id is required", action)
	}
	automation, err := a.store.GetAutomation(automationID)
	if err != nil {
		return store.Automation{}, err
	}
	if automation.ProjectID != scope.ProjectID {
		return store.Automation{}, fmt.Errorf("%s: automation %s belongs to another project", action, automationID)
	}
	return automation, nil
}

// workflowEffect is the payload persisted per (item, phase, tool, hash). It
// carries the arguments so a human reading the ledger can see what was asked
// for, and the result so a skipped replay can answer with the original ids
// instead of a bare "already done".
type workflowEffect struct {
	Args   map[string]any `json:"args"`
	Result map[string]any `json:"result"`
}

// effectPayloadHash is the surface-and-skip identity of one side effect: the
// SHA-256 of the canonical JSON encoding of the request's arguments. Canonical
// means every value has been decoded and re-encoded by encoding/json, which
// sorts object keys — so two invocations that differ only in seed ordering or
// whitespace hash the same, and any real difference in what was asked for does
// not.
func effectPayloadHash(args map[string]any) (string, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("hash effect arguments: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// priorEffect reports what this (item, phase) already did with these exact
// arguments. A hit is the surface-and-skip answer: do NOT re-fire, return the
// original result. Only phase scope has an effect ledger — an interactive
// invocation is a human-approved bash call, and replaying one is the human's
// decision, not the system's.
func (a *App) priorEffect(scope transport.CallerScope, tool, hash string) (workflowEffect, bool, error) {
	if !scope.IsPhase() {
		return workflowEffect{}, false, nil
	}
	stored, found, err := a.store.GetWorkItemEffect(scope.ItemID, scope.PhaseID, tool, hash)
	if err != nil {
		return workflowEffect{}, false, err
	}
	if !found {
		return workflowEffect{}, false, nil
	}
	var effect workflowEffect
	if len(stored.Payload) > 0 {
		if err := json.Unmarshal(stored.Payload, &effect); err != nil {
			return workflowEffect{}, false, fmt.Errorf(
				"recorded effect %s/%s/%s is unreadable: %w", scope.ItemID, scope.PhaseID, tool, err)
		}
	}
	return effect, true, nil
}

// recordEffect persists what a phase just did, after it succeeded. Recording
// before would let a failed start block the retry that should follow it.
func (a *App) recordEffect(scope transport.CallerScope, tool, hash string, effect workflowEffect) error {
	if !scope.IsPhase() {
		return nil
	}
	payload, err := json.Marshal(effect)
	if err != nil {
		return fmt.Errorf("encode recorded effect: %w", err)
	}
	return a.store.RecordWorkItemEffect(store.WorkItemEffect{
		ItemID: scope.ItemID, PhaseID: scope.PhaseID, Tool: tool, PayloadHash: hash,
		Payload: payload, CreatedAt: time.Now().UnixMilli(),
	})
}

// bindOriginThread implements D17's agent-started binding: a run an interactive
// thread asked for reports back into that thread. A thread that cannot legally
// hold the binding (wrong project, a mode that is not a conversation) leaves the
// run unbound and returns the reason — the run is already started and refusing
// it retroactively would be worse than surfacing it in the overlay.
func (a *App) bindOriginThread(scope transport.CallerScope, item store.WorkItem) string {
	if scope.IsPhase() {
		// Phase-started runs are decomposition, not conversation: their results
		// belong to the overlay and to whatever the starting phase does next.
		return ""
	}
	thread, err := a.store.GetThread(scope.ThreadID)
	if err != nil {
		return fmt.Sprintf("run started unbound: %v", err)
	}
	if thread.Archived {
		return fmt.Sprintf("run started unbound: thread %s is archived", thread.ID)
	}
	if err := workflowapp.ValidateBindingThread(item, thread); err != nil {
		return fmt.Sprintf("run started unbound: %v", err)
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		return fmt.Sprintf("run started unbound: %v", err)
	}
	return ""
}

// `agent-overflow run amend --seed k=v` (D53): changing a resting run's inputs
// without throwing the run away.
//
// It exists because the alternative was priced: one seed value wrong on a live
// campaign cost a cancel, a respawn, and $14 of tokens, because no verb could
// change a seed and the repair pattern that works for a prompt (edit the file,
// `run resume --refresh-def`, D50) had no equivalent for an input.
//
// The engine owns every rule — which states may be amended, which names are
// amendable, and when the run will read the new value — because all three are
// facts about the run's own frozen definition and its place in the FSM. This
// file adds what the engine has no business knowing: who is allowed to ask, and
// what a caller has to be told about a run that is not the root of its tree.

// WorkflowAgentAmendSeedsInput is `run amend`. Seeds is a partial object: only
// the named keys change, every other seed the run froze is left alone. Clearing
// a seed is deliberately not expressible — an absent optional input and one
// explicitly set to null are different runs, and "remove this key" is a verb
// nobody has asked for.
type WorkflowAgentAmendSeedsInput struct {
	ItemID string          `json:"itemId"`
	Seeds  json.RawMessage `json:"seeds"`
}

// WorkflowAgentAmendSeedsResult is what the amendment did. Effect is the
// engine's own answer to "when is this read", and AppliesNote states it in the
// words the operator needs; CallerNote is present only for a called run.
type WorkflowAgentAmendSeedsResult struct {
	ItemID  string          `json:"itemId"`
	Names   []string        `json:"names"`
	Seeds   json.RawMessage `json:"seeds"`
	PhaseID string          `json:"phaseId,omitempty"`
	Effect  string          `json:"effect"`
	// AppliesNote is one sentence saying when the run reads the new values and,
	// where it matters, which verb makes it read them sooner. It is composed here
	// rather than by the CLI because the answer depends on the run record, and a
	// caller that had to derive it would be deriving the engine's dispatch.
	AppliesNote string `json:"appliesNote"`
	// CallerNote is set when the amended run was CALLED by another. Its seeds are
	// its own — its remaining phases read this row — but the next run its caller
	// invokes re-evaluates the caller's `args:` and will not carry this change.
	CallerNote string `json:"callerNote,omitempty"`
}

// WorkflowAgentAmendSeeds changes seed values on a run that is resting.
//
// LocalOnly: it mutates what autonomous provider sessions will be told to do,
// which is the same authority as resuming the run.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentAmendSeeds(ctx context.Context, input WorkflowAgentAmendSeedsInput) (WorkflowAgentAmendSeedsResult, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	if err := a.authorizeScopedRunAction(ctx, input.ItemID, "amend workflow run seeds"); err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	values, _, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, fmt.Errorf("amend workflow run seeds: %w", err)
	}
	amendment, err := workflowEngine.AmendSeeds(input.ItemID, values)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	result := WorkflowAgentAmendSeedsResult{
		ItemID: amendment.ItemID, Names: amendment.Names, Seeds: amendment.Seeds,
		PhaseID: amendment.PhaseID, Effect: string(amendment.Effect),
		AppliesNote: workflowAmendmentNote(amendment),
	}
	// The row is re-read rather than trusted from before the write: the caller
	// note is about linkage, which the amendment does not carry, and a run that
	// was called is the one case where "amend the root instead" is the more
	// useful half of the answer.
	item, err := a.store.GetWorkItem(amendment.ItemID)
	if err != nil {
		return WorkflowAgentAmendSeedsResult{}, err
	}
	if item.ParentItemID != "" {
		chain, err := a.workflowApplication().Ancestry(item)
		if err != nil {
			return WorkflowAgentAmendSeedsResult{}, err
		}
		result.CallerNote = fmt.Sprintf(
			"this run was called by %s (root %s), so the change reaches its own remaining phases only; the next run %s starts re-evaluates its call arguments and will not carry it — amend %s to change what later waves are given",
			item.ParentItemID, chain[0].ID, item.ParentItemID, chain[0].ID)
	}
	return result, nil
}

// workflowAmendmentNote states when the run reads what was just written, in the
// terms the operator's next command is expressed in.
//
// Both answers are true statements about the same mechanism: seeds live on the
// run row and the variable context is rebuilt from it whenever a phase attempt
// starts. What differs is whether a bare resume of THIS park starts one — a
// fan-out or a call phase is repaired in place and runs on the variables its
// attempt persisted, so its operator needs to know that re-entering the phase is
// what makes the new value take.
func workflowAmendmentNote(amendment engine.SeedAmendment) string {
	if amendment.Effect == engine.SeedEffectFreshEntry {
		return fmt.Sprintf(
			"the parked attempt of phase %q is repaired in place by a bare resume and keeps the values it froze; the new values are read at the next FRESH phase entry — `agent-overflow run resume %s --phase %s` enters one now, and the run's next phase does so on its own",
			amendment.PhaseID, amendment.ItemID, amendment.PhaseID)
	}
	return "the next attempt this run starts renders the new values; if it continues a provider session that already ran with the old ones, say so in the resume — that session's earlier turns are unchanged"
}

// `agent-overflow run guide <run-id> "<text>"` — steering a run without parking
// it.
//
// It is the thread→run direction of `notify:` (D54): a `notify:` route tells the
// watching thread what a run just decided, and this tells the run what the
// watcher wants next. Before it existed, the only way to redirect a free-running
// campaign was `run pause` → edit → `run resume`, which costs the turn in flight
// and, for a wave, the coordination of every unit under it.
//
// The engine owns every rule — which states may be guided, how many entries fit,
// which phase entry consumes them, and the author stamp. This file adds what the
// engine has no business knowing: who is allowed to ask, whose name goes on the
// entry, and what a caller has to be told about a run that is not the root of
// its tree.

// WorkflowAgentGuideRunInput is `run guide`. One entry per call: the slot is a
// list of instructions, and merging two calls into one entry would lose the
// order and the times they were left at.
type WorkflowAgentGuideRunInput struct {
	ItemID string `json:"itemId"`
	Text   string `json:"text"`
}

// WorkflowAgentGuideRunResult is what is now waiting for the run, and when the
// run will read it. Entries are not echoed back: the caller wrote the newest one
// and the older ones are the run's business, while `run inspect` is the read
// surface for the whole slot.
type WorkflowAgentGuideRunResult struct {
	ItemID string `json:"itemId"`
	// Pending is the slot depth after this entry, against MaxGuidanceEntries.
	Pending    int `json:"pending"`
	MaxPending int `json:"maxPending"`
	// By is the author the ENGINE stamped, echoed so a caller can see that the
	// attribution is not the one it typed — it is the one its credential earned.
	By      string `json:"by"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	PhaseID string `json:"phaseId,omitempty"`
	// DeliversNote is one sentence saying when the run reads this, in the terms
	// the operator's next command is expressed in. It is composed here rather
	// than by the CLI because the answer depends on where the run is resting.
	DeliversNote string `json:"deliversNote"`
	// CallerNote is set when the guided run was CALLED by another. The entry
	// reaches this run's own remaining phases and nothing else.
	CallerNote string `json:"callerNote,omitempty"`
	// QuarantineNote is set when this append landed on a slot the engine had to
	// heal: whatever was pending would not decode, so it was written to the
	// engine log and discarded. The call SUCCEEDED — the caller's entry is
	// pending — which is exactly why the fact has to travel on the result rather
	// than as an error nobody would see.
	QuarantineNote string `json:"quarantineNote,omitempty"`
}

// WorkflowAgentGuideRun leaves one instruction for a run's next phase entry.
//
// LocalOnly: it changes what an autonomous provider session will be told to do,
// which is the same authority as amending its seeds or resuming it.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentGuideRun(ctx context.Context, input WorkflowAgentGuideRunInput) (WorkflowAgentGuideRunResult, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	if err := a.authorizeScopedRunAction(ctx, input.ItemID, "guide workflow run"); err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	state, err := workflowEngine.Guide(input.ItemID, guidanceDraftFor(ctx, input.Text))
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	result := WorkflowAgentGuideRunResult{
		ItemID: state.ItemID, Pending: len(state.Pending), MaxPending: engine.MaxGuidanceEntries,
		State: string(state.State), Reason: string(state.Reason), PhaseID: state.PhaseID,
		DeliversNote:   workflowGuidanceNote(state),
		QuarantineNote: workflowGuidanceQuarantineNote(state.Quarantined),
	}
	if len(state.Pending) > 0 {
		result.By = string(state.Pending[len(state.Pending)-1].By)
	}
	// The row is re-read rather than trusted from before the write, for the same
	// reason `run amend` re-reads it: the caller note is about linkage, which the
	// guidance state does not carry.
	item, err := a.store.GetWorkItem(state.ItemID)
	if err != nil {
		return WorkflowAgentGuideRunResult{}, err
	}
	if item.ParentItemID != "" {
		chain, err := a.workflowApplication().Ancestry(item)
		if err != nil {
			return WorkflowAgentGuideRunResult{}, err
		}
		result.CallerNote = fmt.Sprintf(
			"this run was called by %s (root %s), so the guidance reaches its own remaining phases only; the next run %s starts is a different run and will not see it — guide that run when it exists, or %s to steer the caller's own phases",
			item.ParentItemID, chain[0].ID, item.ParentItemID, chain[0].ID)
	}
	return result, nil
}

// guidanceDraftFor stamps the author from the AUTHENTICATED caller, never from
// the request. A scoped phase session is an agent, an interactive session is the
// person driving it, and the distinction is the whole value of the label: an
// entry that could claim "a human said this" would make the attribution in the
// delivered prompt worth nothing.
func guidanceDraftFor(ctx context.Context, text string) engine.GuidanceDraft {
	draft := engine.GuidanceDraft{Text: text, By: engine.GuidanceByHuman}
	if scope, ok := transport.CallerScopeFrom(ctx); ok && scope.IsPhase() {
		draft.By, draft.ByRun = engine.GuidanceByPhase, scope.ItemID
	}
	return draft
}

// workflowGuidanceNote states when the run reads what was just left, which is a
// different sentence for each place a run can be resting.
//
// The trap it exists to close is the continuable park: a bare `run resume` of a
// `paused`, `interrupted`, `checkpoint`, `unit-failed`,
// `provider-retries-exhausted`, or legacy `retries-exhausted`
// park CONTINUES the attempt that parked instead of entering a phase, and a
// continuation is not a delivery boundary — the guidance would sit pending
// through a resume the operator reasonably expected to consume it. The verb that
// does enter a phase is named instead of left to be inferred. The sentence reads
// `engine.ContinuableReason` rather than restating the set, so a new member
// changes what this says without an edit here.
func workflowGuidanceNote(state engine.GuidanceState) string {
	switch {
	case state.State == engine.StateRunning:
		return fmt.Sprintf(
			"the run is working; this is delivered at its next FRESH phase entry, which is the next phase it advances or loops into — the turn in flight%s is never interrupted",
			workflowGuidancePhaseClause(state.PhaseID))
	case engine.ContinuableReason(state.Reason):
		return fmt.Sprintf(
			"the run is parked %s, which `agent-overflow run resume %s` CONTINUES rather than re-enters — a continuation is not a delivery boundary, so this is read at the next fresh phase entry after that, or immediately by `agent-overflow run resume %s --phase <id>`, which starts a phase over",
			state.Reason, state.ItemID, state.ItemID)
	default:
		// Which verb settles this park is the repair map's answer, not this
		// method's — a `gate` park takes `run resolve`, a `stuck` one takes `run
		// resume`, and naming the wrong one here would be worse than naming none.
		return fmt.Sprintf(
			"the run is parked %s%s; this is delivered at the fresh phase entry the verb that settles that park produces",
			state.Reason, workflowGuidancePhaseClause(state.PhaseID))
	}
}

// workflowGuidanceQuarantineNote says what this append cost, when it cost
// anything. The engine hands back facts (how big the discarded column was, why
// it would not decode, which log event holds it); the sentence is composed here
// for the same reason `workflowGuidanceNote` is — the caller reads prose, not a
// struct, and the CLI's job is to print what the app says rather than to know
// what a quarantine means.
//
// It names what the operator has to DO, because the discard is not repairable
// from the record: whatever was pending is in the log and not in the run, so any
// earlier steer that has not been delivered has to be left again.
func workflowGuidanceQuarantineNote(quarantine *engine.GuidanceQuarantine) string {
	if quarantine == nil {
		return ""
	}
	return fmt.Sprintf(
		"the guidance already pending on this run (%d bytes) could not be decoded (%s), so it was written to the engine log as %q and the slot was cleared before your entry was added; your entry is safe and is the only one pending, but any earlier steer that had not been delivered is gone — re-issue it",
		quarantine.Bytes, quarantine.Reason, quarantine.LogEvent)
}

func workflowGuidancePhaseClause(phaseID string) string {
	if phaseID == "" {
		return ""
	}
	return fmt.Sprintf(" (phase %q)", phaseID)
}

// WorkflowAgentGuidanceEntry is one pending entry as `run inspect` reports it.
// The text is bounded — the slot holds several kilobytes per entry and an
// inspection is read by an agent paying per byte — and the age is computed here
// rather than left as a timestamp for the reader to subtract, because "left four
// hours ago and still not delivered" is the fact the field exists for.
type WorkflowAgentGuidanceEntry struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
	// AgeSeconds is how long this entry has been waiting for a phase entry. It
	// can legitimately be zero (an entry left this second) and is never negative:
	// a clock that moved backwards clamps rather than reporting the future.
	AgeSeconds int64 `json:"ageSeconds"`
	// By is the author the engine stamped, and ByRun the run whose phase left it.
	By    string `json:"by"`
	ByRun string `json:"byRun,omitempty"`
}

// `agent-overflow run inspect` — the whole picture of one run in one call.
//
// Everything here was already persisted and already readable; what was missing
// was a read that returns it together. An agent supervising a campaign asked
// "which worktree, which branch, which seeds, which children, what did the
// latest attempt output" before every gate decision, and answered it with raw
// SQL against the production database because no verb exposed any of it. The
// projection stays narrow for the same reason `run status` does — an agent's
// context window pays for every byte — so envelope outputs arrive as a bounded
// digest until a caller names the attempt it actually wants.

// WorkflowAgentInspectInput is `run inspect`. PhaseID selects a single attempt
// to read whole; Attempt narrows that to one try and is meaningless without it.
type WorkflowAgentInspectInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// WorkflowAgentRunInspection is what `run inspect` returns. Run is exactly the
// `run status` document, so a reader that already parses one parses this; the
// rest is what a run record carries and that projection deliberately omits.
type WorkflowAgentRunInspection struct {
	Run WorkflowAgentRunView `json:"run"`
	// WorktreePath, Branch, and BaseBranch are where the run's work actually
	// happens. Nothing else on the CLI surface names them, and a supervising
	// agent cannot inspect a diff, a log, or a commit without them.
	WorktreePath string `json:"worktreePath,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseBranch   string `json:"baseBranch,omitempty"`
	// Children are the runs this run called, oldest first. They are NOT bounded:
	// a campaign's waves are its children, so eliding them would truncate the
	// answer to the question that is being asked — and one row per child is the
	// same cost `run list` already pays per run.
	Children []WorkflowAgentChildRun `json:"children"`
	// Guidance is what `run guide` left and the run has not reached a phase entry
	// to consume yet, oldest first. It is RUN-level rather than phase-level
	// because the slot is: an entry is delivered at whichever phase the run
	// enters next, not at one it is resting in. Absent when nothing is pending.
	Guidance []WorkflowAgentGuidanceEntry `json:"guidance,omitempty"`
	// Phase is present only when the caller named one, and carries that attempt
	// read whole.
	Phase *WorkflowAgentPhaseDetail `json:"phase,omitempty"`
}

// WorkflowAgentChildRun is one run this run called. The parent coordinate rides
// along because it is what tells two children apart: a call phase re-entered by
// a loop makes one child per attempt, and a fan-out makes one per unit.
type WorkflowAgentChildRun struct {
	ItemID        string `json:"itemId"`
	WorkflowID    string `json:"workflowId,omitempty"`
	Goal          string `json:"goal,omitempty"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	ParentPhaseID string `json:"parentPhaseId,omitempty"`
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
}

// WorkflowAgentPhaseDetail is one phase attempt read whole: the outputs its
// envelope declared, how its gate decided, and the units it expanded. Outputs
// are the full values rather than the digest — naming an attempt is how a caller
// says the digest was not enough — bounded only by the envelope size cap every
// envelope was accepted under.
type WorkflowAgentPhaseDetail struct {
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// Provider, Model, and Effort mirror the attempt line's, so a drill-down is
	// readable without pairing it back up with the attempt it came from.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Cause mirrors the attempt line's: why the ENGINE parked this attempt.
	Cause string `json:"cause,omitempty"`
	// Outputs is empty for an attempt that produced no envelope, and for one
	// that rested on a question or a stuck reason: neither declares outputs.
	Outputs        map[string]json.RawMessage `json:"outputs,omitempty"`
	Decision       string                     `json:"decision,omitempty"`
	DecisionTarget string                     `json:"decisionTarget,omitempty"`
	ExhaustedLoops []string                   `json:"exhaustedLoops,omitempty"`
	// Units is empty for an attempt that is not a fan-out.
	Units []WorkflowAgentUnitView `json:"units"`
}

// WorkflowAgentUnitView is one fan-out unit (or the join) of the inspected
// attempt. Branch and worktree are on it for the same reason they are on the
// run: they are where that unit's work is, and nothing else names them.
type WorkflowAgentUnitView struct {
	UnitID      string `json:"unitId"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	UnitAttempt int    `json:"unitAttempt"`
	// Note is the note the unit row carries: for a settled unit, how it ended;
	// for one a repair reopened, what that repair told its next try. It is here
	// because `failed` alone is ambiguous — a pause tears its in-flight units
	// down `failed` with an interrupted note, since there is no interrupted unit
	// status and `failed` is what the repair verbs recover — so a drill-down
	// without it reports the operator's own pause as a wave of agent failures.
	Note         string `json:"note,omitempty"`
	Branch       string `json:"branch,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	ThreadID     string `json:"threadId,omitempty"`
}

// WorkflowAgentInspectRun is `agent-overflow run inspect`.
//
// LocalOnly: see WorkflowAgentRunStatus. It additionally names local worktree
// paths, which is a fact about this machine's filesystem.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentInspectRun(ctx context.Context, input WorkflowAgentInspectInput) (WorkflowAgentRunInspection, error) {
	inspection, err := a.workflowApplication().InspectRun(ctx, workflowapp.InspectInput{
		ItemID: input.ItemID, PhaseID: input.PhaseID, Attempt: input.Attempt,
	})
	if err != nil {
		return WorkflowAgentRunInspection{}, err
	}
	return projectWorkflowInspection(inspection), nil
}

// `agent-overflow memory add` / `memory list` — the CLI half of campaign memory
// (Packet L).
//
// Neither verb takes a grant. Recording what the work learned is part of doing
// the work, exactly as returning an envelope is: a phase that can run at all can
// say what it found out, and a `grants:` line standing between an element and
// its own campaign's memory would mean every workflow that forgot one silently
// relearns everything each wave. The AUTHORITY that does apply is row-level and
// enforced here: a phase writes into the tree of the run it is a phase of, and
// into no other.
//
// These are LocalOnly for the same reason `run narrative` is — they read and
// write a file under this machine's app-managed config root.

// WorkflowAgentMemoryInput is one recorded note. There is deliberately no
// provenance or timestamp field: those are the system's answer to "who wrote
// this and when", and a shape that could carry a supplied one is a shape that
// could be lied to.
type WorkflowAgentMemoryInput struct {
	// ItemID names the run to attribute the note to. Optional for a phase
	// session, which is already a phase of one; required for an interactive
	// session, which is not.
	ItemID string   `json:"itemId,omitempty"`
	Kind   string   `json:"kind"`
	Text   string   `json:"text"`
	Files  []string `json:"files,omitempty"`
}

// WorkflowAgentMemoryResult reports where the note landed. The path is returned
// so a caller can read the log it just wrote to without being told the layout.
type WorkflowAgentMemoryResult struct {
	ItemID string `json:"itemId"`
	RootID string `json:"rootId"`
	Kind   string `json:"kind"`
	Wave   int    `json:"wave"`
	Path   string `json:"path"`
}

// WorkflowAgentMemoryListInput selects what a read returns.
type WorkflowAgentMemoryListInput struct {
	ItemID string `json:"itemId,omitempty"`
	// Kind narrows to one of the four; empty returns every kind.
	Kind string `json:"kind,omitempty"`
}

// WorkflowAgentMemoryLog is one tree's notes, oldest last — the order they were
// written, which is the order the log holds them in.
type WorkflowAgentMemoryLog struct {
	ItemID string        `json:"itemId"`
	RootID string        `json:"rootId"`
	Path   string        `json:"path"`
	Notes  []memory.Note `json:"notes"`
	// Total is how many notes the tree holds before Kind narrowed them, so a
	// filtered read still states the size of what it read from.
	Total int `json:"total"`
	// Skipped counts lines the log holds that are not readable notes — a torn
	// final line from a crash. Reported rather than hidden: a reader deciding
	// whether the memory is complete needs to know one was lost.
	Skipped int `json:"skipped"`
}

// WorkflowAgentAddMemory is `agent-overflow memory add`.
//
// LocalOnly: it appends to a file under this machine's app-managed config root.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentAddMemory(ctx context.Context, input WorkflowAgentMemoryInput) (WorkflowAgentMemoryResult, error) {
	result, err := a.workflowApplication().AddMemory(ctx, workflowapp.MemoryInput{
		ItemID: input.ItemID, Kind: input.Kind, Text: input.Text, Files: input.Files,
	})
	if err != nil {
		return WorkflowAgentMemoryResult{}, err
	}
	return WorkflowAgentMemoryResult{
		ItemID: result.ItemID, RootID: result.RootID, Kind: result.Kind, Wave: result.Wave, Path: result.Path,
	}, nil
}

// WorkflowAgentListMemory is `agent-overflow memory list`.
//
// LocalOnly: it reads a file under this machine's app-managed config root.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentListMemory(ctx context.Context, input WorkflowAgentMemoryListInput) (WorkflowAgentMemoryLog, error) {
	result, err := a.workflowApplication().ListMemory(ctx, workflowapp.MemoryListInput{
		ItemID: input.ItemID, Kind: input.Kind,
	})
	if err != nil {
		return WorkflowAgentMemoryLog{}, err
	}
	return projectWorkflowMemoryLog(result), nil
}

// `agent-overflow run narrative` — the per-attempt account, by coordinate
// rather than by path.
//
// The narrative is the one human-readable record of what an element did, and it
// is the file a supervising agent reads most: the campaign this verb came out of
// opened twenty-seven of them, every one by hand-assembling
// `workflow-runs/<id>/<phase>.<n>/units/<unit>.<n>/narrative.md` and discovering
// the shape by trial. The path shape is ours, so naming it is our job.

// maxWorkflowNarrativeBytes bounds what one read returns. A narrative is
// model-authored prose with no ceiling of its own, and this answer lands in a
// reader's context window; a file past the cap is reported truncated with its
// real size, so the reader can decide to open it directly rather than being told
// a partial account is the whole one.
const maxWorkflowNarrativeBytes = 64 * 1024

// WorkflowAgentNarrativeInput names one attempt's account. Attempt defaults to
// the latest attempt of the phase, which is the one a parked run is resting on;
// UnitID selects a fan-out unit's account instead of the phase's, on the unit's
// current try — the try is the unit row's, not the caller's to guess.
type WorkflowAgentNarrativeInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt,omitempty"`
	UnitID  string `json:"unitId,omitempty"`
}

// WorkflowAgentNarrative is one resolved account. Path is populated whether or
// not the file exists: an absent narrative is an answer, and the answer has to
// say what was looked for.
type WorkflowAgentNarrative struct {
	ItemID      string `json:"itemId"`
	PhaseID     string `json:"phaseId"`
	Attempt     int    `json:"attempt"`
	UnitID      string `json:"unitId,omitempty"`
	UnitAttempt int    `json:"unitAttempt,omitempty"`
	Path        string `json:"path"`
	Present     bool   `json:"present"`
	// Bytes is the file's real size, so a truncated read still reports how much
	// account there is.
	Bytes     int64  `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Content   string `json:"content,omitempty"`
}

// WorkflowAgentRunNarrative is `agent-overflow run narrative`.
//
// LocalOnly: it reads a file out of this machine's app-managed run directory,
// like every other local-filesystem read on the agent surface.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentRunNarrative(ctx context.Context, input WorkflowAgentNarrativeInput) (WorkflowAgentNarrative, error) {
	narrative, err := a.workflowApplication().RunNarrative(ctx, workflowapp.NarrativeInput{
		ItemID: input.ItemID, PhaseID: input.PhaseID, Attempt: input.Attempt, UnitID: input.UnitID,
	})
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	return projectWorkflowNarrative(narrative), nil
}

// The per-attempt provenance `ao run status` carries (D38). A campaign agent
// reading a parked run has two questions the run row cannot answer: which
// attempt produced the outputs the gate consumed, and what each element
// actually ran with. Both are already persisted — the attempt rows carry
// status and gate trace, and the thread the attempt ran on carries the
// resolved provider/model/effort — so this is a projection, not new state.

// WorkflowAgentPhaseAttempt is one attempt of one phase. It is deliberately
// narrower than the phase row: no envelopes, no predicate trace, no narrative
// path. The decision fields are the gate's OUTCOME — which way the run went and
// which loop budgets it had spent — because that is what a reader deciding
// between `run resolve`, `run resume --phase`, and `run rerun` needs; the
// predicates that produced it are a debugging read, and they live in the app.
type WorkflowAgentPhaseAttempt struct {
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// Provider, Model, and Effort are the settings the attempt's thread was
	// created with, empty for an attempt that has no thread — a tool-driver
	// phase runs a command, not a provider session.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Cause is why the ENGINE parked this attempt, in its own words — a
	// worktree that could not be cut, a phase missing from the snapshot, a
	// budget that ran out. It is empty for every attempt that rested on its
	// own envelope, and for the reasons that name their own cause
	// (`interrupted`, `paused`, `taken-over`). It is never model output.
	Cause string `json:"cause,omitempty"`
	// Session is "continued" when this attempt ran as the next turn of a session
	// an EARLIER attempt of the same phase started, and empty otherwise — which
	// is the same fact the two rows' shared thread id is, named so a reader does
	// not have to compare thread ids to see it. Three things produce it and they
	// are deliberately not distinguished: a loop route declaring
	// `session: continue`, an answered question, and a finalized takeover. All
	// three mean the same thing to anyone reading the run — this round remembers
	// the last one — and the definition says which edge asked for it.
	Session string `json:"session,omitempty"`
	// Decision, DecisionTarget, and ExhaustedLoops are absent until the attempt's
	// gate has been evaluated and persisted.
	Decision       string   `json:"decision,omitempty"`
	DecisionTarget string   `json:"decisionTarget,omitempty"`
	ExhaustedLoops []string `json:"exhaustedLoops,omitempty"`
	// Outputs is a bounded digest of the attempt's envelope outputs and
	// OutputOverflow how many it left out. Only `run inspect` populates them, and
	// only for the LATEST attempt of each phase: the digest exists so a reader
	// deciding a gate does not have to open every attempt, and `run inspect
	// --phase` returns that attempt's outputs whole instead. `run status` carries
	// neither — its projection is deliberately envelope-free.
	Outputs        []WorkflowAgentOutputDigest `json:"outputs,omitempty"`
	OutputOverflow int                         `json:"outputOverflow,omitempty"`
}

// WorkflowAgentOutputDigest is one envelope output rendered small enough that a
// whole run's worth fits in one read. The value is the output's text when it is
// a JSON string and its compact JSON otherwise, rune-capped with the shared
// truncation marker — a reader that needs the untruncated value asks for the
// attempt.
type WorkflowAgentOutputDigest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Digest budgets. A run's inspection names every phase attempt, so the per-
// attempt digest has to stay small enough that a twenty-attempt run is still one
// readable answer.
const (
	maxDigestOutputs    = 8
	maxDigestValueRunes = 200
)

// workflowSessionContinued is the root wire vocabulary asserted by App-level
// integration tests; workflowapp owns the persisted projection that sets it.
const workflowSessionContinued = "continued"

// workflowAttemptOutputs decodes one attempt's envelope outputs. An attempt with
// no envelope, or one that rested on a question or a stuck reason, simply has
// none — that is an answer rather than an error. The size assertion is the read
// side of the contract every envelope was accepted under: a record past the cap
// is corrupt, and shipping it into an agent's context window unremarked is the
// one thing worse than refusing it.
func workflowAttemptOutputs(itemID, phaseID string, attempt int, payload json.RawMessage) (map[string]json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	if len(payload) > def.DefaultEnvelopeSizeCap {
		return nil, fmt.Errorf(
			"workflow run %s: envelope for %s attempt %d is %d bytes; maximum is %d",
			itemID, phaseID, attempt, len(payload), def.DefaultEnvelopeSizeCap)
	}
	var envelope struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf(
			"workflow run %s: envelope for %s attempt %d is unreadable: %w", itemID, phaseID, attempt, err)
	}
	return envelope.Outputs, nil
}

// workflowOutputDigest projects an attempt's outputs into the bounded form, in a
// stable order, and reports how many it left out. The overflow count is returned
// rather than silently dropped: a digest that hides its own truncation is how a
// reader concludes an output does not exist.
func workflowOutputDigest(outputs map[string]json.RawMessage) ([]WorkflowAgentOutputDigest, int) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := make([]WorkflowAgentOutputDigest, 0, min(len(names), maxDigestOutputs))
	for index, name := range names {
		if index == maxDigestOutputs {
			return digest, len(names) - index
		}
		digest = append(digest, WorkflowAgentOutputDigest{
			Name:  name,
			Value: untrustedtext.Truncate(workflowOutputText(outputs[name]), maxDigestValueRunes),
		})
	}
	return digest, 0
}

// workflowOutputText renders one output value for a line. A JSON string becomes
// its text — the caller quotes it as untrusted data when it renders it, and a
// value that arrived already quoted would reach the reader double-quoted —
// while every other shape keeps its JSON, compacted so one value stays one line.
func workflowOutputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

// workflowAttachLatestDigests stamps each phase's LATEST attempt with the digest
// of its envelope outputs. Earlier attempts are left bare: they were superseded,
// and a run that retried a phase five times would otherwise answer with five
// digests of which only one is current.
func workflowAttachLatestDigests(
	itemID string, attempts []WorkflowAgentPhaseAttempt, timeline []store.WorkItemPhaseTimeline,
) error {
	latest := make(map[string]int, len(attempts))
	for _, attempt := range attempts {
		if attempt.Attempt > latest[attempt.PhaseID] {
			latest[attempt.PhaseID] = attempt.Attempt
		}
	}
	envelopes := make(map[string]json.RawMessage, len(timeline))
	for _, phase := range timeline {
		if latest[phase.PhaseID] == phase.Attempt {
			envelopes[phase.PhaseID] = phase.OutputEnvelope
		}
	}
	for index := range attempts {
		attempt := &attempts[index]
		if latest[attempt.PhaseID] != attempt.Attempt {
			continue
		}
		outputs, err := workflowAttemptOutputs(itemID, attempt.PhaseID, attempt.Attempt, envelopes[attempt.PhaseID])
		if err != nil {
			return err
		}
		attempt.Outputs, attempt.OutputOverflow = workflowOutputDigest(outputs)
	}
	return nil
}

// workflowAgentPhaseAttempts projects a run's phase history for the single-run
// status read. A gate trace that no longer decodes fails the read rather than
// reporting an attempt with no decision: "this attempt reached no gate" and
// "this attempt's record is corrupt" are different answers, and only one of
// them is a state a run can legitimately be in.
func (a *App) workflowAgentPhaseAttempts(itemID string) ([]WorkflowAgentPhaseAttempt, error) {
	values, err := a.workflowApplication().PhaseAttempts(itemID)
	if err != nil {
		return nil, err
	}
	attempts := make([]WorkflowAgentPhaseAttempt, 0, len(values))
	for _, value := range values {
		attempts = append(attempts, projectWorkflowPhaseAttempt(value))
	}
	return attempts, nil
}

// The methods the `ao` CLI calls. Each one takes its project from the caller's
// scope rather than from an argument: the credential is what says which project
// this session may touch, and an argument would be the caller's claim about it.

// WorkflowAgentStartInput is `ao run start`. Scope is optional — omitted, the
// workflow id resolves by §8 precedence (project scope wins over shared),
// exactly as a call phase's static target does.
type WorkflowAgentStartInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Goal       string          `json:"goal,omitempty"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
	BaseBranch string          `json:"baseBranch,omitempty"`
	StepMode   bool            `json:"stepMode,omitempty"`
}

// WorkflowAgentStartResult is what `ao run start` prints. Skipped marks a
// surface-and-skip replay: the phase asked for something it had already done,
// so nothing new started and ItemID names the original run.
type WorkflowAgentStartResult struct {
	ItemID         string `json:"itemId"`
	WorkflowID     string `json:"workflowId"`
	WorkflowScope  string `json:"workflowScope"`
	State          string `json:"state"`
	Skipped        bool   `json:"skipped,omitempty"`
	BoundThreadID  string `json:"boundThreadId,omitempty"`
	BindingWarning string `json:"bindingWarning,omitempty"`
}

// WorkflowAgentRunView is the compact run projection `ao run status` and
// `ao run list` render. It deliberately excludes envelopes, snapshots, and
// worktree paths: an agent asking "where is this run" does not need a run's
// whole history crossing into its context.
type WorkflowAgentRunView struct {
	ItemID              string `json:"itemId"`
	WorkflowID          string `json:"workflowId"`
	Goal                string `json:"goal"`
	State               string `json:"state"`
	Reason              string `json:"reason,omitempty"`
	CurrentPhaseID      string `json:"currentPhaseId,omitempty"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal,omitempty"`
	PhaseCount          int    `json:"phaseCount,omitempty"`
	ParentItemID        string `json:"parentItemId,omitempty"`
	Resting             bool   `json:"resting"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	EndedAt             int64  `json:"endedAt,omitempty"`
	// Seeds is what the run was started with, populated only by the single-run
	// reads. It is the run's own frozen input, so a caller reconstructing what a
	// wave was asked for has it without a second surface; the listing projection
	// does not carry it at all, because a summary row blanks the column.
	Seeds json.RawMessage `json:"seeds,omitempty"`
	// FailedUnits names the units `run retry-unit` takes — the attempt's join
	// among them when it is what failed — populated only by
	// `run status` on a run resting needs-human(unit-failed). The reason already
	// says a fan-out needs repair; without the ids the caller has to find them in
	// the app, which is the one thing an agent holding a CLI cannot do. It is
	// deliberately absent from `run list` — one extra query per run is a fan-out
	// of its own, and a list is for locating a run, not for repairing one.
	FailedUnits []WorkflowAgentFailedUnit `json:"failedUnits,omitempty"`
	// Phases is the run's per-attempt provenance, populated only by `run status`
	// for the same reason FailedUnits is: it costs one extra query per run, and a
	// list is for locating a run rather than reading one.
	Phases []WorkflowAgentPhaseAttempt `json:"phases,omitempty"`
	// Budget is the ceiling in force and the tree's spend against it, absent on
	// a run that has none. Populated by the single-run reads only: resolving it
	// costs a root lookup, a profile read, and — for a run that HAS a ceiling —
	// the same tree-spend aggregate the engine's check runs, which is a per-run
	// fan-out a listing must not pay.
	Budget *WorkflowAgentRunBudget `json:"budget,omitempty"`
	// PendingGuidance counts the `run guide` entries waiting for this run's next
	// fresh phase entry. It is a COUNT here and the entries themselves are on
	// `run inspect`: the number is what a reader of a run's state needs — an
	// operator about to leave a fourth steer, or one wondering why the run has
	// not turned yet — while the text of what somebody else left is a read of
	// its own. Populated by the single-run reads only, like the two fields above.
	PendingGuidance int `json:"pendingGuidance,omitempty"`
}

// WorkflowAgentFailedUnit is one unit of a parked fan-out that is resting
// failed. The attempt count rides along because "this unit has already been
// retried twice" is what decides between retrying it again and reading it.
//
// Note is the note the unit rests with, and it is what keeps the status
// honest: a pause tears its in-flight units down `failed` with an interrupted
// note, because there is no interrupted unit status and `failed` is exactly
// what the repair verbs recover. Without the note a run the operator paused
// reports a wave of "failed units" that never failed at anything.
type WorkflowAgentFailedUnit struct {
	UnitID      string `json:"unitId"`
	UnitAttempt int    `json:"unitAttempt"`
	Note        string `json:"note,omitempty"`
}

// WorkflowAgentRunOutputs is `ao run output`: the run's declared outputs plus
// the artifact file names it produced.
type WorkflowAgentRunOutputs struct {
	ItemID    string         `json:"itemId"`
	State     string         `json:"state"`
	Reason    string         `json:"reason,omitempty"`
	Resting   bool           `json:"resting"`
	Outputs   map[string]any `json:"outputs"`
	Artifacts []string       `json:"artifacts"`
}

// WorkflowAgentScheduleInput is `ao schedule`.
type WorkflowAgentScheduleInput struct {
	WorkflowID string          `json:"workflowId"`
	Scope      string          `json:"scope,omitempty"`
	Name       string          `json:"name,omitempty"`
	Cron       string          `json:"cron"`
	Seeds      json.RawMessage `json:"seeds,omitempty"`
}

// WorkflowAgentScheduleResult names the automation, skipped on a replay.
type WorkflowAgentScheduleResult struct {
	AutomationID string `json:"automationId"`
	Name         string `json:"name"`
	Cron         string `json:"cron"`
	Skipped      bool   `json:"skipped,omitempty"`
}

// WorkflowAgentNotesResult is `ao notes set`.
type WorkflowAgentNotesResult struct {
	AutomationID string `json:"automationId"`
	Skipped      bool   `json:"skipped,omitempty"`
}

// WorkflowAgentStartRun starts a run on behalf of an agent session. An
// interactive caller's run binds to its thread (D17); a granted phase's run is
// recorded in the effect ledger so re-entering the phase surfaces the prior
// start instead of firing a second one (§5).
//
// LocalOnly: it starts autonomous provider sessions, like every other entry to
// the workflow start path.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentStartRun(ctx context.Context, input WorkflowAgentStartInput) (WorkflowAgentStartResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	resolved, err := a.resolveAgentWorkflow(scope, input.WorkflowID, input.Scope)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	seedValues, seeds, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentStartResult{}, fmt.Errorf("start workflow run: %w", err)
	}
	goal := strings.TrimSpace(input.Goal)
	if goal == "" {
		// The overlay lists runs by goal; falling back to the workflow's own
		// name keeps an agent-started run identifiable without forcing every
		// caller to invent prose.
		goal = strings.TrimSpace(resolved.Workflow.Name)
	}
	baseBranch := strings.TrimSpace(input.BaseBranch)
	hash, err := effectPayloadHash(map[string]any{
		"workflow": resolved.Workflow.ID, "scope": string(resolved.Scope), "goal": goal,
		"seeds": seedValues, "baseBranch": baseBranch, "stepMode": input.StepMode,
	})
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	prior, found, err := a.priorStartedRun(scope, hash)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	if found {
		return WorkflowAgentStartResult{
			ItemID: prior.ID, WorkflowID: prior.WorkflowID, WorkflowScope: prior.WorkflowScope,
			State: prior.State, Skipped: true,
		}, nil
	}

	sourceRef := ""
	if scope.IsPhase() {
		sourceRef = phaseSourceRef(scope, hash)
	}
	item, err := a.startWorkflowRun(
		scope.ProjectID, resolved.Workflow.ID, string(resolved.Scope), goal, seeds,
		(*profile.Budget)(nil), baseBranch, input.StepMode || resolved.Workflow.DefaultStepMode,
		workflowSourceAgent, sourceRef,
	)
	if err != nil {
		return WorkflowAgentStartResult{}, err
	}
	result := WorkflowAgentStartResult{
		ItemID: item.ID, WorkflowID: item.WorkflowID, WorkflowScope: item.WorkflowScope,
		State: item.State,
	}
	if warning := a.bindOriginThread(scope, item); warning != "" {
		result.BindingWarning = warning
	} else if !scope.IsPhase() {
		result.BoundThreadID = scope.ThreadID
	}
	if err := a.recordEffect(scope, workflowEffectStartRun, hash, workflowEffect{
		Args:   map[string]any{"workflow": item.WorkflowID, "goal": item.Goal},
		Result: map[string]any{"itemId": item.ID},
	}); err != nil {
		// The run is already going and its source ref already carries this key,
		// so re-entry still surfaces it. What is lost is the ledger's record of
		// what was asked for, which a human reads. Report rather than fail.
		log.Printf("workflow: run %s started but its effect record did not persist: %v", item.ID, err)
		result.BindingWarning = strings.TrimSpace(result.BindingWarning + " " +
			"the run started, but its effect record could not be written; check the app log")
	}
	return result, nil
}

// priorStartedRun answers the surface-and-skip question for a run start. It asks
// twice on purpose: the effect ledger is the fast path and the record a human
// reads, while the run's own source ref is the durable one — a crash between the
// run committing and its ledger entry landing must not license a second start.
// An interactive scope has neither, by design: a human-approved invocation
// replays when the human runs it again.
func (a *App) priorStartedRun(scope transport.CallerScope, hash string) (store.WorkItem, bool, error) {
	if !scope.IsPhase() {
		return store.WorkItem{}, false, nil
	}
	effect, found, err := a.priorEffect(scope, workflowEffectStartRun, hash)
	if err != nil {
		return store.WorkItem{}, false, err
	}
	if found {
		itemID, _ := effect.Result["itemId"].(string)
		if itemID == "" {
			return store.WorkItem{}, false, fmt.Errorf(
				"start workflow run: a prior effect is recorded for these arguments but names no run")
		}
		item, err := a.store.GetWorkItem(itemID)
		if err != nil {
			return store.WorkItem{}, false, fmt.Errorf("start workflow run: load prior run %s: %w", itemID, err)
		}
		return item, true, nil
	}
	return a.store.GetWorkItemBySourceRef(workflowSourceAgent, phaseSourceRef(scope, hash))
}

// resolveAgentWorkflow resolves the id the caller named, honouring an explicit
// scope and otherwise §8 precedence. Resolving here — rather than letting the
// start path do it — is what lets the effect hash and the run row agree on which
// definition was meant.
func (a *App) resolveAgentWorkflow(scope transport.CallerScope, workflowID, workflowScope string) (def.ResolvedWorkflow, error) {
	workflowID = strings.TrimSpace(workflowID)
	if workflowID == "" {
		return def.ResolvedWorkflow{}, fmt.Errorf("workflow id is required")
	}
	projectRow, err := a.store.GetProject(scope.ProjectID)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	declared := def.Scope(strings.TrimSpace(workflowScope))
	if declared != "" {
		if declared != def.ScopeProject && declared != def.ScopeShared {
			return def.ResolvedWorkflow{}, fmt.Errorf("scope must be project or shared")
		}
		return aocli.ResolveWorkflow(a.workflowDataRoot(), projectRow.Slug, workflowID, declared)
	}
	calls, err := aocli.NewCallResolver(a.workflowDataRoot(), projectRow.Slug)
	if err != nil {
		return def.ResolvedWorkflow{}, err
	}
	return calls.ResolveCall(workflowID)
}

// WorkflowAgentRunStatus is `ao run status` / the poll behind `ao run wait`.
//
// LocalOnly: the whole ao surface is reachable only through credentials minted
// for local provider processes.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentRunStatus(ctx context.Context, itemID string) (WorkflowAgentRunView, error) {
	view, err := a.workflowApplication().RunStatus(ctx, itemID)
	if err != nil {
		return WorkflowAgentRunView{}, err
	}
	return projectWorkflowRunView(view), nil
}

// WorkflowAgentListRuns is `ao run list`, scoped to the caller's project.
//
// LocalOnly: see WorkflowAgentRunStatus.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentListRuns(ctx context.Context, activeOnly bool) ([]WorkflowAgentRunView, error) {
	views, err := a.workflowApplication().ListRuns(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowAgentRunView, 0, len(views))
	for _, view := range views {
		out = append(out, projectWorkflowRunView(view))
	}
	return out, nil
}

// WorkflowAgentRunOutput is `ao run output` — the "different context that did
// not start the run" path (D15).
//
// LocalOnly: see WorkflowAgentRunStatus.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentRunOutput(ctx context.Context, itemID string) (WorkflowAgentRunOutputs, error) {
	result, err := a.workflowApplication().RunOutput(ctx, itemID)
	if err != nil {
		return WorkflowAgentRunOutputs{}, err
	}
	return WorkflowAgentRunOutputs{
		ItemID: result.ItemID, State: result.State, Reason: result.Reason,
		Resting: result.Resting, Outputs: result.Outputs, Artifacts: result.Artifacts,
	}, nil
}

// WorkflowAgentSchedule is `ao schedule`: it creates one enabled cron
// automation through the same validation the overlay's editor uses.
//
// LocalOnly: an automation is a standing instruction to start autonomous
// provider sessions, like every other automation mutation.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentSchedule(ctx context.Context, input WorkflowAgentScheduleInput) (WorkflowAgentScheduleResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	resolved, err := a.resolveAgentWorkflow(scope, input.WorkflowID, input.Scope)
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	cronExpr := strings.TrimSpace(input.Cron)
	if cronExpr == "" {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: a cron expression is required")
	}
	seedValues, seeds, err := decodeWorkflowSeeds(input.Seeds)
	if err != nil {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: %w", err)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = strings.TrimSpace(resolved.Workflow.Name)
	}
	trigger, err := json.Marshal(scheduler.Trigger{Kind: scheduler.KindCron, Expr: cronExpr})
	if err != nil {
		return WorkflowAgentScheduleResult{}, fmt.Errorf("create automation: encode trigger: %w", err)
	}
	hash, err := effectPayloadHash(map[string]any{
		"workflow": resolved.Workflow.ID, "scope": string(resolved.Scope),
		"name": name, "cron": cronExpr, "seeds": seedValues,
	})
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	if prior, found, err := a.priorEffect(scope, workflowEffectSchedule, hash); err != nil {
		return WorkflowAgentScheduleResult{}, err
	} else if found {
		automationID, _ := prior.Result["automationId"].(string)
		if automationID == "" {
			return WorkflowAgentScheduleResult{}, fmt.Errorf(
				"create automation: a prior effect is recorded for these arguments but names no automation")
		}
		return WorkflowAgentScheduleResult{AutomationID: automationID, Name: name, Cron: cronExpr, Skipped: true}, nil
	}
	view, err := a.WorkflowCreateAutomation(WorkflowAutomationInput{
		ProjectID: scope.ProjectID, WorkflowID: resolved.Workflow.ID,
		WorkflowScope: string(resolved.Scope), Name: name, Enabled: true,
		Trigger: trigger, Seeds: seeds,
	})
	if err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	if err := a.recordEffect(scope, workflowEffectSchedule, hash, workflowEffect{
		Args:   map[string]any{"workflow": view.WorkflowID, "cron": cronExpr},
		Result: map[string]any{"automationId": view.ID},
	}); err != nil {
		return WorkflowAgentScheduleResult{}, err
	}
	return WorkflowAgentScheduleResult{AutomationID: view.ID, Name: view.Name, Cron: cronExpr}, nil
}

// WorkflowAgentGetNotes is `ao notes get`.
//
// LocalOnly: see WorkflowAgentRunStatus.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentGetNotes(ctx context.Context, automationID string) (string, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return "", err
	}
	automation, err := a.scopedAutomation(scope, automationID, "workflow job notes")
	if err != nil {
		return "", err
	}
	return automation.Notes, nil
}

// WorkflowAgentSetNotes is `ao notes set` — the §11 continuity-notes rewrite a
// terminal phase performs. Rewriting the same notes twice is a skip, not a
// second write, so a re-entered phase does not churn the row.
//
// LocalOnly: it mutates local automation state.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentSetNotes(ctx context.Context, automationID, notes string) (WorkflowAgentNotesResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	automation, err := a.scopedAutomation(scope, automationID, "workflow job notes")
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	hash, err := effectPayloadHash(map[string]any{"automation": automation.ID, "notes": notes})
	if err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	if _, found, err := a.priorEffect(scope, workflowEffectSetNotes, hash); err != nil {
		return WorkflowAgentNotesResult{}, err
	} else if found {
		return WorkflowAgentNotesResult{AutomationID: automation.ID, Skipped: true}, nil
	}
	if err := a.WorkflowSetJobNotes(automation.ID, notes); err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	if err := a.recordEffect(scope, workflowEffectSetNotes, hash, workflowEffect{
		Args:   map[string]any{"automation": automation.ID},
		Result: map[string]any{"automationId": automation.ID},
	}); err != nil {
		return WorkflowAgentNotesResult{}, err
	}
	return WorkflowAgentNotesResult{AutomationID: automation.ID}, nil
}

// `agent-overflow run watch` (D53): the one method that BLOCKS.
//
// Every other method on this surface answers from what is already persisted and
// returns. This one holds the request until the run tree it names moves, which
// is the whole feature: a supervising agent waiting on a wave otherwise polls,
// and polling is what produced 712 status reads and seven hand-rolled monitor
// loops in one campaign — one of which died without saying so.
//
// It is a long poll rather than a subscription because the CLI's transport is
// one scoped HTTP POST per invocation (`internal/transport/httprpc.go`). A
// subscription would mean giving scoped tokens a WebSocket, a replay ring, and
// a per-connection channel filter — a second wire for one verb. The hold is
// bounded (maxWorkflowWatchHold) so that the caller's own HTTP timeout is never
// what ends the call, and so a credential revoked mid-watch — the session that
// minted it ended — is discovered by the next call's 401 rather than by a
// watcher that hangs until someone kills it.

// maxWorkflowWatchHold bounds one blocked call. It sits under the CLI's 30s RPC
// timeout on purpose: the client must be the one still waiting when the server
// answers, never the other way round, or every quiet minute would look like a
// dead backend. It is also the worst case for noticing a revoked token.
const maxWorkflowWatchHold = 25 * time.Second

// WorkflowAgentWatchInput is `run watch`. Cursor is the sequence the caller
// already has: zero means "I have none", which is answered immediately with the
// run's current state so a watch on an already-resting run exits instead of
// blocking on a transition that has already happened.
type WorkflowAgentWatchInput struct {
	ItemID string `json:"itemId"`
	Cursor int64  `json:"cursor,omitempty"`
	// Tree widens the watch to the run and every run it called, transitively.
	// The set is re-resolved on every wake, so a wave started while this call
	// was blocked is watched from its birth transition rather than from the
	// next call.
	Tree bool `json:"tree,omitempty"`
	// WaitMillis is how long the caller is willing to have this call block,
	// clamped to maxWorkflowWatchHold. It exists so `--timeout` is exact: the
	// last poll of a bounded watch waits the remainder and not a second more.
	WaitMillis int64 `json:"waitMillis,omitempty"`
}

// WorkflowAgentTransition is one item-state transition of a watched run. The
// coordinate is the engine's own — the phase and attempt the run was in when it
// moved — so Cause names the attempt a park actually rests on.
type WorkflowAgentTransition struct {
	Seq     int64  `json:"seq"`
	At      int64  `json:"at"`
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// From is empty for the birth transition of a run that has just started,
	// which is how a `--tree` watcher sees a new wave appear.
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Reason  string `json:"reason,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Resting bool   `json:"resting"`
}

// WorkflowAgentWatchRunState is where the watched run is right now, read from
// SQLite rather than derived from the transitions — a watcher that resynced
// after a gap has to be told the truth, not the tail of a ring.
type WorkflowAgentWatchRunState struct {
	ItemID     string `json:"itemId"`
	WorkflowID string `json:"workflowId,omitempty"`
	Goal       string `json:"goal,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	PhaseID    string `json:"phaseId,omitempty"`
	Resting    bool   `json:"resting"`
	// Repair is the sentence naming the verb that settles this park, composed by
	// the same helper every wake uses (`wake.RepairSentence`) so the two surfaces
	// cannot send a reader to different commands for one reason. Present only
	// once the run is resting, and empty for the reasons that have no one verb.
	Repair string `json:"repair,omitempty"`
}

// WorkflowAgentWatchResult is one long poll's answer.
type WorkflowAgentWatchResult struct {
	ItemID      string                     `json:"itemId"`
	Cursor      int64                      `json:"cursor"`
	Transitions []WorkflowAgentTransition  `json:"transitions"`
	Run         WorkflowAgentWatchRunState `json:"run"`
	// Gap says transitions between the caller's cursor and the oldest retained
	// one were lost — the ring evicted them, or this backend restarted and
	// re-seeded its sequence. It is a resync instruction, exactly as it is on the
	// event wire: the run state above is current, and the cursor to continue from
	// is the one returned.
	Gap bool `json:"gap,omitempty"`
}

// WorkflowAgentWatchRun blocks until a watched run transitions, the caller's
// wait budget expires, or the run is already resting.
//
// LocalOnly: see WorkflowAgentRunStatus. It takes the same grants as reading a
// run's status, because that is what it is — the same fact, delivered when it
// changes instead of when it is asked for.
//
//ao:scope threads:autonomy
func (a *App) WorkflowAgentWatchRun(ctx context.Context, input WorkflowAgentWatchInput) (WorkflowAgentWatchResult, error) {
	result, err := a.workflowApplication().WatchRun(ctx, workflowapp.WatchInput{
		ItemID: input.ItemID, Cursor: input.Cursor, Tree: input.Tree, WaitMillis: input.WaitMillis,
	})
	if err != nil {
		return WorkflowAgentWatchResult{}, err
	}
	return projectWorkflowWatch(result), nil
}
