package transport

import (
	"testing"

	"agent-overflow/internal/eventchan"
)

// Pins the webview:trim directive's wire posture (see app_webview_trim.go
// for the pipeline). Same shape as the self-update contract test: a policy
// drift ships a directive channel that is no longer loopback-only/ephemeral,
// or a trim RPC a session could be granted.
func TestWebviewTrimContractClassifications(t *testing.T) {
	if channelAudience(string(eventchan.WebviewTrim)) != AudienceLoopbackOnly {
		t.Errorf("%q must be loopback-only: it commands a GC pause in the desktop renderer, and only the launcher on this host may act on it", eventchan.WebviewTrim)
	}
	if channelRetention(string(eventchan.WebviewTrim)) != RetentionEphemeral {
		t.Errorf("%q must be ephemeral: replaying a backlog after a reconnect would fire GC pauses into a session that may be active again", eventchan.WebviewTrim)
	}
	if got := classify("RequestWebviewMemoryTrim").Scope; got != ScopeHost {
		t.Errorf(`"RequestWebviewMemoryTrim" carries scope %q, want %q: a remote client's idleness says nothing about the desktop session, and the method is a jank lever — host presence is the only key that fits it`, got, ScopeHost)
	}
}
