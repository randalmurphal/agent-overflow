package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

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
// as a mode value because discussion threads must come through
// StartDiscussion (which wires the deliberation channel).
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

	mode, err := validateCreateThreadMode(opts.Mode)
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
		parsedRuntimeMode, err := parseRuntimeMode(trimmed)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: %w", err)
		}
		runtimeMode = string(parsedRuntimeMode)
	}
	options := contextWindowOptionsForProviderModel(providerName, model)
	if opts.ContextWindow != 0 && len(options) > 0 && !contextWindowSupported(options, contextWindow) {
		return store.Thread{}, fmt.Errorf("create thread: unsupported context window %d for %s/%s", contextWindow, providerName, model)
	}
	if len(options) > 0 && !contextWindowSupported(options, contextWindow) {
		contextWindow = provider.DefaultContextWindowForModel(providerName, model, options[0].Tokens)
	}
	if !isValidAutoCompactPercent(autoCompactStandardPercent) {
		return store.Thread{}, fmt.Errorf("create thread: auto-compact standard percent must be between 0 and 90")
	}
	if !isValidAutoCompactPercent(autoCompactExtendedPercent) {
		return store.Thread{}, fmt.Errorf("create thread: auto-compact extended percent must be between 0 and 90")
	}

	workspace := strings.TrimSpace(opts.WorkspaceOverride)
	if workspace == "" {
		workspace = project.Path
	}
	var branch, worktreePath string
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
	default:
		// No worktree path supplied. If the caller passes an explicit
		// branch (e.g. for the project root), sanitize it the same way
		// we'd sanitize user-typed branch names elsewhere — preserves
		// `feature/login` while collapsing shell-meta and weird casing.
		if explicit := strings.TrimSpace(opts.Branch); explicit != "" {
			branch = gitops.SanitizeBranchNamePreservingSlashes(explicit)
		}
	}
	if branch == "" {
		branch = currentGitBranch(a.gitCore(), workspace)
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
	return a.store.GetThread(t.ID)
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
	baseBranch := currentGitBranch(a.gitCore(), projectPath)
	if err := a.gitCore().CreateWorktreeFromBranch(projectPath, worktreePath, baseBranch, resolvedBranch); err != nil {
		return "", "", err
	}
	return worktreePath, resolvedBranch, nil
}

func (a *App) defaultContextWindowForModel(providerName, model string) int {
	if a.store != nil {
		profile, err := a.store.GetChatModelProfile(providerName, strings.TrimSpace(model))
		if err == nil && isValidContextWindow(profile.ContextWindow) {
			return profile.ContextWindow
		}
	}
	return defaultContextWindowForProviderModel(providerName, model, 1000000)
}

func defaultContextWindowForProviderModel(providerName, model string, fallback int) int {
	return provider.DefaultContextWindowForModel(providerName, strings.TrimSpace(model), fallback)
}

func isValidContextWindow(tokens int) bool {
	return tokens > 0
}

func isValidAutoCompactPercent(percent int) bool {
	return percent >= 0 && percent <= 90
}

// ListThreads backs the frontend sidebar. It deliberately excludes
// "draft" threads (newly created but never sent) so the sidebar stays
// clean: a thread only becomes visible once its first item lands.
// Internal callers that need every thread (tests, fork inspection,
// discussion runtime) go through a.store.ListThreads directly.
func (a *App) ListThreads() ([]store.Thread, error) {
	return a.store.ListThreadsWithItems()
}

// GetThread returns a single thread row.
func (a *App) GetThread(id string) (store.Thread, error) {
	return a.store.GetThread(id)
}

