package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// --- Thread CRUD and lifecycle ---

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

// CreateThread persists a new thread rooted at a project. The options
// struct carries every knob. Empty chat-bar fields are resolved from the
// most recently used persisted chat model profile; if no profile exists yet,
// built-in provider defaults seed the first draft. Mode defaults to chat so
// every normal new thread starts as a chat thread. "discussion" is rejected
// as mode values because discussion/workflow threads must come through their
// owning coordination sagas, which wire channels or phase run records.
func (a *App) CreateThread(opts CreateThreadOptions) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("create thread: store unavailable")
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	if projectID == "" {
		return store.Thread{}, fmt.Errorf("create thread: projectId is required")
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create thread: resolve project %s: %w", projectID, err)
	}

	mode, err := threadmode.ValidateCreate(opts.Mode)
	if err != nil {
		return store.Thread{}, err
	}

	seed := a.seedChatModelProfile(opts.Provider, opts.Model)
	providerName := seed.Provider
	model := seed.Model
	effort := seed.ReasoningEffort
	fastMode := seed.FastMode
	contextWindow := seed.ContextWindow
	// Compact thresholds are per-provider settings, not per-(provider,model)
	// remembered defaults. New threads start without a per-thread override
	// (0) so the session-start resolution path reads the live Settings
	// value. Explicit caller overrides via opts.AutoCompact* still apply.
	autoCompactStandardPercent := 0
	autoCompactExtendedPercent := 0
	runtimeMode := seed.RuntimeMode

	if trimmed := strings.TrimSpace(opts.Provider); trimmed != "" {
		providerName = trimmed
	}
	if trimmed := strings.TrimSpace(opts.Model); trimmed != "" {
		model = trimmed
	}
	model = provider.NormalizeModelSlug(providerName, model)
	if trimmed := strings.TrimSpace(opts.ReasoningEffort); trimmed != "" {
		effort = trimmed
	}
	if opts.FastMode != nil {
		fastMode = *opts.FastMode
	}
	if opts.ContextWindow != 0 {
		contextWindow = opts.ContextWindow
	}
	if opts.AutoCompactStandardPercent != nil {
		autoCompactStandardPercent = *opts.AutoCompactStandardPercent
	}
	if opts.AutoCompactExtendedPercent != nil {
		autoCompactExtendedPercent = *opts.AutoCompactExtendedPercent
	}
	if trimmed := strings.TrimSpace(opts.RuntimeMode); trimmed != "" {
		parsedRuntimeMode, err := threadmode.ParseRuntime(trimmed)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: %w", err)
		}
		runtimeMode = string(parsedRuntimeMode)
	}
	if trimmed := strings.TrimSpace(opts.ReasoningEffort); trimmed != "" {
		if !a.reasoningEffortSupportedForModel(providerName, model, trimmed) {
			return store.Thread{}, fmt.Errorf("create thread: unsupported reasoning effort %q for %s/%s", trimmed, providerName, model)
		}
	} else {
		effort = a.coerceReasoningEffortForModel(providerName, model, effort)
	}
	if fastMode && !a.supportsFastModeForModel(providerName, model) {
		if opts.FastMode != nil {
			return store.Thread{}, fmt.Errorf("create thread: fast mode unsupported for %s/%s", providerName, model)
		}
		fastMode = false
	}
	options := chatmodel.ContextWindowOptions(providerName, model)
	if opts.ContextWindow != 0 && len(options) > 0 && !chatmodel.ContextWindowSupported(options, contextWindow) {
		return store.Thread{}, fmt.Errorf("create thread: unsupported context window %d for %s/%s", contextWindow, providerName, model)
	}
	if len(options) > 0 && !chatmodel.ContextWindowSupported(options, contextWindow) {
		contextWindow = provider.DefaultContextWindowForModel(providerName, model, 0)
	}
	if !provider.IsValidAutoCompactPercent(autoCompactStandardPercent) {
		return store.Thread{}, fmt.Errorf("create thread: auto-compact standard percent must be between 0 and 90")
	}
	if !provider.IsValidAutoCompactPercent(autoCompactExtendedPercent) {
		return store.Thread{}, fmt.Errorf("create thread: auto-compact extended percent must be between 0 and 90")
	}

	workspace := strings.TrimSpace(opts.WorkspaceOverride)
	if workspace == "" {
		workspace = project.Path
	}
	var branch, worktreePath string
	// cutWorktree records that THIS call created the worktree, which is what
	// makes running the project's setup recipe over it safe. Inheriting a
	// sibling's worktree does not qualify: setup already ran there, and
	// re-running an argv recipe over a checkout someone else is working in is
	// not something a thread creation should decide to do.
	cutWorktree := false
	inheritWorktree := strings.TrimSpace(opts.WorktreePath)
	switch {
	case inheritWorktree != "":
		// Inherit a sibling thread's existing worktree. We deliberately
		// skip git creation/lookup, but the path itself is validated
		// against the project's known worktrees so the caller can't spawn
		// a provider session inside an arbitrary directory like ~/.ssh
		// or /etc. Used by "Implement plan in new thread".
		worktree, ok, err := a.findWorktree(project.Path, inheritWorktree)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: validate inherited worktree: %w", err)
		}
		if !ok {
			return store.Thread{}, fmt.Errorf("create thread: %q is not a worktree of project %s", inheritWorktree, project.Path)
		}
		workspace = worktree.Path
		worktreePath = worktree.Path
		branch = strings.TrimSpace(opts.Branch)
		if branch == "" {
			branch = worktree.Branch
		} else {
			branch = gitops.SanitizeBranchNamePreservingSlashes(branch)
		}
	case strings.TrimSpace(opts.WorktreeBranch) != "":
		// Worktree creation runs through the git core with the project
		// path as the repo root. On success the thread's workspace
		// becomes the worktree directory.
		wtPath, wtBranch, err := a.createWorktreeForNewThread(project.Path, strings.TrimSpace(opts.WorktreeBranch))
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: worktree: %w", err)
		}
		workspace = wtPath
		worktreePath = wtPath
		branch = wtBranch
		cutWorktree = true
	default:
		// No worktree path supplied. If the caller passes an explicit
		// branch (e.g. for the project root), sanitize it the same way
		// we'd sanitize user-typed branch names elsewhere — preserves
		// `feature/login` and branch-name casing while collapsing shell-meta.
		if explicit := strings.TrimSpace(opts.Branch); explicit != "" {
			branch = gitops.SanitizeBranchNamePreservingSlashes(explicit)
		}
	}
	if branch == "" {
		branch = a.gitCore().CurrentBranch(workspace)
	}

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "New Thread"
	}

	now := time.Now().UnixMilli()
	t := store.Thread{
		ID:                         uuid.New().String(),
		ProjectID:                  project.ID,
		ProjectPath:                project.Path,
		Title:                      title,
		Provider:                   providerName,
		WorkspacePath:              workspace,
		WorktreePath:               worktreePath,
		Branch:                     branch,
		Model:                      model,
		Mode:                       mode,
		ReasoningEffort:            effort,
		FastMode:                   fastMode,
		ContextWindow:              contextWindow,
		AutoCompactStandardPercent: autoCompactStandardPercent,
		AutoCompactExtendedPercent: autoCompactExtendedPercent,
		RuntimeMode:                runtimeMode,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
	if err := a.store.CreateThread(t); err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(t)
	if a.settings != nil {
		a.settings.AddRecentWorkspace(workspace)
	}
	if cutWorktree {
		// After the row commits, so the run's durable state has something to
		// write to, and before the read below, so the thread this call returns
		// already carries `running` instead of arriving via a race with the
		// first pushed event. The run itself is async — a slow recipe must not
		// hold thread creation open.
		a.startThreadWorktreeSetup(t)
	}
	return a.store.GetThread(t.ID)
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
func (a *App) StartTerminal(opts StartTerminalOptions) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("start terminal: store unavailable")
	}

	projectID := strings.TrimSpace(opts.ProjectID)
	var projectPath string
	if projectID != "" {
		project, err := a.store.GetProject(projectID)
		if err != nil {
			return store.Thread{}, fmt.Errorf("start terminal: resolve project %s: %w", projectID, err)
		}
		projectPath = project.Path
	}

	// cwd precedence: explicit override → project root → home directory.
	workspace := strings.TrimSpace(opts.Cwd)
	if workspace == "" {
		workspace = projectPath
	}
	if workspace == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return store.Thread{}, fmt.Errorf("start terminal: resolve home directory: %w", err)
		}
		workspace = home
	}

	// Sentinel provider/model/effort. The terminal never starts a session,
	// but CreateThread still validates the coupled CHECK; mirror the
	// normalize+coerce CreateThread runs so the pairing is always legal.
	seed := a.seedChatModelProfile("", "")
	providerName := seed.Provider
	model := provider.NormalizeModelSlug(providerName, seed.Model)
	effort := string(provider.CoerceReasoningEffortForModel(
		providerName,
		model,
		provider.NormalizeReasoningEffort(seed.ReasoningEffort),
	))

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "Terminal"
	}

	now := time.Now().UnixMilli()
	t := store.Thread{
		ID:              uuid.New().String(),
		ProjectID:       projectID, // "" => store persists NULL (standalone)
		ProjectPath:     projectPath,
		Title:           title,
		Provider:        providerName,
		Model:           model,
		WorkspacePath:   workspace,
		Mode:            "terminal",
		ReasoningEffort: effort,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := a.store.CreateThread(t); err != nil {
		return store.Thread{}, fmt.Errorf("start terminal: %w", err)
	}
	return a.store.GetThread(t.ID)
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

// GetThreadDefaults returns the values CreateThread would have seeded
// for a fresh thread in the given project. Used by the frontend's draft
// placeholder flow so "+ New" surfaces a populated toolbar (model name,
// effort) and a real branch label before the row exists. Mode is
// accepted only for symmetry with CreateThread — defaults don't vary by
// mode today, but accepting it keeps the wire shape stable if that
// changes.
func (a *App) GetThreadDefaults(opts CreateThreadOptions) (ThreadDefaults, error) {
	if a.store == nil {
		return ThreadDefaults{}, fmt.Errorf("get thread defaults: store unavailable")
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	if projectID == "" {
		return ThreadDefaults{}, fmt.Errorf("get thread defaults: projectId is required")
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return ThreadDefaults{}, fmt.Errorf("get thread defaults: resolve project %s: %w", projectID, err)
	}
	seed := a.seedChatModelProfile(opts.Provider, opts.Model)
	providerName := seed.Provider
	model := seed.Model
	if trimmed := strings.TrimSpace(opts.Provider); trimmed != "" {
		providerName = trimmed
	}
	if trimmed := strings.TrimSpace(opts.Model); trimmed != "" {
		model = trimmed
	}
	model = provider.NormalizeModelSlug(providerName, model)
	effort, fastMode := a.draftModelDefaults(providerName, model, seed.ReasoningEffort, seed.FastMode)
	contextWindow := seed.ContextWindow
	options := chatmodel.ContextWindowOptions(providerName, model)
	if len(options) > 0 && !chatmodel.ContextWindowSupported(options, contextWindow) {
		contextWindow = provider.DefaultContextWindowForModel(providerName, model, 0)
	}
	branch := a.gitCore().CurrentBranch(project.Path)
	return ThreadDefaults{
		Provider:        providerName,
		Model:           model,
		ReasoningEffort: effort,
		FastMode:        fastMode,
		ContextWindow:   contextWindow,
		RuntimeMode:     seed.RuntimeMode,
		Branch:          branch,
		WorkspacePath:   project.Path,
	}, nil
}

// createWorktreeForNewThread is the small helper CreateThread uses when
// WorktreeBranch is non-empty. Extracted from CreateThread so the inline
// logic there stays focused on building the Thread row.
func (a *App) createWorktreeForNewThread(projectPath, branch string) (string, string, error) {
	resolvedBranch := a.resolveWorktreeBranch(branch)
	worktreePath, err := a.defaultWorktreePath(projectPath, resolvedBranch)
	if err != nil {
		return "", "", err
	}
	baseBranch := a.gitCore().CurrentBranch(projectPath)
	if err := a.cutWorktreeFromFreshBase(a.lifeCtx(), projectPath, worktreePath, baseBranch, resolvedBranch); err != nil {
		return "", "", err
	}
	return worktreePath, resolvedBranch, nil
}

// ListThreads backs the frontend sidebar. It deliberately excludes
// "draft" threads (newly created but never sent) so the sidebar stays
// clean: a thread only becomes visible once its first item lands.
// Internal callers that need every thread (tests, fork inspection,
// discussion runtime) go through a.store.ListThreads directly.
func (a *App) ListThreads() ([]store.Thread, error) {
	return a.store.ListThreadsWithItems()
}

// ListArchivedThreads returns every archived thread for the settings panel.
func (a *App) ListArchivedThreads() ([]store.Thread, error) {
	return a.store.ListArchivedThreads()
}

// GetThread returns a single thread row.
func (a *App) GetThread(id string) (store.Thread, error) {
	return a.store.GetThread(id)
}

// DeleteThread tears down the thread and any child threads. The recursive
// cascade logic lives in app_thread_delete.go.
func (a *App) DeleteThread(id string) error {
	unlock := a.threadLocks().Lock(id)
	defer unlock()
	return a.deleteThreadTreeLocked(id)
}

// ArchiveThread flips archived to true so the thread leaves the active sidebar.
func (a *App) ArchiveThread(id string) error {
	return a.store.ArchiveThread(id)
}

// UnarchiveThread flips archived back to false so the thread reappears in the
// active sidebar. Returns the refreshed row so the caller can re-render
// without a follow-up GetThread round-trip.
func (a *App) UnarchiveThread(id string) (store.Thread, error) {
	if err := a.store.UnarchiveThread(id); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

// RenameThread updates the thread title and emits a thread:updated
// event so any other observer (chat header, multi-tab clients, future
// remote viewers) re-renders with the new title without a follow-up
// poll. Mirrors the emit shape used by applyThreadTitleIfCurrent.
func (a *App) RenameThread(id string, title string) error {
	if err := a.store.UpdateTitle(id, title); err != nil {
		return err
	}
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
		Action: "patch",
		ID:     id,
		Title:  &title,
	})
	// Keep the peer registry honest: a live Claude session with the
	// cross-session inbox open is discoverable by name, and the user just
	// changed the name. No-op for every other thread
	// (app_claude_peer_name.go).
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
func (a *App) MarkThreadRead(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), markThreadReadTimeout)
	defer cancel()
	return a.store.MarkThreadReadNow(ctx, id)
}

