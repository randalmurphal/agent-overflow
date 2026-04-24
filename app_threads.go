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
type CreateThreadOptions struct {
	ProjectID         string `json:"projectId"`
	Provider          string `json:"provider,omitempty"`          // defaults to settings.DefaultProvider
	Model             string `json:"model,omitempty"`             // defaults to settings.DefaultModelClaude/Codex
	Mode              string `json:"mode,omitempty"`              // defaults to chat
	ReasoningEffort   string `json:"reasoningEffort,omitempty"`   // defaults to settings.DefaultReasoningEffort
	FastMode          *bool  `json:"fastMode,omitempty"`          // nil = use setting default
	ContextWindow     int    `json:"contextWindow,omitempty"`     // 0 = setting default
	RuntimeMode       string `json:"runtimeMode,omitempty"`       // defaults to settings.DefaultRuntimeMode
	WorktreeBranch    string `json:"worktreeBranch,omitempty"`    // empty = no worktree
	WorkspaceOverride string `json:"workspaceOverride,omitempty"` // empty = project.path
}

// CreateThread persists a new thread rooted at a project. The options
// struct carries every knob; any empty field except Mode is resolved via
// settings defaults. Mode defaults to chat so every normal new thread starts
// as a chat thread. "discussion" is rejected as a mode value because
// discussion threads must come through StartDiscussion (which wires the
// deliberation channel).
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

	providerName := strings.TrimSpace(opts.Provider)
	if providerName == "" {
		providerName = a.defaultProviderFromSettings()
	}

	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = a.defaultModelForProvider(providerName)
	}

	effort := strings.TrimSpace(opts.ReasoningEffort)
	if effort == "" {
		effort = a.defaultReasoningEffort()
	}

	fastMode := a.defaultFastMode()
	if opts.FastMode != nil {
		fastMode = *opts.FastMode
	}

	contextWindow := opts.ContextWindow
	if contextWindow == 0 {
		contextWindow = a.defaultContextWindowForModel(providerName, model)
	}

	runtimeMode := strings.TrimSpace(opts.RuntimeMode)
	if runtimeMode == "" {
		runtimeMode = a.defaultRuntimeModeForNewThread()
	} else {
		parsedRuntimeMode, err := parseRuntimeMode(runtimeMode)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: %w", err)
		}
		runtimeMode = string(parsedRuntimeMode)
	}

	workspace := strings.TrimSpace(opts.WorkspaceOverride)
	if workspace == "" {
		workspace = project.Path
	}
	var branch, worktreePath string
	if trimmed := strings.TrimSpace(opts.WorktreeBranch); trimmed != "" {
		// Worktree creation runs through the git core with the project
		// path as the repo root. On success the thread's workspace
		// becomes the worktree directory.
		wtPath, wtBranch, err := a.createWorktreeForNewThread(project.Path, trimmed)
		if err != nil {
			return store.Thread{}, fmt.Errorf("create thread: worktree: %w", err)
		}
		workspace = wtPath
		worktreePath = wtPath
		branch = wtBranch
	}

	now := time.Now().UnixMilli()
	t := store.Thread{
		ID:              uuid.New().String(),
		ProjectID:       project.ID,
		Title:           "New Thread",
		Provider:        providerName,
		WorkspacePath:   workspace,
		WorktreePath:    worktreePath,
		Branch:          branch,
		Model:           model,
		Mode:            mode,
		ReasoningEffort: effort,
		FastMode:        fastMode,
		ContextWindow:   contextWindow,
		RuntimeMode:     runtimeMode,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := a.store.CreateThread(t); err != nil {
		return store.Thread{}, err
	}
	if a.settings != nil {
		a.settings.AddRecentWorkspace(workspace)
	}
	return t, nil
}

// createWorktreeForNewThread is the small helper CreateThread uses when
// WorktreeBranch is non-empty. Extracted from CreateThread so the inline
// logic there stays focused on building the Thread row.
func (a *App) createWorktreeForNewThread(projectPath, branch string) (string, string, error) {
	resolvedBranch := strings.TrimSpace(branch)
	if resolvedBranch == "" {
		resolvedBranch = gitops.BuildTemporaryWorktreeBranchName()
	}
	worktreePath := defaultWorktreePath(projectPath, resolvedBranch)
	if err := a.gitCore().CreateWorktree(projectPath, worktreePath, resolvedBranch); err != nil {
		return "", "", err
	}
	return worktreePath, resolvedBranch, nil
}

// defaultProviderFromSettings returns the provider name to seed new
// threads with. Falls back to "claude" when settings are unavailable or
// hold an unexpected value.
func (a *App) defaultProviderFromSettings() string {
	if a.settings == nil {
		return "claude"
	}
	name := strings.TrimSpace(a.settings.Get().DefaultProvider)
	switch name {
	case "claude", "codex":
		return name
	default:
		return "claude"
	}
}

// defaultReasoningEffort returns the seed effort tier for new threads.
func (a *App) defaultReasoningEffort() string {
	if a.settings == nil {
		return "high"
	}
	effort := strings.TrimSpace(a.settings.Get().DefaultReasoningEffort)
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort
	default:
		return "high"
	}
}

