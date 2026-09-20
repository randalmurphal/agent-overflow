package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// How a thread_spawn's thread comes to exist: the options a call resolves
// to, on this computer's own catalogs, and the thread created from them. A
// fresh thread and a fork of another thread's history both end here, and
// every refusal names what this computer offers.

// createSpawnedThread makes the thread a spawn runs in: a fork of another
// thread when from_thread names one, a fresh thread otherwise. A group the
// call names is joined once the thread exists, through the organize patch
// the sidebar's own grouping uses, so the group is created in the NEW
// thread's project: a fork's source project, or the destination's.
func (t threadToolsApp) createSpawnedThread(
	ctx context.Context, call threadtools.SpawnCall, create CreateThreadOptions,
) (store.Thread, error) {
	thread, err := t.createSpawnedThreadUngrouped(ctx, call, create)
	if err != nil || call.Group == "" {
		return thread, err
	}
	group := call.Group
	applied, err := t.app.applyThreadOrganizePatch(ctx, thread.ID, threadapp.OrganizePatch{Group: &group})
	if err == nil {
		return applied.Thread, nil
	}
	// The thread exists and nothing owns it yet: no receipt names it and its
	// prompt has not been sent. Take it back, as the scratch fork does,
	// rather than leave a thread behind a refusal the caller will retry.
	if deleteErr := t.app.DeleteThread(thread.ID); deleteErr != nil {
		log.Printf("thread tools: delete spawned thread %s after its group patch failed: %v", thread.ID, deleteErr)
		return store.Thread{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"Thread %s was created but could not join group %q (%v), and removing it failed too. It is on this computer, ungrouped.",
			thread.ID, group, err), errors.Join(err, deleteErr))
	}
	return store.Thread{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
		"That spawn could not join group %q: %v. No thread was left behind; make the request again.", group, err), err)
}

func (t threadToolsApp) createSpawnedThreadUngrouped(
	ctx context.Context, call threadtools.SpawnCall, create CreateThreadOptions,
) (store.Thread, error) {
	if call.FromThread == "" {
		return t.app.CreateThread(ctx, create)
	}
	// A fork runs in its source's project and workspace with its source's
	// provider: that is what forking is, and the tools layer already refused
	// a from_thread on another computer. Everything else on the new thread
	// was resolved in spawnThreadOptions from the caller's settings and the
	// call's overrides, so it reaches the fork here rather than being
	// dropped for the source's.
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return store.Thread{}, err
	}
	return t.app.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:        create.Mode,
		RuntimeMode: create.RuntimeMode,
		Title:       create.Title,
		Model:       create.Model,
		Effort:      create.ReasoningEffort,
	})
}

