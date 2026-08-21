package settings

import (
	"fmt"
	"log"
	"strings"
)

// Claude Code cross-session messaging — the machine-wide peer inbox.
//
// Claude Code 2.1.224+ lets one Claude session on a host discover
// (`ListAgents`) and address (`SendMessage`) another. A delivered peer
// message arrives in the recipient as a USER-ROLE turn the recipient
// never asked for, wrapped in `<cross-session-message from="..."
// from-name="...">`, and the model answers it like any other prompt.
//
// Two things have to be true for an Agent Overflow thread to take part,
// and they are two different mechanisms, which is why this is one struct
// rather than two settings:
//
//  1. **The inbox has to be bound.** It is gated behind a GrowthBook
//     experiment with one environment override, `CLAUDE_CODE_HARBOR_KITE`
//     — a parsed boolean, so "0" and "" are OFF. With the gate open the
//     session binds a unix socket and `system/init` carries
//     `messaging_socket_path`; without it the CLI logs
//     "cross-session messaging gate off" and binds nothing, which no
//     settings key can undo (spike-verified 2.1.237, /tmp/spike-xsession).
//     That is what `Enabled` controls.
//  2. **Delivery policy.** Once bound, the `crossSessionInbound` key in
//     the `--settings` block decides what happens to an arriving message.
//     That is `Inbound`.
//
// Enabled is NOT the only thing that can bind the inbox, which is why
// "off" is a stated refusal rather than silence. The gate variable is
// only an OVERRIDE: `tengu_harbor_kite` can bind the socket remotely for
// a user who never opted in here, and `tengu_harbor_kite_mode_emit` —
// which has no environment override at all — turns on the permission-
// class attestation the CLI's unset-key default (mode parity) reads. A
// disabled AO session therefore spawns with an explicit
// `"crossSessionInbound":"refuse"`, so a remotely bound inbox still
// delivers nothing. See claudeCrossSessionInbound in
// internal/provider/claude/options.go, which is the wall.
//
// Discovery is keyed on a SHARED `CLAUDE_CONFIG_DIR` — the peer registry
// is `<CLAUDE_CONFIG_DIR>/sessions/<pid>.json`. Agent Overflow's Claude
// sessions deliberately CLEAR that variable (claude.NewSession's
// `UnsetEnv`), so they land in the user's own `~/.claude` and can see —
// and be seen by — the user's terminal sessions, provided those also run
// with the gate open.
type ClaudeCrossSession struct {
	// Enabled opens the gate: the spawn exports
	// CLAUDE_CODE_HARBOR_KITE=1 and passes `--name`, so the session is
	// discoverable and addressable by other Claude sessions on this
	// machine. Default FALSE — letting another process start a turn in
	// the user's thread is opt-in, and the CLI's own default is an
	// experiment flag we neither control nor want to inherit silently.
	Enabled bool `json:"enabled,omitempty"`
	// Inbound is the delivery policy, meaningful only when Enabled.
	// Empty means EffectiveInbound's default, which is Accept — see
	// there for why "unset" is never sent on the wire.
	Inbound string `json:"inbound,omitempty"`
}

// ClaudeCrossSessionInbound values.
//
// The CLI's own schema is `["accept","hold","refuse"]`, and Agent
// Overflow deliberately offers only TWO of the three. "hold" is a black
// hole for a headless session and is never emitted — see
// `claudeCrossSessionInboundHoldIsNeverEmitted` below.
const (
	// ClaudeCrossSessionInboundAccept delivers a peer message as a turn.
	ClaudeCrossSessionInboundAccept = "accept"
	// ClaudeCrossSessionInboundRefuse drops it. The SENDER still gets
	// `success: true` — refusal is silent on both ends, which is why the
	// UI copy says "ignore", not "reject".
	ClaudeCrossSessionInboundRefuse = "refuse"
)

