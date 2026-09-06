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
	// Re-adjudicated 2026-09-03: the nineteen thread- and
	// workspace-state channels that used to be here are AudienceAny now,
	// and TestLoopbackOnlyIsForHostDirectivesOnly holds both halves of
	// that split by name. What is left is the host-directive residue —
	// launcher instructions, harness tooling, the desktop self-updater,
	// the native browser pane — for which loopback-only is a statement
	// about the CONSUMER rather than a disclosure control.
	frozenLoopbackOnlyChannels = []string{
		// Wave 7a: how one attach ended. Same posture as the four
		// host-scoped methods it belongs to — attaching this installation
		// to another machine is something only the person at this keyboard
		// does, and the frame names a machine they work on.
		"backend:attach",
		// 2026-09-03 (the convergence wave): a removal or a rename of an
		// attached machine, same posture as backend:attach — the four RPCs
		// that move the set are `host` and act on this process's own
		// profile directory.
		"backend:set-changed",
		"browser:companion-state",  // local URLs and file paths
		"browser:host",             // 2026-08-31: launcher pane directive, same posture as webview:trim
		"harness:mock",             // 2026-08-25 pass
		"harness:perf",             // W3 bridge: per-process RSS + host detail
		"harness:replay",           // 2026-08-25 pass
		"harness:ui-query",         // W3 bridge: a directive carrying DOM reads back
		"power:keepawake",          // 2026-08-25: launcher power directive, same posture as webview:trim
		"updater:download-started", // 2026-08-25 pass
		"updater:error",            // 2026-08-25 pass
		"updater:install",
		"updater:installing", // 2026-08-25 pass
		"updater:progress",   // 2026-08-25 pass
		"updater:ready",      // 2026-08-25 pass
		"updater:verifying",  // 2026-08-25 pass
		"webview:trim",       // 2026-08-25: launcher GC directive, same posture as updater:install
	}
	frozenRemoteOnlyChannels = []string{
		"highlight:seed",
	}
	frozenEphemeralChannels = []string{
		"browser:companion-state", // the thread-state read returns the complete snapshot
		"browser:host",            // 2026-08-31: replaying a stale pane directive reopens closed pages
		"harness:ui-query",        // a one-shot query directive; a replayed one has no waiter
		"highlight:diff_seed",
		"highlight:seed",
		// Wave 8i: an authorize URL is a one-use PKCE challenge and a
		// device code dies with its flow, so a replayed frame offers a
		// link that no longer answers. GetProviderLoginState is the
		// reconnect path instead.
		"provider:login",
		"updater:install",
		"webview:trim", // 2026-08-25: replaying a stale trim would GC an active session
	}
	frozenLatestOnlyChannels = []string{
		"backend:name-changed", "access:devices-changed",
		"agent-computers:changed", // dirty signal; the selected computer's table is re-read
		// Wave 9: whole-state, replaced in full every scan tick, so a
		// default ring would hold minutes of superseded lists and replay
		// all of them to be overwritten by the last.
		"devserver:list",
		// A LEVEL, not an edge: a reconnecting launcher replays with a
		// zero cursor and must converge on the current keep-awake state.
		"power:keepawake",
		// At most ONE frame per process lifetime, published when the
		// activation gate opens, so "newest supersedes" holds trivially. The
		// ring is what lets a client reconnecting a second after the restart
		// still find its update's outcome.
		"service:update-outcome",
		// ONE global flow per process — RequestServiceUpdate refuses a
		// second while one runs — so the newest frame fully supersedes
		// every earlier one, and a client reconnecting mid-download wants
		// the current phase rather than the ticks it missed.
		"service:update-status",
		"spinner:changed",
		"system:stats",
		"theme:changed",
		"updater:progress", // 2026-08-25 pass
		"workflow:definitions-changed",
		"workflow:engine-state", // 2026-08-25 pass
		// 2026-09-03 (the convergence wave): each an UNKEYED whole-answer or
		// payload-less refetch nudge, so N retained frames would be N
		// identical refetches — the same membership as spinner:changed.
		"chatbar:favorites",
		"discussion:definitions-changed",
		"keybindings:updated",
		"provider:accounts_changed",
	}
	// Wave 6d, extended in 6d2. Membership means a watching connection
	// stops receiving the channel for threads it did not name, so a row
	// joining this list is a claim that NOTHING off-pane reads it —
	// established by sweeping the frontend consumers, never by the
	// channel's name. The two highlight rows are span cache-warmers whose
	// absence costs a highlight RPC and nothing else. provider:item_event
	// joined once its six off-pane consumers were re-homed onto wildcard
	// carriers (thread:error_notice, thread:updated,
	// provider:background_tasks_changed); its row names them.
	frozenEntityFilteredChannels = []string{
		"highlight:diff_seed",
		"highlight:seed",
		"provider:item_event",
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
		{
			name:     "entityFiltered",
			frozen:   frozenEntityFilteredChannels,
			classify: channelEntityFiltered,
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
// `NotificationActivated` binding, which is `//ao:scope host` — a call
// with no remote form, admitted by host presence and by no grant any
// session can hold. Re-scope it and the audience change becomes "any
// paired client can steer every attached client's pane focus" — the
// receive side stays innocuous only while the send side stays host-only.
// The two decisions live in different files, so this is where they are
// held together.
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
	if got := classify("NotificationActivated").Scope; got != ScopeHost {
		t.Errorf("NotificationActivated carries scope %q while notification:activated reaches remote clients: "+
			"any session granted %[1]q could now steer every attached client's pane focus", got)
	}
}