// spawnThreadOptions resolves what a spawn inherits and validates what it
// overrides, before anything durable exists.
//
// Every refusal names what this computer offers, because the caller cannot
// know another provider's model ids and a wrong guess should cost one call,
// not a discovery round trip.
func (t threadToolsApp) spawnThreadOptions(ctx context.Context, origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	opts, err := t.resolveSpawnOptions(origin, call)
	if err != nil {
		return CreateThreadOptions{}, err
	}
	// Judged here, on the mode the new thread will actually RUN in, because
	// this is the one place both spawn shapes resolve it: a fresh thread
	// reaches CreateThread's AuthorizeRuntimeMode hook, and a from_thread
	// fork never does. A local call carries no session and passes; a call a
	// paired computer forwarded is judged against that computer's grants,
	// the same rule the sidebar's own create obeys.
	if err := t.app.requireAutonomy(ctx, opts.RuntimeMode); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// resolveSpawnOptions is the resolution itself, split from the gate above so
// the gate sees one answer for both shapes.
func (t threadToolsApp) resolveSpawnOptions(origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	if call.FromThread != "" {
		return t.forkSpawnOptions(origin, call)
	}
	// A spawn a paired computer forwarded has no caller thread here, so
	// there is no project or checkout to inherit; the call names them and
	// the tools layer refuses it beforehand when it does not.
	var caller store.Thread
	if !origin.foreign {
		var err error
		caller, err = t.localThread(origin.caller.ThreadID)
		if err != nil {
			return CreateThreadOptions{}, err
		}
	}
	opts := CreateThreadOptions{
		ProjectID:       caller.ProjectID,
		Title:           call.Title,
		Provider:        origin.inherit.Provider,
		Model:           origin.inherit.Model,
		Mode:            origin.inherit.Mode,
		ReasoningEffort: origin.inherit.Effort,
		RuntimeMode:     origin.inherit.RuntimeMode,
	}
	if call.ProjectID != "" {
		if _, err := t.app.store.GetProject(call.ProjectID); err != nil {
			return CreateThreadOptions{}, errorsx.Public(threadtools.CodeNotFound,
				fmt.Sprintf("There is no project %s on this computer. thread_options lists the projects it has.", call.ProjectID), err)
		}
		opts.ProjectID = call.ProjectID
	}
	if opts.ProjectID == "" {
		return CreateThreadOptions{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A spawn needs a project: this thread has none to inherit. thread_options lists this computer's projects.", nil)
	}
	if call.Mode != "" {
		opts.Mode = call.Mode
	}
	if call.RuntimeMode != "" {
		opts.RuntimeMode = call.RuntimeMode
	}
	if err := t.applySpawnModel(&opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	if err := t.applySpawnWorkspace(&opts, caller, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// forkSpawnOptions resolves what a `from_thread` spawn runs with.
//
// A fork keeps its source's project, workspace and provider session; every
// other setting defaults to the CALLER's and is overridden by the call
// (docs/specs/agent-thread-tools.md, thread_spawn). Provider is the one axis
// a fork cannot move: a thread is locked to its provider once it holds items
// because the sessions are not interchangeable, so an explicit provider is
// refused here rather than accepted and ignored.
//
// Model and effort follow the caller only when the caller runs the source's
// provider. Across providers the caller's model names nothing this fork could
// start, so the source's stands, and an explicit one is validated against the
// source's provider like any other spawn.
func (t threadToolsApp) forkSpawnOptions(origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return CreateThreadOptions{}, err
	}
	if call.Provider != "" && call.Provider != source.Provider {
		return CreateThreadOptions{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"A fork of %s resumes that thread's %s session, so provider is the one setting from_thread cannot change. Drop provider, or spawn a fresh %s thread instead of forking.",
			source.ID, source.Provider, call.Provider), nil)
	}
	opts := CreateThreadOptions{
		Title:           call.Title,
		Provider:        source.Provider,
		Model:           source.Model,
		ReasoningEffort: source.ReasoningEffort,
		RuntimeMode:     origin.inherit.RuntimeMode,
	}
	if origin.inherit.Provider == source.Provider {
		opts.Model, opts.ReasoningEffort = origin.inherit.Model, origin.inherit.Effort
	}
	// A caller that is itself hidden (a scratch thread answering an ask) has
	// no mode a visible thread may take, so the source's stands.
	if threadmode.IsPostCreationMode(origin.inherit.Mode) {
		opts.Mode = origin.inherit.Mode
	}
	if call.Mode != "" {
		opts.Mode = call.Mode
	}
	if call.RuntimeMode != "" {
		opts.RuntimeMode = call.RuntimeMode
	}
	if opts.RuntimeMode == "" {
		// The fork would inherit the source's mode. Naming it makes the
		// resolved mode the one the caller's gate judges, instead of an
		// empty string that means "whatever the source runs".
		opts.RuntimeMode = source.RuntimeMode
	}
	if err := t.applySpawnModel(&opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// applySpawnModel resolves provider, model and effort against THIS computer's
// catalogs. A provider override with no model takes that provider's default,
// because the two are one choice and the caller cannot be expected to know
// the other provider's slugs.
func (t threadToolsApp) applySpawnModel(opts *CreateThreadOptions, call threadtools.SpawnCall) error {
	if call.Provider != "" && call.Provider != opts.Provider {
		opts.Provider = call.Provider
		// The inherited model and effort belong to the provider that was
		// replaced, and naming them to the new one would be a refusal the
		// caller did not earn.
		opts.Model, opts.ReasoningEffort = "", ""
	}
	option, err := t.providerOption(opts.Provider)
	if err != nil || len(option.Models) == 0 {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("This computer offers no models for %q. thread_options lists what it has.", opts.Provider), err)
	}
	if call.Model != "" {
		opts.Model = call.Model
	}
	if opts.Model == "" {
		opts.Model = option.DefaultModel
	}
	var model threadtools.ModelOption
	for _, candidate := range option.Models {
		if candidate.Slug == opts.Model {
			model = candidate
			break
		}
	}
	if model.Slug == "" {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%s does not offer model %q on this computer. It offers: %s.",
				option.Name, opts.Model, strings.Join(threadToolsModelSlugs(option.Models), ", ")), nil)
	}
	if call.Effort != "" {
		opts.ReasoningEffort = call.Effort
	}
	if len(model.Efforts) == 0 {
		// The model has no effort dimension; an inherited one would be
		// coerced away anyway, and an explicit one is worth saying so about.
		if call.Effort != "" {
			return errorsx.Public(threadtools.CodeInvalidRequest,
				fmt.Sprintf("Model %s has no reasoning effort setting on this computer.", model.Slug), nil)
		}
		opts.ReasoningEffort = ""
		return nil
	}
	if opts.ReasoningEffort == "" {
		opts.ReasoningEffort = model.DefaultEffort
	}
	for _, effort := range model.Efforts {
		if effort == opts.ReasoningEffort {
			return nil
		}
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("Model %s does not offer effort %q. It offers: %s.",
			model.Slug, opts.ReasoningEffort, strings.Join(model.Efforts, ", ")), nil)
}

func threadToolsModelSlugs(models []threadtools.ModelOption) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	return slugs
}

// checkSpawnWorktreeBase refuses a named base the project does not have
// before the request exists, naming what would have worked. A base that is
// only on origin cannot seed a base_local cut, so that case is refused too.
func (t threadToolsApp) checkSpawnWorktreeBase(projectID string, call threadtools.SpawnCall) error {
	if call.WorktreeBase == "" {
		return nil
	}
	project, err := t.app.store.GetProject(projectID)
	if err != nil {
		return err
	}
	known, err := t.app.gitCore().BaseBranchKnown(project.Path, call.WorktreeBase, call.WorktreeBaseLocal)
	if err != nil {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("base %q cannot be used: %v", call.WorktreeBase, err), err)
	}
	if known {
		return nil
	}
	if call.WorktreeBaseLocal {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("This computer has no local branch %q to start from. Omit base_local to start from origin's head of it, or name a local branch.", call.WorktreeBase), nil)
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("Neither this computer nor origin has a branch %q as of the last fetch. Name an existing branch as base.", call.WorktreeBase), nil)
}

