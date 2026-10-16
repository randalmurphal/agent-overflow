package app

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
)

// Claude can change its permission mode without being asked: since 2.1.299
// it leaves auto mode when a model switch lands on a model without auto
// support. The session adopts the reported mode as its base
// (provider/claude/permission_mode.go) and forwards
// EventPermissionModeChanged; this file moves the thread with it, so the
// access picker shows what the session runs and the reconciler does not
// plan a set_permission_mode the CLI would refuse.
//
// Switching back to a model with auto support brings the model's remembered
// Auto back (its chat model profile), never silently: the switch posts the
// matching notice (noticeClaudeAutoRestored).

// runtimeModeLabels names each runtime mode the way the composer's access
// picker does (AccessToggle.svelte), for the notice text.
var runtimeModeLabels = map[provider.RuntimeMode]string{
	provider.RuntimeReadOnly:         "Read-only",
	provider.RuntimeApprovalRequired: "Supervised",
	provider.RuntimeAutoAcceptEdits:  "Auto-accept edits",
	provider.RuntimeAuto:             "Auto",
	provider.RuntimeFullAccess:       "Full access",
}

// followClaudePermissionMode runs off the provider event worker: it waits
// for the per-thread config-apply lock, which a reconcile holds while its
// set_model waits for an ack the read loop delivers behind this event.
// Shutdown joins it (permissionModeFollowWG).
func (a *App) followClaudePermissionMode(threadID, sessionToken string) {
	a.permissionModeFollowWG.Go(func() { a.applyClaudePermissionMode(threadID, sessionToken) })
}

// applyClaudePermissionMode converges the session's launch options and the
// thread row onto the session's current base permission mode. It reads the
// mode from the session rather than the event, so reports applied out of
// order still settle on the newest.
//
// It takes the thread's mutation lock first, as the model switch that
// caused the change does. The switch broadcasts the row it read before this
// change landed; holding the same lock puts this change after that
// broadcast, so a client's access picker ends on the mode the session runs.
func (a *App) applyClaudePermissionMode(threadID, sessionToken string) {
	unlockThread, err := a.threadApplication().LockMutable(a.lifeCtx(), threadID)
	if err != nil {
		log.Printf("thread %s: follow claude permission mode: %v", threadID, err)
		return
	}
	defer unlockThread()
	unlock := a.configApplyLocks().Lock(threadID)
	defer unlock()
	if a.lifeCtx().Err() != nil {
		return
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.Token != sessionToken || sess.Claude == nil {
		return
	}
	cliMode := sess.Claude.BasePermissionMode()
	next, ok := claude.RuntimeModeForPermissionMode(cliMode)
	if !ok {
		log.Printf("thread %s: claude reported permission mode %q, which no runtime mode runs; thread left as is", threadID, cliMode)
		return
	}
	prev := provider.NormalizeRuntimeMode(string(sess.LaunchOptions.RuntimeMode))
	if next == prev {
		return
	}
	if next == provider.RuntimeReadOnly {
		// Read-only also removes tools at spawn, which a mode change on a
		// running process cannot do; claiming it would overstate the session.
		log.Printf("thread %s: claude moved to %q on its own; read-only needs a restart, thread left as is", threadID, cliMode)
		return
	}
	opts := sess.LaunchOptions
	opts.RuntimeMode = next
	a.sessionManager().updateLaunchOpts(threadID, sessionToken, opts)

	changed, err := a.store.SwapRuntimeMode(threadID, string(prev), string(next))
	if err != nil {
		log.Printf("thread %s: follow claude permission mode %q: %v", threadID, cliMode, err)
		a.emitErrorToThread(threadID, fmt.Sprintf(
			"Claude Code now runs %s, but the thread's access could not be updated: %v", runtimeModeLabels[next], err))
		return
	}
	if !changed {
		// The user picked another mode meanwhile; the reconciler applies
		// it against the launch options updated above.
		log.Printf("thread %s: claude moved to %q on its own; the thread's mode changed meanwhile and stays", threadID, cliMode)
		return
	}
	a.emitRuntimeModeChanged(threadID, next)
	notice := fmt.Sprintf("Claude Code turned off %s on %s. This thread's access is now %s.",
		runtimeModeLabels[prev], provider.TrimContextMarker(opts.Model), runtimeModeLabels[next])
	if err := a.emitNoticeToThread(threadID, noticeKindWarning, notice); err != nil {
		log.Printf("thread %s: %v", threadID, err)
	}
}

// noticeClaudeAutoRestored posts the notice for a Claude model switch that
// moved the thread back to Auto from another mode: the new model's
// remembered settings carry Auto. updated is the row after the live
// reconcile; a running session that did not keep auto (the CLI refused it
// for this model, which posts its own notice) gets none.
func (a *App) noticeClaudeAutoRestored(update threadapp.ModelUpdate, updated store.Thread) {
	if updated.Provider != string(provider.Claude) || update.Previous.Model == updated.Model {
		return
	}
	if provider.NormalizeRuntimeMode(update.Previous.RuntimeMode) == provider.RuntimeAuto ||
		provider.NormalizeRuntimeMode(updated.RuntimeMode) != provider.RuntimeAuto {
		return
	}
	if sess, ok := a.sessionManager().get(updated.ID); ok && sess.Claude != nil && sess.Claude.BasePermissionMode() != "auto" {
		return
	}
	notice := fmt.Sprintf("Auto is back on for %s. This thread's access is %s again.",
		provider.TrimContextMarker(updated.Model), runtimeModeLabels[provider.RuntimeAuto])
	if err := a.emitNoticeToThread(updated.ID, noticeKindWarning, notice); err != nil {
		log.Printf("thread %s: %v", updated.ID, err)
	}
}