// defaultFastMode returns the seed fast-mode flag for new threads.
func (a *App) defaultFastMode() bool {
	if a.settings == nil {
		return false
	}
	return a.settings.Get().DefaultFastMode
}

// defaultContextWindow returns the seed context-window size in tokens.
func (a *App) defaultContextWindow() int {
	if a.settings == nil {
		return 1000000
	}
	switch a.settings.Get().DefaultContextWindow {
	case 200000, 1000000:
		return a.settings.Get().DefaultContextWindow
	default:
		return 1000000
	}
}

func (a *App) defaultContextWindowForModel(providerName, model string) int {
	cfg := a.currentSettings()
	if tokens, ok := cfg.ModelContextWindows[strings.TrimSpace(model)]; ok && isValidContextWindow(tokens) {
		return tokens
	}
	return defaultContextWindowForProviderModel(providerName, model, cfg.DefaultContextWindow)
}

func defaultContextWindowForProviderModel(providerName, model string, fallback int) int {
	if providerName == string(provider.Claude) {
		lowered := strings.ToLower(strings.TrimSpace(model))
		if strings.Contains(lowered, "opus") {
			return 1000000
		}
		return 200000
	}
	if isValidContextWindow(fallback) {
		return fallback
	}
	return 1000000
}

func isValidContextWindow(tokens int) bool {
	return tokens == 200000 || tokens == 1000000
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

// RenameThread updates the thread title.
func (a *App) RenameThread(id string, title string) error {
	t, err := a.store.GetThread(id)
	if err != nil {
		return err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UnixMilli()
	return a.store.UpdateThread(t)
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

// sessionAffectingFields enumerates the thread columns that, when
// changed, require a provider-session restart to take effect. The
// centralized restartSessionIfAffected helper consults this list so
// every per-field binding participates in the same restart policy.
var sessionAffectingFields = map[string]struct{}{
	"provider":      {},
	"model":         {},
	"mode":          {},
	"effort":        {},
	"fastMode":      {},
	"contextWindow": {},
	"workspace":     {},
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

	rollbackSettings, err := a.applySettingsPatchWithRollback(map[string]any{
		"defaultProvider": normalized,
	})
	if err != nil {
		return store.Thread{}, fmt.Errorf("update provider default: %w", err)
	}
	if err := a.store.UpdateProvider(id, normalized); err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update provider: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "provider")
	if err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update provider: %w (settings rollback failed: %v)", err, rollbackErr)
		}
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
	rollbackSettings, err := a.applySettingsPatchWithRollback(map[string]any{
		"defaultReasoningEffort": strings.TrimSpace(effort),
	})
	if err != nil {
		return store.Thread{}, fmt.Errorf("update effort default: %w", err)
	}
	if err := a.store.UpdateReasoningEffort(id, effort); err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update effort: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "effort")
	if err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update effort: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
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
	rollbackSettings, err := a.applySettingsPatchWithRollback(map[string]any{
		"defaultFastMode": on,
	})
	if err != nil {
		return store.Thread{}, fmt.Errorf("update fast-mode default: %w", err)
	}
	if err := a.store.UpdateFastMode(id, on); err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update fast mode: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "fastMode")
	if err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update fast mode: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
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
	patch := map[string]any{
		"defaultContextWindow": tokens,
	}
	if strings.TrimSpace(thread.Model) != "" {
		modelContexts := cloneModelContextWindows(a.currentSettings().ModelContextWindows)
		if modelContexts == nil {
			modelContexts = map[string]int{}
		}
		modelContexts[strings.TrimSpace(thread.Model)] = tokens
		patch["modelContextWindows"] = modelContexts
	}
	rollbackSettings, err := a.applySettingsPatchWithRollback(patch)
	if err != nil {
		return store.Thread{}, fmt.Errorf("update context-window default: %w", err)
	}
	if err := a.store.UpdateContextWindow(id, tokens); err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update context window: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
	refreshed, err := a.restartSessionIfAffected(id, "contextWindow")
	if err != nil {
		if rollbackErr := rollbackSettings(); rollbackErr != nil {
			return store.Thread{}, fmt.Errorf("update context window: %w (settings rollback failed: %v)", err, rollbackErr)
		}
		return store.Thread{}, err
	}
	return refreshed, nil
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
	return a.store.GetThread(id)
}

// UpdateThreadBranch persists the branch column. Does NOT perform the
// git checkout — that flow lives in GitCheckout. This binding exists
// because the EnvPicker in the new UI needs to attach a branch string
// to a thread without forcing a checkout.
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
	if a.store == nil {
		return store.Thread{}, fmt.Errorf("update workspace: store unavailable")
	}
	normalized := strings.TrimSpace(path)
	if normalized == "" {
		return store.Thread{}, fmt.Errorf("update workspace: path is required")
	}
	if err := a.store.UpdateWorkspacePath(id, normalized); err != nil {
		return store.Thread{}, err
	}
	return a.restartSessionIfAffected(id, "workspace")
}