// applySpawnWorkspace picks the checkout the new thread runs in: a fresh
// worktree on a named branch, a named checkout of the project, or the
// caller's own.
func (t threadToolsApp) applySpawnWorkspace(opts *CreateThreadOptions, caller store.Thread, call threadtools.SpawnCall) error {
	if call.WorktreeBranch != "" {
		// The sidebar's own door: thread creation cuts the worktree and
		// records its path and branch. Nothing here may name a path as well,
		// or the two would describe different checkouts.
		opts.WorktreeBranch = call.WorktreeBranch
		opts.WorktreeBase = call.WorktreeBase
		opts.WorktreeBaseLocal = call.WorktreeBaseLocal
		return t.checkSpawnWorktreeBase(opts.ProjectID, call)
	}
	if call.WorkspacePath != "" {
		projects, err := t.projectOptions(opts.ProjectID)
		if err != nil {
			return err
		}
		paths := []string{}
		for _, project := range projects {
			if project.ID != opts.ProjectID {
				continue
			}
			for _, workspace := range project.Workspaces {
				paths = append(paths, workspace.Path)
				if workspace.Path != call.WorkspacePath {
					continue
				}
				opts.WorkspaceOverride = workspace.Path
				if workspace.Worktree {
					opts.WorktreePath = workspace.Path
					opts.Branch = workspace.Branch
				}
				return nil
			}
		}
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That project has no checkout at %s. It has: %s. Pass worktree to cut a fresh one instead.",
				call.WorkspacePath, strings.Join(paths, ", ")), nil)
	}
	// Inherit the caller's checkout, worktree included: a spawn that names no
	// workspace belongs where its caller is working.
	if caller.ProjectID != opts.ProjectID {
		return nil
	}
	if caller.WorktreePath != "" {
		opts.WorktreePath = caller.WorktreePath
		opts.WorkspaceOverride = caller.WorktreePath
		opts.Branch = caller.Branch
		return nil
	}
	if caller.WorkspacePath != "" {
		opts.WorkspaceOverride = caller.WorkspacePath
	}
	return nil
}
