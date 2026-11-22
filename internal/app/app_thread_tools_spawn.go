package app

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/worktreeapp"
)

// How a thread_spawn's thread comes to exist: the options a call resolves
// to, on this computer's own catalogs, and the thread created from them. A
// fresh thread and a fork of another thread's history both end here, and
// every refusal names what this computer offers.

// createSpawnedThread makes the thread a spawn runs in: a fork of another
// thread when from_thread names one, a fresh thread otherwise, created in
// its final group and placement. A group the call names is resolved, and
// made if missing, in the NEW thread's project (a fork's source project, or
// the destination's) before the thread exists; with none, a fork is made in
// no group rather than in its source's. Creation is not a move: the thread
// never passes through a group or placement it then leaves. A level a
// paired computer's spawn carried is stored with the thread and counts
// while the thread is owned (store.SubThreadLevel).
//
// Nothing here announces a fresh thread or an owned fork: the spawn's
// request names and places the thread first, and acceptRequest announces
// it then, so a thread that will be owned is never listed in a sidebar.
// The worktree a fresh thread's creation cut and the group it made are
// returned with it, and are the caller's to undo while no request names
// the thread.
func (t threadToolsApp) createSpawnedThread(
	ctx context.Context, call threadtools.SpawnCall, create spawnOptions,
) (store.Thread, madeWithThread, error) {
	var source store.Thread
	project := create.ProjectID
	if call.FromThread != "" {
		// A fork runs in its source's project and workspace with its
		// source's provider: that is what forking is, and the tools layer
		// already refused a from_thread on another computer.
		var err error
		if source, err = t.localThread(call.FromThread); err != nil {
			return store.Thread{}, madeWithThread{}, err
		}
		project = source.ProjectID
	}
	var group store.ThreadGroup
	var made madeWithThread
	if call.Group != "" {
		var err error
		var madeGroup bool
		group, madeGroup, err = t.app.store.EnsureThreadGroup(project, call.Group)
		if err != nil {
			return store.Thread{}, madeWithThread{}, errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
				"That spawn could not join group %q: %v. Nothing was created; make the request again.", call.Group, err), err)
		}
		if madeGroup {
			// Before the thread: a client that receives the thread's groupId
			// before the group row has nowhere to render it.
			t.app.emitThreadGroup("create", group)
			made.group = &group
		}
	}
	thread, cut, err := t.createSpawned(ctx, call, create, source, group.ID)
	if err != nil {
		if made.group != nil {
			t.dropSpawnGroup(*made.group)
		}
		return store.Thread{}, madeWithThread{}, err
	}
	made.cut = cut
	return thread, made, nil
}

// madeWithThread is what a request's creation made for its thread besides
// the thread: the worktree it cut and a group it made. Until the request
// names the thread they are the creation's to take back
// (discardUnnamedThread).
type madeWithThread struct {
	cut   threadapp.CutWorktree
	group *store.ThreadGroup
}

// dropSpawnGroup takes back a group a spawn made for a thread it then did
// not make, unless a thread joined it meanwhile.
func (t threadToolsApp) dropSpawnGroup(group store.ThreadGroup) {
	dropped, err := t.app.store.DropEmptyThreadGroup(group.ID)
	switch {
	case err != nil:
		log.Printf("thread tools: drop group %s a failed spawn made: %v", group.ID, err)
	case dropped:
		t.app.emitThreadGroup("delete", group)
	}
}

// spawnOptions is what a spawn resolved: the thread's options and, for a
// fresh worktree, the cut its name resolved to, which creation uses as is.
// The worktree fields of CreateThreadOptions stay empty.
type spawnOptions struct {
	CreateThreadOptions
	worktree *threadapp.WorktreeCut
	// owned says the naming write will own the thread: a local spawn
	// placed owned, or a paired computer's owned spawn through its link.
	owned bool
	// lineage is the thread the spawned thread works for
	// (threadRequestOrigin.lineage).
	lineage string
}

func (t threadToolsApp) createSpawned(
	ctx context.Context, call threadtools.SpawnCall, spawn spawnOptions, source store.Thread, groupID string,
) (store.Thread, threadapp.CutWorktree, error) {
	create := spawn.CreateThreadOptions
	create.GroupID = groupID
	if call.FromThread == "" {
		// createThreadCutting without its broadcast: acceptRequest announces
		// the row once its request has placed it.
		endWork, err := t.app.workAdmission.begin(ctx)
		if err != nil {
			return store.Thread{}, threadapp.CutWorktree{}, err
		}
		defer endWork()
		return t.app.createThreadRowCutting(ctx, create, spawn.worktree, false)
	}
	// Everything else on the new thread was resolved in spawnThreadOptions
	// from the caller's settings and the call's overrides, so it reaches
	// the fork here rather than being dropped for the source's.
	thread, err := t.app.forkThreadTail(ctx, source.ID, forkOptions{
		Mode:         create.Mode,
		RuntimeMode:  create.RuntimeMode,
		Title:        create.Title,
		Model:        create.Model,
		Effort:       create.ReasoningEffort,
		Owned:        spawn.owned,
		Group:        &groupID,
		Instructions: call.Instructions,
		SpawnLevel:   call.Level,
		Lineage:      spawn.lineage,
	})
	return thread, threadapp.CutWorktree{}, err
}

