package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
)

// threadApplication composes store policy with explicit root runtime ports.
func (a *App) threadApplication() *threadapp.Service {
	a.threadAppOnce.Do(func() {
		a.threadApp = threadapp.New(threadapp.Deps{
			Store:            a.store,
			Models:           threadModelPolicy{app: a},
			Workspace:        threadWorkspacePort{app: a},
			WorktreeSetup:    threadWorktreeSetupPort{app: a},
			RecentWorkspaces: threadRecentWorkspacePort{app: a},
			LifeContext:      a.lifeCtx,
		})
	})
	return a.threadApp
}

func (a *App) hasActiveSession(threadID string) bool {
	_, ok := a.sessionManager().get(threadID)
	return ok
}

type threadModelPolicy struct{ app *App }

func (p threadModelPolicy) Seed(ctx context.Context, providerName, model string) store.ChatModelProfile {
	return p.app.seedChatModelProfile(ctx, providerName, model)
}

func (p threadModelPolicy) Sanitize(profile store.ChatModelProfile) store.ChatModelProfile {
	return p.app.sanitizeChatModelProfile(profile)
}

func (p threadModelPolicy) SupportsReasoningEffort(providerName, model, effort string) bool {
	return p.app.reasoningEffortSupportedForModel(providerName, model, effort)
}

func (p threadModelPolicy) CoerceReasoningEffort(providerName, model, effort string) string {
	return p.app.coerceReasoningEffortForModel(providerName, model, effort)
}

func (p threadModelPolicy) SupportsFastMode(providerName, model string) bool {
	return p.app.supportsFastModeForModel(providerName, model)
}

func (p threadModelPolicy) SavedFastModeStands(providerName, model string) bool {
	return p.app.savedFastModeStands(providerName, model)
}

func (p threadModelPolicy) ContextWindowOptions(providerName, model string) []provider.ContextWindowOption {
	return p.app.contextWindowOptionsForModel(providerName, model)
}

func (p threadModelPolicy) DraftDefaults(providerName, model, effort string, fastMode bool) (string, bool) {
	return p.app.draftModelDefaults(providerName, model, effort, fastMode)
}

func (p threadModelPolicy) DefaultModel(providerName string) string {
	return p.app.defaultModelForProvider(providerName)
}

func (p threadModelPolicy) ValidateModel(providerName, model string) error {
	return p.app.validateModelChoice(providerName, model)
}

type threadWorkspacePort struct{ app *App }

func (p threadWorkspacePort) CurrentBranch(workspacePath string) string {
	return p.app.gitCore().CurrentBranch(workspacePath)
}

func (p threadWorkspacePort) ObserveOrigin(workspacePath string) store.ThreadOrigin {
	return p.app.observeThreadOrigin(workspacePath)
}

func (p threadWorkspacePort) FindWorktree(projectPath, candidate string) (string, string, bool, error) {
	worktree, found, err := p.app.worktreeApplication().FindPresent(projectPath, candidate)
	return worktree.Path, worktree.Branch, found, err
}

// resolveWorktreeTarget resolves a name typed for a fresh worktree in
// projectPath (gitops.Core.ResolveWorktreeTarget); a branch it creates from
// a plain name is named by resolveWorktreeBranch. A failed origin fetch is
// logged and the refs are read as they stand.
func (a *App) resolveWorktreeTarget(ctx context.Context, projectPath, name string) (gitops.WorktreeTarget, error) {
	target, fetchErr, err := a.gitCore().ResolveWorktreeTarget(ctx, projectPath, name, a.resolveWorktreeBranch)
	if fetchErr != nil {
		log.Printf("worktree %q in %s: fetch origin: %v (reading the refs as they stand)", name, projectPath, fetchErr)
	}
	return target, err
}

// CreateWorktree cuts what cut.Target names without resolving anything
// again: an existing branch or tag is checked out as it is, a branch that
// does not exist is created from cut's base.
func (p threadWorkspacePort) CreateWorktree(
	ctx context.Context, projectPath string, cut threadapp.WorktreeCut,
) (threadapp.CutWorktree, error) {
	target := cut.Target
	if target.Ref.IsBranch() && !target.Exists {
		return p.cutNewBranch(ctx, projectPath, target.Ref.Name, cut)
	}
	if !target.Ref.IsBranch() && !target.Ref.IsTag() {
		return threadapp.CutWorktree{}, fmt.Errorf("a worktree checks out a branch or a tag, not %q", target.Ref.Wire())
	}
	if strings.TrimSpace(cut.Base) != "" || cut.BaseLocal {
		return threadapp.CutWorktree{}, fmt.Errorf("%s is checked out as it is, so a base does not apply", target.Ref.Wire())
	}
	if !target.Exists {
		return threadapp.CutWorktree{}, fmt.Errorf("there is no tag %q", target.Ref.Name)
	}
	return p.attachWorktree(projectPath, target.Ref)
}

