package transport

import "agent-overflow/internal/eventchan"

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
// THIS TABLE IS THE DEFINITION of the two POLICY questions. It cannot be
// generated: emit sites are spread across the root package,
// internal/triage, internal/workflow and several others, and some
// construct their channel name at runtime (see "Dynamic channel
// families" below), so an AST scan cannot enumerate them. Adding a
// channel means adding a row here.
//
// The SPELLING lives one layer down, in internal/eventchan: every row's
// Channel is an `eventchan.Channel` constant, and every emit site takes
// that same type rather than a string. The two halves are one table, not
// two parallel ones — TestEveryEventChannelConstantHasAPolicyRow and
// TestEveryChannelPolicyRowHasAConstant (event_channels_eventchan_test.go)
// fail on either half missing. Adding a channel is therefore two edits:
// a constant in internal/eventchan and a row here.
//
// A `Why` containing the substring "unreviewed" marks a row that inherited
// a default rather than a decision anyone made. Every row was reviewed in
// the 2026-08-25 classification pass, so the set should stay empty;
// TestChannelPolicyUnreviewedWorklist prints any that reappear.
//
// # Dynamic channel families
//
// Three emit paths do not spell their channel as a constant at the call
// site. Two resolve to names already in the table; one is unbounded:
//
//   - `internal/appupdate`'s `updaterEventBridge` maps six Wails updater
//     event names onto six `updater:*` channels. All six are listed;
//     `mustBridgedChannel` panics at startup if a row is deleted from
//     that bridge, so the two spellings cannot drift apart silently.
//   - **Unbounded, harness-only:** `Harness.HarnessEmit(channel, payload)`
//     (app_harness.go) publishes onto an ARBITRARY caller-named channel,
//     and `harness.Replayer` (app_harness_replay.go) republishes whatever
//     channel names a recorded NDJSON event log contains. Both exist only
//     under the `--harness` boot path, on a receiver registered
//     `LocalOnly`, so they are reachable only from loopback in a test
//     build. Neither can be enumerated here; both land on the
//     unregistered-channel default below — fail-closed, loopback-only —
//     which still reaches their loopback-by-construction consumers.
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
	// Channel is the exact channel name passed to EventBus.Emit, spelled
	// as its internal/eventchan constant — the newtype is what stops this
	// table and the emit sites becoming two string tables that drift.
	Channel eventchan.Channel
	// Audience is who may receive frames on this channel.
	Audience Audience
	// Retention is how deep this channel's replay ring is.
	Retention Retention
	// Why records the decision. A Why containing "unreviewed" means the
	// row was captured from an emit site, not decided.
	Why string
}

// unreviewedMarker is the substring that identifies an unreviewed row —
// one whose Why records that a default was inherited rather than a
// decision made. The 2026-08-25 classification pass emptied the set; the
// marker (and its test) stay so a future captured-not-decided row is
// still visible as such. Membership is a substring test, not equality,
// so the marker can sit inside a longer sentence.
const unreviewedMarker = "unreviewed"

