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
	// AudienceLoopbackOnly reaches loopback connections only. It is for
	// frames whose ONLY legitimate consumer is a process on this host:
	// launcher directives (the sleep inhibitor, the browser pane, the
	// binary swap, the renderer trim), harness tooling, and the desktop
	// self-updater's own lifecycle.
	//
	// It is NOT a disclosure control for thread or workspace state. Since
	// wave 6d1 every off-host connection names a session, so SCOPE is the
	// gate for that state — a channel carrying it rides the scope its pull
	// RPC carries, and the audience is `any` because a session granted
	// that scope is granted the state. Wave 6d2 deleted the per-method
	// local-only table, and nineteen rows here went on citing it for another
	// three months: a phone could call RegisterQueueItem, RespondToApproval,
	// GetGitStatus and OpenTerminal while every matching push was withheld,
	// so it showed stale queue rows, never saw an approval prompt live, and
	// got a terminal with no output (re-adjudicated 2026-09-03).
	//
	// The column still exists for the directive channels because it is a
	// THIRD DOOR independently of the per-call scope gate: a channel's RPCs
	// being `host`-scoped stops an off-host session arming the stream, but
	// once a local pane subscribes the push side fans out to every
	// subscriber regardless of who armed it. Those rows carry `host` on
	// Scope AND loopback-only here, so neither has to be the only answer.
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
	// Scope is the grant a SESSION-carrying connection must hold to
	// receive this channel's frames (docs/specs/remote-access.md §5).
	//
	// The rule for choosing one: it is the scope of the RPC that reads the
	// same data. A push must not be a way around the authorization its
	// pull half enforces — git:status carries what GetGitStatus returns,
	// so both are git:operate; provider:terminal-output carries what
	// ProviderTerminalReplay returns, so both are terminal:operate. Where
	// no RPC reads the same data (system:stats is push-only), the row
	// takes the observe-tier scope a client needs to render the surface
	// those frames feed.
	//
	// ScopeHost means no session grant can open it and host presence is
	// the only key — the same rule AuthorizeSessionMethod applies to a
	// host-scoped method, for the same reason.
	//
	// It does NOT replace Audience. Both are enforced: Audience is the
	// origin question ("may a LAN peer see this at all") and answers it
	// for the launch-credential connections that name no session, while
	// Scope is the grant question. A connection subject to both is
	// narrowed by both.
	Scope Scope
	// EntityFiltered opts this channel into per-thread subscription
	// narrowing: a connection that sent a `watch` frame receives this
	// channel's frames only for the threads it named (event_entity.go).
	//
	// It is a THIRD, orthogonal question — Audience asks about the peer's
	// locality, Scope about its grants, and this one about whether the peer
	// is looking at the entity the frame is addressed to. A connection
	// subject to all three is narrowed by all three.
	//
	// MEMBERSHIP RULE, and it is narrow: the channel must be a
	// HIGH-FREQUENCY PAYLOAD CARRIER whose only consumers render the named
	// thread, and whose absence must degrade to a slower correct path rather
	// than to missing state. Every low-frequency thread-keyed channel — turn
	// lifecycle, thread:updated, approvals, usage, subagent progress, todo —
	// deliberately stays wildcard: the sidebar, the tray and the
	// thread-status projections read those for threads with no pane open, by
	// design, and narrowing one would silently stop a badge the user relies
	// on to decide WHICH thread to open.
	//
	// A row here is a CLAIM that nothing off-pane reads the channel. The
	// claim is established by sweeping the frontend consumers, never assumed
	// from the channel's name — provider:item_event looks like the obvious
	// member and is not one (see its row).
	//
	// An empty entity key on such a channel is still DELIVERED: an event the
	// extractor cannot attribute must not vanish (event_entity.go).
	EntityFiltered bool
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
		Channel:   eventchan.DevServerList,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopePreviewOpen,
		Why: "Whole-state list of this backend's dev-server ports, replaced " +
			"in full on every scan tick, so a default-depth ring would hold " +
			"minutes of superseded frames and replay all of them to be " +
			"overwritten by the last. Scope: the list names every loopback " +
			"port on the host that answers like a page, plus the pid and " +
			"command holding it — a port-scan oracle for the machine, so it " +
			"rides the same execute-tier scope as the gateway it feeds " +
			"rather than a read scope. Audience: reaching a preview from " +
			"another device is the entire feature, so a LAN or tailnet " +
			"session that holds preview:open is exactly the intended " +
			"receiver.",
	},
	{
		Channel:   eventchan.DiscussionDefinitionsChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeThreadsRead,
		Why: "Retention: payload-less refetch signal, the same matched set " +
			"as workflow:definitions-changed and theme:changed — see " +
			"spinner:changed for the full reasoning. Unkeyed (one " +
			"definitions catalog, one global answer), so it satisfies the " +
			"latest-only membership rule. Scope by the rule: ListDiscussions " +
			"/ ListDiscussionsForThread / GetDiscussion are the reads that " +
			"answer the same data and all three are threads:read, so the " +
			"nudge reaches exactly the sessions the listing already answers. " +
			"Audience any because the composer's Discussions menu and the " +
			"settings editor render on a phone too, and an editor showing a " +
			"definition another device deleted offers a start that fails.",
	},
	{
		Channel:   eventchan.DiscussionMessage,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Deliberately remote-visible. A session granted threads:read can " +
			"already call GetChannelMessages, so pushing the same data " +
			"discloses nothing a poll could not already read — it just saves " +
			"the round-trip. PostChannelMessage sits a tier up in " +
			"threads:operate, because dispatching a turn prompt is session " +
			"control, not a read.",
	},
	{
		Channel:   eventchan.DiscussionState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Deliberately remote-visible, same reasoning as " +
			"discussion:message: GetChannelState is not LocalOnly. Keyed by " +
			"channel id, so it must never become latest-only.",
	},
	{
		Channel:   eventchan.DraftUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "Names a thread whose composer draft moved and the screen that " +
			"moved it; the draft TEXT never rides it, because receivers " +
			"re-read through GetDraft. Scope is the gate: GetDraft / " +
			"SaveDraft / ClearDraft are all threads:operate for the " +
			"disclosure reason (in-progress user-typed work), so the only " +
			"session told WHICH thread someone is typing in is one that may " +
			"read the text a call later — the push is not a way around the " +
			"pull. Audience any because that is the grant: a phone sharing " +
			"the composer converges on the same draft, and one that hears " +
			"nothing overwrites the work the desk just saved. Never " +
			"latest-only: a clear and a save are different edges on the same " +
			"thread, and collapsing them loses the one the client needed.",
	},
	{
		Channel:   eventchan.GitStatus,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeGitOperate,
		Why: "Addressed by the CANONICAL ABSOLUTE workspace path (it has to " +
			"be — one frame serves every pane on that worktree), so every " +
			"frame discloses where the user's repositories live on disk. That " +
			"disclosure is exactly what git:operate grants, and the Scope " +
			"column is the gate: GetGitStatus returns the same paths and the " +
			"same porcelain, so the push cannot reach a session the pull " +
			"would refuse. Audience any because a review pane on a phone has " +
			"to see the working tree move as it moves; a stale file list is " +
			"what a commit gets built from. Keyed by cwd — never " +
			"latest-only.",
	},
	{
		Channel:   eventchan.HarnessMock,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Scope:     ScopeHost,
		Why: "Frames carry the mock's cwd (a local path) and the exact wire " +
			"text the app sent the provider. Harness-only (--harness boot, " +
			"LocalOnly receiver), but the push side is the third door; its " +
			"consumers are loopback test tooling by construction.",
	},
	{
		Channel:   eventchan.HarnessPerf,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
		Why: "Progress frames name the local NDJSON replay-log path and pass " +
			"IO/parse errors verbatim. Harness-only; loopback consumers by " +
			"construction (same story as harness:mock).",
	},
	{
		Channel:   eventchan.HarnessUIQuery,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Scope:     ScopeHost,
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
		Channel:        eventchan.HighlightDiffSeed,
		Audience:       AudienceAny,
		Retention:      RetentionEphemeral,
		Scope:          ScopeFilesRead,
		EntityFiltered: true,
		Why: "Entity-filtered: a seed is a cache warmer for ONE thread's diff " +
			"cards, keyed by thread id, and its two consumers (eventsHighlight " +
			"applyHighlightDiffSeed → diffSpanCache) only ever answer a diff " +
			"card mounted in a pane on that thread. A client watching other " +
			"threads would insert span arrays nothing looks up, and a card that " +
			"mounts later recomputes through HighlightPatch anyway — the RPC " +
			"path is the designed fallback, so a withheld seed costs a round " +
			"trip and never a wrong color. The frames are the large ones (per " +
			"file, per line span arrays), which is what makes the narrowing " +
			"worth having. " +
			"Goes to EVERY client, deliberately: its persist-time seeds can " +
			"be parse-primed with the just-edited workspace file — better " +
			"spans than the loopback RPC path recomputes for a persisted diff " +
			"— so local clients consume them as in-place cache upgrades rather " +
			"than redundant warmers. (It used to be remote-only; the producer " +
			"gate was dropped alongside.) Ephemeral because a seed is a " +
			"point-in-time cache warmer: replaying a superseded one is useless " +
			"and each frame can carry large span/hash arrays.",
	},
	{
		Channel:        eventchan.HighlightSeed,
		Audience:       AudienceRemoteOnly,
		Retention:      RetentionEphemeral,
		Scope:          ScopeFilesRead,
		EntityFiltered: true,
		Why: "Entity-filtered, same argument as highlight:diff_seed and with " +
			"more force: this is per-growth-step span metadata for a STREAMING " +
			"fence, so a busy thread produces frames at the delta rate, and its " +
			"consumers (liveCodeSeeds putLiveCodeSeed, codeSpanCache " +
			"seedFinalBlockSpans) are read only by a StreamdownCodeHost mounted " +
			"in a pane on that thread. Missing seeds degrade to the highlight " +
			"RPC that host already falls back to. One accepted loss: a final " +
			"seed's contentKey entry is content-addressed, so an identical " +
			"fence in another thread would have got a free cache hit it now " +
			"pays an RPC for. " +
			"Pushes syntax-span metadata alongside streaming text so a remote " +
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
		Channel:   eventchan.KeybindingsUpdated,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeSettingsRead,
		Why: "Retention: payload-less refetch signal fired after the user " +
			"keybindings file is rewritten or reset, the same matched set as " +
			"theme:changed and spinner:changed. Unkeyed (one file, one global " +
			"answer), so the latest-only membership rule holds and N retained " +
			"frames would be N identical refetches. Scope by the rule: " +
			"GetKeybindings is the read that answers the same data and it is " +
			"settings:read, one tier below the settings:write the two writes " +
			"carry — a session that may READ the bindings may hear that they " +
			"moved. Audience any: chord dispatch runs on every client, so a " +
			"page that never hears the change keeps dispatching the old " +
			"chords until it is reloaded.",
	},
	{
		Channel:   eventchan.MCPOAuthCompleted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeSettingsWrite,
		Why: "Carries provider-reported MCP error strings verbatim " +
			"(sanitizeMCPError bounds length and collapses newlines — it does " +
			"not redact, and an `invalid_grant` body can quote token " +
			"material). Scope is the gate, and it is the WRITE tier for that " +
			"reason: every MCP RPC is settings:write (GetMcpServerStatus, " +
			"TriggerMcpAuth, ReconnectMcpServer), so the only session told how " +
			"a sign-in ended is one that could have started it. Audience any " +
			"because an MCP sign-in opens a browser, and on a headless host " +
			"the browser is on the device that asked — the frame that says it " +
			"worked has to reach the same screen.",
	},
	{
		Channel:   eventchan.MCPStatus,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeSettingsWrite,
		Why: "Same disclosure class and same gate as mcp:oauth-completed — " +
			"verbatim provider MCP error strings, reaching the settings:write " +
			"sessions that ListMcpServerStatuses already answers, which is " +
			"what makes the MCP panel render at all on a paired device. Keyed " +
			"by server, so it must never become latest-only: capacity 1 would " +
			"evict other servers' latest frames.",
	},
	{
		Channel:   eventchan.NotificationActivated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "A reveal-this-target directive following a notification click. " +
			"Every attached client is the same owner's session, and acting on " +
			"a notification away from the desk is the reason remote access " +
			"exists, so an attached remote client follows the same reveal. " +
			"The target names a thread or work item by id, which such a " +
			"client already reads over its ordinary RPCs. PRODUCING one stays " +
			"host-only — the NotificationActivated RPC is `host`-scoped, which " +
			"no session may be granted — so a remote client can receive a " +
			"reveal and never inject one. " +
			"Retained, because a click can cold-launch the desktop window " +
			"before its first connection; only a loopback page asks for that " +
			"ring, since a remote page was not launched by a toast on this " +
			"host (frontend/src/lib/transport/wsClient.ts).",
	},
	{
		Channel:   eventchan.NotificationSend,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Instructs a presenter to raise, replace or withdraw a " +
			"notification. The host-side presenter (the Windows launcher's " +
			"notification client) is one consumer; an attached remote client " +
			"is the other, and being told a turn finished while away from " +
			"the desk is the whole point of attaching one. It carries a " +
			"bounded thread TITLE and a fixed phrase, never message text, " +
			"tool output or provider prose — internal/notify's mapping is " +
			"the redaction boundary, and a notification is the one surface " +
			"that renders outside the app's window. Even the title is not a " +
			"new disclosure to a client that reads it over its ordinary " +
			"RPCs. Emitting is host-side only (App.notifyOS), so no client " +
			"can make another client raise one. Retained: the launcher " +
			"replays this channel by cursor after a reconnect " +
			"(wsllauncher/notification_client.go), so it must NOT become " +
			"ephemeral or latest-only — and a retraction is ordered behind " +
			"the send it withdraws, which only a real ring preserves.",
	},
	{
		Channel:   eventchan.PowerKeepAwake,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionLatestOnly,
		Scope:     ScopeHost,
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
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeGitOperate,
		Why: "Carries a pull request's full detail and every review thread on " +
			"it — private-repo titles, branch names, reviewer logins and " +
			"comment bodies — plus a poll-failure summary. Scope is the gate: " +
			"SubscribePRUpdates / UnsubscribePRUpdates / SetPRUpdatesActive " +
			"are all git:operate and the subscribe call ANSWERS with this same " +
			"detail, so the push reaches exactly the sessions the pull does. " +
			"Audience any because reading a review away from the desk is one " +
			"of the things that grant is for, and a PR pane that never hears " +
			"a new comment presents a stale review as a live one.",
	},
	{
		Channel:   eventchan.ProjectUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
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
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeAccessAdmin,
		Why: "Carries the user's email/display name plus authenticated " +
			"subscriptionType, tokenSource (oauth | apikey | console), and " +
			"apiProvider — account, auth-model, and billing identity in one " +
			"frame. Scoped rather than loopback-only because signing a " +
			"provider account in is now something a REMOTE admin device " +
			"does: on a headless host there is no browser to open, so the " +
			"page opens on the device reading this and the card that " +
			"appears afterwards is the only confirmation it worked. " +
			"access:admin is the same grant that already lets that device " +
			"list, switch and remove these accounts over RPC, so the frame " +
			"discloses nothing its holder could not read a call later.",
	},
	{
		Channel:   eventchan.ProviderAccountUsageError,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeAccessAdmin,
		Why: "Account-scoped billing/quota failure detail; same identity " +
			"class and same grant as provider:account, and widened " +
			"alongside it — a remote sign-in that lands but cannot read " +
			"its quota has to say so rather than go quiet.",
	},
	{
		Channel:   eventchan.ProviderAccountsChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeAccessAdmin,
		Why: "Payload-less refetch signal for the account LISTING, fired " +
			"when a switch or a removal moved the set. Its own channel " +
			"rather than a second meaning on provider:account, which says " +
			"only that the ACTIVE identity changed: removing an account " +
			"that was not the active one moves the list and moves no " +
			"identity, so it emitted nothing at all and every other client " +
			"kept offering an account that is gone. Scope by the rule: " +
			"ListProviderAccounts returns the set and is access:admin, the " +
			"same grant SwitchProviderAccount and RemoveProviderAccount " +
			"carry, so the nudge reaches exactly the sessions the listing " +
			"answers. Audience any for the reason provider:account is: " +
			"managing these accounts is something a remote admin device " +
			"does. Latest-only and unkeyed — the frame carries nothing, so " +
			"N retained frames are N identical refetches, and retaining the " +
			"newest is what tells a client that reconnected to re-read.",
	},
	{
		Channel:   eventchan.ProviderApproval,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeApprovalsRespond,
		Why: "Tool-use approval requests quote the exact command line, file " +
			"path, or patch the provider wants to run against the user's " +
			"machine, and approving is equivalent to running it. Scope is the " +
			"gate, and it is its own capability rather than a shade of " +
			"threads:read for exactly that reason: approvals:respond is what " +
			"RespondToApproval carries, so the prompt reaches precisely the " +
			"sessions that could answer it and no observe-tier device is " +
			"shown one at all. Audience any because answering every approval " +
			"from the phone is the feature (spec §9): a prompt that arrives " +
			"only at the desk is a turn that waits until somebody walks back " +
			"to it.",
	},
	{
		Channel:   eventchan.ProviderBackgroundTaskState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Background terminal/task state: the local command and its " +
			"output-derived state. Scope is the gate, and threads:read is the " +
			"honest one — the same command lines already reach the same grant " +
			"on provider:item_event, which is where a reader sees them first, " +
			"so this discloses nothing the transcript does not. Audience any " +
			"because the activity rail renders it wherever the thread is " +
			"open, and a task that never leaves `running` on the phone is a " +
			"workspace lock nobody can clear.",
	},
	{
		Channel:   eventchan.ProviderBackgroundTasksChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "threadId plus a full replacement set of task refs (ids and " +
			"model-authored descriptions) — no command lines or paths; that " +
			"loopback-only data rides provider:background_task_state instead. " +
			"Consumers treat it as a refetch nudge. Keyed per thread.",
	},
	{
		Channel:   eventchan.ProviderCommandLifecycle,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Ids and enums only — thread, command uuid, user item id, state, " +
			"delivery — never a message body; the bodies ride the " +
			"provider:queue_* siblings at threads:operate. threads:read is " +
			"therefore the observe-tier scope a client needs to render the " +
			"queue rows this frame LABELS, and the label discloses nothing the " +
			"row it labels does not. Audience any because the phone shows " +
			"those rows: a state that never advances reads as a wedged send. " +
			"States are a progression (queued→started→terminal) correlated by " +
			"userItemId, so every frame matters: never latest-only or " +
			"ephemeral.",
	},
	{
		Channel:   eventchan.ProviderCommands,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Slash-command names and hints for composer autocomplete on any " +
			"client — declared names, never command lines or output. Keyed " +
			"per thread; each frame replaces wholesale, but per-key, so " +
			"never latest-only.",
	},
	{
		Channel:   eventchan.ProviderCompacting,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "{threadId, active} render state any viewer needs; the " +
			"provider's failure prose is deliberately logged, not emitted " +
			"(compaction_status.go). Keyed per thread: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderFastMode,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Per-thread mode-chip state, restated on every session init and " +
			"turn completion; disabledReason is provider prose but names no " +
			"paths or identity. Keyed per thread.",
	},
	{
		Channel:        eventchan.ProviderItemEvent,
		Audience:       AudienceAny,
		Retention:      RetentionDefault,
		Scope:          ScopeThreadsRead,
		EntityFiltered: true,
		Why: "The main transcript stream; a remote viewer that cannot see it " +
			"has no product. Pinned remote-visible by " +
			"TestEventVisibleToOrigin. Keyed by thread/item — never " +
			"latest-only. " +
			"EntityFiltered since wave 6d2, and the membership rule holds " +
			"only because the off-pane consumers the 6d sweep found were " +
			"re-homed onto wildcard carriers first: the sidebar's Failed " +
			"badge rides thread:error_notice, its Plan ready badge and the " +
			"user_text sidebar bump ride thread:updated (a `full` row and a " +
			"`updatedAt` patch respectively), and the workspace-change lock " +
			"reads provider:background_tasks_changed, which now fires on " +
			"Claude's exit / drain / orphan-recovery transitions too. What " +
			"still reads this channel is pane-lifetime or watched-thread " +
			"scoped: the send-queue flush confirm, the proposedPlans warm " +
			"cache (a warm-path optimization — both plan surfaces RPC-load " +
			"on mount), the activity rail's background controller, and " +
			"discussionLiveTail's participant child threads, which have no " +
			"pane but ARE contributed to the watched set by their routing " +
			"table. The one deliberate degradation is eventsItemStream's " +
			"eviction branch: an unwatched thread stops having its warm " +
			"cache and replica window evicted mid-stream, and instead " +
			"validates on read — the next open stamps the window and " +
			"SyncThreadWindow answers stale with a replacing page.",
	},
	{
		Channel:   eventchan.ProviderModelFallback,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Effective-model render state any viewer needs; the provider's " +
			"refusal prose is rune-bounded at the emit site. Keyed per " +
			"thread with a monotonic revision — ordered, never latest-only.",
	},
	{
		Channel:   eventchan.ProviderQueueFlushed,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "Per-thread flush-queue frames carry the queued user message " +
			"bodies and their attachment metadata (local file names). Scope is " +
			"the gate: GetQueueState returns the same bodies and " +
			"RegisterQueueItem puts them there, both threads:operate, so the " +
			"push reaches the sessions the pull answers and no others. " +
			"Audience any because a queue row the phone cannot see is a " +
			"message the user believes was dropped.",
	},
	{
		Channel:   eventchan.ProviderQueueRestored,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "Same payload class, same gate and same audience as " +
			"provider:queue_flushed — queued user message bodies restored " +
			"into the composer, reaching the threads:operate sessions " +
			"GetQueueState already answers. A client that misses it holds a " +
			"queue row the backend has already moved back into the draft.",
	},
	{
		Channel:   eventchan.ProviderQueueStateChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "Same payload class, same gate and same audience as " +
			"provider:queue_flushed — the queue snapshot it announces carries " +
			"the queued message bodies, and GetQueueState is the " +
			"threads:operate read that returns them. This is the frame a " +
			"second screen converges its composer queue on, so withholding it " +
			"is what left a phone showing rows that had already been sent.",
	},
	{
		Channel:   eventchan.ProviderSessionAccount,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeAccessAdmin,
		Why: "Per-session binding of a thread to a provider account identity; " +
			"same identity class as provider:account and the same gate — " +
			"access:admin is what ListProviderAccounts carries, so the push " +
			"names an account only to a session that may already enumerate " +
			"them. Audience any for the reason provider:account is: a remote " +
			"admin device is where a provider sign-in happens on a headless " +
			"host, and which account a thread is spending is how that device " +
			"confirms the switch took.",
	},
	{
		Channel:   eventchan.ProviderSessionDied,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Deliberately remote-visible: without it a remote viewer sees the " +
			"turn silently stop with no explanation. The frame carries " +
			"StderrTail — the dead process's last stderr line, pre-sanitized " +
			"by provider.MarshalProcessExitMeta (single line, hard length " +
			"cap) — and that is a decided disclosure, not an oversight: the " +
			"same string persists to items.meta, which the wire-safe " +
			"ListThreadSliceAround already serves to remote peers " +
			"(2026-08-25 security review, finding 1). Pinned by " +
			"TestEventVisibleToOrigin.",
	},
	{
		Channel:   eventchan.ProviderStatus,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeAccessAdmin,
		Why: "Reports provider CLI installation/auth state and carries an " +
			"actionURL plus provider-side error prose — install paths and " +
			"authentication failures for the local machine's toolchain. " +
			"Scoped rather than loopback-only because \"not signed in\" is " +
			"the banner a remote admin acts on: it now carries the Sign in " +
			"action that starts a provider login, and a device that can " +
			"start one has to be able to see why it is needed. The host " +
			"detail it discloses is where the two provider CLIs are " +
			"installed, which the same access:admin caller reads directly " +
			"from the provider settings it may already edit.",
	},
	{
		Channel:   eventchan.ProviderLogin,
		Audience:  AudienceAny,
		Retention: RetentionEphemeral,
		Scope:     ScopeAccessAdmin,
		Why: "The live state of one provider sign-in: phase, the authorize " +
			"URL to open, and the Codex device code to type. Reaching a " +
			"REMOTE admin device is the entire point — a headless host has " +
			"no browser, so the page opens wherever the owner is — and " +
			"access:admin is the grant that starts, answers and cancels " +
			"the same flow over RPC. Ephemeral because replay is both " +
			"useless and wrong here: an authorize URL is a one-use PKCE " +
			"challenge and a device code dies with its flow, so a replayed " +
			"frame offers a link that no longer answers. A client that " +
			"reconnects mid-flow reads GetProviderLoginState instead, " +
			"which is per-provider and current.",
	},
	{
		Channel:   eventchan.ProviderSubagentProgress,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Subagent tray/progress state; activity and summary are " +
			"model-authored text, lastToolName names a tool without its " +
			"arguments. Keyed per thread + launch item: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderTerminalOutput,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeTerminalOperate,
		Why: "Raw PTY bytes of a claude-tui take-control session — command " +
			"output, file contents, anything on the TUI's screen — the same " +
			"data class as terminal:output. Scope is the gate: " +
			"ProviderTerminalReplay returns these same bytes and " +
			"ProviderTerminalAttach / Write drive the same PTY, all " +
			"terminal:operate, so the push cannot reach a session the replay " +
			"would refuse. Audience any because the phone has a terminal " +
			"(spec §9) and take-control without output is a blank screen.",
	},
	{
		Channel:   eventchan.ProviderTodoUpdate,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Model-authored plan steps every viewer renders; each frame " +
			"replaces the full list. Keyed per thread: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderTurnCompleted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeThreadsRead,
		Why: "Pairs with provider:turn_completed in the active-turn " +
			"registry; ids and timestamps only. Keyed per thread/turn and " +
			"ordered: never latest-only.",
	},
	{
		Channel:   eventchan.ProviderUsage,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Deliberately remote-visible: token counts, context %, and rate " +
			"limits are essential feedback for understanding resource " +
			"consumption. Pinned by TestEventVisibleToOrigin. Keyed by " +
			"provider/account/limit — never latest-only.",
	},
	{
		Channel:   eventchan.ProviderUserInput,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Interactive provider questions quote whatever the provider is " +
			"asking about — local paths, command lines, file content — which " +
			"is the same content the transcript carries to the same grant on " +
			"provider:item_event, and that is what threads:read covers. " +
			"Audience any because a question nobody sees is a turn that never " +
			"finishes, and answering from the phone is the point (spec §9). " +
			"READING and ANSWERING are deliberately different grants on this " +
			"one: RespondToUserInput is approvals:respond, so an observe-tier " +
			"device sees the question and is refused the answer. That is the " +
			"asymmetry a viewer needs — a turn parked on a question it cannot " +
			"see is a turn that looks hung — whereas provider:approval quotes " +
			"a command the viewer has no business reading before it runs, so " +
			"it carries approvals:respond on both halves.",
	},
	{
		Channel:   eventchan.BackendAttach,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Scope:     ScopeHost,
		Why: "How one attach ended, minutes after the RPC that started it " +
			"returned. Scope host and loopback-only, matching the four " +
			"AddBackend/ListBackends/RemoveBackend/RenameBackend methods it " +
			"belongs to: attaching this installation to another machine is " +
			"something only the person at this keyboard does, so the push " +
			"and the pull are refused to the same callers. Retained rather " +
			"than ephemeral because the page that asked may have reloaded " +
			"during a ten-minute wait, and the frame is the only thing " +
			"that says how it ended.",
	},
	{
		Channel:   eventchan.BackendSetChanged,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionDefault,
		Scope:     ScopeHost,
		Why: "Every mutation of the attached-machine SET that is not an " +
			"attach ceremony: a removal, a rename. A HOST DIRECTIVE class " +
			"row, exactly as backend:attach is, and for the same reason — " +
			"the four ListBackends/AddBackend/RemoveBackend/RenameBackend " +
			"methods act on THIS process's own profile directory and are " +
			"`host`, so the push and the pull are refused to the same " +
			"callers, and its only legitimate consumer is a page on this " +
			"machine. Its own channel rather than a second meaning on " +
			"backend:attach: that one answers \"how did the pairing I " +
			"started end\", this one answers \"the list moved\", and a " +
			"receiver that conflated them would retire a pending row on a " +
			"rename. Retained on the ordinary ring, like backend:attach " +
			"beside it: a frame names ONE machine, so a latest-only slot " +
			"would drop the first of two removals for a page that was " +
			"reloading while both happened.",
	},
	{
		Channel:   eventchan.ChatBarFavorites,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeSettingsRead,
		Why: "The whole starred model / discussion list, byte-identical to " +
			"what ListChatBarFavorites answers and what SetChatBarFavorite " +
			"already returned to the client that wrote it. Scope " +
			"settings:read because that read is settings:read: the list is " +
			"a preference, and the push and the pull are refused to the " +
			"same callers. Audience any — it is app state a second device " +
			"renders in every model menu it opens, and withholding it only " +
			"left that device starring into a list it could already read. " +
			"Latest-only, and the membership rule holds: the channel is " +
			"unkeyed and each frame REPLACES the list, so N retained " +
			"frames are N stale copies of one answer.",
	},
	{
		Channel:   eventchan.ChatBarNewThreadDefaults,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "The model profile a future thread seeds from, plus the " +
			"project whose open draft placeholders adopt it — the same " +
			"pair UpdateNewThreadDefaults returns and applies locally, so " +
			"the initiator's echo repeats an apply it already made. Scope " +
			"threads:operate because GetThreadDefaults is threads:operate " +
			"and it answers this exact shape. Audience any: a second " +
			"device with a \"+ New\" composer open is about to create a " +
			"thread with the superseded model, effort and runtime mode, " +
			"which is the one thing the frame exists to stop. Ordinary " +
			"ring rather than latest-only, because the frame is KEYED by " +
			"project and a newest-frame slot would hide a change to any " +
			"other one.",
	},
	{
		Channel:   eventchan.BrowserCompanionState,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Scope:     ScopeHost,
		Why: "Per-thread live page titles and URLs, including file paths. " +
			"BrowserCompanionThreadState returns a complete snapshot, so " +
			"replay is unnecessary. Scope host, matching the six " +
			"BrowserCompanion* RPCs: the pane is a NATIVE view this machine " +
			"paints and has no remote form (embedded-browser spec §9), so " +
			"the push and the pull that return the same payload are refused " +
			"to the same callers — no session grant opens either.",
	},
	{
		Channel:   eventchan.BrowserHost,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionEphemeral,
		Scope:     ScopeHost,
		Why: "An imperative directive, not a notification: the Windows " +
			"launcher acts on it by creating, moving, showing and " +
			"destroying real browser windows inside its own window. Its " +
			"only legitimate consumer is that launcher, which is loopback " +
			"by construction, and a peer that saw it would learn the " +
			"workspace profile ids and the pane geometry. Ephemeral for " +
			"the same reason as updater:install and webview:trim: a " +
			"directive speaks for the layout it was emitted into, so " +
			"replaying a backlog to a launcher that reconnects (the " +
			"Windows-WSL relay tears connections down mid-session) would " +
			"reopen pages the user has closed and position them against a " +
			"pane rect that has moved. The backend re-derives the pane " +
			"state and re-emits.",
	},
	{
		Channel:   eventchan.SessionImportProgress,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsOperate,
		Why: "Reports on files in the user's provider homes: each frame names " +
			"the scan row it settled, and a failure carries the reader's own " +
			"message, which quotes the absolute transcript path. Scope is the " +
			"gate: ListImportableSessions returns those same rows and paths " +
			"and ImportSessions starts the run, both threads:operate, so the " +
			"progress reaches the sessions the listing already answers. " +
			"Audience any because an import started from a device has to " +
			"finish there — a progress bar frozen on its first frame reads as " +
			"a hung run over one that completed.",
	},
	{
		Channel:   eventchan.ReviewCommentsChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Names the inline-review comment SET a persisted write moved — " +
			"a thread plus either a plan item or a diff scope and source " +
			"key — and carries no comment body. It cannot carry one: " +
			"deleting a comment is a delete-or-RESOLVE depending on whether " +
			"it was already sent, so only a re-read says what the set now " +
			"holds. Scope by the rule: ListProposedPlanComments and " +
			"ListDiffReviewComments are the reads that answer the same data " +
			"and both are threads:read; the writes sit a tier up in " +
			"threads:operate, so a session told a set moved may read it a " +
			"call later. Audience any because reviewing a diff from another " +
			"device is one of the things that grant is for, and a comment " +
			"list that never converges shows a resolved comment as open. " +
			"Never latest-only: each frame names a DIFFERENT set, and the " +
			"newest supersedes none of the others.",
	},
	{
		Channel:   eventchan.ServiceUpdateOutcome,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeAccessAdmin,
		Why: "The one frame that closes a remote update: a client that " +
			"asked for one holds the update id, loses its connection while " +
			"the backend restarts, and needs to learn from the backend that " +
			"came back which update this is and how it ended. AudienceAny " +
			"because the peer that asked is exactly the remote owner this " +
			"feature exists for — unlike the updater:* family, whose install " +
			"only this host can perform. access:admin by the rule on Scope: " +
			"GetServiceUpdateStatus is the read RPC that answers the same " +
			"fact, and it is access:admin. It carried `host` until wave 8h2, " +
			"which no session can hold — so the frame the remote owner is " +
			"waiting on reached only the machine they cannot get to, which " +
			"is the one place this feature was never needed. It names an " +
			"update id, an outcome word, this build's version and the " +
			"recorded reason: nothing that read does not already return. " +
			"Latest-only satisfies the membership rule trivially — a process " +
			"publishes this at most ONCE, at the moment its activation gate " +
			"opens — and the ring is what lets a client that reconnects a " +
			"second after boot still find it.",
	},
	{
		Channel:   eventchan.ServiceUpdateStatus,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeAccessAdmin,
		Why: "The whole ServiceUpdateStatus struct on every phase change of " +
			"a remote update flow, and on download progress. access:admin by " +
			"the rule on Scope: GetServiceUpdateStatus returns this exact " +
			"shape, so the push discloses nothing the poll would not — it " +
			"saves the client polling a multi-minute download. AudienceAny " +
			"for the same reason the outcome row is: the peer driving the " +
			"update is the remote owner. Latest-only, and the membership " +
			"rule holds: ONE global flow per process (RequestServiceUpdate " +
			"refuses a second while one runs), so the newest frame fully " +
			"supersedes every earlier one and a reconnecting client wants " +
			"the current phase rather than the progress ticks it missed.",
	},
	{
		Channel:   eventchan.SettingsUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeSettingsRead,
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
		Scope:     ScopeSettingsRead,
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
		Scope:     ScopeThreadsRead,
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
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeTerminalOperate,
		Why: "Local PTY session lifecycle; paired with terminal:output, the " +
			"same data class, the same terminal:operate gate, the same " +
			"audience. A session that ended and never said so leaves a " +
			"live-looking pane on every client that missed the frame.",
	},
	{
		Channel:   eventchan.TerminalOpened,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeTerminalOperate,
		Why: "The other half of terminal:exit: one frame per PTY this " +
			"backend starts, carrying the terminal.SessionSummary " +
			"ListTerminals answers with (id, thread, shell, cwd, size). Same " +
			"data class, same terminal:operate gate, same audience as the " +
			"pair it completes. Without it a second client drops every byte " +
			"of a terminal it never saw open — appendOutput has no tab to " +
			"put them on — and a surface that mounted empty opens a SECOND " +
			"terminal beside the one already running. Never latest-only: " +
			"each frame names a different session, and a dropped one is a " +
			"terminal that stays invisible for its whole life.",
	},
	{
		Channel:   eventchan.TerminalOutput,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeTerminalOperate,
		Why: "Raw local PTY bytes — command output, file contents, anything " +
			"on the terminal's screen. Scope is the gate: GetTerminalReplay " +
			"returns the same bytes and OpenTerminal / WriteTerminal drive the " +
			"same PTY, all terminal:operate, so the push discloses nothing the " +
			"replay would not to the same session. Audience any because the " +
			"phone has a terminal (spec §9), and a terminal that shows no " +
			"output is not one.",
	},
	{
		Channel:   eventchan.ThemeChanged,
		Audience:  AudienceAny,
		Retention: RetentionLatestOnly,
		Scope:     ScopeSettingsRead,
		Why: "Retention: payload-less refetch signal, same matched " +
			"set as spinner:changed and workflow:definitions-changed — see " +
			"spinner:changed for the full reasoning. Audience: GetThemeFiles " +
			"is deliberately wire-safe, same story as spinner:changed.",
	},
	{
		Channel:   eventchan.ThreadErrorNotice,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "{threadId, itemId} — no summary, so the provider's error " +
			"prose stays on provider:item_event and this carries only the " +
			"fact that a row of kind `error` was persisted. It exists to " +
			"stay WILDCARD while provider:item_event is narrowed by watch: " +
			"the sidebar's Failed pill is read on threads with no pane, and " +
			"several error classes never produce a provider:turn_completed " +
			"the pill could have keyed on instead — non-fatal wire errors, " +
			"orphan error results, the Codex ambiguous-turn-start timeout, " +
			"flush-dispatch failures, and steer failures. NOT " +
			"EntityFiltered, deliberately, and low enough frequency that " +
			"the bytes are not worth a filter. Keyed per thread: never " +
			"latest-only.",
	},
	{
		Channel:   eventchan.ThreadGroupUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "A sidebar grouping row: project id, user-chosen name, pin " +
			"state. ListThreadGroups answers identical rows, so the push " +
			"discloses nothing a poll could not, same reasoning as " +
			"thread:updated. Delete frames carry only the removed row, so " +
			"every frame matters: never latest-only.",
	},
	{
		Channel:   eventchan.ThreadModeChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Thread-row state (mode + a one-shot needsReconnect toast) " +
			"every viewer renders; mutation stays LocalOnly " +
			"(UpdateThreadMode). Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadRuntimeModeChanged,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Thread-row approval-posture state; names a mode, not a " +
			"capability a remote peer could invoke (UpdateThreadRuntimeMode " +
			"is LocalOnly). Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadTitleGeneration,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Title-regen completion signal; error text passes " +
			"textgen.RedactError, which collapses CLI failures to an opaque " +
			"sentence precisely because raw ones can quote subprocess " +
			"stderr. Keyed per thread.",
	},
	{
		Channel:   eventchan.ThreadUpdated,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
		Why: "Same loopback-only story as updater:download-started. " +
			"(Bridged from updater.EventInstalling.)",
	},
	{
		Channel:   eventchan.UpdaterProgress,
		Audience:  AudienceLoopbackOnly,
		Retention: RetentionLatestOnly,
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
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
		Scope:     ScopeHost,
		Why: "Same loopback-only story as updater:download-started. " +
			"(Bridged from updater.EventVerifying.)",
	},
	{
		Channel:   eventchan.UsageThreadCost,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "threadId-only nudge: no figure rides it, receivers re-read. " +
			"Scope is the gate and it matches the read the nudge actually " +
			"causes — the handler bumps a version the usage surfaces refetch " +
			"GetUsageStats on, and that is threads:read too, so the push and " +
			"its pull need the same grant. (The two exact reads a user can " +
			"reach by hand sit higher — GetThreadContextUsage at " +
			"threads:operate, GetCodexAccountUsage at access:admin — and both " +
			"are scope-gated or silently caught on the client, so a device " +
			"holding only threads:read is never shown a refusal it did not " +
			"ask for.) Audience any because a context meter that never moves " +
			"is a wrong number presented as a live one. Keyed per thread.",
	},
	{
		Channel:   eventchan.UserMessageReverted,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeHost,
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
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeThreadsRead,
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
		Scope:     ScopeThreadsRead,
		Why: "Run-state transitions the overlay accumulates (a transition " +
			"on an unknown run triggers a full refetch); wire-safe workflow " +
			"reads serve remote overlays. Keyed per run: never latest-only.",
	},
	{
		Channel:   eventchan.WorkflowPhaseState,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "Per-run/phase/attempt/unit patches the run map applies in " +
			"place. Keyed: never latest-only.",
	},
	{
		Channel:   eventchan.WorkflowSoftStop,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeThreadsRead,
		Why: "{itemId, armed} supersede-per-key state; keyed per run-tree " +
			"root, so capacity-1 retention would evict other runs' latest " +
			"frames — never latest-only.",
	},
	{
		Channel:   eventchan.WorktreeSetup,
		Audience:  AudienceAny,
		Retention: RetentionDefault,
		Scope:     ScopeTerminalOperate,
		Why: "Streams the stdout/stderr of the project's own setup commands " +
			"running against the user's checkout — the same data class as " +
			"terminal:output, and it can carry anything a build or install " +
			"script prints, tokens in an env dump included. Scope is the gate " +
			"and terminal:operate is what that content is worth: " +
			"GetThreadWorktreeSetup returns the same buffered output in both " +
			"key spaces (the thread pair, and the workspace pair for " +
			"pre-thread runs), so the stream reaches the sessions the read " +
			"answers. Audience any because a worktree cut from the phone has " +
			"to show its setup running, or the thread looks wedged for as " +
			"long as the recipe takes.",
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
	// ScopeHost is the same fail-closed answer the Audience above gives,
	// stated in the other vocabulary: a channel nobody classified is one
	// nobody decided a remote form for, so host presence is the only key.
	Scope: ScopeHost,
	Why:   "unregistered channel — fail-closed default (see unregisteredChannelPolicy)",
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
