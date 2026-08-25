package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/chatmodel"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
)

// createWorkflowThread creates the AO thread one workflow turn runs on.
func (a *App) createWorkflowThread(spec workflowhost.ThreadSpec) (store.Thread, error) {
	if !provider.CapabilitiesForProvider(spec.ProviderName).EnforcesRuntimeMode {
		// The element declares an access level the provider's session config
		// cannot apply. Refusing here is the point: starting anyway would run
		// unattended work with its `access` declaration silently inert, which is
		// the exact hole D22 closes. Typed as a wiring error — the frozen
		// definition and the runtime cannot produce the work it describes — so
		// the item parks with an actionable reason.
		return store.Thread{}, fmt.Errorf(
			"%w: workflow runner: %s declares access %q but provider %q does not enforce runtime modes",
			engine.ErrWiringFailed, spec.Label, spec.Access, spec.ProviderName,
		)
	}
	workspace := spec.Workspace.Path
	if strings.TrimSpace(workspace) == "" {
		return store.Thread{}, fmt.Errorf("workflow runner: %s has no workspace", spec.Label)
	}
	model := provider.NormalizeModelSlug(spec.ProviderName, spec.Model)
	// A workflow lane's model settings come from the catalog's defaults for
	// (provider, model) plus what the definition authored — deliberately NOT from
	// `chat_model_profiles`. Seeding from the last-remembered CHAT profile would
	// make a phase's reasoning effort, context window, and fast mode depend on
	// unrelated interactive use of the same model, so the same run could behave
	// differently on two machines, or on one machine a week apart.
	seed := chatmodel.FallbackProfile(spec.ProviderName, model)
	effort := seed.ReasoningEffort
	if authored := strings.TrimSpace(spec.Effort); authored != "" {
		// Validation accepted the tier as a NAME; whether this model advertises it
		// is a catalog question, so it is answered here and coerced onto the
		// model's own default when the answer is no. `threads.reasoning_effort` is
		// NOT NULL under a per-provider CHECK — what persists is always a legal
		// tier, and an argv builder is what decides a model with no tiers at all
		// gets no flag (see internal/provider/AGENTS.md §Model catalogs).
		effort = a.coerceReasoningEffortForModel(spec.ProviderName, model, authored)
	}
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: uuid.NewString(), ProjectID: spec.Workspace.Project.ID, ProjectPath: spec.Workspace.Project.Path,
		Title: spec.Title, Provider: spec.ProviderName,
		Model:         model,
		WorkspacePath: workspace, Mode: "workflow",
		ReasoningEffort: effort, FastMode: seed.FastMode,
		ContextWindow: seed.ContextWindow,
		RuntimeMode:   string(workflowPhaseRuntimeMode(spec.Access)),
		CreatedAt:     now, UpdatedAt: now,
	}
	if !gitops.SameFilesystemPath(workspace, spec.Workspace.Project.Path) {
		if spec.Workspace.Branch == "" {
			return store.Thread{}, fmt.Errorf(
				"workflow runner: %s runs in worktree %q with no branch recorded", spec.Label, workspace,
			)
		}
		thread.WorktreePath = workspace
		thread.Branch = spec.Workspace.Branch
	}
	// sanitizeThreadModelSettings does not touch RuntimeMode (see its doc
	// comment), so the access mapping set above survives it.
	thread = a.sanitizeThreadModelSettings(thread)
	if err := a.store.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: create thread for %s: %w", spec.Label, err)
	}
	return a.store.GetThread(thread.ID)
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
