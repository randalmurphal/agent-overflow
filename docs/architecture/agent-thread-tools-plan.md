# Agent thread tools implementation plan

Status: implemented 2026-09-19. Implements
[`agent-thread-tools.md`](../specs/agent-thread-tools.md). The spec is
the contract; this file is the build record: the data model, the code
ownership, the edge cases, and the reasons behind each choice.

Every path and symbol named below names shipped code.

## Amendments to the spec found while planning

All folded into the spec on 2026-09-19; kept here as the record of why.

1. **Item addressing is thread-scoped.** Item ids are unique only inside
   a thread (`items` PK is `(thread_id, id)`), so `thread_item` and the
   `around` and `since` inputs of `thread_show` take `thread_id` beside
   the item id. A `thread_search` hit already carries both.
2. **Attribution key.** `items.meta.origin` is already a string
   (`"external-queue"`, `"peer-session"`) written by
   `triage.persistExternalOriginMessage` and rendered by
   `UserMessage.svelte`'s `originBadge`, whose comment anticipates a
   third value. Agent-written rows use `meta.origin = "agent-thread"`
   and carry the details in `meta.originThread = {computerId,
   computerName, threadId, title}`. Same chip pipeline, no key collision.
3. **Tools take the shape of the user's setup.** A computer with no
   paired computers is the common case, and nothing about other
   computers should show up there. The tool list and the server
   instructions are computed from the current pairing set at each
   `tools/list` and `initialize`: with no paired computer, `computers`
   and `computer_id` are absent from every schema, result rows carry no
   `computer_id` or `computer` field, and the "Other computers"
   paragraph is absent from the instructions. Pairing a computer
   refreshes running sessions through the worker `ao-remote-tools`
   already uses for the same purpose (`startRemoteMCPRefresh`, woken by
   `signalRemotePeers`, which `SetAttachedBackends` and `awaitAttachment`
   already call). No tool is pairing-only, so no tool appears or
   disappears; only the computer parameters and fields do.
4. **Peer methods.** Five instead of two, each typed and scope-annotated
   so the generated method table carries the right floor:
   `ThreadToolResolve` and `ThreadToolQuery` (read tools, scope
   `threads:read`), `ThreadToolCall` (spawn, send, ask, cancel, scope
   `terminal:operate`), `ThreadToolRequestStatus` (settlement poll and
   acknowledgement, `threads:read`), `ThreadToolExportChunk` (chunked
   `to_file` transfer, `threads:read`). Capability string
   `thread-tools.v1`.
5. **Two request tables, one for each side of a request.** The spec
   names a source row and a destination row. A local request writes
   both, so the code that detects settlement (destination side) and the
   code that wakes the caller (source side) are each written once and
   run identically for local and remote targets. Names:
   `thread_requests` (what this computer's threads asked for) and
   `thread_request_receipts` (what this computer's threads were asked).
6. **Wake dismissal.** A settlement collected inline by a parked wait or
   `thread_status` marks the request delivered before any wake is
   queued, so no wake is created. A wake that already sits in the
   thread's durable message queue is not retracted; the `thread_status`
   reply says the same answer is also arriving as a message. Reaching
   into the queue to delete a row would need a new store path with its
   own draft-merge hazards for a case the agent has already handled.
7. **Boot settles every open receipt as `interrupted`**, not only
   scratch ones. A queued `thread_send` that had not reached the
   provider before a restart is restored into the composer draft by
   `restoreDurableFlushQueueAtBoot`, never sent, so it can never settle
   on its own. Honest and uniform: the caller learns the computer
   restarted and decides whether to send again.
8. **Every spawn and send names its sender.** The footer's line naming
   the sender thread (and its computer when it is another one) is on
   every `thread_spawn` first message and every `thread_send`, not only
   the waiting or notifying ones, so the receiving agent can always
   `thread_show` the sender's thread and `thread_send` back. Only the
   reply-token line depends on the request waiting or notifying. The
   instructions say so: "a message from another thread names it; read
   it with `thread_show` and answer it with `thread_send` if it is not
   waiting."
9. **Provider and model validation is the same everywhere.** A
   `thread_spawn` whose provider or model this computer does not offer
   is refused with the list it does, locally as well as on another
   computer, and a provider override without a model uses that
   provider's default model. Cross-model work (Claude spawning Codex
   and back) is the common case and the caller cannot know the other
   provider's model ids.
10. **A tenth tool, `thread_options`, so nothing is guessed.** It
   returns what a spawn can choose from, rendered from the catalogs the
   app already keeps, never from a hand-written list: per computer, the
   providers, each provider's models from `GetModelsForProvider` (slug,
   name, reasoning efforts with the default marked, context windows,
   catalog provenance), the runtime modes from
   `provider.AllRuntimeModes` with a one-line meaning each, and the
   projects with their worktrees. A provider added later (a local model
   server, say) appears the moment it registers a catalog. The tool
   takes an optional `computer_id`; the caller's own row comes first
   and states the caller's current provider, model, effort and mode as
   the defaults. A spawn refusal still lists the valid choices for the
   one thing that was wrong, so a mistake costs one call, but the
   instructions send the agent to `thread_options` before it needs
   something other than its own setup. The `thread_spawn` schema
   descriptions state the live defaults ("default: your provider,
   currently claude") so the common case needs no discovery call at
   all.
11. **The instructions forbid deliberating over modes.** Agents given
   a choice tend to argue with themselves about it. The instructions
   carry this paragraph verbatim and the tool descriptions repeat none
   of it:

   > Defaults. A spawn inherits your provider, model, effort, and
   > runtime mode. Keep them unless the task needs something else, such
   > as a different provider or model for a second opinion. `read-only`
   > is the exception, not the safe choice: it blocks every shell
   > command on Claude Code, so no git, build, test or grep, and
   > sandboxes commands on Codex. Choose it only when the work needs
   > nothing but reading files; a review that runs tests, a thread that
   > commits or a thread that searches with a command needs the
   > inherited mode. A read-only thread that turns out to need a command
   > fails at it and tells you so. `thread_ask` is always read-only and
   > needs no choice. `thread_options` lists what a computer offers when
   > you need something you do not have.

12. **The footers and wake texts are fixed templates.** Both ends read
   them, so they say where a message came from and what to do, in one
   short block, and nothing else. Maintained in
   `internal/threadtools/footer.go` with a test per template.

   A spawn's first message or a send, when the sender waits or asked to
   be notified:

   ```
   ---
   From thread "<title>" (<thread id>, on <computer>).
   It is waiting for your answer. When you are done, call thread_reply
   with token <token>, once. The sender sees only your reply text, so
   make it self-contained. To read the sender's thread, use thread_show
   with its id.
   ```

   The same when the sender is not waiting:

   ```
   ---
   From thread "<title>" (<thread id>, on <computer>).
   It is not waiting. To answer it, use thread_send with its id.
   ```

   The wake in the caller's thread, one of:

   ```
   Reply from "<title>" (<thread id>, on <computer>, answered 3h ago):
   <text>
   ```
   ```
   "<title>" (<thread id>) finished its turn without calling
   thread_reply. Its final message:
   <text>
   ```
   ```
   "<title>" (<thread id>) errored: <error>
   ```
   ```
   "<title>" (<thread id>) was cancelled | was interrupted by a restart
   of <computer> | did not answer within a day.
   ```

   `on <computer>` and `answered <age> ago` appear only when they apply.
   A body over 24 KB ends with `[preview; thread_status <token> for the
   whole answer]`. A late reply after a finished wake is a second `Reply
   from` block. Every incoming footer also carries one line, `The user's
   latest message in that thread: "<first 300 characters>"`, so the
   receiver knows what the person asked for; it is read from the
   sender's last `user_text` row without `meta.origin` at send time and
   is absent when there is none.
13. **Adopted from the Codex app's thread tools** (`codex_app`:
   `list_threads`, `read_thread`, `wait_threads`,
   `send_message_to_thread`, `create_thread`, `fork_thread`,
   `set_thread_title`, `set_thread_archived`, `set_thread_pinned`, the
   sidebar-section tools and `automation_update`):
   - waits return `blocked` when the target needs a person, request
     left open, no wake owed;
   - `thread_status` waits on up to eight tokens and returns on the
     first settlement or block, refusing duplicates and the caller's
     own thread;
   - every read tool's description frames thread content as data; the
     footer names the agent author and quotes the user's latest ask;
   - the instructions draw the line between provider subagents and
     `thread_spawn`;
   - `thread_spawn` takes `from_thread` (visible tail fork, then the
     prompt);
   - listing rows carry group, pin tier, archived and unread;
   - `thread_update` and `thread_group` organize through the sidebar's
     bindings, thirteen tools in all;
   - `thread_remind` is a clock-settled request.
   Not copied: Codex's 1,000-byte prompt and 999-byte result caps, its
   pull-only answers, sidebar reorder tools, `handoff_thread`.
