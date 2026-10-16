package app

import (
	"fmt"
	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// Claude can change its permission mode without being asked: since 2.1.299
// it leaves auto mode when a model switch lands on a model without auto
// support. The session adopts the reported mode as its base
// (provider/claude/permission_mode.go) and forwards
// EventPermissionModeChanged; this file moves the thread with it, so the
// access picker shows what the session runs and the reconciler does not
// plan a set_permission_mode the CLI would refuse.
//
// The thread's runtime mode follows rather than being restored. Switching
// back to a model with auto support leaves the thread on the mode the CLI
// moved to; the user picks Auto again, as in the CLI itself.

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
func (a *App) followClaudePermissionMode(threadID, sessionToken string) {
	go a.applyClaudePermissionMode(threadID, sessionToken)
}

// applyClaudePermissionMode converges the session's launch options and the
// thread row onto the session's current base permission mode. It reads the
// mode from the session rather than the event, so reports applied out of
// order still settle on the newest.
func (a *App) applyClaudePermissionMode(threadID, sessionToken string) {
	unlock := a.configApplyLocks().Lock(threadID)
	defer unlock()
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
