package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/webview2host"
)

// The CDP tunnel's wire contract is split across two packages: this one owns
// the route and who may reach it, internal/webview2host owns the path and the
// directive the launcher answers. Both sides spell the route, so a rename on
// either would leave the launcher dialling a path that no longer exists and
// the only symptom would be a pane that never appears. The path is a literal
// here because internal/surfaces' route gate resolves a pattern only from a
// literal or a package-level constant in the registering package.
func TestCDPTunnelPathMatchesWebview2Host(t *testing.T) {
	if CDPTunnelPath != webview2host.CDPTunnelPath {
		t.Errorf("CDPTunnelPath = %q, webview2host.CDPTunnelPath = %q: the route this mux registers "+
			"and the route the launcher dials must be the same string",
			CDPTunnelPath, webview2host.CDPTunnelPath)
	}
}

// The directive channel that rides beside the tunnel. Its frames create, move,
// show and destroy real browser windows inside the launcher's own window, so
// its only legitimate consumer is that launcher, and no session grant opens it.
func TestBrowserHostDirectiveClassification(t *testing.T) {
	policy, registered := policyForChannel(string(eventchan.BrowserHost))
	if !registered {
		t.Fatalf("%q has no registry row", eventchan.BrowserHost)
	}
	if policy.Audience != AudienceLoopbackOnly {
		t.Errorf("%q must be loopback-only: only the launcher on this host can act on it", eventchan.BrowserHost)
	}
	if policy.Retention != RetentionEphemeral {
		t.Errorf("%q must be ephemeral: replaying a stale directive after a reconnect would reopen pages the user closed", eventchan.BrowserHost)
	}
	if policy.Scope != ScopeHost {
		t.Errorf("%q carries scope %q, want %q: hosting a native view has no remote form", eventchan.BrowserHost, policy.Scope, ScopeHost)
	}
	if got := classify("BrowserHostReport").Scope; got != ScopeHost {
		t.Errorf("BrowserHostReport carries scope %q, want %q: it settles pane-host state for real "+
			"windows on this machine, and its caller is the launcher beside this backend", got, ScopeHost)
	}
}
