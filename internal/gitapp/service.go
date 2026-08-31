package gitapp

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/store"
)

// StatusEvent is the workspace-keyed status update emitted by Service.
type StatusEvent struct {
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// StatusSubscription is the initial snapshot and handle returned by Subscribe.
type StatusSubscription struct {
	ID     string           `json:"id"`
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// Deps are the process boundaries used by Service. Store, Core, and Watch are
// shared app-lifetime objects; the function ports keep root-owned lifecycle,
// settings, and wire projection out of this package.
type Deps struct {
	Store                  *store.Store
	Core                   *gitops.Core
	Watch                  *gitwatch.Manager
	BackgroundFetchEnabled func() bool
	IsShuttingDown         func() bool
	EmitStatus             func(StatusEvent)
	Logf                   func(string, ...any)
	InvalidateWorkspace    func(string)
	ShuttingDownError      error
}

// Service owns gitwatch fan-out and the unattended fetch cadence.
type Service struct {
	store                  *store.Store
	core                   *gitops.Core
	watch                  *gitwatch.Manager
	backgroundFetchEnabled func() bool
	isShuttingDown         func() bool
	emitStatus             func(StatusEvent)
	logf                   func(string, ...any)
	invalidateWorkspace    func(string)
	shuttingDownError      error

	status statusState
	fetch  backgroundFetchState
}

// New constructs an application git service around shared dependencies.
func New(deps Deps) *Service {
	core := deps.Core
	if core == nil {
		core = gitops.NewCore()
	}
	return &Service{
		store:                  deps.Store,
		core:                   core,
		watch:                  deps.Watch,
		backgroundFetchEnabled: deps.BackgroundFetchEnabled,
		isShuttingDown:         deps.IsShuttingDown,
		emitStatus:             deps.EmitStatus,
		logf:                   deps.Logf,
		invalidateWorkspace:    deps.InvalidateWorkspace,
		shuttingDownError:      deps.ShuttingDownError,
		status: statusState{
			pumps:   make(map[string]*statusPump),
			handles: make(map[string]*statusPump),
		},
	}
}

func (s *Service) shuttingDown() bool {
	return s.isShuttingDown != nil && s.isShuttingDown()
}

func (s *Service) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// ResolveThreadPaths resolves the repository and active-workspace paths for a
// thread. The project row fallback preserves legacy imported/test rows whose
// denormalized ProjectPath is empty.
func (s *Service) ResolveThreadPaths(thread store.Thread) (project string, workspace string, err error) {
	workspace = strings.TrimSpace(thread.WorkspacePath)
	project = strings.TrimSpace(thread.ProjectPath)
	if project == "" && thread.ProjectID != "" && s.store != nil {
		if persisted, projectErr := s.store.GetProject(thread.ProjectID); projectErr == nil {
			project = strings.TrimSpace(persisted.Path)
		}
	}
	switch {
	case project == "" && workspace == "":
		return "", "", fmt.Errorf("thread %s has no git paths", thread.ID)
	case project == "":
		project = workspace
	case workspace == "":
		workspace = project
	}
	return project, workspace, nil
}

// ProjectPath resolves and validates a project id for git operations.
func (s *Service) ProjectPath(projectID string) (string, error) {
	if s.store == nil {
		return "", fmt.Errorf("git project path: store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", fmt.Errorf("git project path: projectId is required")
	}
	project, err := s.store.GetProject(projectID)
	if err != nil {
		return "", fmt.Errorf("git project path: resolve project %s: %w", projectID, err)
	}
	return project.Path, nil
}