// MarkThreadUnread stamps last_read_at to zero. NULL is reserved for
// "never tracked" rows that should not flood the sidebar as unread after
// migration; explicit unread uses a concrete old timestamp.
func (a *App) MarkThreadUnread(id string) error {
	return a.store.MarkThreadUnread(id)
}

// PinThread marks the thread as pinned. Pinned threads sort into a
// dedicated tier above needs-attention so the user can keep a reference
// thread permanently visible without status churn shuffling it.
// Re-pinning an already-pinned thread bumps its pinnedAt, which moves
// it within the pinned tier.
func (a *App) PinThread(id string) (store.Thread, error) {
	if err := a.store.PinThread(id); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

// UnpinThread clears the thread's pinned_at and returns the refreshed
// row so the frontend can reconcile its store without a list refetch.
func (a *App) UnpinThread(id string) (store.Thread, error) {
	if err := a.store.UnpinThread(id); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

// sessionAffectingFields enumerates the thread columns that, when
// changed, must be pushed onto a live provider session. The centralized
// restartSessionIfAffected helper consults this list so every per-field
// binding participates in the same reconcile policy: live-apply where the
// provider supports it, otherwise a restart that is deferred while the
// thread has an active turn or running background tasks (see
// app_session_config.go).
//
// `mcpServers` deliberately does NOT live here. MCP disabled state
// lives in the provider's own config (Claude: the workspace project's
// `disabledMcpServers`, written via internal/claudeconfig; Codex:
// config.toml), applied via the provider's live-reconcile API
// (mcp_set_servers for Claude, config.mcp_servers at start for
// Codex). See app_mcp_thread.go. No provider-session restart is
// needed for MCP changes.
var sessionAffectingFields = map[string]struct{}{
	"provider":        {},
	"model":           {},
	"mode":            {},
	"effort":          {},
	"fastMode":        {},
	"contextWindow":   {},
	"contextSettings": {},
	"workspace":       {},
}

// restartSessionIfAffected emits the refreshed thread and, when the
// named field affects provider launch config, reconciles the live
// provider session with the new config. Centralizing the call keeps the
// per-field bindings free of duplicated "is this session live" plumbing.
//
// Workspace changes keep their dedicated synchronous restart: the provider
// process is bound to its cwd, and returning before any old or in-flight
// session is retired lets the next send reuse a stale process. A restart
// failure is surfaced as thread error state while the persisted workspace
// switch still succeeds; once the stale session has been removed, a later
// send can attempt a normal lazy start.
func (a *App) restartSessionIfAffected(threadID, changedField string) (store.Thread, error) {
	refreshed, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if _, ok := sessionAffectingFields[changedField]; !ok {
		return refreshed, nil
	}
	if changedField == "workspace" {
		if err := a.restartWorkspaceSession(threadID); err != nil {
			log.Printf("thread %s: workspace change reconnect failed: %v", threadID, err)
			a.emitErrorToThread(threadID, fmt.Sprintf("workspace change failed to reconnect: %v", err))
		}
		return a.store.GetThread(threadID)
	}
	a.reconcileSessionConfig(threadID)
	return refreshed, nil
}

func (a *App) restartWorkspaceSession(threadID string) error {
	if startState, ok := a.sessionManager().startState(threadID); ok {
		<-startState.done
	}
	if !a.hasActiveSession(threadID) {
		return nil
	}
	// Callers reach here holding the per-thread action lock (the
	// workspace-change bindings), so use the locked reconnect body.
	return a.reconnectSessionLocked(context.Background(), threadID)
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
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update provider: store unavailable")
	}
	normalized := strings.TrimSpace(providerName)
	if normalized == "" {
		return store.Thread{}, fmt.Errorf("thread provider cannot be empty")
	}
	if !validThreadProvider(normalized) {
		return store.Thread{}, fmt.Errorf("%w: %q", store.ErrInvalidProvider, normalized)
	}

	current, err := a.store.GetThread(id)
	if err != nil {
		return store.Thread{}, fmt.Errorf("update provider: %w", err)
	}
	if current.Provider == normalized {
		return current, nil
	}

	profile, err := a.latestProviderProfileForSelection(normalized)
	if err != nil {
		return store.Thread{}, err
	}
	return a.updateThreadFromChatModelProfile(current, profile)
}

// UpdateThreadReasoningEffort persists the effort tier and reconciles a
// live session (Codex applies it on the next turn without a restart;
// Claude needs a restart, deferred until the thread is quiet).
func (a *App) UpdateThreadReasoningEffort(id, effort string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update effort: store unavailable")
	}
	thread, err := a.store.GetThread(id)
	if err != nil {
		return store.Thread{}, err
	}
	normalized := strings.TrimSpace(effort)
	if !a.reasoningEffortSupportedForModel(thread.Provider, thread.Model, normalized) {
		return store.Thread{}, fmt.Errorf("update effort: unsupported reasoning effort %q for %s/%s", normalized, thread.Provider, thread.Model)
	}
	if err := a.store.UpdateReasoningEffort(id, normalized); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "effort")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	return refreshed, nil
}