// spawnThreadOptions resolves what a spawn inherits and validates what it
// overrides, before anything durable exists.
//
// Every refusal names what this computer offers, because the caller cannot
// know another provider's model ids and a wrong guess should cost one call,
// not a discovery round trip.
func (t threadToolsApp) spawnThreadOptions(ctx context.Context, origin threadRequestOrigin, call threadtools.SpawnCall) (spawnOptions, error) {
	opts, err := t.resolveSpawnOptions(ctx, origin, call)
	if err != nil {
		return spawnOptions{}, err
	}
	// Judged here, on the mode the new thread will actually RUN in, because
	// this is the one place both spawn shapes resolve it: a fresh thread
	// reaches CreateThread's AuthorizeRuntimeMode hook, and a from_thread
	// fork never does. A local call carries no session and passes; a call a
	// paired computer forwarded is judged against that computer's grants,
	// the same rule the sidebar's own create obeys.
	if err := t.app.requireAutonomy(ctx, opts.RuntimeMode); err != nil {
		return spawnOptions{}, err
	}
	return opts, nil
}

// resolveSpawnOptions is the resolution itself, split from the gate above so
// the gate sees one answer for both shapes.
func (t threadToolsApp) resolveSpawnOptions(ctx context.Context, origin threadRequestOrigin, call threadtools.SpawnCall) (spawnOptions, error) {
	if call.FromThread != "" {
		opts, err := t.forkSpawnOptions(ctx, origin, call)
		if err == nil {
			err = refuseInstructionsUnappended(opts.Provider, call.Instructions)
		}
		return spawnOptions{CreateThreadOptions: opts}, err
	}
	// A spawn a paired computer forwarded has no caller thread here, so
	// there is no project or checkout to inherit; the call names them and
	// the tools layer refuses it beforehand when it does not.
	var caller store.Thread
	if !origin.foreign {
		var err error
		caller, err = t.localThread(origin.caller.ThreadID)
		if err != nil {
			return spawnOptions{}, err
		}
	}
	settings, err := t.spawnSettings(ctx, origin, call)
	if err != nil {
		return spawnOptions{}, err
	}
	if err := refuseInstructionsUnappended(settings.Provider, call.Instructions); err != nil {
		return spawnOptions{}, err
	}
	opts := spawnOptions{CreateThreadOptions: settings}
	opts.Title, opts.ProjectID = call.Title, caller.ProjectID
	opts.spawnInstructions, opts.spawnLevel = call.Instructions, call.Level
	if call.ProjectID != "" {
		if _, err := t.app.store.GetProject(call.ProjectID); err != nil {
			return spawnOptions{}, errorsx.Public(threadtools.CodeNotFound,
				fmt.Sprintf("There is no project %s on this computer. thread_options lists the projects it has.", call.ProjectID), err)
		}
		opts.ProjectID = call.ProjectID
	}
	owner, err := t.spawnOwner(caller)
	if err != nil {
		return spawnOptions{}, err
	}
	if owner != nil {
		if err := t.placeAssistantSpawn(&opts.CreateThreadOptions, *owner, call); err != nil {
			return spawnOptions{}, err
		}
	}
	if opts.ProjectID == "" && opts.home == "" {
		return spawnOptions{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A spawn needs a project: this thread has none to inherit. thread_options lists this computer's projects.", nil)
	}
	if err := checkSpawnPlacement(origin, call, opts.ProjectID); err != nil {
		return spawnOptions{}, err
	}
	if opts.home != "" {
		// A helper lives in its assistant's home, outside every project:
		// there is no checkout to pick or worktree to cut.
		return opts, nil
	}
	if err := t.applySpawnWorkspace(&opts, caller, call); err != nil {
		return spawnOptions{}, err
	}
	return opts, nil
}

// spawnSettings resolves the provider settings a fresh thread a call starts
// runs with: its caller's provider, model, mode, effort and runtime mode,
// with the call's overrides, checked against this computer's catalogs.
// Where the thread lives is the caller's to add. A spawn and an assistant
// that thread_new_assistant creates both take them.
func (t threadToolsApp) spawnSettings(ctx context.Context, origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	opts := CreateThreadOptions{
		Provider:        origin.inherit.Provider,
		Model:           origin.inherit.Model,
		Mode:            cmp.Or(call.Mode, origin.inherit.Mode),
		ReasoningEffort: origin.inherit.Effort,
		RuntimeMode:     cmp.Or(call.RuntimeMode, origin.inherit.RuntimeMode),
	}
	if err := t.applySpawnModel(ctx, &opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// spawnOwner is the assistant a local caller spawns for: the one whose
// current thread it is, or the one it is a helper of; nil for any other
// thread (docs/specs/standing-agents.md, ruling 38).
func (t threadToolsApp) spawnOwner(caller store.Thread) (*store.StandingAgent, error) {
	agentID := cmp.Or(caller.AgentID, caller.HelperOfAgentID)
	if agentID == "" {
		return nil, nil
	}
	agent, found, err := t.app.store.GetAgent(agentID)
	if err != nil || !found {
		return nil, err
	}
	return &agent, nil
}

// placeAssistantSpawn places a spawn for owner: into a project it serves,
// or, naming none, as a helper of its own work, outside every project in
// its home. A project it does not serve is refused naming the ones it does.
func (t threadToolsApp) placeAssistantSpawn(opts *CreateThreadOptions, owner store.StandingAgent, call threadtools.SpawnCall) error {
	if call.ProjectID == "" {
		if call.WorktreeBranch != "" || call.WorkspacePath != "" {
			return errorsx.Public(threadtools.CodeInvalidRequest,
				"A spawn without project_id is a helper of your own work in your home, so it takes no worktree or workspace_path. Name a project_id you serve to work in a checkout.", nil)
		}
		opts.ProjectID, opts.home, opts.helperOfAgentID = "", owner.Home, owner.ID
		return nil
	}
	return t.requireServedProject(owner, call.ProjectID)
}

// checkSpawnPlacement refuses a placement the spawn's thread could not
// keep, given projectID, the project it will live in: a fresh spawn's
// resolved project or a fork's source's. A thread outside every project
// (a helper, or a fork of a thread under an assistant) has no sidebar row
// to take and is shown only as its spawner's worker, so it takes no group
// or placement, and a paired computer's spawn, which has no spawner here
// to own it, cannot make one. Both spawn shapes run it before anything is
// created.
func checkSpawnPlacement(origin threadRequestOrigin, call threadtools.SpawnCall, projectID string) error {
	if projectID != "" {
		return nil
	}
	if origin.foreign {
		return errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"Thread %s is outside every project, so a fork of it is shown only as the worker of a thread on this computer, and a spawn from another computer has none. Fork it from a thread on its own computer.",
			call.FromThread), nil)
	}
	if call.Group == "" && (call.Placement == "" || call.Placement == threadtools.PlacementOwned) {
		return nil
	}
	if call.FromThread != "" {
		return errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"A fork lives where its source does, and %s is outside every project, so the fork is always your worker and takes no group or placement. Spawn a fresh thread with a project_id to give it a sidebar row.",
			call.FromThread), nil)
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"A spawn without project_id is a helper of your own work, outside every project and always your worker, so it takes no group or placement. Name a project_id you serve to give the thread a sidebar row there.", nil)
}

