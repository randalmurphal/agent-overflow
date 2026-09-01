package threadapp

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
)

// ModelPolicy is the model-catalog boundary used by creation and selection.
// Root supplies the live-catalog-aware implementation; the service owns how
// those answers are applied to thread rows.
type ModelPolicy interface {
	Seed(providerName, model string) store.ChatModelProfile
	Sanitize(profile store.ChatModelProfile) store.ChatModelProfile
	SupportsReasoningEffort(providerName, model, effort string) bool
	CoerceReasoningEffort(providerName, model, effort string) string
	SupportsFastMode(providerName, model string) bool
	DraftDefaults(providerName, model, effort string, fastMode bool) (string, bool)
	Remember(thread store.Thread)
}

// Workspace is the explicit git/worktree subprocess port. Thread policy asks
// for outcomes; root owns the real git implementation and app lifetime.
type Workspace interface {
	CurrentBranch(workspacePath string) string
	FindWorktree(projectPath, candidate string) (path, branch string, found bool, err error)
	CreateWorktree(ctx context.Context, projectPath, branch string) (path, resolvedBranch string, err error)
	// ObserveOrigin reads the workspace's git coordinates as they stand right
	// now. It has no error return because every failure — no repository, no
	// remote, an unborn branch — means the same thing to a caller recording
	// provenance: that coordinate is not known. Empty fields say so.
	ObserveOrigin(workspacePath string) store.ThreadOrigin
}

// WorktreeSetup owns asynchronous setup execution. Creation invokes it only
// after the thread row commits so setup can persist state against that row.
type WorktreeSetup interface {
	Start(thread store.Thread)
}

// RecentWorkspaces is the settings side effect of successful thread creation.
//
// The list is DEVICE tier (docs/specs/remote-access.md §6), so the write is
// attributed to a SCREEN: the bucket the calling connection's device-tier
// settings live in — empty when the backend created the thread on its own
// behalf — and the class of device behind it, which is what the settings
// service resolves device-class defaults from. Root reads both off the
// connection in one step; this package only carries them, and carries them
// TOGETHER, because a bucket paired with somebody else's class is a screen
// that does not exist.
type RecentWorkspaces interface {
	AddRecentWorkspace(bucket, class, path string)
}

type Deps struct {
	Store            *store.Store
	Models           ModelPolicy
	Workspace        Workspace
	WorktreeSetup    WorktreeSetup
	RecentWorkspaces RecentWorkspaces
	LifeContext      func() context.Context
	HomeDir          func() (string, error)
	Now              func() time.Time
	NewID            func() string
}

// Service owns thread persistence policy and action serialization. It does not
// own provider processes or provider session files.
type Service struct {
	deps  Deps
	locks *keyedlock.Registry
}

func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.LifeContext == nil {
		deps.LifeContext = context.Background
	}
	return &Service{deps: deps, locks: keyedlock.New()}
}

func (s *Service) database(action string) (*store.Store, error) {
	if s == nil || s.deps.Store == nil {
		return nil, fmt.Errorf("%s: store unavailable", action)
	}
	return s.deps.Store, nil
}

func (s *Service) modelPolicy(action string) (ModelPolicy, error) {
	if s == nil || s.deps.Models == nil {
		return nil, fmt.Errorf("%s: model policy unavailable", action)
	}
	return s.deps.Models, nil
}

// observeOrigin reads a workspace's git coordinates for a thread about to be
// created, tolerating a service wired without a workspace port (tests, and the
// degraded startup path) by reporting nothing known.
func (s *Service) observeOrigin(workspacePath string) store.ThreadOrigin {
	if s == nil || s.deps.Workspace == nil || workspacePath == "" {
		return store.ThreadOrigin{}
	}
	return s.deps.Workspace.ObserveOrigin(workspacePath)
}

func (s *Service) Lock(threadID string) func() {
	return s.locks.Lock(threadID)
}

func (s *Service) LockCtx(ctx context.Context, threadID string) (func(), error) {
	return s.locks.LockCtx(ctx, threadID)
}

func (s *Service) Refs(threadID string) int {
	return s.locks.Refs(threadID)
}
