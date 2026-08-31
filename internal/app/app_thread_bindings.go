package app

import (
	"context"
	"fmt"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// CreateThreadOptions carries the minimum a caller MUST specify plus an
// optional override bundle for any field that defaults from settings.
// Using a struct (not 10 positional args) means a future field doesn't
// break callers. ProjectID is required; every thread must belong to a
// project as of v13.
//
// Title / WorktreePath / Branch are direct overrides used by paths that
// inherit a sibling thread's workspace (notably "Implement plan in new
// thread"). Setting WorktreePath skips the WorktreeBranch git operation
// entirely — the new thread reuses the existing worktree and the supplied
// Branch verbatim, with no git lookups.
type CreateThreadOptions struct {
	ProjectID                  string `json:"projectId"`
	Title                      string `json:"title,omitempty"`                      // empty = "New Thread"
	Provider                   string `json:"provider,omitempty"`                   // empty = latest chat profile provider
	Model                      string `json:"model,omitempty"`                      // empty = latest provider/model profile
	Mode                       string `json:"mode,omitempty"`                       // defaults to chat
	ReasoningEffort            string `json:"reasoningEffort,omitempty"`            // empty = latest model profile effort
	FastMode                   *bool  `json:"fastMode,omitempty"`                   // nil = latest model profile fast-mode
	ContextWindow              int    `json:"contextWindow,omitempty"`              // 0 = latest model profile context
	AutoCompactStandardPercent *int   `json:"autoCompactStandardPercent,omitempty"` // nil = latest model profile compact setting
	AutoCompactExtendedPercent *int   `json:"autoCompactExtendedPercent,omitempty"` // nil = latest model profile compact setting
	RuntimeMode                string `json:"runtimeMode,omitempty"`                // empty = latest model profile runtime mode
	WorktreeBranch             string `json:"worktreeBranch,omitempty"`             // empty = no worktree
	WorkspaceOverride          string `json:"workspaceOverride,omitempty"`          // empty = project.path
	WorktreePath               string `json:"worktreePath,omitempty"`               // non-empty = inherit existing worktree, skip git ops
	Branch                     string `json:"branch,omitempty"`                     // non-empty = use directly, skip currentGitBranch lookup
}

// StartTerminalOptions selects where a new terminal thread is rooted.
// ProjectID empty roots a standalone "home" terminal at the user's home
// directory with a NULL project; otherwise the terminal roots at the
// project path and is listed under the project. Cwd overrides the
// resolved root when the caller already knows the working directory
// (e.g. the chat header's ctrl-click passes the source thread's
// workspace). Title empty defaults to "Terminal".
type StartTerminalOptions struct {
	ProjectID string `json:"projectId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
}

// ThreadDefaults reports the seed values CreateThread would have used
// for a fresh thread in the given project. The frontend reads it when
// staging an in-memory draft placeholder so the toolbar (model, effort,
// runtime mode) and the workspace strip (current git branch) render
// the same values the materialized thread would carry. Returned model
// is already normalized; reasoning effort is already coerced to a
// supported value for (provider, model).
type ThreadDefaults struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	FastMode        bool   `json:"fastMode"`
	ContextWindow   int    `json:"contextWindow"`
	RuntimeMode     string `json:"runtimeMode"`
	Branch          string `json:"branch"`
	WorkspacePath   string `json:"workspacePath"`
}

// ThreadModeChangedEvent is emitted whenever a thread's mode is updated
// while a provider session is active. The frontend uses NeedsReconnect
// to show a toast that prompts the user to reconnect — the running
// session was started under the previous mode and will keep using it
// until a new session is spawned.
type ThreadModeChangedEvent struct {
	ThreadID       string `json:"threadId"`
	Mode           string `json:"mode"`
	NeedsReconnect bool   `json:"needsReconnect"`
}

// ThreadTitleGenerationEvent is the completion frame of one title
// generation run (auto, heal, or user-triggered regeneration).
// Error is redacted (textgen.RedactError) and empty on success —
// including the no-op outcomes (nothing to title, model returned
// nothing better, a rename won the CAS).
type ThreadTitleGenerationEvent struct {
	ThreadID string `json:"threadId"`
	Error    string `json:"error"`
}

// CreateThread persists a new thread rooted at a project. The options
// struct carries every knob. Empty chat-bar fields are resolved from the
// most recently used persisted chat model profile; if no profile exists yet,
// built-in provider defaults seed the first draft. Mode defaults to chat so
// every normal new thread starts as a chat thread. "discussion" is rejected
// as mode values because discussion/workflow threads must come through their
// owning coordination sagas, which wire channels or phase run records.
//
// The created row is broadcast as `listed` so a second attached client puts
// it in its sidebar without a refresh, matching what the creating client does
// with the return value.
func (a *App) CreateThread(ctx context.Context, opts CreateThreadOptions) (store.Thread, error) {
	thread, err := a.threadApplication().Create(threadapp.CreateOptions{
		ProjectID:                  opts.ProjectID,
		Title:                      opts.Title,
		Provider:                   opts.Provider,
		Model:                      opts.Model,
		Mode:                       opts.Mode,
		ReasoningEffort:            opts.ReasoningEffort,
		FastMode:                   opts.FastMode,
		ContextWindow:              opts.ContextWindow,
		AutoCompactStandardPercent: opts.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: opts.AutoCompactExtendedPercent,
		RuntimeMode:                opts.RuntimeMode,
		WorktreeBranch:             opts.WorktreeBranch,
		WorkspaceOverride:          opts.WorkspaceOverride,
		WorktreePath:               opts.WorktreePath,
		Branch:                     opts.Branch,
		CreatedByDevice:            creatingDevice(ctx),
	})
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRow(triage.ThreadActionListed, thread)
	return thread, nil
}

// StartTerminal mints a persistent terminal-mode thread and returns it.
// A terminal is a first-class sidebar entity that never runs a provider
// session: it carries a CHECK-valid sentinel provider/model/effort purely
// so the threads table's coupled (provider, reasoning_effort) constraint
// passes — no session is ever started from it, and seedChatModelProfile is
// read-only so the sentinel is NOT remembered as a user model choice.
//
// It deliberately does NOT spawn a PTY: the frontend opens one via
// OpenTerminal on pane mount, which is also why a terminal thread restored
// after restart re-spawns a fresh shell in its saved workspace (PTYs are
// ephemeral across restart; the saved cwd is what persists).
func (a *App) StartTerminal(ctx context.Context, opts StartTerminalOptions) (store.Thread, error) {
	thread, err := a.threadApplication().StartTerminal(threadapp.TerminalOptions{
		ProjectID:       opts.ProjectID,
		Cwd:             opts.Cwd,
		Title:           opts.Title,
		CreatedByDevice: creatingDevice(ctx),
	})
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRow(triage.ThreadActionListed, thread)
	return thread, nil
}

// GetThreadDefaults returns the values CreateThread would have seeded
// for a fresh thread in the given project. Used by the frontend's draft
// placeholder flow so "+ New" surfaces a populated toolbar (model name,
// effort) and a real branch label before the row exists. Mode is
// accepted only for symmetry with CreateThread — defaults don't vary by
// mode today, but accepting it keeps the wire shape stable if that
// changes.
func (a *App) GetThreadDefaults(opts CreateThreadOptions) (ThreadDefaults, error) {
	defaults, err := a.threadApplication().Defaults(threadapp.CreateOptions{
		ProjectID: opts.ProjectID,
		Provider:  opts.Provider,
		Model:     opts.Model,
		Mode:      opts.Mode,
	})
	if err != nil {
		return ThreadDefaults{}, err
	}
	return ThreadDefaults{
		Provider:        defaults.Provider,
		Model:           defaults.Model,
		ReasoningEffort: defaults.ReasoningEffort,
		FastMode:        defaults.FastMode,
		ContextWindow:   defaults.ContextWindow,
		RuntimeMode:     defaults.RuntimeMode,
		Branch:          defaults.Branch,
		WorkspacePath:   defaults.WorkspacePath,
	}, nil
}

// ListThreads backs the frontend sidebar. It deliberately excludes
// "draft" threads (newly created but never sent) so the sidebar stays
// clean: a thread only becomes visible once its first item lands.
// Internal callers that need every thread (tests, fork inspection,
// discussion runtime) go through a.store.ListThreads directly.
func (a *App) ListThreads() ([]store.Thread, error) { return a.threadApplication().List() }

// ListArchivedThreads returns every archived thread for the settings panel.
func (a *App) ListArchivedThreads() ([]store.Thread, error) {
	return a.threadApplication().ListArchived()
}

// GetThread returns a single thread row.
func (a *App) GetThread(id string) (store.Thread, error) { return a.threadApplication().Get(id) }

// ArchiveThread / UnarchiveThread live in app_thread_archive.go: archiving
// also releases the thread's host resources, which is more than a row flip.

// RenameThread updates the thread title and emits a thread:updated
// event so any other observer (chat header, multi-tab clients, future
// remote viewers) re-renders with the new title without a follow-up
// poll. Mirrors the emit shape used by applyThreadTitleIfCurrent.
func (a *App) RenameThread(id string, title string) error {
	if err := a.threadApplication().Rename(id, title); err != nil {
		return err
	}
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
		Action: triage.ThreadActionPatch, ID: id, Title: &title,
	})
	a.syncPeerSessionNameAsync(id)
	return nil
}

// MarkThreadRead stamps the thread's last_read_at to at least its latest
// completed turn so the sidebar stops showing a Completed pill. The timestamp
// is owned by the store so the App layer stays free of nowMillis() calls,
// matching ArchiveThread / UpdateTitle etc.
//
// The deadline is the App's to supply, not the caller's: bound methods
// are the frontend's wire surface and take no context. Five seconds
// matches the store's busy_timeout — a wait past that is a wedged writer,
// and the frontend has already patched its own read state optimistically,
// so returning the error beats holding the RPC open.
//
// Broadcasts the stamped row so the other devices' sidebars stop showing the
// pill too. Opening a thread whose marker already covers its newest turn
// changes nothing and says nothing, which is what keeps every thread switch
// off the wire.
func (a *App) MarkThreadRead(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), markThreadReadTimeout)
	defer cancel()
	row, changed, err := a.threadApplication().MarkRead(ctx, id)
	if err != nil {
		return err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	return nil
}

// MarkThreadUnread stamps last_read_at to zero. NULL is reserved for
// "never tracked" rows that should not flood the sidebar as unread after
// migration; explicit unread uses a concrete old timestamp.
func (a *App) MarkThreadUnread(id string) error {
	row, changed, err := a.threadApplication().MarkUnread(id)
	if err != nil {
		return err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	return nil
}

// PinThread marks the thread as front-burner pinned. Pinned threads sort into
// two manual attention groups above needs-attention.
func (a *App) PinThread(id string) (store.Thread, error) {
	row, changed, err := a.threadApplication().Pin(id)
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	return row, nil
}

// SetThreadPinGroup moves an already-pinned thread between the front and back
// burners and returns the refreshed row for frontend reconciliation.
func (a *App) SetThreadPinGroup(id string, group int) (store.Thread, error) {
	row, changed, err := a.threadApplication().SetPinGroup(id, group)
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	return row, nil
}

// UnpinThread clears pinned_at and pin_group and returns the refreshed row.
func (a *App) UnpinThread(id string) (store.Thread, error) {
	row, changed, err := a.threadApplication().Unpin(id)
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	return row, nil
}

// UpdateThreadProvider switches to the provider's latest remembered profile
// and restarts the session if one is live so the new provider takes effect.
//
// A thread is locked to its provider once any item has been persisted:
// provider sessions are not interchangeable (Codex can't resume a Claude
// rollout and vice versa), so an in-flight switch would surface as a
// "thread/resume: no rollout found" error from the reconnect goroutine
// and leave the thread in a confusing half-switched state. Reject at the
// binding boundary with a user-facing message instead. Same-provider
// calls short-circuit so a UI that re-sends the current provider on
// every model change doesn't trip the lock.
func (a *App) UpdateThreadProvider(id, providerName string) (store.Thread, error) {
	update, err := a.threadApplication().UpdateProvider(id, providerName)
	if err != nil {
		return store.Thread{}, err
	}
	return a.finishThreadModelUpdate(update)
}

// UpdateThreadModel changes a thread's model and restarts an active provider
// session so the new model takes effect immediately. Threads without an active
// session are updated in place and will use the new model on the next start.
func (a *App) UpdateThreadModel(threadID string, model string) (store.Thread, error) {
	update, err := a.threadApplication().UpdateModel(threadID, model)
	if err != nil {
		return store.Thread{}, err
	}
	return a.finishThreadModelUpdate(update)
}

// UpdateThreadModelSelection changes provider + model as one atomic model-menu
// selection. The selected provider/model's remembered profile is applied before
// the thread row is persisted, so SQLite never sees an invalid intermediate
// provider/effort pair such as codex + max.
func (a *App) UpdateThreadModelSelection(threadID string, providerName string, model string) (store.Thread, error) {
	update, err := a.threadApplication().UpdateModelSelection(threadID, providerName, model)
	if err != nil {
		return store.Thread{}, err
	}
	return a.finishThreadModelUpdate(update)
}

// UpdateThreadReasoningEffort persists the effort tier and reconciles a
// live session (Codex applies it on the next turn without a restart;
// Claude needs a restart, deferred until the thread is quiet).
func (a *App) UpdateThreadReasoningEffort(id, effort string) (store.Thread, error) {
	_, changed, err := a.threadApplication().UpdateReasoningEffort(id, effort)
	if err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "effort")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	// The row broadcast is the row returned, read after the session
	// reconcile: the initiator's optimistic apply and its own echo then
	// carry identical bytes.
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, refreshed, changed)
	return refreshed, nil
}

// UpdateThreadFastMode persists the fast-mode boolean and reconciles a
// live session (Codex maps it to the per-turn serviceTier override; the
// Claude CLI only reads fast mode from launch settings, so a Claude
// session restarts — deferred until the thread is quiet).
func (a *App) UpdateThreadFastMode(id string, on bool) (store.Thread, error) {
	_, changed, err := a.threadApplication().UpdateFastMode(id, on)
	if err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "fastMode")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	a.broadcastThreadRowIfChanged(triage.ThreadActionFull, refreshed, changed)
	return refreshed, nil
}

// UpdateThreadBranch persists a branch observed in workspacePath onto every
// thread row currently in that workspace. Does NOT perform the git checkout
// — that flow lives in GitCheckout. This is the narrow metadata binding the
// frontend's observed-branch reconciliation writes through.
//
// The branch is a property of the workspace, not of a thread: two threads on
// one worktree see the same checkout, so writing only the observing thread's
// row leaves the other claiming a branch the working tree left behind.
// Keying the write on the workspace is also what makes it safe to issue
// without a thread lock — a thread that moved since the observation is no
// longer matched. See store.UpdateBranchForWorkspace.
//
// Returns exactly the rows whose branch CHANGED, so the caller syncs only
// what moved. None is the ordinary answer: the frontend writes on every
// attach and the observed branch usually already matches what is cached.
//
// The write matches BOTH the spelling the caller observed and its
// symlink-resolved form, because thread rows carry whichever spelling was
// current when they were created (a worktree cut through a symlinked path
// stores that path) while the observing client knows only its own. Matching
// one of the two left half the rows in a workspace claiming a branch the
// working tree had left behind.
//
// A changed row is broadcast on `thread:updated`. The CALLER heals from the
// return value, but a second client's identical write matches zero rows —
// the first one already moved them — so without the broadcast its panes
// would keep the superseded branch until something else refreshed them. A
// write that changed nothing emits nothing.
func (a *App) UpdateThreadBranch(workspacePath, branch string) ([]store.Thread, error) {
	rows, err := a.threadApplication().UpdateBranch(workspacePath, branch)
	if err != nil {
		return nil, err
	}
	for index := range rows {
		a.broadcastThreadRow(triage.ThreadActionFull, rows[index])
	}
	return rows, nil
}

// DeleteThread tears down the thread and any child threads. The recursive
// cascade logic lives in app_thread_delete.go.
//
// Every dropped row — the named thread and each of its children — is
// broadcast as `deleted` so a second attached client drops the same rows and
// their caches without a refresh. The fan-out rides the delete port's
// per-row Deleted callback, which is the only place the child ids are known.
func (a *App) DeleteThread(id string) error {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	return a.deleteThreadTreeLocked(id)
}

// UpdateThreadContextWindow persists the context window size and
// reconciles a live session (context window is spawn-time config on both
// providers, so this restarts — deferred until the thread is quiet).
func (a *App) UpdateThreadContextWindow(id string, tokens int) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update context window: store unavailable")
	}
	thread, err := a.store.GetThread(id)
	if err != nil {
		return store.Thread{}, err
	}
	return a.UpdateThreadContextSettings(id, ContextSettingsUpdate{
		Provider:                   thread.Provider,
		Model:                      thread.Model,
		ContextWindow:              tokens,
		AutoCompactStandardPercent: thread.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: thread.AutoCompactExtendedPercent,
	})
}

// UpdateThreadRuntimeMode persists the runtime mode and reconciles a live
// session (live on both providers, except escalating a Claude session to
// full access — that restarts, deferred until the thread is quiet).
func (a *App) UpdateThreadRuntimeMode(id, mode string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update runtime mode: store unavailable")
	}
	normalized, err := threadmode.ParseRuntime(mode)
	if err != nil {
		return store.Thread{}, fmt.Errorf("update runtime mode: %w", err)
	}
	if err := a.applyRuntimeMode(id, normalized); err != nil {
		return store.Thread{}, fmt.Errorf("update runtime mode: %w", err)
	}
	thread, err := a.store.GetThread(id)
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(thread)
	return thread, nil
}

// UpdateThreadWorkspace persists a new workspace path. Used by the
// EnvPicker to switch a thread between the project root and a worktree
// without creating the worktree itself. Restarts the session if one is
// live because the provider CWD is part of its launch config.
func (a *App) UpdateThreadWorkspace(id, path string) (store.Thread, error) {
	return a.switchThreadWorkspace(id, path)
}

// UpdateThreadMode updates the thread's mode and returns the refreshed
// row. Rejects unknown modes with a clear error message so the frontend
// can surface it verbatim.
//
// Active Claude sessions apply chat/plan changes immediately through
// set_permission_mode. Codex reads the persisted mode on the next turn/start.
// Modes that cannot be applied to the live session emit NeedsReconnect=true.
// The persisted row is always updated regardless.
func (a *App) UpdateThreadMode(threadID string, mode string) (store.Thread, error) {
	update, err := a.threadApplication().UpdateMode(threadID, mode)
	if err != nil {
		return store.Thread{}, err
	}
	needsReconnect := false
	sess, sessionActive := a.sessionManager().get(threadID)
	if sessionActive {
		needsReconnect = a.applyActiveModeChange(threadID, sess, provider.NormalizeInteractionMode(update.Mode))
	}
	a.emitEvent(eventchan.ThreadModeChanged, ThreadModeChangedEvent{
		ThreadID: threadID, Mode: update.Mode, NeedsReconnect: needsReconnect,
	})
	return update.Thread, nil
}

// CreateThreadFromPR creates a new thread seeded with a PR/MR's metadata +
// diff as the first user message. Routes through the appropriate forge CLI
// (`gh` for GitHub, `glab` for GitLab) detected from the `forge` parameter.
//
// Parameters:
//   - project:       "owner/repo" for GitHub, "namespace/.../repo" for GitLab
//   - number:        PR / MR number
//   - providerName + model: provider + model for the new thread
//   - forge:         "github" (default for empty) or "gitlab"
//
// If the user has a local clone of the target repo registered in
// settings.RecentWorkspaces, that path is auto-selected as the workspace.
// Otherwise the caller is expected to pick a workspace; we still create the
// thread but WorkspacePath is left empty and the UI can prompt.
func (a *App) CreateThreadFromPR(
	ctx context.Context,
	project string,
	number int,
	providerName string,
	model string,
	forge string,
) (store.Thread, error) {
	thread, err := a.threadApplication().CreateFromPR(
		threadapp.PullRequestOptions{
			Project:         project,
			Number:          number,
			Provider:        providerName,
			Model:           model,
			Forge:           forge,
			CreatedByDevice: creatingDevice(ctx),
		},
		threadPullRequestPort{app: a},
	)
	if err != nil {
		return store.Thread{}, err
	}
	a.broadcastThreadRow(triage.ThreadActionListed, thread)
	return thread, nil
}

// RegenerateThreadTitle starts a re-title of an existing thread from its
// conversation so far. It acknowledges as soon as the run is under way;
// the outcome arrives on `thread:title_generation`, and the new title
// itself on `thread:updated` when the swap lands.
//
// Asynchronous on purpose. A generation is up to two provider attempts
// of threadtitle.Timeout each, against a transport client timeout of
// 60s: the synchronous form was guaranteed to be abandoned by the caller
// while the backend kept running, and the abandoned caller re-enabled
// its button and stacked retries on top.
//
// An unknown thread is the only synchronous failure. Everything else —
// an empty thread, a model that produced nothing better, a rename that
// won the compare-and-swap, a provider failure — is reported on the
// completion event. A regeneration asked for while one is already
// running for this thread joins it: the running generation's completion
// event is the answer for both callers.
func (a *App) RegenerateThreadTitle(threadID string) error {
	return a.threadTitleApplication().Regenerate(threadID)
}

// finishThreadModelUpdate is the shared tail of the three model-selection
// bindings: reconcile the live session, then broadcast the row those bindings
// return. `SelectionChanged` is the no-change test, so re-selecting the model
// a thread already carries reconciles nothing and broadcasts nothing.
func (a *App) finishThreadModelUpdate(update threadapp.ModelUpdate) (store.Thread, error) {
	if !update.SelectionChanged() {
		a.rememberChatModelProfile(update.Thread)
		return update.Thread, nil
	}
	if update.ProviderChanged() && a.triage != nil {
		if err := a.triage.ResetThreadTodo(update.Thread.ID); err != nil {
			log.Printf("thread %s: reset todo list on provider switch: %v", update.Thread.ID, err)
		}
	}
	a.reconcileSessionConfig(update.Thread.ID)
	updated, err := a.threadApplication().Get(update.Thread.ID)
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(updated)
	a.broadcastThreadRow(triage.ThreadActionFull, updated)
	return updated, nil
}