// requireServedProject refuses work for owner in a project it does not
// serve, naming the ones it does: a spawn or fork there, or an assistant it
// creates to serve it. What it serves is what its sessions are told it
// serves (store.ServedProjects): every unarchived project when it serves
// all of them, otherwise the ones it names.
func (t threadToolsApp) requireServedProject(owner store.StandingAgent, projectID string) error {
	served, err := t.app.store.ServedProjects(owner.ID)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(served))
	for _, project := range served {
		if project.ID == projectID {
			return nil
		}
		names = append(names, fmt.Sprintf("%s (%s)", project.Name, project.ID))
	}
	serves := "no project"
	if len(names) > 0 {
		serves = strings.Join(names, ", ")
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("This assistant serves %s, not project %s. Name a project it serves, or ask the user to add this one from the assistant's menu.", serves, projectID), nil)
}

// requireAssistantFork refuses an assistant's fork of a thread it could
// not have spawned: one in a project it does not serve, or one outside
// every project that lives under another assistant. A fork stays where its
// source is, so a fork of the assistant's own thread or helper is its
// helper.
func (t threadToolsApp) requireAssistantFork(owner store.StandingAgent, source store.Thread) error {
	if source.ProjectID != "" {
		return t.requireServedProject(owner, source.ProjectID)
	}
	if source.OwnerAgentID() != owner.ID {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("Thread %s is in no project you serve and does not live under you, so you cannot fork it.", source.ID), nil)
	}
	return nil
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
func (t threadToolsApp) forkSpawnOptions(ctx context.Context, origin threadRequestOrigin, call threadtools.SpawnCall) (CreateThreadOptions, error) {
	source, err := t.localThread(call.FromThread)
	if err != nil {
		return CreateThreadOptions{}, err
	}
	if !origin.foreign {
		caller, err := t.localThread(origin.caller.ThreadID)
		if err != nil {
			return CreateThreadOptions{}, err
		}
		owner, err := t.spawnOwner(caller)
		if err != nil {
			return CreateThreadOptions{}, err
		}
		if owner != nil {
			if err := t.requireAssistantFork(*owner, source); err != nil {
				return CreateThreadOptions{}, err
			}
		}
	}
	if err := checkSpawnPlacement(origin, call, source.ProjectID); err != nil {
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
	if err := t.applySpawnModel(ctx, &opts, call); err != nil {
		return CreateThreadOptions{}, err
	}
	return opts, nil
}

// applySpawnModel resolves provider, model and effort with the one
// resolution thread_options lists profiles by (threadtools.ResolveSpawn):
// opts' own as the spawner's, the call's as the choice, against what this
// computer offers, which is the read the thread is created with. The call's
// model is read by the provider the thread runs on (threadtools.CallChoice):
// a family runs the newest member this computer lists now. This is the
// spawn's only resolution and only refusal; the caller resolves first only
// for a paired computer that predates families (checkSpawnChoice).
func (t threadToolsApp) applySpawnModel(ctx context.Context, opts *CreateThreadOptions, call threadtools.SpawnCall) error {
	catalog, err := t.Catalog(ctx, threadtools.CatalogQuery{What: threadtools.OptionsModels})
	if err != nil {
		return err
	}
	spawner := threadtools.SpawnDefaults{Provider: opts.Provider, Model: opts.Model, Effort: opts.ReasoningEffort}
	resolved, err := threadtools.ResolveSpawn(spawner, threadtools.CallChoice(call, nil, spawner), catalog.Providers, "this computer")
	if err != nil {
		return threadtools.RefuseSpawnChoice(call.Profile, err)
	}
	opts.Provider, opts.Model, opts.ReasoningEffort = resolved.Provider, resolved.Model, resolved.Effort
	return nil
}

// applySpawnWorktree runs the thread in the worktree of call's branch or
// tag. A branch already checked out runs where it is, because git checks a
// branch out in one place at a time; that place is the project root when
// the root has it. Any other name is resolved here, once
// (resolveWorktreeTarget), and thread creation cuts the answer: a branch
// that exists locally or on a remote is checked out, a tag is checked out
// detached with base and base_local refused, and a name that is neither
// becomes a branch created from base. refs/heads/<name> names a branch
// only.
func (t threadToolsApp) applySpawnWorktree(opts *spawnOptions, call threadtools.SpawnCall) error {
	project, worktrees, err := t.spawnCheckouts(opts.ProjectID)
	if err != nil {
		return err
	}
	named, err := gitops.ParseRef(call.WorktreeBranch)
	if err != nil {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("worktree %q is not a branch or tag name: %v", call.WorktreeBranch, err), err)
	}
	newBranch := call.WorktreeBase != "" || call.WorktreeBaseLocal
	if named.IsBranch() {
		for _, worktree := range worktrees {
			if worktree.Branch != named.Name {
				continue
			}
			if newBranch {
				return spawnBranchExists(call, "is checked out at "+worktree.Path)
			}
			opts.WorkspaceOverride = worktree.Path
			if !gitops.SameFilesystemPath(worktree.Path, project.Path) {
				opts.WorktreePath = worktree.Path
				opts.Branch = worktree.Branch
			}
			return nil
		}
	}
	// Creation uses this answer as is.
	target, err := t.app.resolveWorktreeTarget(t.app.lifeCtx(), project.Path, call.WorktreeBranch)
	if err != nil {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("worktree %q cannot be used: %v", call.WorktreeBranch, err), err)
	}
	switch {
	case target.Ref.IsTag() && !target.Exists:
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("This computer has no tag %q as of its last fetch from origin, which brings only tags on the commits it fetches. Name an existing tag, or a branch.", target.Ref.Name), nil)
	case target.Ref.IsTag() && newBranch:
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%q is a tag, so worktree checks it out as it is, detached at the tag, and base and base_local do not apply. Drop them to work at that tag, or name a new branch and pass the tag as base to start one from it.",
				target.Ref.Name), nil)
	case target.Exists && newBranch:
		return spawnBranchExists(call, "already exists")
	}
	base, err := t.resolveSpawnWorktreeBase(project.Path, call)
	if err != nil {
		return err
	}
	opts.worktree = &threadapp.WorktreeCut{Target: target, Base: base, BaseLocal: call.WorktreeBaseLocal}
	return nil
}

