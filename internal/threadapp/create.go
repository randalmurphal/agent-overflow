package threadapp

import (
	"fmt"
	"os"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/entityid"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

type CreateOptions struct {
	ProjectID                  string
	Title                      string
	Provider                   string
	Model                      string
	Mode                       string
	ReasoningEffort            string
	FastMode                   *bool
	ContextWindow              int
	AutoCompactStandardPercent *int
	AutoCompactExtendedPercent *int
	RuntimeMode                string
	WorktreeBranch             string
	WorkspaceOverride          string
	WorktreePath               string
	Branch                     string
	// CreatedByDevice names the screen this call came from, or "" when the
	// backend created the thread on its own behalf. Root reads it off the
	// connection; this package only records it.
	CreatedByDevice string
	// SettingsBucket names the ui_state bucket holding the calling
	// connection's device-tier settings, empty for a backend-initiated
	// create. The recent-workspace write is attributed to it — see
	// RecentWorkspaces.
	SettingsBucket string
	// AuthorizeRuntimeMode, when set, is asked to approve the RESOLVED
	// runtime mode — the argument if one was given, otherwise whatever the
	// seed profile supplies — before the thread persists. Returning an
	// error aborts the create with that error unwrapped.
	//
	// A hook rather than a resolved mode passed in by the caller: the
	// resolution rules live here, and a caller that re-derived them to
	// authorize would be a second copy that silently disagrees the day
	// one of them changes. This package still knows nothing about scopes.
	AuthorizeRuntimeMode func(mode string) error
}

type TerminalOptions struct {
	ProjectID       string
	Cwd             string
	Title           string
	CreatedByDevice string
}

type Defaults struct {
	Provider        string
	Model           string
	ReasoningEffort string
	FastMode        bool
	ContextWindow   int
	RuntimeMode     string
	Branch          string
	WorkspacePath   string
}

func (s *Service) Create(opts CreateOptions) (store.Thread, error) {
	database, err := s.database("create thread")
	if err != nil {
		return store.Thread{}, err
	}
	models, err := s.modelPolicy("create thread")
	if err != nil {
		return store.Thread{}, err
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	if projectID == "" {
		return store.Thread{}, fmt.Errorf("create thread: projectId is required")
	}
	project, err := database.GetProject(projectID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("create thread: resolve project %s: %w", projectID, err)
	}

	mode, err := threadmode.ValidateCreate(opts.Mode)
	if err != nil {
		return store.Thread{}, err
	}

	seed := models.Seed(opts.Provider, opts.Model)
	providerName := seed.Provider
	model := seed.Model
	effort := seed.ReasoningEffort
	fastMode := seed.FastMode
	contextWindow := seed.ContextWindow
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
		parsedRuntimeMode, parseErr := threadmode.ParseRuntime(trimmed)
		if parseErr != nil {
			return store.Thread{}, fmt.Errorf("create thread: %w", parseErr)
		}
		runtimeMode = string(parsedRuntimeMode)
	}
	// The resolved mode is known here and the thread has not persisted, so
	// this is where an authority decided by the OUTCOME gets asked.
	if opts.AuthorizeRuntimeMode != nil {
		if err := opts.AuthorizeRuntimeMode(runtimeMode); err != nil {
			return store.Thread{}, err
		}
	}
	if trimmed := strings.TrimSpace(opts.ReasoningEffort); trimmed != "" {
		if !models.SupportsReasoningEffort(providerName, model, trimmed) {
			return store.Thread{}, fmt.Errorf("create thread: unsupported reasoning effort %q for %s/%s", trimmed, providerName, model)
		}
	} else {
		effort = models.CoerceReasoningEffort(providerName, model, effort)
	}
	if fastMode && !models.SupportsFastMode(providerName, model) {
		if opts.FastMode != nil {
			return store.Thread{}, fmt.Errorf("create thread: fast mode unsupported for %s/%s", providerName, model)
		}
		fastMode = false
	}
	contextOptions := chatmodel.ContextWindowOptions(providerName, model)
	if opts.ContextWindow != 0 && len(contextOptions) > 0 && !chatmodel.ContextWindowSupported(contextOptions, contextWindow) {
		return store.Thread{}, fmt.Errorf("create thread: unsupported context window %d for %s/%s", contextWindow, providerName, model)
	}
	if len(contextOptions) > 0 && !chatmodel.ContextWindowSupported(contextOptions, contextWindow) {
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
	cutWorktree := false
	inheritWorktree := strings.TrimSpace(opts.WorktreePath)
	switch {
	case inheritWorktree != "":
		if s.deps.Workspace == nil {
			return store.Thread{}, fmt.Errorf("create thread: validate inherited worktree: workspace resolver unavailable")
		}
		path, worktreeBranch, found, findErr := s.deps.Workspace.FindWorktree(project.Path, inheritWorktree)
		if findErr != nil {
			return store.Thread{}, fmt.Errorf("create thread: validate inherited worktree: %w", findErr)
		}
		if !found {
			return store.Thread{}, fmt.Errorf("create thread: %q is not a worktree of project %s", inheritWorktree, project.Path)
		}
		workspace = path
		worktreePath = path
		branch = strings.TrimSpace(opts.Branch)
		if branch == "" {
			branch = worktreeBranch
		} else {
			branch = gitops.SanitizeBranchNamePreservingSlashes(branch)
		}
	case strings.TrimSpace(opts.WorktreeBranch) != "":
		if s.deps.Workspace == nil {
			return store.Thread{}, fmt.Errorf("create thread: worktree: workspace resolver unavailable")
		}
		path, resolvedBranch, createErr := s.deps.Workspace.CreateWorktree(
			s.deps.LifeContext(), project.Path, strings.TrimSpace(opts.WorktreeBranch),
		)
		if createErr != nil {
			return store.Thread{}, fmt.Errorf("create thread: worktree: %w", createErr)
		}
		workspace = path
		worktreePath = path
		branch = resolvedBranch
		cutWorktree = true
	default:
		if explicit := strings.TrimSpace(opts.Branch); explicit != "" {
			branch = gitops.SanitizeBranchNamePreservingSlashes(explicit)
		}
	}
	if branch == "" && s.deps.Workspace != nil {
		branch = s.deps.Workspace.CurrentBranch(workspace)
	}

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "New Thread"
	}
	now := s.deps.Now().UnixMilli()
	thread := store.Thread{
		ID:                         s.newID(),
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
		CreatedByDevice:            opts.CreatedByDevice,
		Origin:                     s.observeOrigin(workspace),
	}
	if err := database.CreateThread(thread); err != nil {
		return store.Thread{}, err
	}
	models.Remember(thread)
	if s.deps.RecentWorkspaces != nil {
		s.deps.RecentWorkspaces.AddRecentWorkspace(opts.SettingsBucket, workspace)
	}
	if cutWorktree && s.deps.WorktreeSetup != nil {
		s.deps.WorktreeSetup.Start(thread)
	}
	return database.GetThread(thread.ID)
}

func (s *Service) StartTerminal(opts TerminalOptions) (store.Thread, error) {
	database, err := s.database("start terminal")
	if err != nil {
		return store.Thread{}, err
	}
	models, err := s.modelPolicy("start terminal")
	if err != nil {
		return store.Thread{}, err
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	var projectPath string
	if projectID != "" {
		project, getErr := database.GetProject(projectID)
		if getErr != nil {
			return store.Thread{}, fmt.Errorf("start terminal: resolve project %s: %w", projectID, getErr)
		}
		projectPath = project.Path
	}
	workspace := strings.TrimSpace(opts.Cwd)
	if workspace == "" {
		workspace = projectPath
	}
	if workspace == "" {
		homeDir := s.deps.HomeDir
		if homeDir == nil {
			homeDir = os.UserHomeDir
		}
		home, homeErr := homeDir()
		if homeErr != nil {
			return store.Thread{}, fmt.Errorf("start terminal: resolve home directory: %w", homeErr)
		}
		workspace = home
	}
	seed := models.Seed("", "")
	providerName := seed.Provider
	model := provider.NormalizeModelSlug(providerName, seed.Model)
	effort := string(provider.CoerceReasoningEffortForModel(
		providerName, model, provider.NormalizeReasoningEffort(seed.ReasoningEffort),
	))
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "Terminal"
	}
	now := s.deps.Now().UnixMilli()
	thread := store.Thread{
		ID:              s.newID(),
		ProjectID:       projectID,
		ProjectPath:     projectPath,
		Title:           title,
		Provider:        providerName,
		Model:           model,
		WorkspacePath:   workspace,
		Mode:            "terminal",
		ReasoningEffort: effort,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedByDevice: opts.CreatedByDevice,
		Origin:          s.observeOrigin(workspace),
	}
	if err := database.CreateThread(thread); err != nil {
		return store.Thread{}, fmt.Errorf("start terminal: %w", err)
	}
	return database.GetThread(thread.ID)
}

func (s *Service) Defaults(opts CreateOptions) (Defaults, error) {
	database, err := s.database("get thread defaults")
	if err != nil {
		return Defaults{}, err
	}
	models, err := s.modelPolicy("get thread defaults")
	if err != nil {
		return Defaults{}, err
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	if projectID == "" {
		return Defaults{}, fmt.Errorf("get thread defaults: projectId is required")
	}
	project, err := database.GetProject(projectID)
	if err != nil {
		return Defaults{}, fmt.Errorf("get thread defaults: resolve project %s: %w", projectID, err)
	}
	seed := models.Seed(opts.Provider, opts.Model)
	providerName := seed.Provider
	model := seed.Model
	if trimmed := strings.TrimSpace(opts.Provider); trimmed != "" {
		providerName = trimmed
	}
	if trimmed := strings.TrimSpace(opts.Model); trimmed != "" {
		model = trimmed
	}
	model = provider.NormalizeModelSlug(providerName, model)
	effort, fastMode := models.DraftDefaults(providerName, model, seed.ReasoningEffort, seed.FastMode)
	contextWindow := seed.ContextWindow
	contextOptions := chatmodel.ContextWindowOptions(providerName, model)
	if len(contextOptions) > 0 && !chatmodel.ContextWindowSupported(contextOptions, contextWindow) {
		contextWindow = provider.DefaultContextWindowForModel(providerName, model, 0)
	}
	branch := ""
	if s.deps.Workspace != nil {
		branch = s.deps.Workspace.CurrentBranch(project.Path)
	}
	return Defaults{
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

// newID mints a thread id. Globally unique by construction — a client
// attached to more than one backend keys its stores, its replica and its
// deep links by this string, so see internal/entityid before minting one
// any other way. `deps.NewID` is a test seam; production has no override.
func (s *Service) newID() string {
	if s.deps.NewID != nil {
		return s.deps.NewID()
	}
	return entityid.New()
}
