package transport

var loopbackOnlyEventChannels = map[string]bool{
	"git:status":                     true,
	"notification:activated":         true,
	"notification:send":              true,
	"provider:approval":              true,
	"provider:status":                true,
	"provider:queue_flushed":         true,
	"provider:queue_restored":        true,
	"provider:queue_state_changed":   true,
	"provider:background_task_state": true,
	"provider:user_input":            true,
	"terminal:exit":                  true,
	"terminal:output":                true,
	// provider:terminal_output carries the raw PTY bytes of a claude-tui
	// take-control session — command output, file contents, anything on the
	// TUI's screen — the same data class as terminal:output. The
	// ProviderTerminal* RPCs are LocalOnly, so a LAN peer cannot arm the
	// fan-out itself, but once a local pane attaches the sink emits to every
	// subscriber; keep these frames loopback-only like their app-terminal twin.
	"provider:terminal_output": true,
	// provider:account carries the user's authenticated subscriptionType
	// + tokenSource (oauth | apikey | console) + apiProvider. tokenSource
	// is upstream-typed and discloses the auth model of the local user;
	// subscriptionType + apiProvider are plan/billing identity. Combined,
	// this is single-frame profiling disclosure to a LAN-attached
	// subscriber. Loopback-only.
	"provider:account": true,

	// provider:usage (token counts, context %, rate limits) and
	// provider:session_died are open to remote clients — essential
	// feedback for understanding resource consumption and provider
	// crashes. Without session_died, a remote viewer sees the turn
	// silently stop with no explanation.

	// discussion:message and discussion:state are intentionally absent
	// from this map — both stay remote-visible, not loopback-only.
	// Remote clients can already call GetChannelMessages and
	// GetChannelState (neither is in transport.LocalOnlyMethods), so
	// pushing the same data over the event channel discloses nothing a
	// poll couldn't already read; it just saves the round-trip. This
	// mirrors the provider:usage reasoning above, not the
	// PostChannelMessage RPC, which moved to LocalOnly separately
	// because dispatching a turn prompt is session control, not a data
	// read.
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return true
	}
	return !loopbackOnlyEventChannels[channel]
}
