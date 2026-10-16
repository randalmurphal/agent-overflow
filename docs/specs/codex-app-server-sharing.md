# Codex app-server sharing

Status: design, not scheduled. Nothing here is implemented. Wire facts are
in [codex-wire.md](../references/codex-wire.md) and
[codex.md](../references/codex.md#app-server-process-cost-measured-01621);
this spec cites them rather than repeating them.

## Goal

Run many Codex threads on one `codex app-server` process, the way the
app-server is built to be used, instead of one process per thread. The
change splits the provider layer into a **connection** (one process) and a
**session** (one conversation), for every provider, so a provider whose
protocol multiplexes (Codex, and ACP for the planned
[Cursor provider](cursor-provider.md)) can host many sessions on one
connection while Claude keeps exactly one. User-visible behavior per thread
stays what it is today, crash included.

The study also found one defect that exists today and does not depend on
sharing: changed developer instructions never reach a resumed Codex thread.
Its fix is part of this design and can ship first.

## Findings

Each finding is verified against codex-cli 0.162.1 by source at
`rust-v0.162.1`; the instruction, channel and unload findings also by the
spike in
[`fixtures/codex/developer_instructions_shared_process_20261016.ndjson`](../references/fixtures/codex/developer_instructions_shared_process_20261016.ndjson)
([wire notes](../references/codex-wire.md#developer-instructions-across-a-restart-and-per-thread-channels-verified-01621)).

### Changed developer instructions do not reach a resumed thread

AO recomputes developer instructions at every session start and sends them
on `thread/resume` (`startOrResumeThread`, `session_start.go`;
`buildThreadParams`, `session_helpers.go`). Codex accepts the value into
the thread's configuration but never shows it to the model: a resume
restores the reference context from the rollout
(`record_initial_history` -> `apply_rollout_reconstruction`,
`core/src/session/mod.rs`), the restored `TurnContextItem` has no
developer-instructions field, and developer instructions are not a
world-state section, so no diff is emitted. The process boundary is
irrelevant: a restart in a new process (spike, process 2) and a cold resume
inside one process take the same path. The model followed the first token
after the restart.

What this breaks today, on Codex only:

- The app guide and the browser, thread and workflow tool guides: a setting
  change or a tool switch "reaches the text at the next start or resume"
  (`spawnProviderSession`, `app_session.go`) only for a new thread.
- A standing agent's home block: slice 9 of
  [standing-agents.md](standing-agents.md#slices-and-verification) requires
  a rewrite of `instructions.md` to reach the next start. On a resumed Codex
  thread it does not. The harness mock cannot show what a model reads, so
  that slice's proof stops at the wire parameter.
- The user's own `developer_instructions` in `config.toml`.

A fork has the same defect: `InitialHistory::Forked` also runs
`apply_rollout_reconstruction` (`core/src/session/mod.rs:1677-1680`), and
`forkThreadWith` (`session_fork.go`) sends `developerInstructions` the
same way.

The system-prompt override is not affected: `baseInstructions` on a resume
wins over the rollout value
([codex-instructions-tools.md](../references/codex-instructions-tools.md#base-instructions)).

The new text does take effect after any compaction, which rebuilds the
context from the thread's current configuration (spike: the reply switched
to the resumed token after `thread/compact/start`).

### Per-thread instruction channels exist

Two channels deliver new instructions to one thread without a restart, a
compaction or a new thread; both worked on a thread that shared its process
with another thread holding different instructions:

| Channel | Result | Limits |
|---|---|---|
| `thread/inject_items` with a `developer` message | Followed on the next turn. Written to the rollout, so it survives later resumes. | Dropped by compaction, which then installs the thread-level value. Not experimental. |
| `turn/start` `additionalContext`, kind `application` | Followed. Emitted once per changed value per load; an unchanged value is not re-sent. | Each value is middle-truncated to 1,000 tokens (`additional_context.rs`). After compaction the stored value is not re-emitted, so the text is lost while unchanged. Experimental. |

Rejected: collaboration-mode `developer_instructions`. A catalog
collaboration-mode text wins over a client value
(`world_state/collaboration_mode.rs`); the bundled 0.162.1 catalog sets one
for the gpt-6 family, and the remote catalog can set one for any model,
gpt-5.6 and gpt-5.5 included, at runtime. A channel that a server-side
catalog can silently override cannot carry AO's text.

### Account is process-wide

One `AuthManager` serves every thread of an app-server
(`app-server/src/message_processor.rs`). A running process never adopts a
different account written to disk: its guarded reload
(`reload_if_account_id_matches`, `login/src/auth/manager.rs`) skips a file
whose account id differs from the cached one, and both refresh paths then
return a permanent account-mismatch error. Its cached access token keeps
serving every turn, on every thread, until a refresh is needed; after that
its turns fail. So "new turns run on the new account" holds only when AO
moves the thread to a process started after the switch, which is what AO
does today on the next send and what the pool keeps doing
([Account switch](#account-switch)). A per-thread account inside one
process is impossible.

### Unload is prompt when asked for

`thread/unsubscribe` followed by `thread/closed` took under 15 ms with
`-c thread_unload_delay_secs=0` (this spike's fixture), against 3.0 s at
`3` and 60 s at the default (the earlier unload fixture;
[unload](../references/codex-wire.md#thread-unload-and-cold-resume-verified-01621)).
The delay is process configuration. An unload kills that thread's MCP
children; a thread that is active does not unload.

### Thread loads read the latest config

Every thread load builds its configuration with `ConfigBuilder`, which
re-reads `config.toml` (`app-server/src/config_manager.rs`
`load_with_cli_overrides`). A user's edit to MCP servers, features or
`developer_instructions` therefore reaches the next thread start or cold
resume on a running process. What stays fixed for a process's life is its
binary, its environment, its `-c` flags and its auth.

### Frames can be demultiplexed

Every notification and server request that concerns a thread carries
`threadId`. `thread/started` also carries `sessionId`, "shared by threads
that belong to the same session tree" (`v2/thread_data.rs`, present at
AO's floor 0.143), so a spawned child names its root. A `thread/start`
attaches the thread's listener before it writes the response
(`thread_processor.rs`), so frames of a new thread can precede the response
that names it. A `thread/resume` names the thread in its request. Child
frames can arrive before AO knows the child is a child: today's session
holds them in a bounded quarantine until the root's ownership items claim
the child (`routeChildWireEvent`, `child_routing.go:152`). Whether a real
child's `thread/started` always comes first is not established, and the
mock sends child `turn/started` and item frames with no `thread/started`
at all (`agent-visibility-codex.spec.ts`, `codex-collab.spec.ts`).

## Architecture

### Connection and session

A **connection** is one provider process and everything about it: spawn,
framing, request ids, pending requests, the read loop, notifications about
the process rather than a thread, lifetime, exit and crash. A **session**
is one conversation on a connection: its provider thread id, its
configuration, its turns, approvals, side channels and teardown.

Codex multiplexes sessions on a connection. Claude, `claude-tui`, and any
provider without a multiplexed mode host exactly one session per
connection, and their connection ends with that session. One session per
connection is not a capability flag: such a provider's pool key always
includes the AO thread id and the session token, so no two sessions ever
match a key. Nothing else branches on it.

```go
// package provider

// Connection is one provider process.
type Connection interface {
	// PID is the process group leader; 0 once the process has exited.
	PID() int
	// Binary is the resolved binary identity the process runs.
	Binary() BinaryIdentity
	// Done closes after the process exited and every session on it has
	// received its teardown events.
	Done() <-chan struct{}
	// Close ends the process. Every session still on it receives the
	// teardown a host-initiated close produces.
	Close() error
}

// Session is one conversation on a connection.
type Session interface {
	Send(ctx context.Context, content string, opts SendOptions) error
	Interrupt(ctx context.Context) error
	RespondToApproval(ctx context.Context, resp ApprovalResponse) error
	RespondToUserInput(ctx context.Context, resp UserInputResponse) error
	// Connection is the process this session runs on.
	Connection() Connection
	// Close ends this conversation and releases what it holds on the
	// provider side. It never ends a connection other sessions use.
	Close() error
	// OwnedProcesses are the process roots a listening socket of this
	// conversation can descend from, for dev-server attribution.
	OwnedProcesses() []OwnedProcess
}

// OwnedProcess is one root: a PID to match by ancestry and a PGID to match
// a daemonised child still in its group.
type OwnedProcess struct{ PID, PGID int }
```

`PID()` leaves `Session`: every caller that wanted a process asks the
connection. `devServerOwners` (`app_preview.go`) reads `OwnedProcesses` of
every session with no provider branch (row 11).

What Claude and `claude-tui` implement, and nothing more:

- a `Connection` type in `internal/provider` wrapping one
  `provider.Process`: `PID` and `Binary` from the process, `Done` closed at
  the end of the session's read loop (for `claude-tui`, the PTY's exit),
  `Close` the session's current process teardown;
- `Session.Connection()` returning it;
- `Session.OwnedProcesses()` returning the connection's process,
  `{PID: pid, PGID: pid}`, which is what `devServerOwners` matches today;
- `Session.Close()` closing the connection, as it does now.

In `internal/provider/codex` the current `Session` splits in two:

- `Conn` owns the `provider.Process`, `nextID`, the pending map, the read
  loop, the `initialize` handshake and app-server version, the route table
  and the connection-level handlers. `Dial(ctx, ConnConfig)` spawns and
  initializes.
- `Session` keeps everything thread-scoped it owns today (turn state,
  collaboration and child routing, review, plan buffers, settings sync,
  rollout tail, approvals). `conn.Open(ctx, aoThreadID, Config, onEvent)`
  runs the one `thread/start` or `thread/resume` and its checks.
  `sendRequest` and `writeResponse` go through the connection.

The lock order gains one leaf, the connection's route and pending mutex,
taken after every session lock and never held across a callback or a
pipe write.

### Where the pool lives

`internal/sessionruntime` already owns live sessions under one mutex. It
gains the connection registry: connections by pool key, the sessions on
each, start single-flight per key, and drain marks. The registry does no
provider I/O under its lock; dialing runs outside it, single-flighted per
key, the way `BeginStart` single-flights a thread today. The provider
supplies the key and the idle grace; the app supplies the inputs (account
generation, settings environment). `Entry` gains its connection; its
`Codex`, `Claude` and `ClaudeTUI` fields stay, because provider-specific
calls stay behind concrete types.

No orchestration layer is added. `startSessionNow` stays the one start
path: it asks the registry for a connection for the thread's key, then
opens the session on it.

### What becomes connection-scoped

| Concern | Today | After |
|---|---|---|
| Orphan reaper (macOS) | `watchSessionProcess` per session PID, released in `closeProviderSession` | Watched when a connection dials, released when it exits |
| Binary-stale banner | Flag pinned per session token (`app_provider_binary_watch.go`) | A connection fact, projected onto each session on it; Restart moves the session to a connection on the current binary |
| Process liveness | Read loop end per thread | `Connection.Done`; per-session teardown follows from it |
| Raw event log and process logs | `SpawnConfig.ThreadID` (`process.go`) | Process lines name the connection; raw frames are logged after routing, under the routed thread |
| `skills/changed`, `account/updated` | One handler per session | One handler per connection |
| `account/rateLimits/updated` (no `threadId`) | Each session emits `EventRateLimits` for its thread (`protocol_thread.go:219`), shown as that thread's `provider:usage` (`triage/router.go:1016`, `providerlifecycleapp/session.go:17`) | Delivered to every live session on the connection, each emitting it for its own thread |
| `configWarning`, `deprecationNotice` (no `threadId`) | A warning row on the session's thread (`protocol.go:180-193`) | Delivered once to every session live on the connection when it arrives |
| `mcpServer/oauthLogin/completed`, `mcpServer/startupStatus/updated` (optional `threadId`) | The session's handlers; the OAuth handler takes a thread id (`app_session.go:720`) | Routed to the session owning `threadId` when present, delivered to every session on the connection when absent |
| Process-wide reads (skills, account usage, models) | `codexapp` borrows any live session (`AnySession`, `SessionForBinary`) | Calls on a registry connection of the matching key, or the existing one-shot app-server when none is live |

An event a connection fans out to its sessions is not activity: it must
not bump `Liveness` (`RecordActivity` bumps on every event today), or one
busy thread would keep every idle thread on its process from being reaped.

Session-scoped, unchanged in meaning: interrupt, End Session, background
terminals, approvals, the `ao` credential, AO's MCP registrations,
`provider:session_live`, the prompt-changed banner, the idle reaper's
decision, account gating per thread, triage's session token.

## Codex pool policy

### Pool key

Threads share a connection when all of these match:

- the resolved binary identity (path, size, mtime, version);
- the provider home (`CODEX_HOME` as resolved by the account owner);
- the account credential generation and account id
  (`providerSessionMatchesAccount`);
- a digest of the process environment as `sessionProcessEnv` builds it,
  with every `AO_*` name removed (they move per thread, see
  [Identity](#identity));
- the spawn flags (`codexAppServerArgs` plus the unload delay).

The process runs in the provider probe working directory
(`providerProbeWorkDir`), not the first thread's workspace, which
`NewSession` uses today (`Dir: cfg.WorkDir`); every thread names its own
`cwd` on `thread/start` and `thread/resume`.

Stage 1 of the rollout adds the AO thread id to the key, which is exactly
today's process-per-thread behavior through the new code. The stage is a
key input, not a second implementation.

### When a new process is required

A thread goes to a new connection when its key no longer matches the
connection it would otherwise use: a binary upgrade, a provider-environment
setting change, an account switch, or a crash. Config file edits do not
need one ([thread loads read the latest config](#thread-loads-read-the-latest-config)).
Thread-level configuration (model, sandbox, approvals, reviewer, MCP
servers, disabled tools, base and developer instructions, shell
environment) is per thread and never needs one.

### Bounds

One active connection per key, plus draining connections for keys that
changed while a session on them was busy. With one account, one binary and
one provider environment, that is one process. Every session leaving a
connection, by close, crash or move, decrements its count; a connection
with no sessions closes after a grace of 5 minutes, so switching between
threads does not pay a process start (190 MiB, and about 18 s of startup
work at 4.5% mean CPU) each time. A Claude connection's grace is zero.

One `thread/start` or `thread/fork` in flight per connection (row 16)
serializes new threads on a process: `thread/start` to its response took
0.5 s for a process's first thread and 0.08 s for the second in the spike
fixture. Resumes name their thread and are not
serialized, so a boot that resumes many threads is not slowed.

### Idle unload

The idle reaper keeps its policy and its per-session decision
(`reapIdleSessions`). Reaping a Codex session closes the session, which
unloads the thread; it no longer kills a process. AO spawns its app-servers
with `-c thread_unload_delay_secs=0`: AO is the only client, every thread
it is not using is unsubscribed on purpose, and holding an unsubscribed
thread loaded only keeps its writer lock and MCP children alive.

### End Session

`Session.Close` on Codex, in order:

1. interrupt the thread's active turn and each live owned child's turn
   (`turn/interrupt`), and settle its pending approvals with
   `turnTransition` as `Close` does today;
2. `thread/backgroundTerminals/clean`, which kills the thread's PTYs
   ([background terminals](../references/codex-wire.md#background-terminals));
3. `thread/unsubscribe` for the root and every loaded child it owns;
4. wait for `thread/closed` for each, bounded at 10 s;
5. emit the session's own `disconnected`, deregister its routes, release
   its registry count.

A `thread/closed` that does not arrive in time is an error returned to the
caller and shown on the thread. The connection keeps the thread marked as
loaded, and a later start of that thread first unloads it there (another
process would be refused by the thread's writer lock): `thread/unsubscribe`
again and wait for `thread/closed` on the old connection, then start on the
connection of the thread's current key. That is the same connection unless
the old one is draining after an account or binary change, in which case
the start goes to the current key's connection. A dead connection settles
every such mark.

### Account switch

The per-thread gate in `ensureProviderAccountSelectionReadyForSendLocked`
keeps its rules: a thread whose session is on another generation moves at
its next send when it has no active turn and no running background work,
and is told the switch is pending otherwise. Moving is End Session on the
old connection, then a start on a connection of the current key. The old
connection drains: it takes no new sessions and closes when its last
session leaves, by a move, End Session or the idle reaper. Threads nobody
sends to stay on the old account until they are reaped, as today.

## Instructions delivery

Thread-level `developerInstructions` stays as it is: the composed value
(the cwd's configured text, then AO's guide and home block) is sent on
every start and resume, so a new thread and every compaction carry the
current text.

AO stores, per thread, a digest of the developer instructions the thread
last received, persisted on the thread's row so it survives an app
restart. A start whose composed value differs from that digest, on a
resumed or forked thread, sends one `thread/inject_items` developer
message before the session is handed to a caller. The message states
that it replaces the earlier developer instructions and carries the
whole new text. The digest is written when the injection succeeds; a
failed injection is a non-fatal error on the thread, like a failed
`config/read` today, and the next start tries again. A fresh
`thread/start` writes the digest of what it sent, since a new thread shows
its start value. An existing thread has no
digest, which counts as different: it gets one injection at its next
start. A fork child has no digest and always gets one injection at its
first start: a fork at `lastTurnId` cuts history, so the child can lack an
injection its parent received after the cut, and the parent's digest says
nothing about what the child's history holds.
`additionalContext` is not used: AO's guide and a home block
exceed its 1,000-token cap.

This keeps today's product rule, a change applies at the thread's next
session start, and makes it true on Codex. With sharing on it also means a
restart needs no process restart: the start is a cold resume on the same
connection.

## Identity

At stage 1 the `ao` credential stays in the spawn environment, as today:
the key holds one thread per process, so the environment is still that
thread's own. At stage 2, `AO_ENDPOINT`, `AO_TOKEN` and `AO_THREAD_ID`
leave the spawn environment (`sessionProcessEnv`, `app_session.go`) and
ride the session's
`thread/start` and `thread/resume` as
`config.shell_environment_policy.set`, which reaches that thread's shell
commands only and is not written under `CODEX_HOME`
([per-thread shell environment](../references/codex-wire.md#per-thread-shell-environment-and-mcp-processes-verified-01621)).
Claude keeps them in its process environment, since its connection is its
session. The credential's lifetime contract is unchanged: minted in
`mintAOCredential` while the start is assembled, registered when the
session enters the session map, revoked when it leaves
(`app_session_runtime.go`). A cold resume carries the current session's
token because every start sends the `config` bag.

`shell_environment_policy` covers shell commands only, so from stage 2 a
user's Codex hooks and stdio MCP servers from `config.toml` no longer see
`AO_*` ([decision 4](#decisions-for-the-user)).

The raw provider event log logs outbound frames too, and its Codex
redactor only rewrites `in` frames (`newCodexProviderEventLogRedactor`,
`raw_tool_calls.go`), so `AO_TOKEN` would be written to the log in every
`thread/start` and `thread/resume`. The redactor masks
`shell_environment_policy.set.AO_TOKEN` in outbound frames (row 17).

Test, at stage 2: a Go test that the Codex spawn environment has no `AO_*`
name and that `buildThreadParams` carries all three under
`shell_environment_policy.set`; a harness spec with two Codex threads on
one mock process where each thread's shell step reports its own
`AO_THREAD_ID`, and after End Session of one, its token is refused by the
`remote` route while the other's still works.

## Crash and restart

A process death ends every session on it. The connection's read loop
ends, and for each session in its route table, in no particular order:

- its pending requests fail with "session stopped before request completed";
- its approvals and user-input requests resolve as lost
  (`drainPendingApprovals`);
- it emits its own `error` (with the process exit meta) and
  `disconnected`, through `provider.EmitTeardownStatus` with its own
  `closing` flag, so a session that was already closing gets only
  `disconnected`.

From there each thread runs today's single-thread path: triage synthesizes
the truncated turn's completion, the session leaves the registry, its
queue and owed deliveries go back through `giveBackToOwners`, and its next
send or wake starts it again, on a new connection.

Test: a harness spec with three Codex threads on one mock process, one
streaming a turn, one waiting on an approval, one idle. The harness kills
the process. Each thread's timeline must match what the single-thread
crash assertions require: the streaming turn ends as interrupted with the
error row, the approval card shows lost, the idle thread shows the session
ended and nothing else; a send on each starts a new process (one new mock
process registration, not three) and completes.

## Assumptions to break

Each row: where the assumption lives, the invariant that replaces it, the
test that fails if the invariant is wrong. Line numbers are at the base of
this change.

| # | Assumption | Code | New invariant | Test |
|---|---|---|---|---|
| 1 | The `ao` credential is process environment | `app_session_runtime.go:248` `mintAOCredential`; `app_session.go:509` `sessionProcessEnv` | From stage 2, Codex carries it per thread in `shell_environment_policy.set`; the pool key never sees it | [Identity](#identity) |
| 2 | A start is a cold resume | `session_start.go:168` `startOrResumeThread`; `session_helpers.go:18` `buildThreadParams` | A start on a connection where the thread is loaded first unloads it, so every override applies | Mock enforces Codex's rule that a resume of a loaded, subscribed thread ignores overrides; Go test that a restart on one connection sends `thread/unsubscribe`, waits for `thread/closed`, then resumes; harness spec that changes the runtime mode of a thread sharing a process and reads the mock's reported thread config |
| 3 | A session owns the process, ids, pending map and read loop | `jsonrpc.go:23-130`, `:457-565`; `session_start.go:27-148` | `Conn` owns them; ids are unique per connection; closing a session never closes the pipe | Go test over a fake pipe: two sessions with concurrent requests, one closed mid-request, the other's responses still delivered |
| 4 | Every frame on the pipe belongs to one root | `child_routing.go:85`, `:152`; `session_notifications.go:43-80`; `server_requests.go:56-67` | The connection routes by `threadId` to the owning session, and `thread/started` by `sessionId`. Frames for a thread no session owns go to the session whose `thread/start` is in flight (one at a time per connection); otherwise into today's deferred-child quarantine, lifted from the session to the connection with its deadline and its bounds kept per owning root (128 events per child, 512 events and 4 MiB per root, `child_routing.go:14-16`; a frame held before any claim counts against the root its `thread/started` `sessionId` names, or against an unattributed pool of the same size), until a session's ownership items claim the child and receive its held frames in order. At the deadline a held server request is rejected and notifications are dropped with a drift warning, as today. A server request with no `threadId` (`attestation/generate`, `account/chatgptAuthTokens/refresh`) is the connection's to answer, never a root's | Go replay of interleaved fixtures from two roots with collaboration children, with and without child `thread/started`, asserting no frame or server request reaches a non-owner and none is dropped while a claim is pending; `codex-collab.spec.ts` and `agent-visibility-codex.spec.ts` with a second thread on the same process |
| 5 | Process-wide notifications are per session | `session_notifications.go:31-41`; `protocol.go:180-193`; `protocol_thread.go:219`; `app_session.go:720-737` | Per the [connection-scoped table](#what-becomes-connection-scoped): process facts once per connection, thread-shown notices to every session, optional `threadId` routed when present; fanned-out events never bump `Liveness` | Go tests: one `skills/changed` on a three-session connection calls the handler once; one `account/rateLimits/updated` yields one `EventRateLimits` per session and leaves every session's activity stamp unchanged; a `configWarning` writes one warning row per session; `mcpServer/oauthLogin/completed` with a `threadId` reaches only that session |
| 6 | A read loop's end is one thread's end | `jsonrpc.go:471-517`; `readloop.go:31` | Each session on a dead connection gets its own teardown | [Crash and restart](#crash-and-restart) |
| 7 | Close, End Session and the orphan reaper act on a PID | `session.go:854-920`; `app_session_runtime.go:174`; `app_orphan_reaper.go:74-107`; `app_session_reaper.go:138` | Session close is the [End Session](#end-session) sequence; the reaper watches connections | Go tests for the close sequence and the bounded wait; harness spec: End Session on one of two threads leaves the other's turn running, kills only the ended thread's terminals, and the mock's loaded list drops only that thread |
| 8 | An account switch reconnects one thread's own process | `app_provider_account_sessions.go:49-100`, `:102` | A session's generation is its connection's; a move is End Session plus a start on the current key; old connections drain | Harness spec beside `harness-provider-signin.spec.ts`: two threads on one process, one busy, switch account; the idle one moves at its next send (new mock process), the busy one keeps the old process until its turn ends, then the old process exits |
| 9 | The mock is one thread per process | `cmd/ao-mockprovider/main.go:89-130`; `scenario_source.go:30`; `internal/harness/control/control.go` `Registration`; `internal/harnessrpc/mock.go`; `seed.go:619-640` | See [Mock provider and harness](#mock-provider-and-harness) | Mock binary tests in `codex_bin_test.go` with two threads on one process |
| 10 | Process telemetry names the spawning thread | `process.go:428`, `:503-514` | Process lines name the connection; frames are logged under the routed thread | Go test of the event logger with two sessions |
| 11 | A dev server descends from its thread's process | `app_preview.go:406` `devServerOwners`; `sessionruntime/manager.go:447` | `devServerOwners` reads `Session.OwnedProcesses` with no provider branch: a Codex session returns its background terminals' `osPid` from `thread/backgroundTerminals/list`, a Claude session its connection's process | Go test: two Codex sessions on one connection, each with a terminal, attribute a listener to the right thread; a Claude session still attributes by its process |
| 12 | Process-wide reads may borrow any thread's session | `app_codex_bindings.go:189-212`; `codexapp/service.go:35-36`, `:139`, `:214`; `manager.go:371-399` | They use a registry connection of the matching key or a one-shot | Go test that no codexapp read reaches a session |
| 13 | A forked thread is released when its process exits | `session_fork.go`; mock `codex_writer_lock.go` | Every thread a connection loads is owned by one session or unsubscribed at once | `codex-revert.spec.ts` and `fork-midturn.spec.ts` with sharing on; mock refuses a resume while it still holds the fork loaded |
| 14 | A resume or a fork delivers developer instructions | `session_start.go:168`; `session_fork.go` `forkThreadWith`; `developer_instructions.go` | A changed composed value is injected once per change, on resumed and forked threads, against a digest persisted on the thread row | [Instructions delivery](#instructions-delivery): Go tests that a changed value sends one `thread/inject_items` and an unchanged one none, that a thread with no digest gets one injection, that a fork child injects once at its first start, and that the digest survives an app restart; mock reports injected items; `standing-agent-home-contents.spec.ts` asserts the injection after an `instructions.md` rewrite on Codex |
| 15 | The binary banner is a session fact | `app_provider_binary_watch.go` | It is a connection fact projected to sessions | Existing watcher tests with two sessions on one connection |
| 16 | Starts on a process never overlap | `NewSession`, `session_start.go:27` | At most one `thread/start` or `thread/fork` in flight per connection | Go test with concurrent opens on one connection |
| 17 | Outbound frames carry no secret | `raw_tool_calls.go:357` `newCodexProviderEventLogRedactor` (`in` only) | Outbound `thread/start`, `thread/resume` and `thread/fork` are logged with `AO_TOKEN` masked | Go test that a logged outbound start frame contains no token |

## Mock provider and harness

The mock already speaks the app-server protocol; it becomes a real
multi-thread app-server:

- One engine per `thread/start` or `thread/resume`, keyed by thread id,
  created from the scenario the harness assigns to that thread. The
  scenario is resolved at thread start, matched on the `cwd` from the
  thread params, so `HarnessSetScenario`'s `cwd` scoping keeps working.
- `thread/start` mints a unique thread id; `thread/resume` and the engine
  echo the id the request names. Today every Codex mock thread is the
  constant `mock-codex-thread` (`cmd/ao-mockprovider/main.go:104`), which
  two threads on one process would share. Code that names the constant
  changes with it: `e2e/tests/midturn-gated-helpers.ts`,
  `workflows-helpers.ts`, `workflows-agent-tools-helpers.ts`,
  `internal/harness/scenario/scenario.go`, the library scenario
  `codex-steer-while-running.json`, `codex_writer_lock.go` and the mock's
  binary tests. A scenario that pins its own id keeps it, and the mock
  refuses a second thread with that id on one process.
- A spawned child's first frame is a `thread/started` carrying the root's
  `sessionId` and `parentThreadId`, as the scenario libraries' child frames
  are rewritten to send.
- The control channel registers the process once and each engine on
  thread start. `mockId` names an engine, so every spec that commands a
  mock by id keeps working. `registration.pid` stays the process PID:
  `codex-steer.spec.ts` stops a mock with `SIGSTOP` on it, and
  `standing-agent-homes.spec.ts` reads `sessionConfig` per registration,
  which becomes per engine.
- `exit` stays process-scoped: it kills the process and so every engine on
  it, which is what a crash is. Specs that use `exit` to end one thread's
  session (`standing-agent-wakes.spec.ts`, `wake-rows.spec.ts`) put that
  thread under its own key through a harness-only seam that adds the
  thread id to the key, the stage-1 input, so their meaning is unchanged.
  The seam exists only in isolated harness boots, like the other
  `ConfigureIsolation` inputs; the product has no such option. The mock
  invents no single-thread death the real app-server does not have.
- The mock tracks loaded and subscribed threads per process and applies
  Codex's resume rule (row 2), the writer lock across processes (row 13),
  `thread/unsubscribe` and `thread/closed` at the configured delay, and
  `shell_environment_policy.set` to its shell steps.
- `HarnessReset` stops every session, then closes every registry
  connection and waits for its exit, so no process survives into the next
  test.

## Verification and rollout

What must pass unchanged, at both stages:

- every Playwright spec that mentions Codex (68 at the base of this
  change: `grep -li codex e2e/tests/*.spec.ts`),
  with particular weight on `codex-steer`, `codex-collab`, `codex-revert`,
  `codex-terminal-polls`, `agent-visibility-codex`, `fork-midturn`,
  `session-replace-midturn`, `stale-session-frames`,
  `request-requeue-on-session-end`, `standing-agent-wakes`,
  `standing-agent-home-contents`, `skills-settings`, `mcp-menu` and
  `harness-provider-signin`;
- the Go tests of every package the rows change: `internal/provider/codex`
  (76 test files: `ls internal/provider/codex/*_test.go | wc -l`),
  `internal/app` (148 test files mention Codex:
  `grep -li codex internal/app/*_test.go | wc -l`), `internal/provider`
  (`readloop.go`, `process.go`), `internal/sessionruntime`,
  `internal/codexapp`, `internal/providerlifecycleapp`, `internal/triage`
  (the crash's turn-completion synthesis), `internal/harnessrpc`
  (`mock.go`, `seed.go`), `internal/harness/...` and `cmd/ao-mockprovider`
  (6 Codex test files: `ls cmd/ao-mockprovider/codex*_test.go | wc -l`).

Process-identity dependents, checked by name at stage 1: `codex-steer`
(signals `registration.pid`) and `standing-agent-homes` (reads
`sessionConfig` per registration).

New tests: the ones named in the [assumptions table](#assumptions-to-break),
the identity spec, the crash spec and the account-switch spec.

Stages:

1. **Connection and session split, key per thread.** The interface split,
   the registry, the instructions fix, the multi-thread mock with minted
   thread ids and child `thread/started`. The pool key includes the AO
   thread id, so every thread still gets its own process, and `AO_*` stays
   in its spawn environment. Every existing spec passes, with only the
   helper and scenario updates the minted ids require; the new unit tests
   pass. This stage changes no visible behavior except the instructions
   defect.
2. **Sharing on.** The thread id leaves the key and `AO_*` moves to
   `shell_environment_policy.set`. The whole e2e suite runs
   with sharing; the crash, identity, End Session and account specs run
   here. Specs that start more than one Codex thread exercise shared
   processes, on the mock changes stage 1 made (unique ids, child
   `thread/started`). Gate: before this stage starts, a spike on 0.162.1
   establishes whether a real child's `thread/started` precedes its other
   frames, and the replay test of row 4 runs against that capture.

The instructions fix does not need the split and can ship before stage 1
on its own.

## Cost and benefit

Measured idle cost: 190 MiB per process, 75 MiB for its first loaded
thread, 5 MiB per further thread
([process cost](../references/codex.md#app-server-process-cost-measured-01621)).
Today each thread pays 265 MiB. Shared: 265 + 5 x (N - 1).

| Live Codex threads | Today (MiB) | Shared (MiB) | Saved (MiB) |
|---|---|---|---|
| 1 | 265 | 265 | 0 |
| 5 | 1,325 | 285 | 1,040 |
| 14 | 3,710 | 330 | 3,380 |
| 40 | 10,600 | 460 | 10,140 |

Limits of these numbers: they are idle, on one Linux machine; one streaming
turn measured no growth over idle, but many concurrent turns on one process
are unmeasured. Stdio MCP children are per thread either way. N counts
live sessions, which the idle reaper already bounds. A second key (another
binary, a draining account) adds 265 MiB each.

Cost: the split touches every Codex session path and the mock, and a crash
now ends every thread on the process at once.

## Left out

- Moving a thread between processes while it runs.
- A process cap, LRU eviction or a per-process thread cap.
- Codex's external auth mode (`chatgptAuthTokens`) or custom model
  providers to fake per-thread accounts.
- A setting or flag that keeps the per-thread path alive after stage 2;
  stage 1 is a key input. The harness-only isolation seam in
  [Mock provider and harness](#mock-provider-and-harness) is a test input,
  not a product path.
- A multiplexed mode for Claude, or a general provider pool framework
  beyond acquire, release and drain.

## Decisions for the user

1. **Blast radius.** One process per key with no thread cap means a Codex
   crash ends every live Codex thread at once. Pick: accept it, because each
   thread recovers on its next send exactly as it does today, and a cap
   would bring back part of the memory cost to shrink a failure that
   already recovers.
2. **When an instructions change applies.** At the next session start, as
   today's rule says, or at the thread's next turn (possible with
   injection). Pick: next session start, so the prompt-changed banner and
   its Restart keep one meaning across providers.
3. **Order.** Ship the instructions fix now, before the sharing work.
   Pick: yes; it is small, independent, and standing agents on Codex are
   wrong without it.
4. **`AO_*` outside shell commands at stage 2.** Per-thread environment
   reaches shell commands only, so a user's Codex hooks and stdio MCP
   servers from `config.toml` lose `AO_*`, and `agent-overflow remote` run
   from one of them reports that it is not inside a session, the legible
   failure a session without a credential already gives. Mitigation: AO
   sets `mcp_servers.<name>.env` per thread in the same `config` bag after
   reading the server list with `config/read`; hooks have no per-thread
   environment and stay without it. Pick: ship the MCP mitigation with
   stage 2 and accept the loss for hooks; the only way to keep it for
   hooks is a process per thread, which is what stage 2 removes.

Decided (veto if wrong): the pool lives in `sessionruntime`; the key is as
listed; a 5-minute empty-connection grace; `thread_unload_delay_secs=0` on
AO's processes; the process cwd is the probe working directory; account
moves stay lazy at the next send; one thread start in flight per
connection; the deferred-child quarantine moves to the connection;
`thread/inject_items` as the change channel with a digest on the thread
row.

## Not established

- Whether unsubscribing a parent thread unloads or stops its spawned
  children by itself; the End Session sequence handles children explicitly
  until a spike shows otherwise.
- Whether a real child thread's `thread/started` always precedes its
  other frames. Row 4 does not depend on it, since the connection
  quarantine holds frames until a claim; the spike is the stage-2 gate.
- Memory and CPU of many concurrent turns on one process.
- Whether `thread/inject_items` text is retained across compaction when
  `retain_client_developer_messages` is on; the design does not depend on
  it.
