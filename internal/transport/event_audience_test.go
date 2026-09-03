package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
)

// The audience split, by name, in two lists (re-adjudicated 2026-09-03).
//
// AudienceAny group: every channel here carries THREAD or WORKSPACE state.
// A session that holds the channel's Scope holds the state, and that scope
// equals the scope of the RPC returning the same data — so the push is not
// a way around the pull's authorization, and withholding it only left a
// connected client rendering a screen it was already allowed to read.
//
// AudienceLoopbackOnly group: every channel here is a directive to a
// process on this host (the launcher's power, pane, swap and trim
// instructions), harness tooling, the desktop self-updater's own
// lifecycle, or the native browser pane. There is no remote consumer to
// serve, which is a fact about the CONSUMER rather than a disclosure
// control — the remote update path has its own service:update-* channels
// and they are AudienceAny.
//
// Both lists are spelled as eventchan constants and checked in both
// directions, so the next re-classification is a deliberate edit of two
// lists rather than a row somebody flipped in passing.
var (
	audienceAnyStateChannels = []eventchan.Channel{
		eventchan.DraftUpdated,
		eventchan.GitStatus,
		eventchan.MCPOAuthCompleted,
		eventchan.MCPStatus,
		eventchan.PRUpdated,
		eventchan.ProviderApproval,
		eventchan.ProviderBackgroundTaskState,
		eventchan.ProviderCommandLifecycle,
		eventchan.ProviderQueueFlushed,
		eventchan.ProviderQueueRestored,
		eventchan.ProviderQueueStateChanged,
		eventchan.ProviderSessionAccount,
		eventchan.ProviderTerminalOutput,
		eventchan.ProviderUserInput,
		eventchan.SessionImportProgress,
		eventchan.TerminalExit,
		eventchan.TerminalOutput,
		eventchan.UsageThreadCost,
		eventchan.WorktreeSetup,
	}
	audienceLoopbackOnlyHostChannels = []eventchan.Channel{
		eventchan.BackendAttach,
		eventchan.BrowserCompanionState,
		eventchan.BrowserHost,
		eventchan.HarnessMock,
		eventchan.HarnessPerf,
		eventchan.HarnessReplay,
		eventchan.HarnessUIQuery,
		eventchan.PowerKeepAwake,
		eventchan.UpdaterDownloadStarted,
		eventchan.UpdaterError,
		eventchan.UpdaterInstall,
		eventchan.UpdaterInstalling,
		eventchan.UpdaterProgress,
		eventchan.UpdaterReady,
		eventchan.UpdaterVerifying,
		eventchan.WebviewTrim,
	}
)

// TestLoopbackOnlyIsForHostDirectivesOnly pins the split above by name,
// and pins the RULE that produced it: loopback-only is decided by who can
// legitimately consume a frame, never by how sensitive the frame is. A
// channel of thread or workspace state that shows up in the second list
// is the wave 6d2 regression coming back — the per-method local-only
// table is gone, so an off-host caller reaching the RPC and not the push
// is a client rendering a screen that silently stopped updating.
func TestLoopbackOnlyIsForHostDirectivesOnly(t *testing.T) {
	inAny := make(map[eventchan.Channel]bool, len(audienceAnyStateChannels))
	for _, channel := range audienceAnyStateChannels {
		inAny[channel] = true
		policy, registered := policyForChannel(string(channel))
		if !registered {
			t.Errorf("%s has no registry row", channel)
			continue
		}
		if policy.Audience != AudienceAny {
			t.Errorf("%s audience = %s, want any: it carries thread or workspace state, "+
				"and %s is the gate that decides who reaches it", channel, policy.Audience, policy.Scope)
		}
		// The audience is only sound because the Scope column is doing the
		// work. A row that widened its audience and took `host` on Scope
		// would reach nobody it was widened for.
		if policy.Scope == ScopeHost {
			t.Errorf("%s is AudienceAny with scope host, which no session can hold: "+
				"the widening reaches only the machine it was not needed on", channel)
		}
		if !eventVisibleToOrigin(string(channel), false) {
			t.Errorf("%s is withheld from a non-loopback connection", channel)
		}
	}

	for _, channel := range audienceLoopbackOnlyHostChannels {
		if inAny[channel] {
			t.Fatalf("%s is in both lists", channel)
		}
		policy, registered := policyForChannel(string(channel))
		if !registered {
			t.Errorf("%s has no registry row", channel)
			continue
		}
		if policy.Audience != AudienceLoopbackOnly {
			t.Errorf("%s audience = %s, want loopback-only: its only legitimate consumer "+
				"is a process on this host", channel, policy.Audience)
		}
		if eventVisibleToOrigin(string(channel), false) {
			t.Errorf("%s reached a non-loopback connection", channel)
		}
	}

	// Both directions: no THIRD loopback-only row may appear without
	// joining the host-directive list and earning that argument.
	declared := make(map[eventchan.Channel]bool, len(audienceLoopbackOnlyHostChannels))
	for _, channel := range audienceLoopbackOnlyHostChannels {
		declared[channel] = true
	}
	for _, policy := range channelPolicies {
		if policy.Audience == AudienceLoopbackOnly && !declared[policy.Channel] {
			t.Errorf("%s is loopback-only and is on neither list: say which host process "+
				"consumes it, or give it the scope its pull RPC carries and open it", policy.Channel)
		}
	}
}
