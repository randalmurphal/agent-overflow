package transport

// Event-channel policy registry.
//
// Every Go → frontend event rides a NAMED CHANNEL through the event bus
// (`EventBus.Emit`). Two properties are decided per channel and nowhere
// else: WHO may receive its frames (Audience) and HOW DEEP its replay
// ring is (Retention). Before this table those two questions were
// answered by four hand-maintained sets in event_visibility.go, and a
// channel absent from all four silently inherited the fail-open default
// (broadcast to every client, full-depth ring). About two thirds of the
// channels the app emits were in that state.
//
// THIS TABLE IS THE DEFINITION. It cannot be generated: emit sites are
// spread across the root package, internal/triage, internal/workflow and
// several others, and some construct their channel name at runtime (see
// "Dynamic channel families" below), so an AST scan cannot enumerate
// them. Adding a channel means adding a row here.
//
// A `Why` containing the substring "unreviewed" marks a row that inherited
// the fail-open default rather than a decision anyone made. Those rows are
// the worklist for the follow-up review pass, and
// TestChannelPolicyUnreviewedWorklist prints them.
//
// # Dynamic channel families
//
// Four emit paths do not spell their channel as a literal at the call
// site. Three resolve to names already in the table; one is unbounded:
//
//   - `internal/triage/subagent_progress.go` emits
//     `"provider:" + subagentProgressEventName`. One member,
//     `provider:subagent_progress`, listed below.
//   - `app_updater.go`'s `updaterEventBridge` maps six Wails updater
//     event names onto six `updater:*` channels. All six are listed;
//     `mustBridgedChannel` panics at startup if a row is deleted from
//     that bridge, so the two spellings cannot drift apart silently.
//   - `internal/design/watcher.go` emits a `WatchSubject` that
//     `app_design.go` switches onto `design:reload-main` /
//     `design:options-update`. Both are listed.
//   - **Unbounded, harness-only:** `Harness.HarnessEmit(channel, payload)`
//     (app_harness.go) publishes onto an ARBITRARY caller-named channel,
//     and `harness.Replayer` (app_harness_replay.go) republishes whatever
//     channel names a recorded NDJSON event log contains. Both exist only
//     under the `--harness` boot path, on a receiver registered
//     `LocalOnly`, so they are reachable only from loopback in a test
//     build. Neither can be enumerated here; both land on the
//     unregistered-channel default below.
//
// The registry is deliberately NOT a prefix table: the bus keys rings and
// visibility by exact channel name, and a prefix rule would let a new
// `provider:*` channel inherit a classification nobody chose for it —
// which is the exact failure this table exists to end.

// Audience says which connections may receive a channel's frames.
// It is enforced in EventBus deliver (per-subscriber) and again in
// conn.go's event pump (per-connection), both via eventVisibleToOrigin.
type Audience uint8

const (
	// AudienceAny reaches every connected client, loopback or LAN.
	AudienceAny Audience = iota
	// AudienceLoopbackOnly reaches loopback connections only. For frames
	// carrying local filesystem paths, local-terminal bytes, provider
	// identity/billing data, or imperative host directives. Note this is a
	// THIRD DOOR concern independently of RPC classification: a channel's
	// RPCs being in LocalOnlyMethods stops a LAN peer arming the stream,
	// but once a local pane subscribes the push side fans out to every
	// subscriber regardless of who armed it.
	AudienceLoopbackOnly
	// AudienceRemoteOnly reaches non-loopback connections only. For frames
	// that exist purely to hide WAN round-trip latency and are pure waste
	// on a local pipe.
	AudienceRemoteOnly
)

// String makes the worklist and any failure message readable.
func (a Audience) String() string {
	switch a {
	case AudienceAny:
		return "any"
	case AudienceLoopbackOnly:
		return "loopback-only"
	case AudienceRemoteOnly:
		return "remote-only"
	default:
		return "unknown"
	}
}

// Retention says how deep a channel's replay ring is. Rings are a network
// jitter buffer, not a history store (root CLAUDE.md principle 3).
type Retention uint8