// spawnBranchExists refuses base or base_local for a branch that exists:
// they describe where a new branch starts, and this one has its commits.
func spawnBranchExists(call threadtools.SpawnCall, where string) error {
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("Branch %q %s, so worktree checks it out as it is and base and base_local do not apply. Drop them to work on that branch, or name a new branch to start one from base.",
			call.WorktreeBranch, where), nil)
}

// spawnCheckouts reads a project's checkouts from git: the root first,
// then its linked worktrees.
func (t threadToolsApp) spawnCheckouts(projectID string) (store.Project, []gitops.Worktree, error) {
	project, err := t.app.store.GetProject(projectID)
	if err != nil {
		return store.Project{}, nil, err
	}
	worktrees, err := t.app.gitCore().ListWorktrees(project.Path)
	if err != nil {
		return store.Project{}, nil, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("The checkouts of %s cannot be read: %v", project.Path, err), err)
	}
	return project, worktrees, nil
}

// resolveSpawnWorktreeBase resolves a named base to its wire ref, a branch
// first and then a tag, and refuses one the project does not have before
// the request exists, naming what would have worked. A branch only origin
// has cannot seed a base_local cut, so that case is refused too; a tag is
// local and serves either way.
func (t threadToolsApp) resolveSpawnWorktreeBase(projectPath string, call threadtools.SpawnCall) (string, error) {
	if call.WorktreeBase == "" {
		return "", nil
	}
	base, found, err := t.app.gitCore().ResolveRef(projectPath, call.WorktreeBase, call.WorktreeBaseLocal)
	if err != nil {
		return "", errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("base %q cannot be used: %v", call.WorktreeBase, err), err)
	}
	if found {
		return base.Wire(), nil
	}
	if call.WorktreeBaseLocal {
		return "", errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("This computer has no local branch or tag %q to start from. Omit base_local to start from origin's head of a branch, or name a local branch or a tag.", call.WorktreeBase), nil)
	}
	return "", errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("Neither this computer nor origin has a branch %q as of the last fetch, and this computer has no tag of that name. Name an existing branch or tag as base.", call.WorktreeBase), nil)
}