// attachWorktree checks out an existing branch or tag as it is in a fresh
// worktree named after it. A tag leaves the worktree on no branch, so
// undoing the cut deletes none.
func (p threadWorkspacePort) attachWorktree(projectPath string, ref gitops.Ref) (threadapp.CutWorktree, error) {
	worktreePath, err := p.app.defaultWorktreePath(projectPath, ref.Name)
	if err != nil {
		return threadapp.CutWorktree{}, err
	}
	created, err := p.app.gitCore().AttachWorktreeAt(projectPath, worktreePath, ref)
	if err != nil {
		return threadapp.CutWorktree{}, err
	}
	made := threadapp.CutWorktree{Path: worktreePath}
	if ref.IsBranch() {
		made.Branch, made.CreatedBranch = ref.Name, created
	}
	return made, nil
}

// cutNewBranch creates branch resolvedBranch, exactly as named, in a fresh
// worktree from cut's base: the project's current branch when none is
// named. Naming it is resolveWorktreeTarget's, before this.
func (p threadWorkspacePort) cutNewBranch(ctx context.Context, projectPath, resolvedBranch string, cut threadapp.WorktreeCut) (threadapp.CutWorktree, error) {
	worktreePath, err := p.app.defaultWorktreePath(projectPath, resolvedBranch)
	if err != nil {
		return threadapp.CutWorktree{}, err
	}
	base, err := gitops.ParseRef(cut.Base)
	if err != nil {
		return threadapp.CutWorktree{}, fmt.Errorf("base: %w", err)
	}
	if base.IsZero() {
		if current := p.app.gitCore().CurrentBranch(projectPath); current != "" {
			base = gitops.BranchRef(current)
		}
	}
	if cut.BaseLocal {
		// The caller asked for this computer's head of the base, unpushed
		// commits included, so origin is not consulted at all.
		if err := p.app.gitCore().CreateWorktreeFromRef(projectPath, worktreePath, base, resolvedBranch); err != nil {
			return threadapp.CutWorktree{}, err
		}
		return threadapp.CutWorktree{Path: worktreePath, Branch: resolvedBranch, CreatedBranch: true}, nil
	}
	if err := p.app.cutWorktreeFromFreshRef(ctx, projectPath, worktreePath, base, resolvedBranch); err != nil {
		return threadapp.CutWorktree{}, err
	}
	return threadapp.CutWorktree{Path: worktreePath, Branch: resolvedBranch, CreatedBranch: true}, nil
}

func (p threadWorkspacePort) RemoveCutWorktree(projectPath string, cut threadapp.CutWorktree) error {
	core := p.app.gitCore()
	if err := core.RemoveWorktreeForce(projectPath, cut.Path, true); err != nil {
		return fmt.Errorf("remove worktree %s: %w", cut.Path, err)
	}
	if cut.CreatedBranch {
		if err := core.DeleteBranch(projectPath, cut.Branch, true); err != nil {
			return fmt.Errorf("delete branch %s: %w", cut.Branch, err)
		}
	}
	return nil
}

type threadWorktreeSetupPort struct{ app *App }

func (p threadWorktreeSetupPort) Start(thread store.Thread) {
	p.app.startThreadWorktreeSetup(thread)
}

type threadRecentWorkspacePort struct{ app *App }

// AddRecentWorkspace attributes the write to the connection that asked for the
// thread. The list is device tier, so an empty bucket — a backend-initiated
// create, an import, a test — lands on the backend machine's own screen
// rather than being dropped (internal/settings/residency.go).
//
// The class travels with the bucket because a Caller is one screen, not two
// halves. It moves no key on this path — the only key the write touches is
// recentWorkspaces, which no class row names — but it is what the pre-write
// projection is taken against, and a Caller assembled from a bucket and
// somebody else's class is the bug the pair exists to prevent.
func (p threadRecentWorkspacePort) AddRecentWorkspace(bucket, class, path string) {
	if p.app.settings != nil {
		p.app.settings.For(bucket, settings.DeviceClass(class)).AddRecentWorkspace(path)
	}
}

// Thread action locks are owned by threadapp. Session config-apply locks stay
// at root because they serialize live session mutations rather than thread
// application workflows.
func (a *App) threadLocks() *threadapp.Service { return a.threadApplication() }