14. **Codex review (gpt-6-astra, high effort, 2026-09-19).** Adopted:
   whole answers stored in the request record (the 24 KB cap was a cap
   on the answer, and its pointer led to a deleted scratch thread);
   receipts kept for the reply window instead of deleted at collection
   (late replies and retry safety need them); settlement bound to the
   turn that consumed the request's message; `thread_cancel` by token
   removes a queued message or interrupts only the request's own turn;
   every unsettled positive wait arms `notify`; `notify` defined once;
   wakes and status rows carry the token and answer kind; a prefix
   always fans out and a peer timeout is `incomplete`, not `not found`;
   a request stays with the accepting computer on a move; a lost
   computer can be forgotten by local abandon; the responder enable is
   an effective per-thread rule because the transport requires both its
   flags; `thread_update` validates the whole patch before touching a
   thread; opaque `cursor` paging; `thread_status` on `thread_ids` and
   `after_revision`; thinking opt-in; independent waiters per wait;
   self-send refused at admission; three documented contradictions
   fixed. Kept against its advice, by the owner's ruling: the no-pairing
   schema shape (pairing is rare and one-way, and an agent must not see
   parameters that cannot work) and all tools inside read-only scratch
   threads (read-only is the responder's own runtime, a light safety
   option, not an isolation boundary).

15. **The tools never prompt, in any runtime mode.** Planned as a
    read-only-only allowlist; the spikes showed both providers deny
    MCP calls in read-only and prompt per call elsewhere, and Codex
    cannot follow a live mode change with a thread-start MCP entry.
    Every session admits the server (Claude `--allowedTools
    "mcp__ao-thread-tools__*"`, Codex `default_tools_approval_mode =
    "approve"` on the entry). The spec's "every other runtime mode
    keeps its normal behavior" line is replaced. Ruled 2026-09-19:
    admit in every mode.

## Verified facts the design rests on

- FTS5 is compiled into `modernc.org/sqlite` v1.56.0 (SQLite 3.53.3):
  `unicode61 remove_diacritics 2`, `trigram`, `snippet()` and
  `contentless_delete` all work. Probed on the plan date; the probe is
  not kept.
- `internal/threadmcp` computes the tool list per `tools/list` through
  the `tools func(T) []map[string]any` callback and returns
  `instructions` only from `initialize`. Claude re-initializes on
  `ReconnectMcpServer`; Codex on `ApplyManagedServerEnabled(..., true)`.
  Both are what `startRemoteMCPRefresh` calls today.
- The browser-tools switch is live: the server is in every session's
  configuration and `ApplyManagedServerEnabled` flips it inside the
  running provider (Claude `mcp_toggle` control request, Codex MCP
  reload). The composer's per-thread MCP toggle uses the same call.
- `registerQueueItem` with `injectedQueueOptions{preserveDraft: true,
  persist: ...}` hands an injected message to the durable queue in one
  transaction; `QueueRemoteCompletion` is the precedent and
  `remoteCompletionSendID` is the idempotency key.
- Own-device peer sessions carry every scope (`PairingAccess("full")`);
  `remoteCommandOwner` yields the authenticated device id, or `"local"`
  for an in-process call; `requireScope` passes with no session.
- `CallAgentPeer` gates by method allowlist and the agent-computer
  opt-in; `openRPC(ctx, capability)` refuses a peer whose hello lacks
  the capability with `errUnsupportedPeerOperation`, translated to
  `remote_unsupported`.
- The remote watch poller: 2 s ticker, 32 due rows, four concurrent,
  +5 s normal, +30 s after an error, +5 s while a wait is parked,
  resumed at boot from the table. `beginRemoteWait` ends after the reply
  is written so the poller cannot double-deliver.
- `ForkThread` locks the source, forks at the tail mid-turn, settles the
  fork as interrupted, copies `Mode` and `RuntimeMode`, has no override
  options. Title generation is skipped when the title is not
  "New Thread".
- `threads.mode` has a CHECK constraint (last rebuilt in v72);
  `threadmode.hiddenModes` holds the workflow modes; six store queries
  use `hiddenThreadModesClause`. The chain stood at v100 before this
  work.
- `DeleteThread` runs paced in 500-row chunks; `threadapp.DeleteTree`
  calls `StopRemoteWork` before and after the session stop.
- Move keeps the thread id and records the new owner
  (`thread_transfers.peer_backend_id`, surfaced by
  `CheckThreadTransferAccess` as `ThreadTransferError{BackendID,
  Moved}`). Copy mints a new id.
- `RestoreFrom` keeps `remote_jobs`, `remote_watches`, `thread_transfers`
  and `thread_transfer_sessions` local. The new request tables join that
  list.
- Claude read-only mode is `--permission-mode dontAsk` plus
  `--disallowedTools` for the write tools; `Config.AllowedTools` existed
  unused and now carries this server. Under `dontAsk` an MCP call is
  denied unless the server's tools are allowlisted;
  `--allowedTools "mcp__<server>__*"` admits every tool of that server
  and widens nothing else (spike, claude 2.1.261, 2026-09-19).
- Codex read-only is approval `never` plus the read-only sandbox. Under
  `never` an MCP call is auto-approved only when the sandbox has full
  disk write or the server entry sets
  `default_tools_approval_mode = "approve"`; otherwise a tool without a
  `readOnlyHint` annotation needs an approval that `never` turns into a
  denial before the call runs (spike, codex 0.153.4;
  `core/src/mcp_tool_call.rs`, `core/src/mcp/mod.rs`). With the key set
  the call runs and the sandbox still refuses shell writes.
- Claude shows the model the server `instructions` string (spike). Codex
  never does: that string is only the namespace description of its tool
  search (`codex-mcp/src/connection_manager/tool_catalog.rs`). A
  `developer_instructions` override reaches the Codex model verbatim
  (spike).
- A long MCP call survives on Codex with `tool_timeout_sec` raised (15
  minutes verified). On Claude it does not: with the server `timeout`
  entry, `MCP_TOOL_TIMEOUT`, `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` and
  `CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS` all raised, a call whose HTTP
  response is one JSON body fails at 359 s with "The operation timed
  out" (three runs). The same call answered as a `text/event-stream`
  response that carries a `: keepalive` comment every 15 s and the
  JSON-RPC result as its final event completes (7 minutes verified;
  `notifications/progress` events work the same and are not needed).
  `internal/threadmcp` now streams every call that way for a client
  whose Accept lists `text/event-stream` (both CLIs), which also
  repaired `remote_run` waits above six minutes on Claude.
- `receiveRemoteArtifact(ctx, directory, path, threadID, read)` is
  already generic over its chunk reader and verifies a whole-file
  SHA-256; only the destination side is tied to jobs and workspaces.

## Packages and ownership

| Piece | Where | Owns |
|---|---|---|
| Tool contract | `internal/threadtools` | Tool schemas, the instructions text, argument parsing, id resolution rules, transcript rendering, paging, item range reads and search, result shapes, the thread state derivation. Depends on an `App` interface it declares; never on `internal/app`. |
| Request ledger | `internal/store` | Migrations, `thread_requests`, `thread_request_receipts`, `scratch_threads`, the FTS index and its build progress, all queries. |
| App glue | `internal/app/app_thread_tools*.go` | Server registration, the switch, live toggles, spawn/send/ask/reply/status/cancel handlers, waits, settlement observer, wake delivery, the poller, peer methods, lifecycle hooks, boot sweep. |
| Peer transport | `internal/attachedbackends`, `internal/transport` | `CallThreadPeer`, `CapabilityThreadTools`, method annotations. |
| Providers | `internal/provider/claude`, `internal/provider/codex` | Claude `AllowedTools` on the argv; Codex `DeveloperInstructions` on start, resume and fork. Nothing else. |
| UI | `frontend/src/lib` | Origin chip, settings switch, `/side-chat`, `side-chat` companion pane, `aoTools` registry entry. |
| Tests | beside each piece, `e2e/tests` | See Validation. |

`internal/threadtools` is the piece with the most logic and the least
dependency, so it is testable against a fake app and reusable unchanged
on the destination side of a peer call.

## Data model

Three migrations, v101 to v103, each a separate `Migration` so a
failure is attributable.

### v101: `scratch` thread mode

