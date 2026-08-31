package transport

import (
	"encoding/json"
	"net/http"
	"testing"

	"agent-overflow/internal/eventchan"
)

// The scope dimension on the event registry (docs/specs/remote-access.md
// §5). A push must not be a way around the authorization its pull half
// enforces, and these are the tests that say so.

// TestEveryChannelPolicyRowNamesAValidScope. A row with the zero Scope is
// the failure this test exists for: Scope("") is not declared, so
// scopeBits has no entry, so its mask is zero, so the channel would be
// invisible to every session — a silent outage rather than a decision.
func TestEveryChannelPolicyRowNamesAValidScope(t *testing.T) {
	for _, policy := range channelPolicies {
		if !policy.Scope.Valid() {
			t.Errorf("%s carries scope %q, which is not a declared scope",
				policy.Channel, policy.Scope)
			continue
		}
		if scopeBits[policy.Scope] == 0 {
			t.Errorf("%s: scope %s has no bit", policy.Channel, policy.Scope)
		}
	}
}

// TestChannelScopeMatchesItsReadRPC pins the rule the Scope field's doc
// comment states, on the channels where getting it wrong would matter
// most: the push and the pull of the same data must need the same grant.
func TestChannelScopeMatchesItsReadRPC(t *testing.T) {
	for _, tc := range []struct {
		channel eventchan.Channel
		want    Scope
		because string
	}{
		{eventchan.TerminalOutput, ScopeTerminalOperate, "GetTerminalReplay carries the same bytes"},
		{eventchan.TerminalExit, ScopeTerminalOperate, "same PTY, same authorization"},
		{eventchan.ProviderTerminalOutput, ScopeTerminalOperate, "ProviderTerminalReplay carries the same bytes"},
		{eventchan.ProviderAccount, ScopeAccessAdmin, "ListProviderAccounts is the pull half — billing identity"},
		{eventchan.ProviderSessionAccount, ScopeAccessAdmin, "names which account a session is spending"},
		{eventchan.ProviderApproval, ScopeApprovalsRespond, "the prompt RespondToApproval answers"},
		{eventchan.ProviderQueueStateChanged, ScopeThreadsOperate, "GetQueueState is threads:operate"},
		{eventchan.GitStatus, ScopeGitOperate, "GetGitStatus is git:operate"},
		{eventchan.HighlightSeed, ScopeFilesRead, "HighlightCode is files:read"},
		{eventchan.ProviderItemEvent, ScopeThreadsRead, "the timeline ListItems returns"},
		{eventchan.SettingsUpdated, ScopeSettingsRead, "GetSettings is settings:read"},
		{eventchan.WebviewTrim, ScopeHost, "an imperative directive at this desktop's renderer"},
	} {
		policy, registered := policyForChannel(string(tc.channel))
		if !registered {
			t.Errorf("%s has no registry row", tc.channel)
			continue
		}
		if policy.Scope != tc.want {
			t.Errorf("%s scope = %s, want %s (%s)", tc.channel, policy.Scope, tc.want, tc.because)
		}
	}
}

// TestEventScopeFilterAdmitsOnlyGrantedChannels is the filter itself: a
// session holding threads:read sees the timeline and not the terminal,
// whatever its locality says.
func TestEventScopeFilterAdmitsOnlyGrantedChannels(t *testing.T) {
	filter := sessionScopeFilter([]string{string(ScopeThreadsRead)}, false)

	if !filter.allows(string(eventchan.ProviderItemEvent)) {
		t.Error("a threads:read session cannot see the timeline")
	}
	for _, channel := range []eventchan.Channel{
		eventchan.TerminalOutput, eventchan.ProviderAccount, eventchan.GitStatus,
	} {
		if filter.allows(string(channel)) {
			t.Errorf("a threads:read session sees %s", channel)
		}
	}
}

// TestHostScopedChannelFollowsHostPresence. `host` is not a grant, so the
// only thing that opens such a channel is being on this machine — the
// same rule AuthorizeSessionMethod applies to a host-scoped method, and
// what keeps the embedded webview's own session receiving updater and
// keep-awake frames.
func TestHostScopedChannelFollowsHostPresence(t *testing.T) {
	const channel = string(eventchan.UpdaterProgress)

	if !sessionScopeFilter(nil, true).allows(channel) {
		t.Error("a host-present session cannot see a host-scoped channel")
	}
	// Every GRANTABLE scope, and still refused off-host.
	var grantable []string
	for _, scope := range Scopes {
		if scope != ScopeHost {
			grantable = append(grantable, string(scope))
		}
	}
	if sessionScopeFilter(grantable, false).allows(channel) {
		t.Error("a remote session holding every grant sees a host-scoped channel")
	}
}

// TestUnregisteredChannelNeedsHostPresence is the fail-closed default in
// the scope vocabulary: a channel nobody classified is one nobody decided
// a remote form for. The harness's caller-named channels land here and
// keep working, because the harness is loopback by construction.
func TestUnregisteredChannelNeedsHostPresence(t *testing.T) {
	const channel = "not-a-real:channel"
	if _, registered := policyForChannel(channel); registered {
		t.Fatalf("%s is unexpectedly registered", channel)
	}
	if sessionScopeFilter(nil, false).allows(channel) {
		t.Error("an unregistered channel reached a session with no host presence")
	}
	if !sessionScopeFilter(nil, true).allows(channel) {
		t.Error("an unregistered channel was withheld from the host")
	}
}

// TestInactiveEventScopeFilterAdmitsEverything is the compatibility case
// and the one that must never regress: a connection naming no session —
// every launch-credential client — is judged by locality alone, exactly
// as it was before the scope dimension existed.
func TestInactiveEventScopeFilterAdmitsEverything(t *testing.T) {
	var inactive eventScopeFilter
	for _, policy := range channelPolicies {
		if !inactive.allows(string(policy.Channel)) {
			t.Fatalf("the zero filter withheld %s", policy.Channel)
		}
	}
	if !inactive.allows("not-a-real:channel") {
		t.Error("the zero filter withheld an unregistered channel")
	}
}

// TestScopedConnectionReceivesOnlyGrantedChannels is the wiring, over a
// real socket. The connection is LOOPBACK, so the origin gate admits
// every channel below and the grant filter is the only thing that can
// withhold one — which is the whole point of the wave.
func TestScopedConnectionReceivesOnlyGrantedChannels(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.SessionForRequest = func(*http.Request) (string, bool) { return "session-under-test", true }
		cfg.SessionLive = func(string) bool { return true }
		cfg.SessionScopes = func(string) ([]string, string) {
			return []string{string(ScopeThreadsRead)}, ""
		}
	})
	conn := f.dial(t)

	// Withheld first, then admitted. If the scope filter leaked, the
	// terminal frame is what the read below returns, and the assertion
	// names it.
	if _, err := f.bus.Emit(eventchan.TerminalOutput, "withheld"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Emit(eventchan.ProviderItemEvent, "admitted"); err != nil {
		t.Fatal(err)
	}

	var got ServerFrame
	if err := json.Unmarshal(readPastHello(t, conn), &got); err != nil {
		t.Fatal(err)
	}
	if got.Channel != string(eventchan.ProviderItemEvent) {
		t.Fatalf("first frame = %s, want %s — a channel outside the session's grants reached it",
			got.Channel, eventchan.ProviderItemEvent)
	}
}
