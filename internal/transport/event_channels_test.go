package transport

import (
	"sort"
	"strings"
	"testing"
)

// TestChannelPolicyHasNoDuplicates guards the one failure mode the index
// build swallows: two rows for the same channel silently keep the last.
func TestChannelPolicyHasNoDuplicates(t *testing.T) {
	seen := make(map[string]int, len(channelPolicies))
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
// can review. Unreviewed rows say so explicitly (see unreviewedWhy).
func TestChannelPolicyEveryRowHasAWhy(t *testing.T) {
	for _, policy := range channelPolicies {
		if strings.TrimSpace(policy.Why) == "" {
			t.Errorf("%s has no Why", policy.Channel)
		}
	}
}

// The frozen contents of the four hand-authored maps this registry
// replaced, captured verbatim at the refactor. They are the behavior
// contract: every channel one of them named must still resolve to the same
// effective policy, and no channel outside them may have acquired a
// non-default one. Changing a list below is a deliberate behavior change,
// not a refactor.
var (
	frozenLoopbackOnlyChannels = []string{
		"git:status",
		"mcp:oauth-completed",
		"mcp:status",
		"notification:activated",
		"notification:send",
		"pr:updated",
		"provider:account",
		"provider:account_usage_error",
		"provider:approval",
		"provider:background_task_state",
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
		"updater:install",
		"worktree:setup",
	}
	frozenRemoteOnlyChannels = []string{
		"highlight:seed",
	}
	frozenEphemeralChannels = []string{
		"highlight:diff_seed",
		"highlight:seed",
		"updater:install",
	}
	frozenLatestOnlyChannels = []string{
		"spinner:changed",
		"system:stats",
		"theme:changed",
		"workflow:definitions-changed",
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
				if tc.classify(policy.Channel) && !want[policy.Channel] {
					t.Errorf("%s gained a %s classification the old map did not give it", policy.Channel, tc.name)
				}
			}
		})
	}
}

// TestChannelPolicyFailsOpenForUnregisteredChannels pins today's behavior
// for a channel with no row: full-depth ring, visible to everyone. See the
// TODO on unregisteredChannelPolicy for the planned fail-closed flip —
// when that lands, this test flips with it.
func TestChannelPolicyFailsOpenForUnregisteredChannels(t *testing.T) {
	const channel = "not-a-real:channel"
	policy, registered := policyForChannel(channel)
	if registered {
		t.Fatalf("%s is unexpectedly registered", channel)
	}
	if policy.Channel != channel {
		t.Errorf("fallback policy channel = %q, want %q", policy.Channel, channel)
	}
	if policy.Audience != AudienceAny {
		t.Errorf("fallback audience = %v, want AudienceAny", policy.Audience)
	}
	if policy.Retention != RetentionDefault {
		t.Errorf("fallback retention = %v, want RetentionDefault", policy.Retention)
	}
	if !eventVisibleToOrigin(channel, true) || !eventVisibleToOrigin(channel, false) {
		t.Errorf("unregistered channel is not visible to both origins")
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
			unreviewed = append(unreviewed, policy.Channel)
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