// UpdateThreadFastMode persists the fast-mode boolean and reconciles a
// live session (Codex maps it to the per-turn serviceTier override; the
// Claude CLI only reads fast mode from launch settings, so a Claude
// session restarts — deferred until the thread is quiet).
func (a *App) UpdateThreadFastMode(id string, on bool) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update fast mode: store unavailable")
	}
	if on {
		thread, err := a.store.GetThread(id)
		if err != nil {
			return store.Thread{}, err
		}
		if !a.supportsFastModeForModel(thread.Provider, thread.Model) {
			return store.Thread{}, fmt.Errorf("update fast mode: unsupported for %s/%s", thread.Provider, thread.Model)
		}
	}
	if err := a.store.UpdateFastMode(id, on); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "fastMode")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	return refreshed, nil
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
	if a.store == nil {
		return nil, fmt.Errorf("update branch: store unavailable")
	}
	if strings.TrimSpace(workspacePath) == "" {
		return nil, fmt.Errorf("update branch: workspace path is required")
	}
	// The value reaches argv the moment any later git operation reads the
	// cached branch back, so it is validated at the door rather than trusted
	// because today's only writer happens to have read it off a git status.
	// Empty stays legal — it is how the column is cleared.
	if branch != "" {
		if err := gitops.ValidateBranchName(branch); err != nil {
			return nil, fmt.Errorf("update branch: %w", err)
		}
	}
	rows, err := a.store.UpdateBranchForWorkspace(
		workspacePath, gitops.CanonicalPath(workspacePath), branch)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		row := rows[i]
		a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: "full", Thread: &row})
	}
	return rows, nil
}

// UpdateThreadWorkspace persists a new workspace path. Used by the
// EnvPicker to switch a thread between the project root and a worktree
// without creating the worktree itself. Restarts the session if one is
// live because the provider CWD is part of its launch config.
func (a *App) UpdateThreadWorkspace(id, path string) (store.Thread, error) {
	return a.switchThreadWorkspace(id, path)
}
