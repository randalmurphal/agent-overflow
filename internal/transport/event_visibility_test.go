package transport

import "testing"

func TestEventVisibleToOrigin(t *testing.T) {
	// Loopback-only is the host-directive residue: what a launcher, the
	// harness, the desktop self-updater or the native browser pane acts
	// on, and nothing a client across a network could consume. The full
	// list, in both directions, is
	// TestLoopbackOnlyIsForHostDirectivesOnly.
	for _, channel := range []string{
		"power:keepawake",
		"webview:trim",
		"browser:host",
		"updater:install",
		"backend:attach",
		"harness:ui-query",
	} {
		if eventVisibleToOrigin(channel, false) {
			t.Fatalf("%s visible to non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("%s hidden from loopback peer", channel)
		}
	}
	for _, channel := range []string{
		"provider:item_event",
		"provider:usage",
		"provider:session_died",
		// Re-adjudicated 2026-09-03: every event about a thread or a
		// workspace reaches any connected client that has visibility of
		// it, phone included. These eleven were withheld because their
		// rows still cited a per-method local-only table wave 6d2 had
		// deleted, so a phone could call the RPC and never see the push —
		// stale queue rows, no live approval prompt, a terminal with no
		// output. Their Scope column is the gate now, and it equals the
		// scope of the RPC that returns the same data.
		"git:status",
		"provider:approval",
		"provider:queue_flushed",
		"provider:queue_restored",
		"provider:queue_state_changed",
		"provider:background_task_state",
		"provider:user_input",
		"provider:session_account",
		"terminal:exit",
		"terminal:output",
		"provider:terminal_output",
		// Wave 8i: a provider sign-in is driven from wherever the owner
		// is, so the three channels that report one reach a remote admin
		// device. All three are scoped access:admin, the same grant that
		// starts and answers the flow over RPC.
		"provider:account",
		"provider:account_usage_error",
		"provider:status",
		"provider:login",
		"provider:account_signin",
		"provider:account_auto_switch",
		// highlight:diff_seed goes everywhere: its persist-time seeds can
		// be parse-primed — better than the loopback RPC recompute — so
		// local clients consume them as in-place cache upgrades.
		"highlight:diff_seed",
		"highlight:live",
	} {
		if !eventVisibleToOrigin(channel, false) {
			t.Fatalf("remote-safe event %s hidden from non-loopback peer", channel)
		}
		if !eventVisibleToOrigin(channel, true) {
			t.Fatalf("remote-safe event %s hidden from loopback peer", channel)
		}
	}
}