// claudeCrossSessionInboundHoldIsNeverEmitted records why the CLI's third
// value is missing from the allow-list above, because "we only support
// two of three" reads like an oversight otherwise.
//
// A held message parks awaiting APPROVAL, and a headless stream-json
// session has no approval surface. Spike-verified against 2.1.237
// (/tmp/spike-xsession/logs/q4-expiry-hold, q4-expiry-nomode):
//
//   - `"hold"` (explicit) arms NO expiry timer. The message parks
//     forever and is settled "expired" only when the process shuts down.
//     Nothing whatsoever reaches stdout — not a notice, not a
//     command_lifecycle frame. The message is simply gone.
//   - Leaving the key UNSET is not "off" either: the CLI applies MODE
//     PARITY, and a sender that asserts no permission-mode class holds
//     with cause `no-mode-asserted`. That one DOES arm a timer
//     (CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS, default 5m) and then drops
//     the message with an expired receipt — again with nothing on
//     stdout. Mode attestation sits behind a SECOND GrowthBook flag
//     (`tengu_harbor_kite_mode_emit`) that has no environment override,
//     so Agent Overflow cannot make parity resolve predictably.
//
// Both of those are "the user was told a peer could reach this thread,
// and it silently could not". So Agent Overflow always sends an EXPLICIT
// value: accept or refuse, never hold, never unset-while-enabled.
const claudeCrossSessionInboundHoldIsNeverEmitted = "hold"

var allowedClaudeCrossSessionInbound = map[string]bool{
	"":                              true,
	ClaudeCrossSessionInboundAccept: true,
	ClaudeCrossSessionInboundRefuse: true,
}

// EffectiveInbound is the user's resolved policy, and the value that
// reaches the `--settings` block whenever the feature is ON.
//
// Empty when the feature is off, because a policy for an inbox the user
// did not open is not the user's choice to express. What the spawn sends
// in that case is a flat "refuse" stamped one layer down, in
// claudeCrossSessionInbound — see the type doc. When ON, an empty stored
// Inbound resolves to Accept rather than staying empty, and that
// substitution is the point: an enabled-but-unset session would fall
// into the CLI's mode-parity path, whose hold outcome is the silent drop
// documented above. "Enabled" has to mean something on the wire.
func (c ClaudeCrossSession) EffectiveInbound() string {
	if !c.Enabled {
		return ""
	}
	inbound := strings.TrimSpace(c.Inbound)
	if inbound == "" {
		return ClaudeCrossSessionInboundAccept
	}
	return inbound
}

// validateClaudeCrossSession is the strict path. An unrecognised policy
// is a refused save: the CLI's `.catch(void 0)` turns a value it does not
// know into UNSET, which is mode parity — a different behavior from
// every value the user could have meant.
func validateClaudeCrossSession(field string, value ClaudeCrossSession) (ClaudeCrossSession, error) {
	value.Inbound = strings.TrimSpace(value.Inbound)
	if !allowedClaudeCrossSessionInbound[value.Inbound] {
		if value.Inbound == claudeCrossSessionInboundHoldIsNeverEmitted {
			return ClaudeCrossSession{}, fmt.Errorf(
				"%s.inbound: %q parks a peer message forever with no approval surface in a headless session; use %q or %q",
				field, claudeCrossSessionInboundHoldIsNeverEmitted,
				ClaudeCrossSessionInboundAccept, ClaudeCrossSessionInboundRefuse)
		}
		return ClaudeCrossSession{}, fmt.Errorf("%s.inbound must be %q or %q",
			field, ClaudeCrossSessionInboundAccept, ClaudeCrossSessionInboundRefuse)
	}
	return value, nil
}

// sanitizeClaudeCrossSession is the lenient load-time half. A settings
// file written by a build that still offered "hold" degrades to the
// enabled default rather than making the whole file unloadable — audibly,
// because a value that vanishes on load is otherwise indistinguishable
// from a save that never happened.
func sanitizeClaudeCrossSession(field string, value ClaudeCrossSession) ClaudeCrossSession {
	sanitized, err := validateClaudeCrossSession(field, value)
	if err != nil {
		log.Printf("settings: %s: dropping unusable cross-session config %+v: %v", field, value, err)
		return ClaudeCrossSession{Enabled: value.Enabled}
	}
	return sanitized
}

// ClaudeCrossSessionForProvider returns the cross-session preference for a
// provider name.
//
// Headless `claude` only, and claude-tui is excluded for the same reason
// `ClaudeSessionAxesForProvider` excludes it: half of this axis rides the
// `--settings` block, which the PTY launch does not send. The other half
// (the gate variable and `--name`) would reach it, but an interactive
// session that is discoverable and then silently refuses every delivery
// is worse than one that never advertised itself.
func (s Settings) ClaudeCrossSessionForProvider(providerName string) ClaudeCrossSession {
	if strings.TrimSpace(providerName) != "claude" {
		return ClaudeCrossSession{}
	}
	return s.ClaudeCrossSession
}
