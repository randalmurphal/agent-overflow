package transport

var loopbackOnlyEventChannels = map[string]bool{
	// git:status is addressed by the CANONICAL ABSOLUTE workspace path (it
	// has to be — one frame serves every pane on that worktree), so every
	// frame discloses where the user's repositories live on disk. That makes
	// this entry load-bearing for path disclosure, not merely a way to spare
	// a LAN peer the watcher cost: GitStatusSubscribe is LocalOnly, so a
	// remote peer cannot arm the stream itself, but once a local pane does
	// the push side reaches every subscriber. Same third-door reasoning as
	// worktree:setup below.
	"git:status": true,
	// pr:updated carries a pull request's full detail and every review
	// thread on it — private-repo titles, branch names, reviewer logins and
	// comment bodies — plus a poll-failure summary. Every one of its RPCs
	// (SubscribePRUpdates / UnsubscribePRUpdates / SetPRUpdatesActive) is
	// LocalOnly, so a LAN peer can neither arm nor pause the stream, but
	// once a local pane subscribes the pump emits to every subscriber:
	// the push side is the third door, same reasoning as worktree:setup
	// and git:status.
	"pr:updated":                     true,
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
	// worktree:setup streams the stdout/stderr of the project's own setup
	// commands running against the user's checkout — the same data class as
	// terminal:output, and it can carry anything a build or install script
	// prints, tokens in an env dump included. Its RPCs
	// (GetThreadWorktreeSetup / RetryThreadWorktreeSetup) are LocalOnly, so a
	// LAN peer can neither read the snapshot nor start a run; keeping the push
	// side loopback-only closes the third door.
	"worktree:setup": true,
	// provider:terminal_output carries the raw PTY bytes of a claude-tui
	// take-control session — command output, file contents, anything on the
	// TUI's screen — the same data class as terminal:output. The
	// ProviderTerminal* RPCs are LocalOnly, so a LAN peer cannot arm the
	// fan-out itself, but once a local pane attaches the sink emits to every
	// subscriber; keep these frames loopback-only like their app-terminal twin.
	"provider:terminal_output": true,
	// provider:account carries the user's email/display name plus authenticated
	// subscriptionType, tokenSource (oauth | apikey | console), and
	// apiProvider. This is account, auth-model, and billing identity in one
	// frame, so it remains loopback-only.
	"provider:account":             true,
	"provider:session_account":     true,
	"provider:account_usage_error": true,

	// updater:install is an imperative directive, not a notification: the
	// Windows launcher acts on it by swapping the app binary and killing this
	// backend. Its only legitimate consumer is the launcher on this host, which
	// is loopback by construction, so a LAN peer has no reason to see it — and
	// a peer that did would learn exactly which staged file to tamper with.
	"updater:install": true,

	// session-import:progress reports on files in the user's provider homes:
	// each frame names the scan row it settled, and a failure carries the
	// reader's own message, which quotes the absolute transcript path. Its
	// RPCs are all LocalOnly, so a LAN peer can neither list nor start an
	// import; keeping the push side loopback-only closes the third door, the
	// same reasoning as worktree:setup above.
	"session-import:progress": true,

	// mcp:status and mcp:oauth-completed carry provider-reported MCP error
	// strings verbatim (sanitizeMCPError bounds length and collapses
	// newlines — it does not redact, and an `invalid_grant` body can quote
	// token material). Every MCP RPC (ListThreadMcpServers, TriggerMcpAuth,
	// GetMcpServerStatus, …) is LocalOnly, so a LAN peer can neither list
	// nor act on MCP servers and these frames buy it nothing; keeping the
	// push side loopback-only closes the third door, same reasoning as
	// git:status above.
	"mcp:status":          true,
	"mcp:oauth-completed": true,

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
	//
	// highlight:diff_seed is deliberately NOT here anymore: its seeds
	// can be parse-primed with the just-edited workspace file — better
	// spans than what the loopback RPC path recomputes for a persisted
	// diff — so local clients consume them as in-place upgrades rather
	// than redundant warmers. Its producer gate was dropped alongside.
	"highlight:seed": true,
}

// ephemeralEventChannels are never retained in the replay ring:
// point-in-time cache warmers whose replay after a reconnect would be
// useless (the client re-requests over RPC on demand) and whose frames
// are large (span arrays + hash chains per streaming flush window) —
// ring retention would hold up to DefaultRingCapacity superseded
// frames per channel. Emit still assigns sequence numbers; Replay
// finds an empty ring and returns nothing (no gap marker, so clients
// never try to "recover" frames that were never history). The one
// exception is a cursor ABOVE the channel's head, which is not a
// retention question at all — see ring.replayAfter.
//
// updater:install joins them for the opposite reason — not size, but
// imperativeness. It is a one-shot instruction to swap the app binary and
// restart, valid only for the install that was in flight when it was emitted.
// Retaining it would mean a launcher that reconnects (the Windows↔WSL relay
// tears connections down mid-session) replays a directive whose install has
// long since been abandoned or already applied, and spontaneously restarts the
// app. The backend re-emits on the next RestartToUpdate; there is nothing here
// worth recovering.
var ephemeralEventChannels = map[string]bool{
	"highlight:seed":      true,
	"highlight:diff_seed": true,
	"updater:install":     true,
}

// latestOnlyEventChannels get a capacity-1 replay ring: unkeyed
// channels where every frame carries the COMPLETE current state, so
// the newest frame fully supersedes all prior ones. A default-depth
// ring would retain up to DefaultRingCapacity superseded frames
// forever (system:stats emits every 2s, so its ring held ~33 minutes
// of stale CPU samples) and replay them all on reconnect just to be
// overwritten by the last one. Replay for these channels delivers the
// single newest frame and never a gap marker — "missed" frames are not
// lost history, they're superseded state, so there is no gap to
// recover from. A cursor ABOVE the head still gaps: that client is not
// behind, it is holding another server's sequence space, and the
// newest frame's lower seq would read as a duplicate to it.
//
// Membership rule: the channel must be UNKEYED — one global state, not
// per-thread / per-workspace / per-server payloads multiplexed on one
// channel. Keyed channels (git:status, provider:usage,
// discussion:state, mcp:status) must NOT go here: capacity 1 would
// evict other keys' latest frames and turn their reconnect replay into
// data loss.
var latestOnlyEventChannels = map[string]bool{
	// Host CPU + memory sample for the sidebar footer; a fresh sample
	// lands every 2s regardless (app_sysstat.go).
	"system:stats": true,

	// The two PAYLOAD-LESS refetch signals, and a matched pair: both are
	// `a.emit(name, nil)` from a debounced fsnotify watcher over one
	// directory, and both mean exactly "read that directory again".
	// Because the payload is nil, N retained frames are N IDENTICAL
	// frames, and a reconnect after an agent rewrote a file a dozen times
	// would replay a dozen refetches of the same directory — every one of
	// them an RPC that reads the whole listing.
	//
	// Latest-only rather than ephemeral: the signal is not point-in-time
	// (nothing is cached in it, so there is nothing to warm) and it is not
	// imperative (replaying it costs one read, not a restart). A client
	// that was disconnected while the directory changed DOES need to hear
	// about it once, which is precisely what a capacity-1 ring delivers.
	// Both are unkeyed whole-state channels — one directory, one global
	// answer — so they satisfy the membership rule above.
	"theme:changed":                true,
	"workflow:definitions-changed": true,
}

func eventVisibleToOrigin(channel string, isLoopback bool) bool {
	if isLoopback {
		return !remoteOnlyEventChannels[channel]
	}
	return !loopbackOnlyEventChannels[channel]
}