Rebuild `threads` with `scratch` added to the `mode` CHECK, following
v72's rebuild shape. `threadmode.ModeScratch` joins `hiddenModes`, which
makes all six `hiddenThreadModesClause` callers hide it (sidebar lists,
project counts, both UI search paths) with no further change.
`threadmode.ValidateSet` keeps refusing a switch into or out of
`scratch`; promotion has its own path (below).

New table `scratch_threads(thread_id PK REFERENCES threads, source_thread_id,
return_mode, created_at, request_token NULL)`: which thread a scratch
fork came from, the mode Keep returns it to, and the ask it serves when
it is one. Boot and the settlement observer read it; deleting a thread
cascades it.

### v102: request ledger

`thread_requests` (source side, one row per spawn, send or ask this
computer's threads made):

| Column | Meaning |
|---|---|
| `token` PK | Source-minted UUID, the idempotency key on both sides. |
| `caller_thread_id` | The thread that called the tool. |
| `kind` | `spawn`, `send`, `ask`, `remind`. |
| `due_at` | For `remind`, when the clock settles it; the poller sweep collects due rows. Null otherwise. |
| `target_computer_id` | `''` for this computer, else the paired backend id. Rewritten when the target moves. Empty for `remind`. |
| `target_thread_id` | The thread the request runs in. For `ask` it is the scratch fork, set once the destination reports it. |
| `origin_thread_id` | For `ask`, the thread that was forked. |
| `notify` | Whether a wake is owed on settlement. Set by `notify: true` or by any positive wait that ended unsettled (timed out, blocked, interrupted). |
| `state` | `unconfirmed`, `accepted`, `running`, `replied`, `finished`, `errored`, `cancelled`, `interrupted`, `expired`, `refused`. |
| `answer`, `answer_kind` | The whole settled text, uncapped (`payloads`-style BLOB, never truncated), and whether it was an explicit `reply`, the `final` assistant text, an `error`, or a reminder `note`. Wakes carry a 24 KB preview; `thread_status` pages the whole. |
| `late_reply`, `late_reply_at` | A `thread_reply` that arrived after `finished`. Delivered as a second wake. |
| `revision` | Increments on every settlement and late reply; `thread_status` waits with `after_revision`. |
| `settled_at`, `delivered_at`, `delivered_how`, `late_delivered_at` | When settlement was stored here; when and how each wake was handed over: `inline`, `queued`, or `draft` (a queued wake restored into the composer by boot recovery, which never redispatches). |
| `expires_at` | From the destination: when the uncollected answer is dropped there. |
| `next_check`, `attempts`, `issue` | Poller schedule and last error text for remote targets. |
| `created_at`, `updated_at` | |

Indexes: `(caller_thread_id, created_at DESC)` for `thread_status`
listing and lineage; partial `(next_check) WHERE target_computer_id != ''
AND state IN ('unconfirmed','accepted','running','finished')` for the
poller (`finished` stays polled for a late reply until acknowledged, see
Expiry); partial `(due_at) WHERE kind = 'remind' AND state = 'accepted'`
for reminders. `blocked` is never stored: it is derived at wait time
from the target's live state (`hasActionableProposedPlan`, a pending
approval or user-input request), locally through `GetThreadLiveState`
and remotely through `ThreadToolRequestStatus`, which reports it beside
the receipt state.