// applySpawnWorkspace picks the checkout the new thread runs in and refuses
// a worktree git still registers whose folder is gone, naming it: thread
// creation refuses it too, in prose the model never sees.
func (t threadToolsApp) applySpawnWorkspace(opts *spawnOptions, caller store.Thread, call threadtools.SpawnCall) error {
	if err := t.pickSpawnWorkspace(opts, caller, call); err != nil {
		return err
	}
	if opts.WorktreePath != "" && worktreeapp.DirectoryGone(opts.WorktreePath) {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("The worktree at %s is registered with git but its folder is gone, so nothing can run there. Name another checkout or branch, or remove that worktree first.",
				opts.WorktreePath), nil)
	}
	return nil
}

// pickSpawnWorkspace resolves the checkout: the worktree of a named branch,
// a named checkout of the project, or the caller's own. Checkouts are read
// from git, as the sidebar's picker reads them, so a worktree made outside
// the app is one too.
func (t threadToolsApp) pickSpawnWorkspace(opts *spawnOptions, caller store.Thread, call threadtools.SpawnCall) error {
	if call.WorktreeBranch != "" {
		return t.applySpawnWorktree(opts, call)
	}
	if call.WorkspacePath != "" {
		project, worktrees, err := t.spawnCheckouts(opts.ProjectID)
		if err != nil {
			return err
		}
		paths := []string{project.Path}
		if gitops.SameFilesystemPath(call.WorkspacePath, project.Path) {
			opts.WorkspaceOverride = project.Path
			return nil
		}
		for _, worktree := range worktrees {
			if gitops.SameFilesystemPath(worktree.Path, project.Path) {
				continue
			}
			paths = append(paths, worktree.Path)
			if gitops.SameFilesystemPath(worktree.Path, call.WorkspacePath) {
				opts.WorkspaceOverride = worktree.Path
				opts.WorktreePath = worktree.Path
				opts.Branch = worktree.Branch
				return nil
			}
		}
		return errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That project has no checkout at %s. It has: %s. Pass worktree with a branch name to check that branch out in a fresh worktree instead.",
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
