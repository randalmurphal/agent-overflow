package transport

import (
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/eventchan"
)

// TestChannelPolicyHasNoDuplicates guards the one failure mode the index
// build swallows: two rows for the same channel silently keep the last.
func TestChannelPolicyHasNoDuplicates(t *testing.T) {
	seen := make(map[eventchan.Channel]int, len(channelPolicies))
	for i, policy := range channelPolicies {
		if policy.Channel == "" {
			t.Fatalf("channelPolicies[%d] has an empty channel name", i)
		}
		if first, dup := seen[policy.Channel]; dup {
			t.Fatalf("channelPolicies has %q twice (rows %d and %d)", policy.Channel, first, i)
		}
		seen[policy.Channel] = i
	}
	if len(channelPolicyIndex) != len(channelPolicies) {
		t.Fatalf("index has %d entries for %d rows", len(channelPolicyIndex), len(channelPolicies))
	}
}

// TestChannelPolicyEveryRowHasAWhy: a row without a reason is a row nobody
// can review. Rows still awaiting a decision must say so explicitly (see
// unreviewedMarker).
func TestChannelPolicyEveryRowHasAWhy(t *testing.T) {
	for _, policy := range channelPolicies {
		if strings.TrimSpace(policy.Why) == "" {
			t.Errorf("%s has no Why", policy.Channel)
		}
	}
}

// The frozen non-default classifications, updated at each deliberate
// review. First captured verbatim from the four hand-authored maps the
// registry replaced; extended by the 2026-08-25 classification pass that
// reviewed every remaining fail-open row (members from that pass are
// commented). They are the behavior contract: every channel listed must
// still resolve to the same effective policy, and no channel outside them
// may acquire a non-default one silently. Changing a list below is a
// deliberate behavior change, not a refactor — the registry row's Why and
// this list move together.
var (
	frozenLoopbackOnlyChannels = []string{
		"browser:companion-state",  // local URLs and file paths
		"browser:install-progress", // 2026-08-26 pass
		"git:status",
		"harness:mock",     // 2026-08-25 pass
		"harness:perf",     // W3 bridge: per-process RSS + host detail
		"harness:replay",   // 2026-08-25 pass
		"harness:ui-query", // W3 bridge: a directive carrying DOM reads back
		"mcp:oauth-completed",
		"mcp:status",
		"power:keepawake", // 2026-08-25: launcher power directive, same posture as webview:trim
		"pr:updated",
		"provider:account",
		"provider:account_usage_error",
		"provider:approval",
		"provider:background_task_state",
		"provider:command_lifecycle", // 2026-08-25 pass
		"provider:queue_flushed",
		"provider:queue_restored",
		"provider:queue_state_changed",
		"provider:session_account",
		"provider:status",
		"provider:terminal_output",
		"provider:user_input",
		"session-import:progress",
		"terminal:exit",
		"terminal:output",
		"updater:download-started", // 2026-08-25 pass
		"updater:error",            // 2026-08-25 pass
		"updater:install",
		"updater:installing", // 2026-08-25 pass
		"updater:progress",   // 2026-08-25 pass
		"updater:ready",      // 2026-08-25 pass
		"updater:verifying",  // 2026-08-25 pass
		"usage:thread_cost",  // 2026-08-25 pass
		"webview:trim",       // 2026-08-25: launcher GC directive, same posture as updater:install
		"worktree:setup",
	}
	frozenRemoteOnlyChannels = []string{
		"highlight:seed",
	}
	frozenEphemeralChannels = []string{
		"browser:companion-state", // subscribe returns the complete snapshot
		"harness:ui-query",        // a one-shot query directive; a replayed one has no waiter
		"highlight:diff_seed",
		"highlight:seed",
		"updater:install",
		"webview:trim", // 2026-08-25: replaying a stale trim would GC an active session
	}
	frozenLatestOnlyChannels = []string{
		"browser:install-progress", // superseding artifact-install phase
		// A LEVEL, not an edge: a reconnecting launcher replays with a
		// zero cursor and must converge on the current keep-awake state.
		"power:keepawake",
		"spinner:changed",
		"system:stats",
		"theme:changed",
		"updater:progress", // 2026-08-25 pass
		"workflow:definitions-changed",
		"workflow:engine-state", // 2026-08-25 pass
	}
)

func TestChannelPolicyPreservesFrozenClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		frozen   []string
		classify func(string) bool
	}{
		{
			name:     "loopbackOnly",
			frozen:   frozenLoopbackOnlyChannels,
			classify: func(c string) bool { return channelAudience(c) == AudienceLoopbackOnly },
		},
		{
			name:     "remoteOnly",
			frozen:   frozenRemoteOnlyChannels,
			classify: func(c string) bool { return channelAudience(c) == AudienceRemoteOnly },
		},
		{
			name:     "ephemeral",
			frozen:   frozenEphemeralChannels,
			classify: func(c string) bool { return channelRetention(c) == RetentionEphemeral },
		},
		{
			name:     "latestOnly",
			frozen:   frozenLatestOnlyChannels,
			classify: func(c string) bool { return channelRetention(c) == RetentionLatestOnly },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := make(map[string]bool, len(tc.frozen))
			for _, channel := range tc.frozen {
				want[channel] = true
				if _, registered := policyForChannel(channel); !registered {
					t.Errorf("%s was in the old %s map but has no registry row", channel, tc.name)
					continue
				}
				if !tc.classify(channel) {
					t.Errorf("%s lost its %s classification", channel, tc.name)
				}
			}
			// And nothing gained one. Scanning the whole registry (not a
			// derived set) is what catches a NEW row quietly acquiring a
			// non-default policy nobody signed off on.
			for _, policy := range channelPolicies {
				if tc.classify(string(policy.Channel)) && !want[string(policy.Channel)] {
					t.Errorf("%s gained a %s classification the old map did not give it", policy.Channel, tc.name)
				}
			}
		})
	}
}

// TestChannelPolicyFailsClosedForUnregisteredChannels pins the behavior
// for a channel with no row: full-depth ring, delivered to loopback
// connections only. A channel nobody classified must never reach a LAN
// peer by omission (see unregisteredChannelPolicy — the fail-closed flip
// landed 2026-08-25 after every registered row was reviewed).
func TestChannelPolicyFailsClosedForUnregisteredChannels(t *testing.T) {
	const channel = "not-a-real:channel"
	policy, registered := policyForChannel(channel)
	if registered {
		t.Fatalf("%s is unexpectedly registered", channel)
	}
	if policy.Channel != eventchan.Channel(channel) {
		t.Errorf("fallback policy channel = %q, want %q", policy.Channel, channel)
	}
	if policy.Audience != AudienceLoopbackOnly {
		t.Errorf("fallback audience = %v, want AudienceLoopbackOnly", policy.Audience)
	}
	if policy.Retention != RetentionDefault {
		t.Errorf("fallback retention = %v, want RetentionDefault", policy.Retention)
	}
	if !eventVisibleToOrigin(channel, true) {
		t.Errorf("unregistered channel must stay visible to loopback (harness dynamic channels depend on it)")
	}
	if eventVisibleToOrigin(channel, false) {
		t.Errorf("unregistered channel is visible to a non-loopback origin — the fail-closed default regressed")
	}
}

// TestChannelPolicyUnreviewedWorklist never fails. It prints the rows
// whose classification was inherited rather than decided, so the review
// pass has a mechanical worklist:
//
//	go test ./internal/transport/ -run UnreviewedWorklist -v
func TestChannelPolicyUnreviewedWorklist(t *testing.T) {
	var unreviewed []string
	for _, policy := range channelPolicies {
		if strings.Contains(policy.Why, unreviewedMarker) {
			unreviewed = append(unreviewed, string(policy.Channel))
		}
	}
	sort.Strings(unreviewed)

	t.Logf("event channels: %d registered, %d unreviewed, %d classified",
		len(channelPolicies), len(unreviewed), len(channelPolicies)-len(unreviewed))
	for _, channel := range unreviewed {
		policy := channelPolicyIndex[channel]
		t.Logf("  unreviewed: %-32s audience=%v retention=%v", channel, policy.Audience, policy.Retention)
	}
}

// TestNotificationChannelsReachRemoteButStayHostProduced pins the pairing
// the notification audience rests on.
//
// Both channels are AudienceAny, so an attached remote client is told a
// turn finished and follows the reveal a click produces — the reason to
// attach one at all. That is only sound because PRODUCING either frame is
// host-side: `notification:send` comes from App.notifyOS, which no RPC
// exposes, and `notification:activated` comes from the
// `NotificationActivated` binding, which is LocalOnly. Drop that entry and
// the audience change becomes "any LAN token-holder can steer every
// attached client's pane focus" — the receive side stays innocuous only
// while the send side stays local. The two decisions live in different
// files, so this is where they are held together.
func TestNotificationChannelsReachRemoteButStayHostProduced(t *testing.T) {
	for _, channel := range []eventchan.Channel{
		eventchan.NotificationSend,
		eventchan.NotificationActivated,
	} {
		if !eventVisibleToOrigin(string(channel), false) {
			t.Errorf("%s must reach a non-loopback client", channel)
		}
		if !eventVisibleToOrigin(string(channel), true) {
			t.Errorf("%s must still reach a loopback client", channel)
		}
		// The launcher replays notification:send by cursor after a
		// reconnect, and an activation can arrive before the desktop
		// window's first socket exists. Neither survives a zero-capacity
		// or capacity-1 ring.
		if got := channelRetention(string(channel)); got != RetentionDefault {
			t.Errorf("%s retention = %v, want %v", channel, got, RetentionDefault)
		}
	}
	if !LocalOnlyMethods["NotificationActivated"] {
		t.Error("NotificationActivated left LocalOnlyMethods while notification:activated reaches remote clients: " +
			"a LAN peer could now steer every attached client's pane focus")
	}
}