// channelPolicies is the authored table. Alphabetical by channel.
//
// Keep AudienceAny / RetentionDefault spelled out explicitly on every row
// rather than relying on the zero value — a row that omits its audience
// is indistinguishable from one that chose "any", and this table is read
// to answer exactly that question.
var channelPolicies = []ChannelPolicy{
	{
		Channel:   eventchan.DiscussionMessage,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible. Remote clients can already call " +
			"GetChannelMessages (not in LocalOnlyMethods), so pushing the same " +
			"data discloses nothing a poll could not already read — it just " +
			"saves the round-trip. PostChannelMessage is separately LocalOnly " +
			"because dispatching a turn prompt is session control, not a read.",
	},
	{
		Channel:   eventchan.DiscussionState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible, same reasoning as " +
			"discussion:message: GetChannelState is not LocalOnly. Keyed by " +
			"channel id, so it must never become latest-only.",
	},
	{
		Channel:   eventchan.GitStatus,
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
		Channel:   eventchan.HarnessMock,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Frames carry the mock's cwd (a local path) and the exact wire " +
			"text the app sent the provider. Harness-only (--harness boot, " +
			"LocalOnly receiver), but the push side is the third door; its " +
			"consumers are loopback test tooling by construction.",
	},
	{
		Channel:   eventchan.HarnessPerf,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "One fps/heap/RSS sample per tick of an armed perf run " +
			"(app_harness_perf.go). Frames carry backend process names and " +
			"per-process RSS read from /proc — host detail no LAN peer may " +
			"see — and the run is harness-only either way. Full ring on " +
			"purpose: a sample is a POINT in a series, and a watcher that " +
			"reconnects mid-run wants the samples it missed rather than only " +
			"the newest one.",
	},
	{
		Channel:   eventchan.HarnessReplay,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Progress frames name the local NDJSON replay-log path and pass " +
			"IO/parse errors verbatim. Harness-only; loopback consumers by " +
			"construction (same story as harness:mock).",
	},
	{
		Channel:   eventchan.HarnessUIQuery,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Why: "A DIRECTIVE, not a state frame: each event asks the attached " +
			"frontend bridge to answer query <id>, and the backend waiter for " +
			"that id is gone 10s later. Ephemeral for the same reason " +
			"updater:install is — replaying it to a reconnecting client would " +
			"re-run every query of the last ring's worth against a backend " +
			"with nothing left to receive the answers, so the replies land as " +
			"unknown ids and the work is pure waste. Loopback-only because " +
			"the answers (DOM text, local paths in diagnostic globals) and " +
			"the harness itself are.",
	},
	{
		Channel:   eventchan.HighlightDiffSeed,
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
		Channel:   eventchan.HighlightSeed,
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
		Channel:   eventchan.MCPOAuthCompleted,
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
		Channel:   eventchan.MCPStatus,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Same disclosure class as mcp:oauth-completed — verbatim " +
			"provider MCP error strings, with every MCP RPC already LocalOnly. " +
			"Keyed by server, so it must never become latest-only: capacity 1 " +
			"would evict other servers' latest frames.",
	},
	{
		Channel:   eventchan.NotificationActivated,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "OS-notification click routing for the local desktop window; the " +
			"target names the local thread/workspace to reveal. A LAN peer has " +
			"no OS notification to have clicked.",
	},
	{
		Channel:   eventchan.NotificationSend,
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
		Channel:   eventchan.PowerKeepAwake,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionLatestOnly,
		Why: "Imperative host directive: it commands the process that owns " +
			"this machine's power state (the Windows launcher) to hold or " +
			"release an OS sleep inhibitor. A LAN peer has no business " +
			"pinning the desktop awake, and UpdateSettings — the only thing " +
			"that produces it — is already LocalOnly. Latest-only, NOT " +
			"ephemeral: the frame is a LEVEL, not an edge. A launcher that " +
			"reconnects (or connects after the backend's boot emit) must end " +
			"up holding what the current setting says, and a capacity-1 ring " +
			"is what makes its replay deliver exactly that. Legal here " +
			"because the channel is UNKEYED — one global power state, no " +
			"per-thread or per-workspace multiplexing.",
	},
	{
		Channel:   eventchan.PRUpdated,
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
		Channel:   eventchan.ProjectUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Carries a store.Project — the project's absolute path among " +
			"them — but ListProjects is wire-reachable and returns exactly " +
			"these rows, so the push discloses nothing a poll could not " +
			"(same reasoning as thread:updated). The mutations behind it " +
			"split: CreateProject and the worktree-setup writes are " +
			"LocalOnly, rename / archive / reorder / delete are not, and " +
			"the frames look identical either way, so no receiver can infer " +
			"who issued one. Never latest-only: each frame names a " +
			"different row and a different action, and a dropped one is a " +
			"project the client keeps or loses wrongly.",
	},
	{
		Channel:   eventchan.ProviderAccount,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Carries the user's email/display name plus authenticated " +
			"subscriptionType, tokenSource (oauth | apikey | console), and " +
			"apiProvider — account, auth-model, and billing identity in one " +
			"frame.",
	},
	{
		Channel:   eventchan.ProviderAccountUsageError,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Account-scoped billing/quota failure detail; same identity class as provider:account.",
	},
	{
		Channel:   eventchan.ProviderApproval,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Tool-use approval requests quote the exact command line, file " +
			"path, or patch the provider wants to run against the user's " +
			"machine. Approving is RCE-equivalent and the resolve RPCs are " +
			"LocalOnly; the request side stays loopback-only to match.",
	},
	{
		Channel:   eventchan.ProviderBackgroundTaskState,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Background terminal/task state carries the local command and " +
			"its output-derived state — the same local-execution data class as " +
			"terminal:output.",
	},
	{
		Channel:   eventchan.ProviderBackgroundTasksChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "threadId plus a full replacement set of task refs (ids and " +
			"model-authored descriptions) — no command lines or paths; that " +
			"loopback-only data rides provider:background_task_state instead. " +
			"Consumers treat it as a refetch nudge. Keyed per thread.",
	},
	{
		Channel:   eventchan.ProviderCommandLifecycle,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Queue UX is loopback: GetQueueState and every queue RPC are " +
			"LocalOnly, and the provider:queue_* siblings are already " +
			"loopback-only — a remote peer cannot render the rows these " +
			"frames label. States are a progression (queued→started→" +
			"terminal) correlated by userItemId, so every frame matters: " +
			"never latest-only or ephemeral.",
	},
	{
		Channel:   eventchan.ProviderCommands,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Slash-command names and hints for composer autocomplete on any " +
			"client — declared names, never command lines or output. Keyed " +
			"per thread; each frame replaces wholesale, but per-key, so " +
			"never latest-only.",
	},
	{
		Channel:   eventchan.ProviderCompacting,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "{threadId, active} render state any viewer needs; the " +
			"provider's failure prose is deliberately logged, not emitted " +
			"(compaction_status.go). Keyed per thread: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderFastMode,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Per-thread mode-chip state, restated on every session init and " +
			"turn completion; disabledReason is provider prose but names no " +
			"paths or identity. Keyed per thread.",
	},
	{
		Channel:   eventchan.ProviderItemEvent,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "The main transcript stream; a remote viewer that cannot see it " +
			"has no product. Pinned remote-visible by " +
			"TestEventVisibleToOrigin. Keyed by thread/item — never " +
			"latest-only.",
	},
	{
		Channel:   eventchan.ProviderModelFallback,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Effective-model render state any viewer needs; the provider's " +
			"refusal prose is rune-bounded at the emit site. Keyed per " +
			"thread with a monotonic revision — ordered, never latest-only.",
	},
	{
		Channel:   eventchan.ProviderQueueFlushed,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Per-thread flush-queue frames carry the queued user message " +
			"bodies and their attachment metadata (local file names), and the " +
			"queue-mutating RPCs are LocalOnly.",
	},
	{
		Channel:   eventchan.ProviderQueueRestored,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Same payload class as provider:queue_flushed — queued user message bodies restored into the composer.",
	},
	{
		Channel:   eventchan.ProviderQueueStateChanged,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Same payload class as provider:queue_flushed — the queue snapshot it announces carries the queued message bodies.",
	},
	{
		Channel:   eventchan.ProviderSessionAccount,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Per-session binding of a thread to a provider account identity; same identity class as provider:account.",
	},
	{
		Channel:   eventchan.ProviderSessionDied,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible: without it a remote viewer sees the " +
			"turn silently stop with no explanation. The frame carries " +
			"StderrTail — the dead process's last stderr line, pre-sanitized " +
			"by provider.MarshalProcessExitMeta (single line, hard length " +
			"cap) — and that is a decided disclosure, not an oversight: the " +
			"same string persists to items.meta, which the wire-safe " +
			"ListRecentThreadItems already serves to remote peers " +
			"(2026-08-25 security review, finding 1). Pinned by " +
			"TestEventVisibleToOrigin.",
	},
	{
		Channel:   eventchan.ProviderStatus,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Reports provider CLI installation/auth state and carries an " +
			"actionURL plus provider-side error prose — install paths and " +
			"authentication failures for the local machine's toolchain.",
	},
	{
		Channel:   eventchan.ProviderSubagentProgress,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Subagent tray/progress state; activity and summary are " +
			"model-authored text, lastToolName names a tool without its " +
			"arguments. Keyed per thread + launch item: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderTerminalOutput,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Raw PTY bytes of a claude-tui take-control session — command " +
			"output, file contents, anything on the TUI's screen — the same " +
			"data class as terminal:output. The ProviderTerminal* RPCs are " +
			"LocalOnly, so a LAN peer cannot arm the fan-out, but once a local " +
			"pane attaches the sink emits to every subscriber.",
	},
	{
		Channel:   eventchan.ProviderTodoUpdate,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Model-authored plan steps every viewer renders; each frame " +
			"replaces the full list. Keyed per thread: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderTurnCompleted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Core turn lifecycle — drives the active-turn registry on every " +
			"client; a dropped frame is a stuck working indicator, which is " +
			"why the gap handler forces a pane refresh. errorMessage is " +
			"user-facing turn state (principle 5), the same text any client " +
			"reads back from history. Keyed per thread/turn and ordered: " +
			"never latest-only.",
	},
	{
		Channel:   eventchan.ProviderTurnStarted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Pairs with provider:turn_completed in the active-turn " +
			"registry; ids and timestamps only. Keyed per thread/turn and " +
			"ordered: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderUsage,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Deliberately remote-visible: token counts, context %, and rate " +
			"limits are essential feedback for understanding resource " +
			"consumption. Pinned by TestEventVisibleToOrigin. Keyed by " +
			"provider/account/limit — never latest-only.",
	},
	{
		Channel:   eventchan.ProviderUserInput,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Interactive provider questions quote whatever the provider is " +
			"asking about — local paths, command lines, file content — and the " +
			"answer RPCs are LocalOnly. Same class as provider:approval.",
	},
	{
		Channel:   eventchan.BrowserCompanionState,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Why: "Per-thread live page titles and URLs, including file paths. The " +
			"subscribe RPC returns a complete snapshot, so replay is unnecessary.",
	},
	{
		Channel:   eventchan.BrowserInstallProgress,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionLatestOnly,
		Why: "Managed full-Chrome install progress; error strings can quote " +
			"manifest URLs. Each phase supersedes the prior phase, so reconnect " +
			"and live-drop recovery need only the newest state.",
	},
	{
		Channel:   eventchan.SessionImportProgress,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Reports on files in the user's provider homes: each frame names " +
			"the scan row it settled, and a failure carries the reader's own " +
			"message, which quotes the absolute transcript path. Its RPCs are " +
			"all LocalOnly, so keeping the push side loopback-only closes the " +
			"third door.",
	},
	{
		Channel:   eventchan.SettingsUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Carries a tier plus the changed key NAMES, never values — " +
			"GetSettings is wire-safe precisely because it redacts endpoint " +
			"tokens and sensitive environment values, and this channel must " +
			"not become the path around that redaction. A remote peer that " +
			"may already poll GetSettings therefore learns nothing new from " +
			"the push. Retention default, not latest-only: each frame names " +
			"a DIFFERENT set of keys, so the newest does not supersede the " +
			"ones before it, and a client that dropped the frame naming " +
			"`fontSize` would keep rendering the old size forever.",
	},
	{
		Channel:   eventchan.SpinnerChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention: a payload-less refetch signal — `emit(name, " +
			"nil)` from a debounced fsnotify watcher over one directory, " +
			"meaning exactly \"read that directory again\". N retained frames " +
			"are N IDENTICAL frames, so a reconnect after an agent rewrote a " +
			"file a dozen times would replay a dozen full-listing refetches. " +
			"Latest-only rather than ephemeral: a client that was disconnected " +
			"while the directory changed DOES need to hear about it once. " +
			"Unkeyed (one directory, one global answer), so it satisfies the " +
			"membership rule. Audience: GetSpinnerFiles is deliberately " +
			"wire-safe — remote clients fetch the same sprite assets, so the " +
			"nudge must reach them too.",
	},
	{
		Channel:   eventchan.SystemStats,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention reviewed: host CPU + memory sample for the sidebar " +
			"footer, a fresh whole-state sample every 2s (app_sysstat.go), so " +
			"a default-depth ring held ~33 minutes of stale samples and " +
			"replayed all of them on reconnect just to be overwritten by the " +
			"last one. Unkeyed. Audience: coarse whole-host aggregates " +
			"(CPU %, RAM, isWsl) — monitoring the backend host is the " +
			"channel's purpose, including from a remote viewer, and the " +
			"payload doc bars per-process detail from ever joining it.",
	},
	{
		Channel:   eventchan.TerminalExit,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Local PTY session lifecycle; paired with terminal:output and the same local-execution class.",
	},
	{
		Channel:   eventchan.TerminalOutput,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why:       "Raw local PTY bytes — command output, file contents, anything on the terminal's screen.",
	},
	{
		Channel:   eventchan.ThemeChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention: payload-less refetch signal, same matched " +
			"set as spinner:changed and workflow:definitions-changed — see " +
			"spinner:changed for the full reasoning. Audience: GetThemeFiles " +
			"is deliberately wire-safe, same story as spinner:changed.",
	},
	{
		Channel:   eventchan.ThreadModeChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Thread-row state (mode + a one-shot needsReconnect toast) " +
			"every viewer renders; mutation stays LocalOnly " +
			"(UpdateThreadMode). Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadRuntimeModeChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Thread-row approval-posture state; names a mode, not a " +
			"capability a remote peer could invoke (UpdateThreadRuntimeMode " +
			"is LocalOnly). Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadTitleGeneration,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Title-regen completion signal; error text passes " +
			"textgen.RedactError, which collapses CLI failures to an opaque " +
			"sentence precisely because raw ones can quote subprocess " +
			"stderr. Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Full frames embed the whole store.Thread — absolute project/" +
			"workspace/worktree paths and session refs — but ListThreads is " +
			"deliberately wire-safe, so a remote peer can poll identical " +
			"rows: the push discloses nothing a poll could not (same " +
			"reasoning as discussion:message). If thread reads ever go " +
			"LocalOnly, this row must flip with them. Patch frames merge " +
			"field-by-field, so every frame matters: never latest-only.",
	},
	{
		Channel:   eventchan.UpdaterDownloadStarted,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Update lifecycle is host-local: CheckForUpdate / " +
			"DownloadUpdate / RestartToUpdate are all LocalOnly and only " +
			"this host can install, so a LAN peer can neither arm nor act " +
			"on these frames; the verbatim Release payload also discloses " +
			"host OS/arch. Sibling updater:install was already " +
			"loopback-only. (Bridged from updater.EventDownloadStarted.)",
	},
	{
		Channel:   eventchan.UpdaterError,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Verbatim updater/launcher error text — can quote URLs and " +
			"staged file paths, and the WSL path forwards text from a " +
			"separate Windows launcher process this backend does not " +
			"control. Same loopback-only story as updater:download-started. " +
			"(Bridged from updater.EventError, plus two direct emits.)",
	},
	{
		Channel:   eventchan.UpdaterInstall,
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
		Channel:   eventchan.UpdaterInstalling,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Same loopback-only story as updater:download-started. " +
			"(Bridged from updater.EventInstalling.)",
	},
	{
		Channel:   eventchan.UpdaterProgress,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionLatestOnly,
		Why: "Per-chunk byte counters — the highest-frequency updater " +
			"channel, unkeyed (one install at a time, ErrUpdateBusy), each " +
			"frame fully superseding the last; replaying a backlog of stale " +
			"counts is pure waste, so it satisfies the latest-only " +
			"membership rule. Loopback-only with the rest of the lifecycle. " +
			"(Bridged from updater.EventDownloadProgress.)",
	},
	{
		Channel:   eventchan.UpdaterReady,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Arms the local Restart button; same loopback-only story as " +
			"updater:download-started. Default ring so a reconnecting local " +
			"pane still learns an update is staged. (Bridged from " +
			"updater.EventUpdateReady on desktop; emitted directly by " +
			"stageWSLUpdate on WSL.)",
	},
	{
		Channel:   eventchan.UpdaterVerifying,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "Same loopback-only story as updater:download-started. " +
			"(Bridged from updater.EventVerifying.)",
	},
	{
		Channel:   eventchan.UsageThreadCost,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Why: "threadId-only nudge, but both RPCs it triggers " +
			"(GetThreadContextUsage, GetCodexAccountUsage) are LocalOnly — " +
			"a remote peer receiving it cannot act on it, so frames off " +
			"loopback are pure waste (mcp:status reasoning). Keyed per " +
			"thread.",
	},
	{
		Channel:   eventchan.UserMessageReverted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Timeline truncation directive every viewer must apply or its " +
			"rendered history diverges from SQLite; ids and counters only, " +
			"no message bodies. The client self-dedups on the monotonic " +
			"historyRev, so replay is safe and out-of-order frames are " +
			"refused. Keyed per thread.",
	},
	{
		Channel:   eventchan.WebviewTrim,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Why: "An imperative directive, not a notification: the Windows " +
			"launcher acts on it by forcing a memory-reducing GC in the " +
			"renderer over the WebView2 DevTools bridge. Its only " +
			"legitimate consumer is the launcher on this host, which is " +
			"loopback by construction. Ephemeral for the same reason as " +
			"updater:install: it speaks for the idle moment it was emitted " +
			"in, so replaying a backlog to a reconnecting launcher would " +
			"fire GC pauses into a session that may be active again. The " +
			"backend re-emits on the next idle report.",
	},
	{
		Channel:   eventchan.WorkflowDefinitionsChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "Retention: payload-less refetch signal, same matched " +
			"set as theme:changed and spinner:changed — see spinner:changed " +
			"for the full reasoning. Audience: workflow reads are " +
			"deliberately wire-safe so remote workflow overlays can render; " +
			"they need this nudge to refetch definitions.",
	},
	{
		Channel:   eventchan.WorkflowEngineState,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Why: "One unkeyed process-wide boolean ({paused}) where the newest " +
			"frame is the whole answer — satisfies the latest-only " +
			"membership rule, and replaying exactly the newest frame on " +
			"reconnect heals the documented dropped-frame banner hazard " +
			"(workflowRuns.svelte.ts resync note). Wire-safe workflow reads " +
			"let remote overlays render the pause banner.",
	},
	{
		Channel:   eventchan.WorkflowError,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Run-failure toasts remote overlay viewers need too (workflow " +
			"reads are wire-safe); emit sites keep the real cause in local " +
			"diagnostics and send a hand-written opaque sentence. One " +
			"deliberate exception: reportUnitWorktreeRetained names the " +
			"retained worktree's absolute path — the only place a human can " +
			"recover the uncommitted work — which matches the accepted " +
			"posture that absolute workspace paths already reach remote " +
			"peers (thread:updated). Every frame is a distinct toast " +
			"(client dedups over an LRU): never latest-only.",
	},
	{
		Channel:   eventchan.WorkflowGateNotify,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Workflow-authored ids and enums only; the one surface that " +
			"reports a gate crossing without a park, so each frame is a " +
			"distinct crossing and the ring must hold them all — never " +
			"latest-only. (No frontend subscriber today; workflowapp consumes " +
			"it after transport emission to schedule progress wakes.)",
	},
	{
		Channel:   eventchan.WorkflowItemState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Run-state transitions the overlay accumulates (a transition " +
			"on an unknown run triggers a full refetch); wire-safe workflow " +
			"reads serve remote overlays. Keyed per run: never latest-only.",
	},
	{
		Channel:   eventchan.WorkflowPhaseState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "Per-run/phase/attempt/unit patches the run map applies in " +
			"place. Keyed: never latest-only.",
	},
	{
		Channel:   eventchan.WorkflowSoftStop,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Why: "{itemId, armed} supersede-per-key state; keyed per run-tree " +
			"root, so capacity-1 retention would evict other runs' latest " +
			"frames — never latest-only.",
	},
	{
		Channel:   eventchan.WorktreeSetup,
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
//
// Keyed by plain string, not eventchan.Channel, deliberately: every
// lookup path is reached by a channel name that came off the WIRE at
// least some of the time (Replay's cursor map, the launcher's replay
// request), and converting untrusted input into the newtype would assert
// a registration nobody checked. Emit's typed channel converts on the
// way in for free — string and eventchan.Channel share a representation.
var channelPolicyIndex = func() map[string]ChannelPolicy {
	index := make(map[string]ChannelPolicy, len(channelPolicies))
	for _, policy := range channelPolicies {
		index[string(policy.Channel)] = policy
	}
	return index
}()

// unregisteredChannelPolicy is what a channel with no registry row gets:
// fail-CLOSED. An unregistered channel is delivered to loopback
// connections only, so a channel nobody classified can never reach a LAN
// peer by omission — the forgotten registration degrades to "invisible to
// remote clients", never to "leaked to remote clients". Local UX keeps
// working, and TestChannelPolicyUnreviewedWorklist keeps the registered
// table honest.
//
// The two harness-only dynamic emit paths (HarnessEmit, harness.Replayer)
// need no carve-out either, but not because their channels are
// unregistrable — a caller-named channel that spells a REGISTERED name
// inherits that row's audience, and a replay log's kinds are exactly the
// registered names emitWithReplay recorded. Their safety is reachability:
// both exist only under --harness/--soak, on a LocalOnly receiver, so
// only loopback test tooling can drive them; unrecognized names still
// land on this default (2026-08-25 security review, finding 3).
//
// The flip from the original fail-open default landed only after every
// row above had a decided Why (2026-08-25); it is a deliberate behavior
// change, not a side effect of building the table.
var unregisteredChannelPolicy = ChannelPolicy{
	Audience:  AudienceLoopbackOnly,
	Retention: RetentionDefault,
	Why:       "unregistered channel — fail-closed default (see unregisteredChannelPolicy)",
}

// policyForChannel returns the registered policy for a channel, or the
// fail-closed default for a channel with no row. The bool reports whether
// the channel was registered.
func policyForChannel(channel string) (ChannelPolicy, bool) {
	if policy, ok := channelPolicyIndex[channel]; ok {
		return policy, true
	}
	fallback := unregisteredChannelPolicy
	// Labels the fallback row with the name that missed; it is a
	// diagnostic, not an assertion that this name is registered.
	fallback.Channel = eventchan.Channel(channel)
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
