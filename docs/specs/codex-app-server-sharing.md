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

Rejected: collaboration-mode `developer_instructions`, because the model
catalog's own collaboration-mode text wins over a client value for the
gpt-6 family (`world_state/collaboration_mode.rs`, `models.json`).

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
`-c thread_unload_delay_secs=0` (spike) and 2.0 s at `2`, against 60 s at
the default ([unload](../references/codex-wire.md#thread-unload-and-cold-resume-verified-01621)).
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
that names it. A `thread/resume` names the thread in its request.

## Architecture

### Connection and session

A **connection** is one provider process and everything about it: spawn,
framing, request ids, pending requests, the read loop, notifications about
the process rather than a thread, lifetime, exit and crash. A **session**
is one conversation on a connection: its provider thread id, its
configuration, its turns, approvals, side channels and teardown.

Codex multiplexes sessions on a connection. Claude, `claude-tui`, and any
provider without a multiplexed mode host exactly one session per
connection, and their connection ends with that session. That is the only
difference between providers in this model; nothing else branches on it.

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
	// OwnedProcesses are the OS processes this conversation started that
	// outlive a tool call (background terminals), for attribution.
	OwnedProcesses() []OwnedProcess
}
```

`PID()` leaves `Session`: every caller that wanted a process asks the
connection. `OwnedProcesses` replaces the ancestry assumption described
under [dev-server attribution](#assumptions-to-break). For Claude, `Close`
closes the connection, and the connection's `Done` is the read loop's end;
nothing else changes in `internal/provider/claude` or `claudetui`.

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
| `skills/changed`, `account/updated`, `account/rateLimits/updated`, `mcpServer/oauthLogin/completed` | One handler per session | One handler per connection |
| Process-wide reads (skills, account usage, models) | `codexapp` borrows any live session (`AnySession`, `SessionForBinary`) | Calls on a registry connection of the matching key, or the existing one-shot app-server when none is live |

Session-scoped, unchanged in meaning: interrupt, End Session, background
terminals, approvals, the `ao` credential, AO's MCP registrations,
`provider:session_live`, the prompt-changed banner, the idle reaper's
decision, account gating per thread, triage's session token.

## Codex pool policy

### Pool key

Threads share a connection when all of these match:

- the resolved binary identity (path, size, mtime, version);
- the provider home (`CODEX_HOME` as resolved by the account owner);
- the account credential generation and account id (`providerSessionMatchesAccount`);
- a digest of the process environment as `sessionProcessEnv` builds it,
  with every `AO_*` name removed (they move per thread, see
  [Identity](#identity));
- the spawn flags (`codexAppServerArgs` plus the unload delay).

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
threads does not pay a process start (190 MiB and about 18 s of startup
CPU) each time. A Claude connection's grace is zero.

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
loaded, so a later start of the same thread runs on that connection (a
second process would be refused by the thread's writer lock) and unloads it
first. A dead connection settles every such mark.

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
last received. A start whose composed value differs from that digest, on
a resumed thread, sends one `thread/inject_items` developer message before
the session is handed to a caller. The message states that it replaces the
earlier developer instructions and carries the whole new text. The digest
is written when the injection succeeds; a failed injection is a non-fatal
error on the thread, like a failed `config/read` today, and the next start
tries again. `additionalContext` is not used: AO's guide and a home block
exceed its 1,000-token cap.

This keeps today's product rule, a change applies at the thread's next
session start, and makes it true on Codex. With sharing on it also means a
restart needs no process restart: the start is a cold resume on the same
connection.

## Identity

`AO_ENDPOINT`, `AO_TOKEN` and `AO_THREAD_ID` leave the spawn environment
(`sessionProcessEnv`, `app_session.go`) and ride the session's
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

Test: a Go test that the Codex spawn environment has no `AO_*` name and
that `buildThreadParams` carries all three under
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
| 1 | The `ao` credential is process environment | `app_session_runtime.go:248` `mintAOCredential`; `app_session.go:509` `sessionProcessEnv` | Codex carries it per thread in `shell_environment_policy.set`; the pool key never sees it | [Identity](#identity) |
| 2 | A start is a cold resume | `session_start.go:168` `startOrResumeThread`; `session_helpers.go:18` `buildThreadParams` | A start on a connection where the thread is loaded first unloads it, so every override applies | Mock enforces Codex's rule that a resume of a loaded, subscribed thread ignores overrides; Go test that a restart on one connection sends `thread/unsubscribe`, waits for `thread/closed`, then resumes; harness spec that changes the runtime mode of a thread sharing a process and reads the mock's reported thread config |
| 3 | A session owns the process, ids, pending map and read loop | `jsonrpc.go:23-130`, `:457-565`; `session_start.go:27-148` | `Conn` owns them; ids are unique per connection; closing a session never closes the pipe | Go test over a fake pipe: two sessions with concurrent requests, one closed mid-request, the other's responses still delivered |
| 4 | Every frame on the pipe belongs to one root | `child_routing.go:85`, `:152`; `session_notifications.go:43-80`; `server_requests.go:56-67` | The connection routes by `threadId` to the owning session, `thread/started` by `sessionId`; unknown-thread frames go to the session whose `thread/start` is in flight (one at a time per connection), else they are a drift error; a server request with no `threadId` (`attestation/generate`, `account/chatgptAuthTokens/refresh`) is the connection's to answer, never a root's | Go replay of interleaved fixtures from two roots with collaboration children, asserting no frame or server request reaches a non-owner; `codex-collab.spec.ts` with a second thread on the same process |
| 5 | Process-wide notifications are per session | `session_notifications.go:31-41`; `app_session.go:720-737` | One handler call per connection | Go test: one `skills/changed` on a three-session connection calls the handler once |
| 6 | A read loop's end is one thread's end | `jsonrpc.go:471-517`; `readloop.go:31` | Each session on a dead connection gets its own teardown | [Crash and restart](#crash-and-restart) |
| 7 | Close, End Session and the orphan reaper act on a PID | `session.go:854-920`; `app_session_runtime.go:174`; `app_orphan_reaper.go:74-107`; `app_session_reaper.go:138` | Session close is the [End Session](#end-session) sequence; the reaper watches connections | Go tests for the close sequence and the bounded wait; harness spec: End Session on one of two threads leaves the other's turn running, kills only the ended thread's terminals, and the mock's loaded list drops only that thread |
| 8 | An account switch reconnects one thread's own process | `app_provider_account_sessions.go:49-100`, `:102` | A session's generation is its connection's; a move is End Session plus a start on the current key; old connections drain | Harness spec beside `harness-provider-signin.spec.ts`: two threads on one process, one busy, switch account; the idle one moves at its next send (new mock process), the busy one keeps the old process until its turn ends, then the old process exits |
| 9 | The mock is one thread per process | `cmd/ao-mockprovider/main.go:89-130`; `scenario_source.go:30`; `internal/harness/control/control.go` `Registration`; `internal/harnessrpc/mock.go`; `seed.go:619-640` | See [Mock provider and harness](#mock-provider-and-harness) | Mock binary tests in `codex_bin_test.go` with two threads on one process |
| 10 | Process telemetry names the spawning thread | `process.go:428`, `:503-514` | Process lines name the connection; frames are logged under the routed thread | Go test of the event logger with two sessions |
| 11 | A dev server descends from its thread's process | `app_preview.go:406` `devServerOwners`; `sessionruntime/manager.go:447` | Codex owner roots are the session's background terminals' `osPid`, from `thread/backgroundTerminals/list`, not the connection PID | Go test: two sessions on one connection, each with a terminal, attribute a listener to the right thread |
| 12 | Process-wide reads may borrow any thread's session | `app_codex_bindings.go:189-212`; `codexapp/service.go:35-36`, `:139`, `:214`; `manager.go:371-399` | They use a registry connection of the matching key or a one-shot | Go test that no codexapp read reaches a session |
| 13 | A forked thread is released when its process exits | `session_fork.go`; mock `codex_writer_lock.go` | Every thread a connection loads is owned by one session or unsubscribed at once | `codex-revert.spec.ts` and `fork-midturn.spec.ts` with sharing on; mock refuses a resume while it still holds the fork loaded |
| 14 | A resume delivers developer instructions | `session_start.go:168`; `developer_instructions.go` | A changed composed value is injected once per change | [Instructions delivery](#instructions-delivery): Go test that a changed value sends one `thread/inject_items` and an unchanged one none; mock reports injected items; `standing-agent-home-contents.spec.ts` asserts the injection after an `instructions.md` rewrite on Codex |
| 15 | The binary banner is a session fact | `app_provider_binary_watch.go` | It is a connection fact projected to sessions | Existing watcher tests with two sessions on one connection |
| 16 | Starts on a process never overlap | `NewSession`, `session_start.go:27` | At most one `thread/start` or `thread/fork` in flight per connection | Go test with concurrent opens on one connection |

## Mock provider and harness

The mock already speaks the app-server protocol; it becomes a real
multi-thread app-server:

- One engine per `thread/start` or `thread/resume`, keyed by thread id,
  created from the scenario the harness assigns to that thread. The
  scenario is resolved at thread start, matched on the `cwd` from the
  thread params, so `HarnessSetScenario`'s `cwd` scoping keeps working.
- The control channel registers the process once and each engine on
  thread start. `mockId` names an engine, so every spec that commands a
  mock by id keeps working; a registration also reports its `processId`.
- `exit` stays process-scoped: it kills the process and so every engine on
  it, which is what a crash is. Specs that use `exit` to end one thread's
  session (`standing-agent-wakes.spec.ts`, `wake-rows.spec.ts`) put that
  thread under its own key with a harness isolation option that adds the
  thread id to the key, the stage-1 input, so their meaning is unchanged.
  The mock invents no single-thread death the real app-server does not
  have.
- The mock tracks loaded and subscribed threads per process and applies
  Codex's resume rule (row 2), the writer lock across processes (row 13),
  `thread/unsubscribe` and `thread/closed` at the configured delay, and
  `shell_environment_policy.set` to its shell steps.
- `HarnessReset` stops every session, then closes every registry
  connection and waits for its exit, so no process survives into the next
  test.

## Verification and rollout

What must pass unchanged, at both stages:

- every Playwright spec that runs a Codex thread (68 at the base of this
  change: `grep -lE "provider: ?'codex'|'codex'" e2e/tests/*.spec.ts`),
  with particular weight on `codex-steer`, `codex-collab`, `codex-revert`,
  `codex-terminal-polls`, `agent-visibility-codex`, `fork-midturn`,
  `session-replace-midturn`, `stale-session-frames`,
  `request-requeue-on-session-end`, `standing-agent-wakes`,
  `standing-agent-home-contents`, `skills-settings`, `mcp-menu` and
  `harness-provider-signin`;
- the Go tests in `internal/provider/codex` (76 files), the app tests that
  drive Codex sessions (37 files), `cmd/ao-mockprovider` and
  `internal/harness/...`.

New tests: the ones named in the [assumptions table](#assumptions-to-break),
the identity spec, the crash spec and the account-switch spec.

Stages:

1. **Connection and session split, key per thread.** The interface split,
   the registry, the instructions fix, the `AO_*` move, the multi-thread
   mock. The pool key includes the AO thread id, so every thread still
   gets its own process. Every existing test passes unchanged; the new unit
   tests pass. This stage changes no visible behavior except the
   instructions defect.
2. **Sharing on.** The thread id leaves the key. The whole e2e suite runs
   with sharing; the crash, identity, End Session and account specs run
   here. Specs that start more than one Codex thread exercise shared
   processes without new fixtures.

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
- A connection-level quarantine beyond the start window in row 4.
- Codex's external auth mode (`chatgptAuthTokens`) or custom model
  providers to fake per-thread accounts.
- A setting or flag that keeps the per-thread path alive after stage 2;
  stage 1 is a key input.
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

Decided (veto if wrong): the pool lives in `sessionruntime`; the key is as
listed; a 5-minute empty-connection grace; `thread_unload_delay_secs=0` on
AO's processes; account moves stay lazy at the next send; one thread start
in flight per connection; `thread/inject_items` as the change channel.

## Not established

- Whether unsubscribing a parent thread unloads or stops its spawned
  children by itself; the End Session sequence handles children explicitly
  until a spike shows otherwise.
- Whether a child thread's `thread/started` always precedes its other
  frames; row 4's routing needs it, and a replayed collaboration fixture on
  0.162.1 is the check before stage 2.
- Memory and CPU of many concurrent turns on one process.
- Whether `thread/inject_items` text is retained across compaction when
  `retain_client_developer_messages` is on; the design does not depend on
  it.