const (
	// RetentionDefault gives the channel a ring of EventBus.capacity
	// (DefaultRingCapacity, 1000) frames.
	RetentionDefault Retention = iota
	// RetentionEphemeral gives the channel a ZERO-capacity ring: Emit still
	// assigns a monotonic seq and live subscribers still get the frame, but
	// nothing is retained. Replay returns no events and no gap marker —
	// these frames were never history, so there is nothing to recover. (An
	// above-head cursor still gaps; that is a client-state fault, not a
	// retention question — see ring.replayAfter.)
	//
	// Two membership reasons, both represented below: point-in-time cache
	// warmers whose replay is useless and whose payloads are large, and
	// imperative one-shot directives that would be actively harmful to
	// replay against a stale instruction.
	RetentionEphemeral
	// RetentionLatestOnly gives the channel a capacity-1 ring: the newest
	// frame fully supersedes every prior one, so retain exactly it. Replay
	// delivers that single newest frame and never an eviction-side gap
	// marker — "missed" frames are superseded state, not lost history. (A
	// cursor ABOVE the head still gaps: that client holds another server's
	// sequence space, and the newest frame's lower seq would read to it as
	// a duplicate.)
	//
	// MEMBERSHIP RULE: the channel must be UNKEYED — one global state, not
	// per-thread / per-workspace / per-server payloads multiplexed onto one
	// channel. Keyed channels (git:status, provider:usage,
	// discussion:state, mcp:status) must NOT go here: capacity 1 would
	// evict other keys' latest frames and turn their reconnect replay into
	// silent data loss.
	RetentionLatestOnly
)

// String makes the worklist and any failure message readable.
func (r Retention) String() string {
	switch r {
	case RetentionDefault:
		return "default"
	case RetentionEphemeral:
		return "ephemeral"
	case RetentionLatestOnly:
		return "latest-only"
	default:
		return "unknown"
	}
}

// ChannelPolicy is one row of the registry.
type ChannelPolicy struct {
	// Channel is the exact channel name passed to EventBus.Emit.
	Channel string
	// Audience is who may receive frames on this channel.
	Audience Audience
	// Retention is how deep this channel's replay ring is.
	Retention Retention
	// Why records the decision. A Why containing "unreviewed" means the
	// row was captured from an emit site, not decided.
	Why string
}

// unreviewedWhy is the exact marker a row carries when its classification
// was never decided — it fell through to the fail-open default and this
// table merely wrote that down. The follow-up review pass greps for it.
const unreviewedWhy = "unreviewed — inherited the fail-open default"

// unreviewedMarker is the substring that identifies an unreviewed row.
// Rows whose retention WAS reviewed but whose audience was not embed it
// in a longer sentence, so membership is a substring test, not equality.
const unreviewedMarker = "unreviewed"

