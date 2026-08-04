package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// workflowThreadSpec is one provider-backed piece of workflow work described in
// thread terms. A phase attempt, a fan-out unit, and a join differ only in these
// fields, so they share one creation path: the access → runtime-mode enforcement
// below cannot be applied to a phase and forgotten for a unit.
type workflowThreadSpec struct {
	itemID string
	// label names the element in diagnostics — `phase "review"`, `unit "port-0"
	// of phase "port"`. It is the only place the two shapes read differently.
	label        string
	title        string
	providerName string
	model        string
	access       def.Access
	workspace    preparedWorkflowWorkspace
}

// createWorkflowThread creates the AO thread one workflow turn runs on.
func (a *App) createWorkflowThread(spec workflowThreadSpec) (store.Thread, error) {
	if !provider.CapabilitiesForProvider(spec.providerName).EnforcesRuntimeMode {
		// The element declares an access level the provider's session config
		// cannot apply. Refusing here is the point: starting anyway would run
		// unattended work with its `access` declaration silently inert, which is
		// the exact hole D22 closes. Typed as a wiring error — the frozen
		// definition and the runtime cannot produce the work it describes — so
		// the item parks with an actionable reason.
		return store.Thread{}, fmt.Errorf(
			"%w: workflow runner: %s declares access %q but provider %q does not enforce runtime modes",
			engine.ErrWiringFailed, spec.label, spec.access, spec.providerName,
		)
	}
	workspace := spec.workspace.path
	if strings.TrimSpace(workspace) == "" {
		return store.Thread{}, fmt.Errorf("workflow runner: %s has no workspace", spec.label)
	}
	seed := a.seedChatModelProfile(spec.providerName, spec.model)
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: spec.workspace.project.ID, ProjectPath: spec.workspace.project.Path,
		Title: spec.title, Provider: spec.providerName,
		Model:         provider.NormalizeModelSlug(spec.providerName, spec.model),
		WorkspacePath: workspace, Mode: "workflow",
		ReasoningEffort: seed.ReasoningEffort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow,
		RuntimeMode:   string(workflowPhaseRuntimeMode(spec.access)),
		CreatedAt:     now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(workspace, spec.workspace.project.Path) {
		if spec.workspace.branch == "" {
			return store.Thread{}, fmt.Errorf(
				"workflow runner: %s runs in worktree %q with no branch recorded", spec.label, workspace,
			)
		}
		thread.WorktreePath = workspace
		thread.Branch = spec.workspace.branch
	}
	// sanitizeThreadModelSettings does not touch RuntimeMode (see its doc
	// comment), so the access mapping set above survives it.
	thread = a.sanitizeThreadModelSettings(thread)
	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: create thread for %s: %w", spec.label, err)
	}
	return a.store.GetThread(thread.ID)
}

// workflowThreadTitle names a phase's thread. The phase id is the fallback for
// an unnamed phase, so the title is never bare "Workflow:".
func workflowThreadTitle(name, phaseID string) string {
	title := strings.TrimSpace(name)
	if title == "" {
		title = phaseID
	}
	return "Workflow: " + title
}

// workflowUnitThreadTitle names one fan-out unit's thread. The unit id is
// appended to the phase's own title so a sidebar full of sibling units reads as
// one phase's parallel work rather than N unrelated threads.
func workflowUnitThreadTitle(phase def.Phase, unitID string) string {
	return workflowThreadTitle(phase.Name, phase.ID) + " / " + unitID
}

// workflowPhaseRuntimeMode maps a phase's or unit's effective access declaration
// onto the provider session's runtime mode (decision D22, spec §9). This is the
// single translation point: the thread row it feeds is the source of truth that
// ThreadView → SessionOptions derives from, so restarts, resumes, and
// Answer-continuations all inherit the declaration without re-deriving it.
//
// `write` gets full access rather than a supervised tier because writing work
// already runs inside its own isolated workspace and there is nobody present to
// answer a prompt; `read-only` gets the restricted mode, which denies mutations
// outright instead of asking about them.
func workflowPhaseRuntimeMode(access def.Access) provider.RuntimeMode {
	if access == def.AccessWrite {
		return provider.RuntimeFullAccess
	}
	return provider.RuntimeReadOnly
}

// validatePriorThread checks that a continued session still matches the element
// it is being reused for. An Answer, a takeover finalize, or a join
// continuation resumes an existing provider session; a definition edited in
// between must not quietly run on a session configured for the old one.
//
// It takes the same spec creation does, so the checks a reused thread is held
// to cannot drift from the thread a fresh one would have been given — and a
// phase, a unit, and a join are all held to them.
func (r *workflowAppRunner) validatePriorThread(spec workflowThreadSpec, threadID string) error {
	thread, err := r.app.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("workflow runner: load prior thread %q for %s: %w", threadID, spec.label, err)
	}
	if thread.Mode != "workflow" {
		return fmt.Errorf("workflow runner: prior thread %q has mode %q, want workflow", threadID, thread.Mode)
	}
	if thread.Provider != spec.providerName ||
		thread.Model != provider.NormalizeModelSlug(spec.providerName, spec.model) {
		return fmt.Errorf(
			"workflow runner: prior thread %q provider/model no longer matches %s (%s/%s)",
			threadID, spec.label, spec.providerName, spec.model,
		)
	}
	workspace := spec.workspace.path
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(thread.WorkspacePath) ||
		!gitops.SameFilesystemPath(thread.WorkspacePath, workspace) {
		return fmt.Errorf(
			"workflow runner: prior thread %q workspace %q no longer matches %s workspace %q",
			threadID, thread.WorkspacePath, spec.label, workspace,
		)
	}
	return nil
}