// DeleteThread tears down the thread and any child threads. The recursive
// cascade logic lives in app_thread_delete.go.
func (a *App) DeleteThread(id string) error {
	return a.deleteThreadTree(id)
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
// poll. Mirrors the emit shape used by applyGeneratedThreadTitle.
func (a *App) RenameThread(id string, title string) error {
	t, err := a.store.GetThread(id)
	if err != nil {
		return err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(t); err != nil {
		return err
	}
	if updated, gerr := a.store.GetThread(id); gerr == nil {
		a.emitEvent("thread:updated", updated)
	}
	return nil
}

// MarkThreadRead stamps the thread's last_read_at to at least its latest
// completed turn so the sidebar stops showing a Completed pill. The timestamp
// is owned by the store so the App layer stays free of nowMillis() calls,
// matching ArchiveThread / UpdateTitle etc.
func (a *App) MarkThreadRead(id string) error {
	return a.store.MarkThreadReadNow(id)
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
// changed, require a provider-session restart to take effect. The
// centralized restartSessionIfAffected helper consults this list so
// every per-field binding participates in the same restart policy.
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
// named field affects provider launch config AND a session is live,
// fires a best-effort session restart in the background. Centralizing
// the restart call keeps the per-field bindings free of duplicated
// "is this session live" plumbing.
//
// Returns the refreshed thread and any GetThread error. Restart
// failures are surfaced via a thread-scoped error event; we do NOT
// propagate them synchronously so the UI can still render the
// optimistic state.
func (a *App) restartSessionIfAffected(threadID, changedField string) (store.Thread, error) {
	refreshed, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	if _, ok := sessionAffectingFields[changedField]; !ok {
		return refreshed, nil
	}
	if !a.hasActiveSession(threadID) {
		return refreshed, nil
	}
	go func() {
		if err := a.ReconnectSession(threadID); err != nil {
			log.Printf("thread %s: %s change reconnect failed: %v", threadID, changedField, err)
			a.emitErrorToThread(threadID, fmt.Sprintf("%s change failed to reconnect: %v", changedField, err))
		}
	}()
	return refreshed, nil
}

// UpdateThreadProvider persists the provider column and restarts the
// session if one is live so the new provider takes effect.
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

	current, err := a.store.GetThread(id)
	if err != nil {
		return store.Thread{}, fmt.Errorf("update provider: %w", err)
	}
	if current.Provider == normalized {
		return current, nil
	}

	hasItems, err := a.store.HasItems(id)
	if err != nil {
		return store.Thread{}, fmt.Errorf("update provider: check prior items: %w", err)
	}
	if hasItems {
		return store.Thread{}, fmt.Errorf("update provider: thread is locked to %s (start a new thread to use %s)", current.Provider, normalized)
	}

	if err := a.store.UpdateProvider(id, normalized); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "provider")
	if err != nil {
		return store.Thread{}, err
	}
	return refreshed, nil
}

// UpdateThreadReasoningEffort persists the effort tier and restarts the
// session if one is live.
func (a *App) UpdateThreadReasoningEffort(id, effort string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update effort: store unavailable")
	}
	if err := a.store.UpdateReasoningEffort(id, effort); err != nil {
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "effort")
	if err != nil {
		return store.Thread{}, err
	}
	a.rememberChatModelProfile(refreshed)
	return refreshed, nil
}

// UpdateThreadFastMode persists the fast-mode boolean and restarts the
// session if one is live. Fast mode typically swaps the model to the
// provider's small-model tier (per the per-provider translator) so a
// running session won't pick up the change without a restart.
func (a *App) UpdateThreadFastMode(id string, on bool) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update fast mode: store unavailable")
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
// restarts the session if one is live.
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

// UpdateThreadRuntimeMode persists the runtime mode and restarts the
// session if one is live. Replaces the older SetThreadRuntimeMode
// naming; kept as a single surface so the frontend speaks one shape.
func (a *App) UpdateThreadRuntimeMode(id, mode string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update runtime mode: store unavailable")
	}
	normalized, err := parseRuntimeMode(mode)
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

// UpdateThreadBranch persists the branch column. Does NOT perform the
// git checkout — that flow lives in GitCheckout. Kept as a narrow metadata
// binding for callers that need to repair/import thread state without touching
// the repository checkout.
func (a *App) UpdateThreadBranch(id, branch string) (store.Thread, error) {
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update branch: store unavailable")
	}
	if err := a.store.UpdateBranch(id, branch); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(id)
}

// UpdateThreadWorkspace persists a new workspace path. Used by the
// EnvPicker to switch a thread between the project root and a worktree
// without creating the worktree itself. Restarts the session if one is
// live because the provider CWD is part of its launch config.
func (a *App) UpdateThreadWorkspace(id, path string) (store.Thread, error) {
	return a.switchThreadWorkspace(id, path)
}
