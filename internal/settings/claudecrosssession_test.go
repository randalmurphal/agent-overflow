package settings

import (
	"strings"
	"testing"
)

// "hold" is the CLI's own third value and the one Agent Overflow must
// never write: it parks a peer message with no approval surface a
// headless session can present. A save carrying it is refused with a
// message naming the two usable values, not silently coerced.
func TestValidateClaudeCrossSessionRefusesHold(t *testing.T) {
	_, err := validateClaudeCrossSession("claudeCrossSession", ClaudeCrossSession{Enabled: true, Inbound: "hold"})
	if err == nil {
		t.Fatal("validateClaudeCrossSession(hold) = nil error, want refusal")
	}
	for _, want := range []string{"accept", "refuse"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

func TestValidateClaudeCrossSessionAllowlist(t *testing.T) {
	for _, ok := range []ClaudeCrossSession{
		{},
		{Enabled: true},
		{Enabled: true, Inbound: ClaudeCrossSessionInboundAccept},
		{Enabled: true, Inbound: ClaudeCrossSessionInboundRefuse},
		// A stored policy on a disabled feature is legal: turning the
		// feature back on must not lose the choice.
		{Inbound: ClaudeCrossSessionInboundRefuse},
	} {
		if _, err := validateClaudeCrossSession("claudeCrossSession", ok); err != nil {
			t.Fatalf("validateClaudeCrossSession(%+v) = %v, want nil", ok, err)
		}
	}
	// Case matters and near-misses are refused: the CLI's own parse turns
	// an unknown value into UNSET, which is a third behavior the user
	// cannot have meant.
	for _, bad := range []string{"Accept", "ACCEPT", "deny", "off", "reject"} {
		if _, err := validateClaudeCrossSession("claudeCrossSession", ClaudeCrossSession{Enabled: true, Inbound: bad}); err == nil {
			t.Fatalf("validateClaudeCrossSession(%q) = nil error, want refusal", bad)
		}
	}
}

// The lenient path must keep an unreadable value from bricking the whole
// settings file — and must keep the ENABLED bit, which is not the thing
// that was unreadable.
func TestSanitizeClaudeCrossSessionDegradesToEnabledDefault(t *testing.T) {
	got := sanitizeClaudeCrossSession("claudeCrossSession", ClaudeCrossSession{Enabled: true, Inbound: "hold"})
	if !got.Enabled {
		t.Fatalf("sanitizeClaudeCrossSession dropped Enabled: %+v", got)
	}
	if got.Inbound != "" {
		t.Fatalf("sanitizeClaudeCrossSession kept %q, want the unusable policy dropped", got.Inbound)
	}
	// And the dropped policy resolves to the safe explicit value rather
	// than to unset-while-enabled.
	if eff := got.EffectiveInbound(); eff != ClaudeCrossSessionInboundAccept {
		t.Fatalf("EffectiveInbound after sanitize = %q, want %q", eff, ClaudeCrossSessionInboundAccept)
	}
}

// Enabled must never resolve to empty on the wire: an enabled-but-unset
// session falls into the CLI's mode-parity hold, which drops peer
// messages silently after a timeout.
func TestEffectiveInboundNeverEmptyWhileEnabled(t *testing.T) {
	if got := (ClaudeCrossSession{Enabled: true}).EffectiveInbound(); got != ClaudeCrossSessionInboundAccept {
		t.Fatalf("EffectiveInbound(enabled, unset) = %q, want %q", got, ClaudeCrossSessionInboundAccept)
	}
	if got := (ClaudeCrossSession{Enabled: true, Inbound: ClaudeCrossSessionInboundRefuse}).EffectiveInbound(); got != ClaudeCrossSessionInboundRefuse {
		t.Fatalf("EffectiveInbound(refuse) = %q", got)
	}
	// Disabled says nothing at all — the key is omitted and the CLI's own
	// unbound-inbox behavior stands.
	if got := (ClaudeCrossSession{Inbound: ClaudeCrossSessionInboundAccept}).EffectiveInbound(); got != "" {
		t.Fatalf("EffectiveInbound(disabled) = %q, want empty", got)
	}
}

func TestClaudeCrossSessionForProviderIsHeadlessClaudeOnly(t *testing.T) {
	s := Settings{ClaudeCrossSession: ClaudeCrossSession{Enabled: true, Inbound: ClaudeCrossSessionInboundRefuse}}
	if got := s.ClaudeCrossSessionForProvider("claude"); got != s.ClaudeCrossSession {
		t.Fatalf("ClaudeCrossSessionForProvider(claude) = %+v", got)
	}
	for _, other := range []string{"claude-tui", "codex", ""} {
		if got := s.ClaudeCrossSessionForProvider(other); got != (ClaudeCrossSession{}) {
			t.Fatalf("ClaudeCrossSessionForProvider(%q) = %+v, want zero", other, got)
		}
	}
}

// Off by default, and absent from a sparse write of zero settings so
// every settings file written before this field existed keeps reading as
// "cross-session messaging off".
func TestClaudeCrossSessionDefaultsOffAndIsSparse(t *testing.T) {
	if DefaultSettings.ClaudeCrossSession.Enabled {
		t.Fatal("cross-session messaging defaults on; it must be opt-in")
	}
	sparse, err := buildSparseMap(Settings{})
	if err != nil {
		t.Fatalf("buildSparseMap: %v", err)
	}
	if _, present := sparse["claudeCrossSession"]; present {
		t.Fatal("claudeCrossSession present in a sparse write of zero settings")
	}
}