`thread_request_receipts` (destination side, one row per request
against this computer's threads, including local ones):

| Column | Meaning |
|---|---|
| `token` PK | Same token as the source row. |
| `owner_device_id` | `"local"` or the authenticated device of the source computer. Authorization principal for every later call about this token. |
| `source_computer_id`, `source_computer_name`, `source_thread_id`, `source_thread_title` | Display metadata from the call for the footer and the chip. Never trusted for authorization. |
| `kind` | As above. |
| `target_thread_id` | The thread that answers. For `ask`, the scratch fork. |
| `message_item_id`, `turn_id` | The user row the request wrote, and the turn that consumed it, both set when the row is written to the provider. Distinguishes queued from running, and binds settlement to that turn and no other. |
| `state` | `accepted`, `running`, `replied`, `finished`, `errored`, `cancelled`, `interrupted`. |
| `answer`, `answer_kind`, `late_reply`, `late_reply_at`, `revision`, `settled_at` | As above. The answer is whole here too; it is the copy of record for a remote caller until collected. |
| `collected_revision`, `collected_at` | The revision the source acknowledged and when. A late reply after collection is a new revision, collected again. |
| `expires_at` | `settled_at` plus one day; the sweep drops `answer` and `late_reply` after it, never the row. |
| `created_at`, `updated_at` | |

Indexes: `(target_thread_id)` for the settlement observer and the
responder enable rule; `(expires_at)` for the sweep. Receipts are never
deleted while their target thread exists and the row is younger than
the retention floor (30 days): a token must keep answering "already
ran" to a late retry and keep accepting a late `thread_reply`. Past the
floor the row goes; a reply then gets `thread_request_unknown`.

Both tables are in the `RestoreFrom` keep-local set, for the same reason
as `remote_watches`: a history restore cannot revive a request or make
a retry run twice.

### v103: search index

Contentless FTS5 so no text is stored twice:

```sql
CREATE TABLE thread_search_rows (
  rowid INTEGER PRIMARY KEY,
  thread_id TEXT NOT NULL, item_id TEXT NOT NULL, source TEXT NOT NULL,
  kind TEXT NOT NULL,
  UNIQUE (thread_id, item_id, source));
CREATE VIRTUAL TABLE thread_search USING fts5(
  text, content='', contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2');
CREATE TABLE thread_search_build (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  cursor_thread_id TEXT NOT NULL, cursor_item_id TEXT NOT NULL,
  imports_done INTEGER NOT NULL, titles_done INTEGER NOT NULL,
  started_at INTEGER NOT NULL);
```

`source` is `item` or `import`; `kind` is `user`, `assistant`, `tool`,
`title` (a title row has `item_id = ''`). The FTS rowid is the
`thread_search_rows` rowid. Snippets are produced in Go from the source
text (the `items.summary`, the import row, or the title) around the
first matched term, since a contentless table cannot return them.
Ranking is `bm25()` per computer.

Indexing points, all in the store, all inside the transaction that
settles the text:

- a `user_text` item at insert;
- an `assistant_text` or `tool_call` item when it leaves the running
  state, through the one settle hook `indexSettledItemTx` that every item
  write path calls; `AppendItemSummaryTail` and every other streaming
  append stay untouched, pinned by a test that streams a long message and
  asserts zero FTS rows until settlement;
- an import chunk when written, and again when an import override is
  applied or removed;
- a title when set or changed;
- delete rows when an item, an import chunk or a thread is deleted
  (`DeleteThreadPaced` gains the two tables in its chunk loop).

`scratch` and workflow-mode filtering happens at query time by joining
`owned_threads`, not at index time, so promoting a scratch thread needs
no reindex and a thread whose ownership moved leaves the results.

Background build: `initSubsystems` starts one goroutine when
`thread_search_build` has a row. It walks `items` in `(thread_id, id)`
order in batches of 500 with a short pause between batches, then import
chunks, then titles, advancing the cursor after each committed batch;
restart resumes from the cursor; `INSERT OR IGNORE` on the unique key
means rows settled during the build are indexed once. The build skips
rows still streaming (their settle hook indexes them later) and
re-indexes a row whose text changed after indexing (an import override
applied or removed, a title edit) by delete-then-insert inside the
change's own transaction. `thread_search_rows` declares
`FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE` and
the item and import delete paths remove their rows explicitly; the FTS
table is contentless, so a stale FTS row without a mapping row is
harmless and the build's final pass sweeps any. Clone and restore:
`RestoreFrom` drops and recreates the index tables and inserts a fresh
progress row, since the restored history is a different corpus. When done it
deletes the progress row. `thread_search` reports `indexing: true`
while the row exists. Migration v103 inserts the progress row so a
fresh install and an upgrade take the same path.

## `internal/threadtools`

```go
type Server struct { app App }
func New(app App) *Server
func (s *Server) Tools(shape Shape) []map[string]any   // schemas
func (s *Server) Instructions(shape Shape) string
func (s *Server) Call(ctx context.Context, caller Caller, name string, args json.RawMessage) (any, error)
```

`Shape{Computers, Defaults}` is what amendment 3 needs: an empty
computer list produces the single-computer schemas and text, and
`Defaults` carries the calling thread's live provider settings into the
spawn schema descriptions. `Caller{ThreadID, ComputerID, ComputerName,
Title}` identifies who is calling, local or via a peer.

`App` is the interface the package needs, satisfied by `*app.App` on
the local side and by the destination app on the peer side:

- reads: `Thread`, `LiveState`, `ResolveThreadRef`, `ResolveWindow`,
  `Transcript`, `ItemPayload`, `SearchThreads`, `ExportTranscript`,
  `ExportAnswer`, `Catalog` (providers, models, runtime modes, projects);
- writes: `Spawn` (with `FromThread`), `Send`, `Ask`, `Reply`,
  `RequestStates`, `ListRequests`, `Cancel`, `Remind`, `UpdateThreads`,
  `UpdateGroup`;
- reach: `PairedComputers`, `Peer(computerID)` returning a client with
  the same five operations the peer methods expose.

Each method's input, output and failure contract is a comment on the
interface in `app.go`; this list is the shape, not the contract.

Everything about how a result looks lives here: the transcript renderer
(role and item id prefixes, turn delimiters, tool call one-liners with
sizes, the clipped-item pointer to `thread_item`), the byte budget and
the opaque `cursor` (an encoded `{window, bounds, last item position,
snapshot high-water item}` so a `head` or `around` page keeps its
bounds and streaming growth never shifts a page; the same shape pages
`thread_search` listings and ranked hits, `thread_item` matches, and
`thread_status` answers and listings), the `thread_item` range reader
(one selector per call, absolute and negative offsets widened to UTF-8
boundaries, `lines`, `query` with up to 50 match offsets and context),
the per-computer grouping with `errors` rows, the id resolution order
(full id local-first, prefix always fanned out, one move followed,
`incomplete` and `partial` on peer failure), and the ambiguity error
listing candidates with their computers.

Thread state derivation lives here too. There is no Go enum today; the
frontend's `resolveEffectiveThreadStatus` in `threadStatusPill.ts` is
the reference. `threadtools.State(thread, live)` reimplements it from
`LiveStateSnapshot` and the thread columns (`hasIncompleteTurn`,
`hasFailedTurn`, `hasActionableProposedPlan`, `worktreeSetupState`), and
one JSON fixture, `internal/threadtools/testdata/thread_states.json`,
holds the shared table: the Go test runs every case, and each case also
carries the frontend input `resolveEffectiveThreadStatus` derives from,
so the sidebar's rule can be pinned to the same table.

The instructions string is the spec's "Server instructions" blockquote,
kept in `instructions.go` as two variants assembled from paragraphs;
a test asserts every tool name and parameter it mentions exists in the
schemas for that shape.

## App wiring

### Registration and the switch

`app_thread_tools_mcp.go`, modeled on `app_browser.go`:

- `threadMCPServer()` lazily builds `threadmcp.New("ao-thread-tools",
  instructions, tools, call)`. The `tools` and `instructions` callbacks
  consult `Shape` from `backends.List()` at call time. Because
  `threadmcp` takes instructions as a string at construction, it gains
  an optional `InstructionsFunc func(T) string` so the text can follow
  the shape; the browser and remote servers keep passing a string.
- `threadMCPConfigForThread(thread, token)` returns the config for
  every interactive Claude or Codex session (phase sessions excluded
  through `deriveCallerScope`, like remote tools) regardless of the
  switch, exactly as `browserMCPConfigForThread` ignores the global
  browser toggle. The merge point is `app_session.go` beside the other
  two servers.
- `Settings.ThreadToolsEnabled` (default true) beside `BrowserEnabled`.
  `UpdateSettings` flipping it calls `setThreadToolsEnabled`, which
  walks live sessions and calls `ApplyManagedServerEnabled(threadID,
  "ao-thread-tools", on)` for each thread whose effective state changed.
  The transport refuses a call unless both `enabled` and
  `ThreadEnabled(threadID)` are true, so the switch never touches the
  server-wide flag (it stays true for the process lifetime). The
  per-thread flag holds the effective value, `threadToolsEnabledFor`:
  the switch, or `threadToolsOpenForeign(threadID)`, reapplied by
  `setThreadToolsEnabled` and `refreshThreadToolsAdmission`. The
  composer's own per-conversation toggle is a separate bit in `mcpapp`
  that the effective value is ANDed with, so a user who turned the
  server off for one conversation keeps that. A racing call is refused
  with `thread_tools_disabled`.
- The per-thread composer toggle is already generic: `mcpapp` addresses
  managed servers by name. No new code beyond listing the server.
- `startThreadMCPRefresh` is not a new worker: `startRemoteMCPRefresh`
  gains the thread server in its loop (reconnect on Claude, reload on
  Codex) so one wake covers both servers when pairing changes.

### Responder enable

`refreshThreadToolsAdmission(threadID)` recomputes the target's
effective flag when a paired computer's request is accepted against it
and, when it flipped on and the session is live, calls
`ApplyManagedServerEnabled(target, name, true)`.
`threadToolsOpenForeign` reads the thread's open receipts, so the flag
falls back to the switch as soon as the last one settles. A settled
receipt still accepts a late `thread_reply` through the transport only
while the flag is on, so a responder whose switch is off has until its
receipt settles to reply, and afterwards its late reply is accepted through the ordinary
tool call only if the switch is on; that matches the spec's "server on
for as long as the request is open". Local receipts never touch this:
the switch already governs the caller, and a local target is the same
computer.

### Admission without prompts

Both providers deny every MCP call in a read-only session unless told
otherwise, and every other mode prompts per call. The server is
admitted in every mode on both providers (amendment 15):

- Claude: `Config.AllowedTools` is plumbed into the argv builder beside
  `mcpConfigForCLI`, and every session passes
  `mcp__ao-thread-tools__*`. The spike confirms the wildcard spelling
  and that it widens nothing else. Under `dontAsk` that is what admits
  the call; under the prompting modes it is what skips the prompt.
- Codex: `threadMCPConfigForThread` adds
  `default_tools_approval_mode: "approve"` to the `ao-thread-tools`
  entry of `mcp_servers`, the way `remoteMCPServerConfig` adds
  `tool_timeout_sec`. `approve` short-circuits Codex's approval check
  before the policy or sandbox is consulted, so the entry works under
  `never` and under the prompting policies alike.

Why every mode and not only `read-only`: Codex applies a runtime-mode
change as a per-turn override with no restart, while `mcp_servers` is
thread-start configuration, so a key that followed the mode would be
stale after the first live switch, and a restart on that boundary would
contradict the Codex live-override contract. Admitting in every mode is
the only shape that is the same on both providers and survives a mode
change. The tools are AO's own actions, each visible in the sidebar
(a spawned thread, a queued message, a renamed title), which is what a
per-call prompt would have shown. Ruled 2026-09-19 (amendment 15); the
spec's Availability section carries the same text.

The read-only sandbox still applies to the responder itself: with the
key set, the spike's Codex read-only session ran the tool and the
sandbox refused its shell write.

The same denial applies to `ao-browser-tools` and `ao-remote-tools` in
read-only sessions on both providers: neither is allowlisted, neither
sets the approve key, and neither declares tool annotations, so a
read-only Claude or Codex thread cannot use them. Ruled 2026-09-19:
they stay denied, because both act outside the thread
([decisions.md](../decisions.md#remote-access-browser-pane-phone)).

### Decision guide delivery

Claude reads the server `instructions` string at `initialize` and again
on `ReconnectMcpServer`, so the guide travels with the server. Codex
never shows the model that string, so a Codex session gets the same text
as `developerInstructions` on `thread/start`, `thread/resume` and
`thread/fork`: `codex.Config` gains `DeveloperInstructions`, and
`buildThreadParams` sends it. Codex resolves developer instructions from
its config on a cold start, so AO first reads the thread's cwd-scoped
`developer_instructions` through `config/read` and sends that value with
the guide appended, omitting the override entirely when the guide is
absent (switch off, composer toggle off, phase session) so a user's own
value is never replaced. The paragraphs follow `Shape` like the server
string and come from the same `instructions.go`, so the two channels
cannot drift. A pairing change reaches a live Codex thread only at its
next start or resume, unlike the tool list, which reloads live; the
`thread_options` result carries the computer list, so a stale guide
costs one call. The `developer_instructions: null` AO sends in every
turn's collaboration-mode settings is the mode's own field and leaves
the thread-level text in place, which a Codex test pins.

### Spawn, send, ask

`app_thread_tools_start.go`. Shared prologue: mint token, insert the
`thread_requests` row as `unconfirmed`, then dispatch locally or to the
peer. The local dispatch and the peer's `ThreadToolCall` land in the
same function, `acceptRequest(ctx, owner, call)`, which:

1. inserts the receipt (`INSERT ... ON CONFLICT(token) DO NOTHING`, then
   reads it back: a retry returns the existing acceptance);
2. for `spawn`: `CreateThread` with the inherited or overridden options
   (project and workspace validated against this computer; a missing
   `project_id` from another computer is refused with the registered
   projects and worktrees in the error, produced from the same query
   `RemoteCommandProjects` uses), then the first send;
3. for `send`: refuse a target equal to the caller
   (`thread_self_send`); then `SendMessageWithOptions` on the target
   with `SendID = "thread-request:" + token`, `meta.origin` and
   `meta.originThread` (token included) set, and the footer always
   appended, worded for "an answer has been requested" when the request
   waits or notifies and "no answer notification was requested"
   otherwise, with the `thread_show` and `thread_send`-back lines only
   when the sender's computer is paired from the destination;
4. for `ask`: `forkThreadTail` (the new internal `ForkThread` core with
   `forkOptions{Mode: scratch, RuntimeMode: read-only, Title: "Ask: " +
   source title}`), a `scratch_threads` row, then the send as in 3;
5. the send path marks the receipt `running` with `message_item_id` and
   `turn_id` when the user row is written to the provider (the queued
   case sets it at flush; the idle case at once), through the
   `onDurable` hook of the queue item so a merged queued send still
   reports the turn it joined. Until then the receipt is `accepted`.

The source then advances its row from `unconfirmed` to `accepted` with
a conditional update (`WHERE state = 'unconfirmed'`), so a local
request that already settled during dispatch keeps its settled state.
Every state transition in both tables is a conditional update of this
shape; a no-op result means the other path won and is not an error.
Steps 1 and 2 to 4 are one durable transaction where the store allows
it; where a provider call sits between (session start), the receipt is
written first so a crash leaves a receipt the boot sweep settles as
`interrupted` rather than a thread with no record.

Cross-computer acceptance is retried with the same token: three
attempts with the peer-call timeout, then `thread_request_unconfirmed`
to the model with the token. The poller reconciles it (below).

### Waiting

`app_thread_tools_wait.go`. A `requestWaits` registry keyed by token
holds a `context.CancelCauseFunc` and a broadcast channel, the same
shape as `remoteWaits`. `waitRequest(ctx, token, seconds)` returns when
the source row reaches a settled state, when the caller's turn is
interrupted (`cancelRemoteWaits` gains a sibling called from the same
interrupt path), or when the time runs out. The wait ends after the
reply is written; the collector checks `waitActive(token)` under the
per-token mutex before queuing a wake, exactly as the remote watcher
checks `remoteWaitActive`. A timed-out wait sets `notify = 1` on the
row before returning `backgrounded`.

A parked call only reaches the model if the HTTP response is already
open: Claude drops a call whose response has not started after six
minutes, whatever the configured timeouts say (spike). `threadmcp`
therefore answers `tools/call` as a `text/event-stream` response for a
client whose Accept lists it (both CLIs do): a `: keepalive` comment
before the handler runs, another every 15 s while it runs, and the
JSON-RPC body as the final `message` event. That landed with the
spikes and applies to all three servers, so the waits here need
nothing more than the existing `remoteMCPCallCeiling` decoration.

### Settlement (destination side)

`app_thread_tools_settle.go`. One global turn observer
(`subscribeGlobalTurnObserver`) reacts to the end of a turn whose id
matches a `running` receipt's `turn_id` (a turn the user or another
request started ends nothing):

- turn completed: `finished` with the final assistant text of that turn
  (from the last `assistant_text` item), `answer_kind = final`;
- turn errored: `errored` with the error text;
- an interrupt caused by `thread_cancel`: `cancelled` (the cancel call
  marks the receipt before interrupting so the observer knows);
- a pending approval or question is not turn end and does nothing.

`thread_reply(token, text)`: the token must name a receipt whose
`target_thread_id` is the caller's thread; `running` becomes `replied`;
`finished` with no `late_reply` stores the late reply; a repeat with the
same text returns the existing acceptance; anything else is refused
with the state as the reason. Unknown token: `thread_request_unknown`.

Scratch receipts settle on any non-running state and the settle
function deletes the scratch thread (through `threadapp.DeleteTree`
outside the lock, DB rows only) after the answer is stored and after
the `thread_reply` call that stored it has written its response, so the
responder's own tool call never fails against a vanished thread. The
`scratch_threads` row goes with it. The answer lives in the receipt and
the source row, never in the thread, so nothing is lost.

Every settlement calls `collectLocal(token)`: if a source row with that
token exists on this computer, hand the settlement over now (this is
the whole local path); otherwise leave it for the source's poller.

### Collection and wakes (source side)

`collect(token, settlement)` writes the source row, broadcasts to any
parked wait, and if `notify` is set and no wait is active, queues the
wake: `registerQueueItem(callerThreadID, wakeMessage, SendMessageOptions{
SendID: "thread-wake:" + token}, injectedQueueOptions{preserveDraft:
true, persist: a.store.QueueThreadWake(token, item)})`, where
`QueueThreadWake` inserts the queue row and sets `delivered_at` and
`delivered_how = queued` in one transaction. Inline delivery sets
`delivered_how = inline` only after the tool response was written. Boot
recovery, which restores queued rows into the composer draft, marks the
matching requests `delivered_how = draft` in the same pass. A late reply
uses `SendID "thread-wake-late:" + token` and `late_delivered_at`. The
caller archived after the request: unarchive first (spec). Caller
deleted or missing: mark delivered with nothing queued and log.
No live session: `startSession` after queuing, as `deliverRemoteCompletion`
does. Workflow-mode callers never get here (phase sessions have no
server).

The wake body: status line (`Reply from <title>`, `<title> finished
without replying`, `<title> errored`, `... on <computer>` when remote,
`(answered <age> ago)` when old, always with `token <token>`), the
origin link, then the text capped at a 24 KB preview with a pointer to
`thread_status` with the token for the whole answer.

### `thread_status` and `thread_cancel`

`thread_status(tokens | thread_ids, wait_seconds, after_revision,
cursor, to_file)` reads the source rows or the threads' live state,
refuses duplicates and the caller's own thread, and waits if asked on
all of them at once. Waiters are independent: each call registers its
own waiter per token or thread (a list per key, as `remoteWaits`
models), so two concurrent calls sharing a token both return. The wait
ends on the first settlement past `after_revision`, the first listed
thread resting, or the first `blocked` target; blocked and rest are
checked at entry and on every live-state change of a local target, and
on every poll of a remote one. An answer present with `delivered_at`
null is marked delivered inline after the response is written
(amendment 6). The whole answer is returned; past `max_bytes` it pages
by `cursor` or goes to a file. Without tokens or thread ids it lists
the caller's rows, open first then newest first, paged by `cursor`.

### `thread_update` and `thread_group`

`app_thread_tools_organize.go`. `thread_update` resolves each id (local
or peer), groups the ids by computer, and on each computer validates
the whole resulting state per thread before touching it (title
non-empty after trim; group and pin not both set; not archiving the
caller; group name resolvable or creatable in the thread's project),
then applies the patch inside one store transaction per thread through
a new `threadapp.ApplyOrganizePatch` that the existing bindings'
service calls share, and emits that thread's final `thread:updated`
once. A refusal names the thread and the reason (`ErrThreadGrouped`
for pin on a grouped thread, a new `thread_is_caller` for archiving
the calling thread) and leaves that thread untouched; other threads in
the call still apply, and the result is per id. Each binding already emits its sidebar events, so the
UI follows live. `thread_group` maps to `RenameThreadGroup`,
`DeleteThreadGroup`, `PinThreadGroup` / `SetThreadGroupPinGroup` /
`UnpinThreadGroup`; a name resolves within the caller's project unless
`project_id` is given. On a peer these run inside `ThreadToolCall`
under `terminal:operate`, which the own-device session holds; the
bindings' own `threads:operate` floor is rechecked per call.

### `thread_remind`

`app_thread_tools_remind.go`. Inserts a `thread_requests` row of kind
`remind` with `due_at` and the note as the pending answer, state
`accepted`, `notify = 1`. The poller's sweep (already ticking) collects
rows whose `due_at` has passed: `collect` settles them `finished` with
the note and queues the wake, or hands it to a parked `thread_status`
wait. Restart-safe because it is only a row. `thread_cancel` on the
token deletes it. A reminder for a thread that is deleted goes with the
thread's other requests.

`thread_cancel(token)` resolves to a source row owned by the caller in
an open state and cancels that request on the destination through
`ThreadToolCall` with `cancel`: a receipt still `accepted` has its
queued message removed by `SendID` (a new `removeQueuedItem` in the
flush queue, durable and in-memory, refusing once the item reached the
provider), a `running` receipt gets `interruptTurnCtx` only if the
thread's current turn id equals the receipt's `turn_id`, and a
reminder row is deleted. Either way the receipt settles `cancelled`.
`thread_cancel(thread_id)` interrupts the thread's current turn when
any source row, settled or not, links the caller to it. A thread with
no such lineage is refused with `thread_not_yours`.

### Poller (source side, remote targets)

`app_thread_tools_poll.go`, a sibling of `startRemoteWatches`, not a
tenant of `remote_watches`: same ticker, same due-row batch, same
four-way limit, same backoff table, same boot resume. One RPC per
destination per tick carries every due token for that computer
(`ThreadToolRequestStatus(tokens, ack)`), so a source with twenty open
asks on one laptop makes one call, not twenty. The reply carries each
token's receipt state and answer, or `unknown`. `ack` lists the tokens
whose settlement the source has durably stored since the last poll; the
destination sets `collected_at` and may delete them.

Rules per token:

- `unknown` and the source row is `unconfirmed` and no retry is in
  flight: settle `refused` (never accepted).
- `unknown` and the source row was `accepted` or later: the
  destination lost the row (past the retention floor, or restored from
  a backup): settle `errored` ("the other computer no longer knows this
  request"). `expired` is a distinct reply from a receipt whose answer
  was dropped: settle `expired` with the answer's `settled_at` in the
  wake text.
- a settled state with a revision above `collected_revision`:
  `collect`, then acknowledge with that revision.
- pairing revoked or ended (`remote_pairing_expired`): settle `errored`
  with that reason.
- a moved target never changes the destination: the accepting computer
  settles it `interrupted` itself (Lifecycle hooks).

Cadence: +5 s normal, +2 s while a wait is parked on the token, +30 s
after an error, exactly the remote table.

### Expiry (destination side)

A sweep in the poller's process (every 10 minutes, cheap query): a
receipt past `expires_at` whose latest revision is uncollected has its
`answer` and `late_reply` cleared and its state set `expired`; the row
stays until the retention floor so the token keeps answering. Rows past
the floor (30 days from `created_at`) are deleted, source rows the
same. Destination-side export files awaiting transfer are deleted after
one day; delivered export files stay until removed.

### Resolution

`resolveThread(ctx, ref, hint)`: a full UUID matches locally first and
fans out only on a miss; a prefix matches locally (`ResolveThreadPrefix`
queries `threads` by `id LIKE ?` bounded to 8 rows, hidden modes
included, scratch excluded unless the caller owns it) and always fans
out as well, so an ambiguity across computers is seen. No paired
computers: local is the whole answer. Hint given: ask that computer
only. Otherwise `ThreadToolResolve(ref)` on every paired computer
concurrently under a 10 s context. Each answer is matches, `moved_to`,
or an error. One match wins; more than one across computers is
`thread_ambiguous` with candidates and their computers; a computer
that errored or timed out with no match elsewhere makes the result
`thread_resolution_incomplete` naming it, and with a single match
elsewhere the match is returned with a `partial` note listing the
computers that did not answer; a `moved_to` naming a paired computer
is followed once. The local store's
own `thread_transfers` rows answer `moved_to` for threads this computer
moved away, through `CheckThreadTransferAccess`.

### Peer methods

In `app_thread_tools_peer.go`, all `//ao:route selected`:

- `ThreadToolResolve(ctx, prefix string)` `//ao:scope threads:read`
- `ThreadToolQuery(ctx, call ThreadPeerCall)` `//ao:scope threads:read`
  (`thread_search`, `thread_show`, `thread_item`, `thread_options`)
- `ThreadToolCall(ctx, call ThreadPeerCall)` `//ao:scope terminal:operate`
  (`spawn` including `from_thread`, `send`, `ask`, `cancel`,
  `thread_update`, `thread_group`)
- `ThreadToolRequestStatus(ctx, poll ThreadPeerPoll)` `//ao:scope threads:read`
- `ThreadToolExportChunk(ctx, exportID string, offset int64)` `//ao:scope threads:read`

`ThreadPeerCall{Tool, Args json.RawMessage, Source Caller}`. Each
method: `threadToolOwner(ctx)` (the `remoteCommandOwner` rule), a
`requireScope` matching the annotation, then `threadtools.Call` with a
`Caller` whose computer fields come from the call and whose device
comes from the session. `make methodgen` regenerates the table and the
TS mirror.

`attachedbackends.CallThreadPeer` shares `callAgentPeer`'s body with a
capability parameter (`CapabilityThreadTools`) and its own allowlist of
the five names, without the agent-computer opt-in check, because reach
is pairing alone. `CapabilityThreadTools = "thread-tools.v1"` is
appended to every `serverCapabilities` variant. An older peer:
`thread_unsupported` with the update hint.

Errors follow `remoteOperationError`: a `threadOperationError(action,
computerID, threadID, err)` builds the same prose with the thread in
place of the request, reuses `remoteErrorDetails` for classification,
and keeps raw causes in the host log with a reference id.

### `to_file` across computers

The destination renders to `<configDir>/thread-exports/<id>.txt`,
returns `{export_id, size, sha256}`, and serves `ThreadToolExportChunk`
in 256 KiB pieces with the file stamp rule from `readRemoteArtifactChunk`.
The source calls `receiveRemoteArtifact` with a reader that wraps the
peer method and lands the file in its own `thread-exports/`. That
receiver refuses files over `remoteArtifactMaxBytes` (1 GiB); an
export larger than that is refused on the destination with its size
and a suggestion to narrow `include` or the window, since no thread
export should approach it. Local
`to_file` writes there directly. Files are retained until the user or
the agent removes them, except destination-side copies awaiting
transfer, which expire with receipts.

### Lifecycle hooks

- Caller thread deleted, archived or moved (`DeletePorts.StopRemoteWork`,
  `app_thread_archive.go`, `app_thread_transfer.go`): `cancelThreadRequests`
  ends parked waits, cancels open `ask` requests it owns (local and
  remote, best effort with the 20 s cancel timeout, logged on failure),
  and marks every open row `notify = 0` so no wake lands. Spawned and
  sent threads keep running.
- Copy: `HasOpenThreadRequests` joins `HasPendingRemoteWatches` in the
  copy refusal.
- Target thread deleted while a receipt is open: `DeleteTree` settles
  its receipts `errored` ("the thread was deleted") through the same
  `StopRemoteWork` port before rows go.
- Target moved: the transfer stops the thread's session, and the
  transfer hook settles every open receipt on it `interrupted` ("moved
  to <computer>") before the rows leave. The receipt stays on the
  accepting computer, the source collects it as usual, and nothing is
  re-addressed. The caller sends again to the new owner if it wants.
- Forget computer: `RemoveBackend` refuses once with the open requests
  listed; the confirm path (`abandon: true`) settles them `errored`
  ("computer forgotten") locally, drops their wakes, and proceeds. No
  remote call is made, and the wording says the other computer was not
  told.
- Caller archived: open requests get `notify = 0`; a later wait or
  `thread_status` on them re-arms it, and delivery for a re-armed
  request unarchives the caller (the two spec rules, reconciled).
- Boot (`initSubsystems`): every receipt in `accepted` or `running`
  becomes `interrupted`; every `scratch_threads` row is deleted with its
  thread; the poller resumes; the search build resumes.

## `/side-chat` and scratch threads in the UI

- `scratch` joins the hidden modes in `utils/threadModes.ts`, and the
  timeline and composer treat it as `chat` for rendering (one mapping in
  the mode helpers, checked wherever `mode` picks a layout).
- `/side-chat` is an intercepted composer command
  (`INTERCEPTED_COMMANDS` and `runInterceptedCommand`, the `runClear`
  precedent). It calls a new binding `ForkSideChat(threadID)` which uses
  `forkThreadTail` with `Mode: scratch`, the source's runtime mode, and
  title `Side chat: <title>`, then opens companion kind `side-chat` via
  `openCompanion` beside the source. `side-chat` joins
  `isEphemeralCompanionKind`, `COMPANION_SHAPED_PANE_ID`, and stays out of
  `isPersistedCompanionKind`. `CompanionPane.svelte` loads the ordinary
  thread pane for it.
- Closing the pane (explicitly, with its source, or on a source thread
  change through `closeCompanionsForSource`) calls `DeleteThread` on the
  fork. A boot sweep covers any pane that never got to close.
- Keep: a `PaneHeaderIconButton` calling `PromoteScratchThread(threadID)`,
  which sets `mode = return_mode` from `scratch_threads`, deletes that
  row, and emits the thread change; the frontend swaps the companion for
  a normal thread pane in place through the pane layout's replace path.
- Works on remote-owned threads through `withBackendTarget`, since the
  fork and the pane both address the owning backend.

## Origin chip and settings

- `UserMessage.svelte` `originBadge` gains the `agent-thread` branch:
  "from <title>" plus " on <computer>" when `originThread.computerId`
  differs from the viewing backend, clickable through
  `openThreadInPane` when `attachedBackendEntry` says the computer is
  attached and reachable, inert otherwise. `utils/userMessageMeta.ts`
  parses the new field.
- Settings: a `ThreadToolsSettings` switch beside the browser switch
  (`BrowserSettings.svelte`, `sections.ts`, `pages.ts`), reading and
  writing `threadToolsEnabled`.
- `aoTools.ts` `AO_TOOL_SERVERS` gains `ao-thread-tools` with the
  thirteen tool names, an icon, and `computerField: "computer_id"` so
  `GenericToolCallRow` resolves computer names the way it does for
  remote tools; the row body shows the thread title from the result.

## Edge cases and their answers

Requests and waits:

- Same token retried after a lost reply: destination returns the
  existing receipt; no second spawn, fork or message.
- Reply and turn end race: the receipt's state transition is a
  conditional `UPDATE ... WHERE state = 'running'`; whichever lands first
  wins and the other is a no-op or a late reply.
- Two `thread_reply` calls: the second is refused with "already replied".
- `thread_reply` from a thread that is not the receipt's target: refused
  `thread_request_not_yours`; the token alone is not a capability.
- Caller's turn interrupted while parked: the wait returns
  `backgrounded`, `notify` is set, the answer arrives later.
- Wait timed out at the same instant as settlement: serialized by the
  per-token mutex; either the wait carries the answer or the wake does,
  never both, never neither.
- `wait_seconds: 0` with no `notify`: fire-and-forget, the row still
  exists for `thread_status` and `thread_cancel`.
- `thread_status` after a wake was queued: returns the answer and says
  the message is also arriving.
- Caller deleted before the wake: delivered-with-nothing, logged.
- Caller archived: unarchived by delivery (spec).
- Target mid-turn for `send`: the message queues; receipt stays
  `accepted` until flush; a restart before flush leaves the message in
  the draft and the receipt `interrupted`.
- Responder rests on a pending approval: not settled; the human
  decides; the caller's wait returns `blocked` at once with `notify`
  armed, and the answer arrives when the turn finally rests.
- Responder's computer restarts mid-turn: `interrupted`, collected by
  the poller when the computer is back.
- Scratch fork of a thread whose provider session cannot fork (no
  session file yet, provider error): the receipt, already inserted for
  idempotency, is settled `errored` with the fork error and that is the
  reply; a retry with the same token returns the errored receipt instead
  of forking again.
- A send queued behind the user's own message: the user's turn ends
  and settles nothing; the request's turn is the one that consumed its
  message.
- Two queued sends merged into one provider turn: both receipts get
  that `turn_id` and both settle on it with the same final text.
- `thread_cancel` by token while the message is still queued: the
  message is removed, nothing is interrupted, the receipt is
  `cancelled`.
- Same prefix on two computers, one of them local: `thread_ambiguous`,
  because a prefix always fans out.
- Resolution with one paired computer offline: a local match returns
  with a `partial` note; no match anywhere is `incomplete`, not
  `not found`.
- Receiver's computer holds no credential for the sender's: the footer
  offers `thread_reply` only; the reply still reaches the sender
  through its own polling.
- Late `thread_reply` after the source collected `finished`: a new
  revision; the poller collects it and the second wake lands.
- Two `thread_status` calls waiting on the same token: both return.
- A thread sending to itself: refused at admission.
- Wake restored to the composer draft by a restart: the request reads
  `delivered: draft`; `thread_status` still returns the answer; the
  user sees the text in the composer and decides.
- Scratch responder needs a write: read-only refuses instantly; the
  model says so in its reply; nothing waits on a human.
- Scratch responder calls `thread_ask` itself: allowed (all tools in
  scratch); nested scratch threads settle and delete independently.
- Target blocked on an approval: the wait returns `blocked` at once;
  the request stays open; when the user answers and the turn later
  rests, it settles as usual and the wake lands if `notify` is set.
- Multi-token wait where one token is already settled at entry:
  returns immediately with every state.
- `thread_status` with a token from another caller thread: refused
  `thread_request_not_yours`.
- `from_thread` on a thread mid-turn: the tail fork settles as
  interrupted, the same as `thread_ask`; the new thread starts with the
  prompt.
- `thread_update` with fifty ids across three computers: grouped per
  computer, one peer call each, results and refusals per id; one
  unreachable computer fails only its ids.
- `thread_update` renaming to "New Thread": allowed; title generation
  re-arms, which is the app's deliberate rule.
- `thread_update` group name that exists in another project: a group
  is per project, so a new group of that name is created in the
  thread's project.
- `thread_group` delete while a thread in it is pinned through the
  group: the store ungroups members and drops the pin, as the sidebar
  does.
- `thread_remind` with `at` in the past: settles on the next sweep,
  which is what "now" means; there is no ceiling.
- Reminder due while the caller's turn is running: queued at the
  boundary like every wake.

Reach and computers:

- No paired computers: single-computer shapes (amendment 3); every
  cross-computer branch is unreachable and `computer_id` is not a
  parameter.
- A computer paired mid-session: the refresh worker re-initializes the
  server; the next `tools/list` carries the computer parameters.
- Destination switch off: accepts everything; responder's session gets
  the server on for the request; off again when the receipt closes.
- Destination older than this build: `thread_unsupported` before any
  state is written on the source (the capability check happens in
  `openRPC`), so the source row is deleted rather than left
  `unconfirmed`.
- Pairing revoked while requests are open: poller sees
  `remote_pairing_expired`, settles `errored`; receipts on the far side
  expire after a day.
- Source computer unreachable when the responder replies: reply stored
  in the receipt; delivered on the next successful poll with its age.
- Same thread id prefix on two computers: `thread_ambiguous` with both.
- Target moved between resolution and call: the call returns
  `thread_moved`; the source follows once and retries the same token
  there.
- Target moved while the request is open: `errored` with the move as
  the reason (Lifecycle hooks).
- A wake from another computer for a caller that has since moved: the
  poller's `collect` refuses on `CheckThreadExecutionAccess` and
  reschedules at +30 s, as remote completions do; the new owner never
  had the source row, so after the source's own transfer hook cancels
  its requests nothing is orphaned.
- Search on a computer still building its index: rows carry
  `indexing: true`; the error row is only for unreachable or too-old
  computers.
- Fan-out with one slow computer: 10 s bound per computer, results
  from the others return with an error row for it.

Search and reads:

- Streaming a long message: zero FTS writes until settlement (pinned).
- Import overrides: applying one deletes the import row from the index
  and indexes the override.
- Deleting a thread mid-build: the build's next batch skips ids that no
  longer exist; `INSERT OR IGNORE` and the cascade keep the side table
  consistent.
- `thread_show` on a 38k-item thread with `all`: each page renders one
  window bounded by `max_bytes`, cursor is the last item id rendered;
  the renderer never materializes more than one page.
- `thread_item` with an offset past the end: empty read with the size in
  the reply, not an error; a negative offset larger than the item clamps
  to the start.
- `thread_item` on an item whose payload is chunked: `GetPayloadChunk`
  serves the range; `lines` walks chunks without loading the whole
  payload.
- `to_file` when the export directory is not writable or the disk is
  full: the error names the path; no partial file is left (`.partial`
  and rename, as artifacts do).

Switch and sessions:

- Switch flipped while a call is in flight: the call completes;
  subsequent calls are refused `thread_tools_disabled`; the provider
  drops the tools at the toggle.
- Switch off on a computer answering another computer: the responder's
  session keeps the server until its receipt closes, then loses it.
- Composer per-thread toggle off: the same refusal, same as the other
  two servers.
- Phase session: no server, ever.

## What was considered and rejected

- **`LIKE` instead of FTS5.** The UI search already does `LIKE` over
  titles and summaries. It is linear in corpus size, cannot rank, and
  gives no snippets. Agents search far more often than people, and a
  38k-item thread exists. FTS5 is present in the build; a contentless
  table stores no text twice.
- **Content-bearing FTS table.** Would give `snippet()` for free at the
  cost of duplicating every assistant message. Snippets in Go are a
  hundred lines; the duplication is unbounded.
- **Reusing `remote_watches` for request polling.** The row shape is a
  job receipt, `checkRemoteWatch` is job-specific at every step, and
  batching per destination does not fit it. A sibling loop with the same
  constants shares the discipline without contorting either.
- **One request table with a role column.** Local requests would then
  be one row doing two jobs, and the settle and collect paths would
  branch on locality. Two tables with a zero-hop local collect keep one
  path each.
- **Pushing settlements from the destination.** The pairing is
  directional; the destination holds no credential for the source.
  Polling with per-destination batching is bounded and already the
  established pattern.
- **Transporting receipts when a target moves.** Would make the new
  owner responsible for a request it never accepted and require the
  transfer protocol to learn about tokens. Settling `errored` on the
  move is honest and a resend is one call.
- **A separate `resolve` inside `ThreadToolQuery`.** A typed method
  gets its own scope annotation and a clear signature on the wire.
- **`ParentThreadID` for lineage.** Means Codex subagent; would mark
  the caller busy (spec).
- **Retracting a queued wake.** Needs a new queue-removal path with
  draft-merge hazards for a case the agent already handled.
- **Hiding scratch by a flag instead of a mode.** A mode rides every
  existing hidden-mode filter for free and the CHECK constraint refuses
  a stray value.
- **Per-tool pairing gating.** No tool is pairing-only; shaping the
  parameters and text is the whole difference, so the tool set is
  stable and the model never sees a tool appear or vanish.
- **Always-present optional computer fields** (the Codex review's
  alternative to the shape variants). Simpler to build, but an agent
  on an unpaired computer would see parameters that cannot work, which
  the owner ruled out; pairing is rare and one-way, so the refresh path
  runs about once per install.
- **Making read-only scratch threads unable to spawn or send.** Ruled
  out: read-only is the responder's own runtime, not an isolation
  boundary, and a consulted thread may legitimately hand work on.
- **Following a moved target with the open request.** Transporting a
  receipt to a computer that never accepted it, or re-addressing from
  the source, both add a path for the rare case; settling
  `interrupted` on the accepting computer and resending is one call.

## Build order

The build ran in these phases, each green on its own tests and each a
usable increment.

0. **Spikes**, done 2026-09-19 in a scratch directory per the spike
   policy against a throwaway loopback MCP server (claude 2.1.261,
   codex 0.153.4); nothing from them is kept. Outcomes, all folded into
   Verified facts and the wiring above: Claude under `dontAsk` denies
   MCP calls without `--allowedTools "mcp__<server>__*"` and admits
   every tool of the server with it, and nothing else widens (A1, A2);
   Claude reads the server `instructions` and Codex does not (B1, B2);
   Codex read-only denies MCP calls until the server entry sets
   `default_tools_approval_mode = "approve"`, after which the call runs
   and the sandbox still refuses a shell write (C, C2); a Codex
   `developer_instructions` override reaches the model (B3); a
   15-minute MCP call parked under the ceiling
   returns on Codex with `tool_timeout_sec` = 1200 (D2) but fails on
   Claude at six minutes whatever timeout is configured (D1, three
   runs), and completes once the response is a server-sent event stream
   with a keepalive comment every 15 s (E, two variants). See Verified
   facts and the Waiting section.
1. **Store**: v101 to v103, `scratch_threads`, request tables and their
   queries, FTS index with settle-time hooks, background build,
   `DeleteThreadPaced` and `RestoreFrom` updates. Tests: migration on a
   populated store, index correctness against the `timeline_items`
   view, zero writes during streaming, build resume after a simulated
   restart, request state transitions, receipt idempotency, expiry.
2. **`internal/threadtools`** read side against a fake app: schemas and
   instructions for both shapes, resolution rules, transcript rendering
   with budgets and cursors, `thread_item` ranges and search, grouping
   and error rows, state derivation with the shared fixture.
3. **App wiring for reads**: registration in `app_session.go`, the
   settings switch and live toggle on both providers, per-thread toggle,
   `aoTools` registry entry, settings UI. After this phase the three
   read tools and `thread_options` work locally on both providers.
4. **Local start and settle**: `forkThreadTail` refactor, spawn (with
   `from_thread`), send, ask, reply, status (multi-token, blocked
   return), cancel, waits, settlement observer, wakes, scratch deletion,
   boot sweep, lifecycle hooks, origin chip, admission flags,
   `thread_update`, `thread_group`, `thread_remind`. After this phase
   everything in the spec works on one computer.
5. **Cross-computer**: capability, `CallThreadPeer`, the five peer
   methods, `methodgen`, resolution fan-out, poller, acknowledgement and
   expiry, export chunks, moved-target handling, forget-computer
   refusal, shape switching on pairing changes. Two-computer TLS tests
   after the `app_remote_mcp_extended_test.go` recipe.
6. **`/side-chat`**: intercepted command, `ForkSideChat`, companion kind,
   close-deletes, Keep, remote-owned threads.
7. **End to end and docs**: the three Playwright specs below, the
   spec's success criteria, the `docs/README.md` rows, and the guide and
   architecture-doc updates ([schema.md](schema.md),
   [sqlite-store.md](sqlite-store.md),
   [turn-lifecycle.md](turn-lifecycle.md),
   [user-message-ordering.md](user-message-ordering.md),
   [agent-harness.md](agent-harness.md), `internal/app/AGENTS.md`,
   `internal/threadmcp/AGENTS.md`). This document is the record; the
   mechanism has no separate architecture file.

## Validation

- Store: `make go-test ./internal/store/...`.
  `migration_thread_tools_test.go` upgrades a populated fixture through
  v101 to v103; `thread_search_test.go` covers the index against the
  `timeline_items` view, zero writes while a row streams, and build
  resume; `thread_requests_test.go` covers both sides of the ledger,
  receipt idempotency, revisions and expiry; `thread_organize_test.go`,
  `threads_lookup_test.go` and `items_range_test.go` cover the organize
  transaction, prefix resolution and the range reads.
- `internal/threadtools`: unit tests against the fake app in
  `fake_app_test.go`, no app, fast. Schemas and instructions in both
  shapes, resolution, rendering budgets and cursors, item ranges and
  search, per-computer grouping and error rows, footers and wake
  templates, the state fixture.
- App: `kerneltest`-isolated tests with the mock providers.
  `app_thread_tools_mcp_test.go` (registration and its absence for phase
  sessions, the switch on both providers, the admission flags, the
  per-conversation toggle ANDed with the switch),
  `app_thread_tools_requests_test.go` (spawn, send, ask, reply, waits,
  wakes, `thread_status`, cancel, reminders, lifecycle hooks, the boot
  sweep), `app_thread_tools_organize_test.go`,
  `app_thread_tools_test.go` (the adapter against real store rows),
  `app_thread_tools_e2e_test.go` (every read tool over the loopback
  transport), `app_side_chat_test.go`.
- Two computers: `app_thread_tools_reach_test.go` pairs two isolated
  apps over a real TLS connection (`newReachPair`, `attachedbackends.New`
  per device) and covers the caller's switch, a destination serving with
  its own switch off, ownership by device, fan-out resolution,
  lost-reply retry on one token, unconfirmed reconciliation, expiry,
  collection after a restart of either side, revocation, a moved target,
  forgetting a computer, the wake naming the thread and computer that
  answered, and a deleted caller cancelling the ask it left running
  there. It drives `pollRemoteThreadRequests`,
  `expireThreadRequests` and `sweepThreadRequestsAtBoot` directly rather
  than waiting on the ticker.
- Frontend: Vitest for `agentThreadOrigin` and `userMessageMeta`, the
  `sideChat` store and the companion persistence rules, the settings
  switch, and the shared thread-state values; `pnpm run check` and
  `pnpm run build`.
- Playwright (`e2e/tests`): `thread-tools.spec.ts` (21 tests),
  `side-chat.spec.ts` (4), `thread-tools-paired.spec.ts` (16, on two
  harness hosts) and `thread-tools-three-computers.spec.ts` (2, on
  three, the third stopped mid-file for the search error row).
  Run one with `bin/ao-harness-e2e tests/<spec>`. The
  mock provider makes real MCP calls through its `mcpCall` scenario step
  ([agent-harness.md](agent-harness.md)).
- Go: `make go-build`, `make go-test`, `gofmt -w`, `make methodgen`
  committed (the generator diff test fails otherwise), Wails bindings
  regenerated for the new methods.
- Manual provider smoke, only when explicitly requested: one real
  `thread_ask` per provider on a read-only fork, to confirm the spike
  results survive a real session.