// channelPolicies is the authored table. Alphabetical by channel.
//
// Keep the fail-open default (AudienceAny / RetentionDefault) spelled out
// explicitly on every row rather than relying on the zero value — a row
// that omits its audience is indistinguishable from one that chose "any",
// and this table is read to answer exactly that question.
var channelPolicies = []ChannelPolicy{
	{
		Channel:   "design:options-update",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "design:reload-main",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "discussion:message",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible. Remote clients can already call " +
			"GetChannelMessages (not in LocalOnlyMethods), so pushing the same " +
			"data discloses nothing a poll could not already read — it just " +
			"saves the round-trip. PostChannelMessage is separately LocalOnly " +
			"because dispatching a turn prompt is session control, not a read.",
	},
	{
		Channel:   "discussion:state",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible, same reasoning as " +
			"discussion:message: GetChannelState is not LocalOnly. Keyed by " +
			"channel id, so it must never become latest-only.",
	},
	{
		Channel:   "git:status",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Addressed by the CANONICAL ABSOLUTE workspace path (it has to " +
			"be — one frame serves every pane on that worktree), so every " +
			"frame discloses where the user's repositories live on disk. " +
			"Load-bearing for path disclosure, not merely sparing a LAN peer " +
			"the watcher cost: GitStatusSubscribe is LocalOnly so a remote " +
			"peer cannot arm the stream, but once a local pane does, the push " +
			"side reaches every subscriber. Keyed by cwd — never latest-only.",
	},
	{
		Channel:   "harness:mock",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (emitted only under --harness)",
	},
	{
		Channel:   "harness:replay",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (emitted only under --harness)",
	},
	{
		Channel:   "highlight:diff_seed",
		Audience:  AudienceAny,
		Retention: RetentionEphemeral,
		Why: "Goes to EVERY client, deliberately: its persist-time seeds can " +
			"be parse-primed with the just-edited workspace file — better " +
			"spans than the loopback RPC path recomputes for a persisted diff " +
			"— so local clients consume them as in-place cache upgrades rather " +
			"than redundant warmers. (It used to be remote-only; the producer " +
			"gate was dropped alongside.) Ephemeral because a seed is a " +
			"point-in-time cache warmer: replaying a superseded one is useless " +
			"and each frame can carry large span/hash arrays.",
	},
	{
		Channel:   "highlight:seed",
		Audience:  AudienceRemoteOnly,
		Retention: RetentionEphemeral,
		Why: "Pushes syntax-span metadata alongside streaming text so a remote " +
			"client colors code without a highlight RPC per growth step. " +
			"Loopback clients get faster spans from the RPC path (sub-ms round " +
			"trip), so these frames carry nothing they would use. The producer " +
			"is also gated on Server.HasRemoteClient; this filter is what keeps " +
			"the frames off loopback pipes while a remote viewer keeps the " +
			"producer running. Caveat: SSH-tunneled remotes arrive as loopback " +
			"and are invisible to the probe — they keep the RPC path. Ephemeral " +
			"for the same reason as highlight:diff_seed.",
	},
	{
		Channel:   "mcp:oauth-completed",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Carries provider-reported MCP error strings verbatim " +
			"(sanitizeMCPError bounds length and collapses newlines — it does " +
			"not redact, and an `invalid_grant` body can quote token " +
			"material). Every MCP RPC is LocalOnly, so a LAN peer can neither " +
			"list nor act on MCP servers and these frames buy it nothing; " +
			"keeping the push side loopback-only closes the third door.",
	},
	{
		Channel:   "mcp:status",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Same disclosure class as mcp:oauth-completed — verbatim " +
			"provider MCP error strings, with every MCP RPC already LocalOnly. " +
			"Keyed by server, so it must never become latest-only: capacity 1 " +
			"would evict other servers' latest frames.",
	},
	{
		Channel:   "notification:activated",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "OS-notification click routing for the local desktop window; the " +
			"target names the local thread/workspace to reveal. A LAN peer has " +
			"no OS notification to have clicked.",
	},
	{
		Channel:   "notification:send",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Instructs a host-side presenter (the Windows launcher's " +
			"notification client) to raise a real OS notification carrying " +
			"thread titles and message text. Its only legitimate consumer is " +
			"on this host, which is loopback by construction. Retained: the " +
			"launcher replays this channel by cursor after a reconnect " +
			"(wsllauncher/notification_client.go), so it must NOT become " +
			"ephemeral or latest-only.",
	},
	{
		Channel:   "pr:updated",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Carries a pull request's full detail and every review thread on " +
			"it — private-repo titles, branch names, reviewer logins and " +
			"comment bodies — plus a poll-failure summary. Every one of its " +
			"RPCs (SubscribePRUpdates / UnsubscribePRUpdates / " +
			"SetPRUpdatesActive) is LocalOnly, so a LAN peer can neither arm " +
			"nor pause the stream, but once a local pane subscribes the pump " +
			"emits to every subscriber: the push side is the third door.",
	},
	{
		Channel:   "provider:account",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Carries the user's email/display name plus authenticated " +
			"subscriptionType, tokenSource (oauth | apikey | console), and " +
			"apiProvider — account, auth-model, and billing identity in one " +
			"frame.",
	},
	{
		Channel:   "provider:account_usage_error",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Account-scoped billing/quota failure detail; same identity class as provider:account.",
	},
	{
		Channel:   "provider:approval",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Tool-use approval requests quote the exact command line, file " +
			"path, or patch the provider wants to run against the user's " +
			"machine. Approving is RCE-equivalent and the resolve RPCs are " +
			"LocalOnly; the request side stays loopback-only to match.",
	},
	{
		Channel:   "provider:background_task_state",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Background terminal/task state carries the local command and " +
			"its output-derived state — the same local-execution data class as " +
			"terminal:output.",
	},
	{
		Channel:   "provider:background_tasks_changed",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:command_lifecycle",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:commands",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:compacting",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:fast_mode",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:item_event",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "The main transcript stream; a remote viewer that cannot see it " +
			"has no product. Pinned remote-visible by " +
			"TestEventVisibleToOrigin. Keyed by thread/item — never " +
			"latest-only.",
	},
	{
		Channel:   "provider:model_fallback",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:queue_flushed",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Per-thread flush-queue frames carry the queued user message " +
			"bodies and their attachment metadata (local file names), and the " +
			"queue-mutating RPCs are LocalOnly.",
	},
	{
		Channel:   "provider:queue_restored",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Same payload class as provider:queue_flushed — queued user message bodies restored into the composer.",
	},
	{
		Channel:   "provider:queue_state_changed",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Same payload class as provider:queue_flushed — the queue snapshot it announces carries the queued message bodies.",
	},
	{
		Channel:   "provider:session_account",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Per-session binding of a thread to a provider account identity; same identity class as provider:account.",
	},
	{
		Channel:   "provider:session_died",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible: without it a remote viewer sees the " +
			"turn silently stop with no explanation. Pinned by " +
			"TestEventVisibleToOrigin.",
	},
	{
		Channel:   "provider:status",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Reports provider CLI installation/auth state and carries an " +
			"actionURL plus provider-side error prose — install paths and " +
			"authentication failures for the local machine's toolchain.",
	},
	{
		Channel:   "provider:subagent_progress",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (dynamic name: \"provider:\" + subagentProgressEventName)",
	},
	{
		Channel:   "provider:terminal_output",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Raw PTY bytes of a claude-tui take-control session — command " +
			"output, file contents, anything on the TUI's screen — the same " +
			"data class as terminal:output. The ProviderTerminal* RPCs are " +
			"LocalOnly, so a LAN peer cannot arm the fan-out, but once a local " +
			"pane attaches the sink emits to every subscriber.",
	},
	{
		Channel:   "provider:todo_update",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:turn_completed",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:turn_started",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "provider:usage",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible: token counts, context %, and rate " +
			"limits are essential feedback for understanding resource " +
			"consumption. Pinned by TestEventVisibleToOrigin. Keyed by " +
			"provider/account/limit — never latest-only.",
	},
	{
		Channel:   "provider:user_input",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Interactive provider questions quote whatever the provider is " +
			"asking about — local paths, command lines, file content — and the " +
			"answer RPCs are LocalOnly. Same class as provider:approval.",
	},
	{
		Channel:   "screenshot:install-progress",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "session-import:progress",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Reports on files in the user's provider homes: each frame names " +
			"the scan row it settled, and a failure carries the reader's own " +
			"message, which quotes the absolute transcript path. Its RPCs are " +
			"all LocalOnly, so keeping the push side loopback-only closes the " +
			"third door.",
	},
	{
		Channel:   "spinner:changed",
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention reviewed: a payload-less refetch signal — `emit(name, " +
			"nil)` from a debounced fsnotify watcher over one directory, " +
			"meaning exactly \"read that directory again\". N retained frames " +
			"are N IDENTICAL frames, so a reconnect after an agent rewrote a " +
			"file a dozen times would replay a dozen full-listing refetches. " +
			"Latest-only rather than ephemeral: a client that was disconnected " +
			"while the directory changed DOES need to hear about it once. " +
			"Unkeyed (one directory, one global answer), so it satisfies the " +
			"membership rule. Audience unreviewed — inherited the fail-open " +
			"default.",
	},
	{
		Channel:   "system:stats",
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention reviewed: host CPU + memory sample for the sidebar " +
			"footer, a fresh whole-state sample every 2s (app_sysstat.go), so " +
			"a default-depth ring held ~33 minutes of stale samples and " +
			"replayed all of them on reconnect just to be overwritten by the " +
			"last one. Unkeyed. Audience unreviewed — inherited the fail-open " +
			"default.",
	},
	{
		Channel:   "terminal:exit",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Local PTY session lifecycle; paired with terminal:output and the same local-execution class.",
	},
	{
		Channel:   "terminal:output",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Raw local PTY bytes — command output, file contents, anything on the terminal's screen.",
	},
	{
		Channel:   "theme:changed",
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention reviewed: payload-less refetch signal, same matched " +
			"set as spinner:changed and workflow:definitions-changed — see " +
			"spinner:changed for the full reasoning. Audience unreviewed — " +
			"inherited the fail-open default.",
	},
	{
		Channel:   "thread:mode_changed",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "thread:runtime_mode_changed",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "thread:title_generation",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "thread:updated",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "updater:download-started",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventDownloadStarted)",
	},
	{
		Channel:   "updater:error",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventError)",
	},
	{
		Channel:   "updater:install",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Why: "An imperative directive, not a notification: the Windows " +
			"launcher acts on it by swapping the app binary and killing this " +
			"backend. Its only legitimate consumer is the launcher on this " +
			"host, which is loopback by construction — and a peer that saw it " +
			"would learn exactly which staged file to tamper with. Ephemeral " +
			"for the opposite reason to the seed channels: not size, but " +
			"imperativeness. It is valid only for the install in flight when " +
			"it was emitted, so replaying it to a launcher that reconnects " +
			"(the Windows↔WSL relay tears connections down mid-session) would " +
			"spontaneously restart the app on a stale instruction. The backend " +
			"re-emits on the next RestartToUpdate.",
	},
	{
		Channel:   "updater:installing",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventInstalling)",
	},
	{
		Channel:   "updater:progress",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventDownloadProgress)",
	},
	{
		Channel:   "updater:ready",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventUpdateReady)",
	},
	{
		Channel:   "updater:verifying",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (bridged from updater.EventVerifying)",
	},
	{
		Channel:   "usage:thread_cost",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "user_message:reverted",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "workflow:definitions-changed",
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention reviewed: payload-less refetch signal, same matched " +
			"set as theme:changed and spinner:changed — see spinner:changed " +
			"for the full reasoning. Audience unreviewed — inherited the " +
			"fail-open default.",
	},
	{
		Channel:   "workflow:engine-state",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "workflow:error",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "workflow:gate-notify",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy + " (no frontend subscriber today; App consumes it in afterWorkflowEngineEvent)",
	},
	{
		Channel:   "workflow:item-state",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "workflow:phase-state",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "workflow:soft-stop",
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why:       unreviewedWhy,
	},
	{
		Channel:   "worktree:setup",
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Streams the stdout/stderr of the project's own setup commands " +
			"running against the user's checkout — the same data class as " +
			"terminal:output, and it can carry anything a build or install " +
			"script prints, tokens in an env dump included. Its RPCs are " +
			"LocalOnly in both key spaces (the thread pair and the workspace " +
			"pair for pre-thread runs), so keeping the push side loopback-only " +
			"closes the third door.",
	},
}

