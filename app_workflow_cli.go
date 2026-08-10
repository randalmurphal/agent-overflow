package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
)

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
	if err := validWorkflowBindingThread(item, thread); err != nil {
		return fmt.Sprintf("run started unbound: %v", err)
	}
	if err := a.store.UpdateWorkItemOriginThread(item.ID, thread.ID); err != nil {
		return fmt.Sprintf("run started unbound: %v", err)
	}
	return ""
}