// configApplyLocks serializes liveApplySessionConfig per thread. It remains a
// separate registry because reconcile callers arrive with and without the
// thread action lock held.
func (a *App) configApplyLocks() *keyedlock.Registry {
	a.sessionConfigApplyLocksOnce.Do(func() {
		if a.sessionConfigApplyLocks == nil {
			a.sessionConfigApplyLocks = keyedlock.New()
		}
	})
	return a.sessionConfigApplyLocks
}

var sessionAffectingFields = map[string]struct{}{
	"provider": {}, "model": {}, "mode": {}, "effort": {}, "fastMode": {},
	"contextWindow": {}, "contextSettings": {}, "workspace": {},
}

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
	a.reconcileSessionConfig(threadID, false)
	return refreshed, nil
}

func (a *App) restartWorkspaceSession(threadID string) error {
	if startState, ok := a.sessionManager().startState(threadID); ok {
		<-startState.Done
	}
	if !a.hasActiveSession(threadID) {
		return nil
	}
	return a.reconnectSessionLocked(context.Background(), threadID)
}

func (a *App) deleteThreadTreeLocked(threadID string) error {
	return a.threadApplication().DeleteTree(threadID, false, a.threadDeletePorts())
}

func (a *App) deleteThreadTreeWithSubtreeLocksHeld(threadID string) error {
	return a.threadApplication().DeleteTree(threadID, true, a.threadDeletePorts())
}

func (a *App) threadDeletePorts() threadapp.DeletePorts {
	return threadapp.DeletePorts{
		StopRemoteWork: func(threadID string) error {
			// Runs twice for a thread with a subtree; both halves are
			// conditional updates, so the second pass finds nothing to do.
			if err := a.stopThreadRequestsForDeletedThread(context.Background(), threadID); err != nil {
				return err
			}
			return a.cancelThreadRemoteCommands(context.Background(), threadID)
		},
		CleanProviderBackground: a.cleanThreadProviderBackground,
		StopSession:             a.stopSession,
		CancelWorktreeSetup:     a.cancelThreadWorktreeSetup,
		CancelShellCommand:      a.cancelThreadShellCommand,
		CloseTerminals: func(threadID string) error {
			if a.terminals == nil {
				return nil
			}
			return a.terminals.CloseThread(threadID)
		},
		CloseBrowserPages:  a.closeThreadBrowserPages,
		ClearSystemPrompt:  a.clearThreadSystemPrompt,
		RemoveDiscussion:   a.removeDeliberation,
		ClearAutoReconnect: a.clearAutoReconnectAttempted,
		CleanupAttachments: a.cleanupThreadAttachmentFiles,
		CleanupReplayLog: func(threadID string) error {
			if a.replay == nil {
				return nil
			}
			return a.replay.RemoveThreadLog(threadID)
		},
		Forget: func(threadID string) {
			a.forgetThreadMCP(threadID)
			a.terminalThreads.forget(threadID)
		},
		Deleted: func(thread store.Thread) { a.broadcastThreadDeleted(thread.ID) },
	}
}

func (a *App) cleanThreadProviderBackground(thread store.Thread) error {
	if provider.CapabilitiesForProvider(thread.Provider).BackgroundTerminalCleaner != provider.CodexBackgroundTerminalCleaner {
		return nil
	}
	if _, active := a.activeCodexSession(thread.ID); !active {
		return nil
	}
	if err := a.CleanCodexBackgroundTerminals(thread.ID); err != nil {
		log.Printf("delete thread %s: clean codex background terminals: %v", thread.ID, err)
	}
	return nil
}

func (a *App) cleanupThreadAttachmentFiles(threadID string) error {
	if a.attachments == nil {
		return nil
	}
	if err := a.attachments.DeleteThreadDir(threadID); err != nil {
		return fmt.Errorf("remove attachment files for %s: %w", threadID, err)
	}
	return nil
}

func (a *App) removeDeliberation(thread store.Thread) {
	a.discussionService().RemoveForThread(thread)
}

func (a *App) removeDeliberationByID(channelID string) {
	a.discussionService().Remove(channelID)
}

const applyActiveModeTimeout = 5 * time.Second

func (a *App) applyActiveModeChange(threadID string, sess session, mode provider.InteractionMode) bool {
	switch mode {
	case provider.ModeChat, provider.ModePlan:
		if sess.Claude != nil {
			ctx, cancel := context.WithTimeout(context.Background(), applyActiveModeTimeout)
			defer cancel()
			if err := sess.Claude.SetInteractionMode(ctx, mode); err != nil {
				log.Printf("thread %s: apply active Claude mode %q failed: %v", threadID, mode, err)
				return true
			}
		}
		return false
	default:
		log.Printf("thread %s: mode changed to %q while session is active; reconnect required", threadID, mode)
		return true
	}
}