// channelPolicyIndex is the by-name lookup, built from channelPolicies at
// init. A duplicate channel would silently keep the LAST row;
// TestChannelPolicyHasNoDuplicates is the gate that stops one landing.
var channelPolicyIndex = func() map[string]ChannelPolicy {
	index := make(map[string]ChannelPolicy, len(channelPolicies))
	for _, policy := range channelPolicies {
		index[policy.Channel] = policy
	}
	return index
}()

// unregisteredChannelPolicy is what a channel with no registry row gets.
//
// TODO(transport): flip this to fail-CLOSED — an unregistered channel
// should be treated as AudienceLoopbackOnly, so a channel nobody
// classified can never reach a LAN peer by omission. That flip is blocked
// on reviewing every row whose Why contains unreviewedMarker (run
// TestChannelPolicyUnreviewedWorklist for the list): today ~37 of the 66
// registered channels carry that marker, and the two harness-only dynamic
// emit paths (HarnessEmit, harness.Replayer) are unregistrable by
// construction and would need an explicit carve-out. Until then this stays
// fail-open so the flip is a reviewed behavior change of its own rather
// than a side effect of building the table.
var unregisteredChannelPolicy = ChannelPolicy{
	Audience:  AudienceAny,
	Retention: RetentionDefault,
	Why:       "unregistered channel — fail-open default (see unregisteredChannelPolicy)",
}

// policyForChannel returns the registered policy for a channel, or the
// fail-open default for a channel with no row. The bool reports whether
// the channel was registered.
func policyForChannel(channel string) (ChannelPolicy, bool) {
	if policy, ok := channelPolicyIndex[channel]; ok {
		return policy, true
	}
	fallback := unregisteredChannelPolicy
	fallback.Channel = channel
	return fallback, false
}

// channelAudience and channelRetention are the two questions the registry
// answers.
//
// channelRetention is cold — EventBus.Emit consults it once per channel
// (on ring creation) and Replay once per channel per replay request — so
// it reads the index directly. channelAudience is likewise cold; the HOT
// visibility check (per event, per subscriber, and again per event per
// connection) goes through eventVisibleToOrigin's derived bool sets
// instead, which is one hash probe rather than a struct copy.
func channelAudience(channel string) Audience {
	policy, _ := policyForChannel(channel)
	return policy.Audience
}

func channelRetention(channel string) Retention {
	policy, _ := policyForChannel(channel)
	return policy.Retention
}
