package transport

var loopbackOnlyEventChannels = map[string]bool{
	"git:status":                     true,
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

// remoteOnlyEventChannels are the inverse cut: frames that exist to
// hide WAN round-trip latency and are pure waste on a local pipe.
var remoteOnlyEventChannels = map[string]bool{
	// highlight:seed pushes syntax-span metadata alongside streaming
	// text so a remote client colors code without a highlight RPC per
	// growth step. Loopback clients get faster spans from the RPC path
	// (sub-ms round trip), so these frames carry nothing they'd use.
	// Producer side is gated too (Server.HasRemoteClient) — this
	// filter is what keeps the frames off loopback pipes while a
	// remote viewer has the producer running.
	"highlight:seed": true,
	// highlight:diff_seed pushes patch-aligned spans for just-persisted
	// inline-diff previews — same rationale and same producer gate as
	// highlight:seed, keyed for the diff span cache instead of the
	// markdown code-block cache.
	"highlight:diff_seed": true,
}

// ephemeralEventChannels are never retained in the replay ring:
// point-in-time cache warmers whose replay after a reconnect would be
// useless (the client re-requests over RPC on demand) and whose frames
// are large (span arrays + hash chains per streaming flush window) —
// ring retention would hold up to DefaultRingCapacity superseded
// frames per channel. Emit still assigns sequence numbers; Replay
// finds an empty ring and returns nothing (no gap marker, so clients
// never try to "recover" frames that were never history).
var ephemeralEventChannels = map[string]bool{
	"highlight:seed":      true,
	"highlight:diff_seed": true,
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return !remoteOnlyEventChannels[channel]
	}
	return !loopbackOnlyEventChannels[channel]
}
