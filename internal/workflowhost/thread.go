package workflowhost

import (
	"fmt"
	"path/filepath"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// ThreadSpec is one provider-backed piece of workflow work described in
// thread terms. A phase attempt, a fan-out unit, and a join differ only in these
// fields, so they share one creation path: the access → runtime-mode enforcement
// below cannot be applied to a phase and forgotten for a unit.
type ThreadSpec struct {
	ItemID string
	// Label names the element in diagnostics — `phase "review"`, `unit "port-0"
	// of phase "port"`. It is the only place the two shapes read differently.
	Label        string
	Title        string
	ProviderName string
	Model        string
	// Effort is the reasoning tier the definition authored (`effort:` on the
	// phase or the unit), or empty for "whatever the model's catalog default is".
	// It is coerced against the model at creation, never validated statically —
	// which tiers a model advertises is provider-owned and partly live.
	Effort    string
	Access    def.Access
	Workspace PreparedWorkspace
}

// ThreadTitle names a phase's thread. The phase id is the fallback for
// an unnamed phase, so the title is never bare "Workflow:".
func ThreadTitle(name, phaseID string) string {
	title := strings.TrimSpace(name)
	if title == "" {
		title = phaseID
	}
	return "Workflow: " + title
}

// UnitThreadTitle names one fan-out unit's thread. The unit id is
// appended to the phase's own title so a sidebar full of sibling units reads as
// one phase's parallel work rather than N unrelated threads.
func UnitThreadTitle(phase def.Phase, unitID string) string {
	return ThreadTitle(phase.Name, phase.ID) + " / " + unitID
}

// validatePriorThread checks that a continued session still matches the element
// it is being reused for. An Answer, a takeover finalize, or a join
// continuation resumes an existing provider session; a definition edited in
// between must not quietly run on a session configured for the old one.
//
// It takes the same spec creation does, so the checks a reused thread is held
// to cannot drift from the thread a fresh one would have been given — and a
// phase, a unit, and a join are all held to them.
func (r *Runner) validatePriorThread(spec ThreadSpec, threadID string) (store.Thread, error) {
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("workflow runner: load prior thread %q for %s: %w", threadID, spec.Label, err)
	}
	if thread.Mode != "workflow" {
		return store.Thread{}, fmt.Errorf("workflow runner: prior thread %q has mode %q, want workflow", threadID, thread.Mode)
	}
	if thread.Provider != spec.ProviderName ||
		thread.Model != provider.NormalizeModelSlug(spec.ProviderName, spec.Model) {
		return store.Thread{}, fmt.Errorf(
			"workflow runner: prior thread %q provider/model no longer matches %s (%s/%s)",
			threadID, spec.Label, spec.ProviderName, spec.Model,
		)
	}
	// `effort` is deliberately NOT compared. Provider, model, and workspace decide
	// whether the parked session is still the right one to resume; the reasoning
	// tier is stamped once, at creation, onto the session that is now mid-turn.
	// Refusing a continuation because the definition's `effort:` was edited would
	// strand a run whose session is perfectly usable, and re-stamping the thread
	// row would not reconfigure the process anyway.
	workspace := spec.Workspace.Path
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(thread.WorkspacePath) ||
		!gitops.SameFilesystemPath(thread.WorkspacePath, workspace) {
		return store.Thread{}, fmt.Errorf(
			"workflow runner: prior thread %q workspace %q no longer matches %s workspace %q",
			threadID, thread.WorkspacePath, spec.Label, workspace,
		)
	}
	return thread, nil
}
