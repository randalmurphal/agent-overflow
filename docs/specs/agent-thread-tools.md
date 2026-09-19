# Agent thread tools

Status: design. Local scope signed off 2026-09-04; the connected-computers
scope and the tool reshaping were settled 2026-09-19 (see Open
questions for the rulings). Nothing implemented yet. `(Qn)`
tags are the ids from the original brainstorm session and carry no
other meaning.

## Goal

An agent in any interactive thread can search thread history, read any
thread, send a message to another thread or ask it a one-shot question,
open new threads, wait for their answers or let them arrive later, and
interrupt the threads it started, as seamlessly as if the human were
coordinating those threads by hand, with no restrictions beyond what its
own thread already has.

Threads on the user's other computers are reachable through the same
tools with the same semantics. A thread is addressed by its id wherever
it lives; every result says which computer it came from. The wire and
the pairing are the ones `ao-remote-tools` already uses
([remote-commands.md](../architecture/remote-commands.md)); the tool
contract is its own, because these tools move conversation, not
processes.

## Approach

An in-process MCP server, `ao-thread-tools`, built on the shared
`internal/threadmcp` transport like `ao-browser-tools` and
`ao-remote-tools`: loopback HTTP, a per-thread capability URL, wired
into Claude via `--mcp-config` and Codex via `mcp_servers` at session
start, with the decision guide in the server's `instructions` string.
Handlers resolve the capability to the calling thread and call the app
in-process. Claude and Codex get the tools identically; Claude TUI is not
supported, as with the other two servers.

Tool logic lives in `internal/threadtools` and runs against the app that
owns the target thread. A local target runs in the caller's process. A
target on another computer runs the same handler on that computer,
reached through one peer RPC over the existing paired connection; the
result comes back unchanged. Nothing is replicated and no second copy of
thread state exists anywhere.

A call that starts work (spawn, send, ask) can wait for the answer like
`remote_run` waits for a command, and backgrounds when the wait ends.
An answer that was not collected inline arrives as a message: a queued
row in the calling thread, delivered at its next turn boundary through
the queued-message path workflow wakes and remote-job completions use.
The caller reads it when it lands, whichever computer it comes from.

Ephemeral asks are hidden forks (`scratch` thread mode) forced into the
`read-only` runtime mode. `/side-chat` is the human's version of the same
fork: a companion pane, discarded on close.

## Why MCP and not a CLI (Q1, revised)

The workflow CLI stays a CLI because it has offline verbs and humans run
it in a shell. Thread tools are agent-only and in-session, and one of
them must work from a read-only fork: Claude's `read-only` mode is
`dontAsk`, which auto-denies any Bash call that would prompt, so a CLI
`reply` would need a Bash prefix allow rule. An MCP tool is allowlisted
by exact name. Typed schemas plus server instructions also mean nobody
types a slash command to make the tools discoverable. There is no
`/ao-tools` command.

## Addressing threads and computers

- Thread ids are canonical v4 UUIDs minted by `internal/entityid`,
  unique across computers, and a Move keeps the id
  ([conversation-transfer.md](conversation-transfer.md)). So a thread id
  is a complete address. Tools accept an unambiguous prefix of six or
  more characters.
- Resolution tries the caller's computer first. On a miss it asks every
  paired computer concurrently, bounded to 10 seconds, and uses the one
  that owns the thread. No match is `thread_not_found`; an ambiguous
  prefix is `thread_ambiguous` listing the candidates with their
  computers. An optional `computer_id` skips the fan-out when the caller
  already knows.
- A thread that moved since the caller last saw it resolves to its new
  owner: the old computer records the new owner at transfer and answers
  `moved to <computer>` instead of `not found`, and resolution follows
  that once. A request already in flight when the target moves follows
  the same way (see Wakes).
- Every result row that names a thread carries `computer_id` and
  `computer` (the name this computer's pairing profile gives it).
  People read names, the model reads ids: transcript rows and wake
  badges show titles and computer names; tool inputs and outputs carry
  ids beside them.

## Tools

Nine tools. Long text (prompts, replies) travels as string params.

### `thread_search` (Q2, Q3, Q14, Q23, Q24)

One tool for "find threads" and "what threads are there". With `query`
it is full-text search; without it, a listing by last activity. Either
way the rows are the same shape: computer id and name, thread id,
title, project, provider, model, state, last activity, branch, and with
a query the matched item id and a snippet. State is the UI's enum (idle
/ running / awaiting-input / pending-approval / plan-ready / error /
interrupted), reachable because the handler runs inside the app that
owns the thread.

Filters: `computers` (a list of computer ids; omitted means the
caller's computer plus every paired one; `["local"]` means only the
caller's), `thread_id` (search within one thread), `kind` (user |
assistant | tool | title), `project`, `provider`, `state`, `archived`,
`spawned_by_me`, `since`, `limit`. Defaults: every computer, all
projects, archived included with a query and excluded without one,
workflow-mode threads included, scratch threads excluded, 20 rows per
computer with a query and 30 without.

Search is FTS5 over `items.summary` for `user_text`, `assistant_text`,
and `tool_call` rows, plus thread titles, plus the imported-history
table behind the `timeline_items` view (import overrides honored). Rows
enter the index when they settle, never per streamed append: an update
trigger would re-tokenize the whole row on every chunk. Built in the
background on first boot after the migration; until it completes,
results carry `indexing: true` and cover what is indexed so far.
Thinking is not indexed: `items.summary` holds only its 400-rune tail,
and the full text is one `thread_show` away.

Each computer indexes its own database. A multi-computer call runs on
each computer concurrently, bounded to 10 seconds each, and returns
rows grouped per computer in that computer's own order (FTS5 ranks are
not comparable across databases); `limit` applies per computer. A
computer that is offline or too old contributes an `errors` row naming
it and the reason; the other computers' rows still return.

### `thread_show` (Q4)

Reads one thread. Everything the database holds for it is reachable;
the agent decides how much comes back per call and whether it comes
back inline or as a file.

The window: `tail` (the last N turns, the default with N=20), `head`
(the first N), `since` (everything after an item id or timestamp),
`around` + `context` (the turns surrounding one item, which is how a
`thread_search` hit is opened), or `all`. Inline results carry
`max_bytes`, default 64KB, raised per call up to what the provider will
accept in one tool result (both providers truncate large tool results
on their own, which would lose the tail silently). A result that stops
short is never a dead end: it returns `next`, the item id to pass as
`since` for the following page, so any window, `all` included, can be
walked to its end in as many calls as the agent wants. 38k-item threads
exist, so paging is load-bearing.

`to_file: true` skips the inline budget entirely: the whole requested
window is rendered to a file under the source data directory (beside
`remote-artifacts/`, retained until explicitly removed), and the call
returns its path and size for the agent's own read and search tools. A
remote thread renders on its own computer and the file crosses in
chunks with a whole-file SHA-256, as remote artifacts do; the inline
form moves at most `max_bytes` over the wire.

Output is plain text, turn-delimited, each row prefixed with role and
item id. Default content is what people said: user text, assistant
text, and the assistant's thinking (from `payloads.data`). Tool calls
collapse to one line each that states the item id and the size of what
it holds; `include` adds their outputs, diffs and subagent runs, up to
everything stored for the thread. A single item that is itself large
(a long tool output, a big diff) is shown clipped in the transcript
with its size and a pointer to `thread_item`; the transcript never
carries megabytes for one row. Any thread by id, hidden workflow
threads included.

### `thread_item`

Reads inside one item once the agent knows which one: the full payload
of a tool output, diff, subagent run, or long message, by `item_id`.
Bounded reads with `offset` and `max_bytes` (default 16KB, absolute
byte offsets, a negative offset reads from the end); `lines` for a line
range instead; `query` for a literal search inside the item returning
each match's offset with a little context, so the agent finds the part
it wants and then reads exactly that range. This is `remote_read_log`
and `remote_search_log` for thread content, and it is what makes a
massive tool call inspectable without ever pulling it whole. The item
renders on the computer that holds it; only the requested bytes cross.

### `thread_spawn` (Q5, Q6, Q18)

Creates a normal sidebar thread, sends `prompt` as its first user
message, starts the turn, and returns the thread id, its computer, and
a request token. Locally it inherits the caller's project, workspace,
provider, model, mode, and runtime mode; each has an override param,
plus `title`. `worktree` (optional branch name) creates a fresh worktree
through the existing draft-worktree path instead of inheriting the
workspace. No launch card: the call renders as the tool call it is, and
the spawned thread is a normal thread with no sidebar marking (Q16).

With `computer_id`, project and workspace cannot be inherited (projects
are registered per computer), so `project_id` names one of that
computer's projects and `workspace_path` or `worktree` selects the
checkout. Omitting `project_id` is refused with that computer's
registered projects and worktrees in the error, so no separate
discovery tool is needed. Provider, model, mode and runtime mode still
default to the caller's and are validated on the destination: a
provider or model that computer does not offer is refused with the list
it does. The destination's own account runs and bills the thread. The
thread is created on the destination, appears in its sidebar and in
every frontend attached to it, and has no special marking there either.

`wait_seconds` and `notify` are described under Waiting.

### `thread_send` (Q9, Q15)

Continues an existing thread as if the human had typed `message`.
Mid-turn, it queues for the turn boundary with the draft preserved;
idle, it sends now and lazily starts the session. Works across projects
and computers. Returns a request token. `wait_seconds` and `notify` as
under Waiting.

### `thread_ask` (Q10, Q25, Q26, Q28)

One-shot. Forks the target at its tail, in-flight turn included (the
existing mid-turn tail fork, settled as interrupted), into a `scratch`
thread: hidden from the sidebar, runtime mode forced to `read-only`
regardless of the source's mode, no title generation, all tools
available. Sends `question` with the reply footer. The answer comes
back inline when it arrives within the wait, otherwise as a message.
The scratch thread is deleted (DB rows only, Q21) as soon as its answer
is stored. If the answer is a clarifying question, the caller re-asks
or switches to `thread_send` on the real thread.

`read-only` exists for unattended work: writes and mutating commands are
refused immediately and the refusal goes straight back to the model on
both providers, so an ephemeral thread never waits on a human. If it
needed a write to answer, it says so in its reply.

A remote target is forked on its own computer, with that computer's
provider account and session files, and runs there. The scratch thread
never leaves the destination; only the answer does.

### `thread_reply` (Q25)

The responder's half. The footer on every ask, and on every spawn or
send that is waiting or notifying, names the sender thread, its
computer when it is another one, and this tool. A `token` resolves to
one pending request on the responder's own computer; one reply per
token, a second is refused with the reason. Unknown token: loud error. A
reply is stored where the responder runs, so it succeeds even when the
caller's computer is unreachable at that moment; the caller receives it
when the connection is back.

A responder must be able to reply even when its own computer's switch
is off. A thread that receives a spawn, send or ask from another
computer has the server turned on in its session for as long as that
request is open, through the same live toggle the switch uses, and
returns to the switch's state once the request settles. A scratch ask
is created with it on. The switch governs what this computer's agents
start on their own, not whether they can answer.

### `thread_status`

Re-attaches to a request this thread made: `token`, optional
`wait_seconds`. Returns the request's state (`running` / `replied` /
`finished` / `errored` / `cancelled` / `interrupted` / `expired`), the
target thread and computer, and the answer when there is one. This is
the `remote_status` of thread tools: after a call backgrounded, the
agent can wait again instead of ending its turn, and after a lost
reply from another computer it can learn what happened without asking
twice. A reply that carries the settled answer is the delivery; the
queued message for it is dismissed, as remote jobs do.

Without a `token` it lists this thread's requests, open ones first,
latest 64, each with its token, kind, target and state. That is how an
agent recovers its tokens after a context compaction; the tokens
themselves are never something it has to remember.

### `thread_cancel` (Q19)

Interrupts the running turn of a thread the caller spawned, sent to, or
asked (lineage from `thread_requests`), by `thread_id` or `token`, on
whichever computer it runs. Interrupt only, never a revert. Refused for
any other thread with the reason.

## Server instructions

The `instructions` string is the decision guide the model reads once
per session. It is short enough to be read, and it covers every
interaction the tools have with each other. The text, maintained beside
the tool schemas in `internal/threadtools`:

> These tools let you work with other Agent Overflow threads, on this
> computer and on the user's other paired computers, the way the user
> would from the sidebar. Every result names the computer a thread is
> on; you address a thread by its id alone.
>
> Finding things. `thread_search` with a `query` searches what people
> and tools said across all threads; without a `query` it lists recent
> threads with what each is doing now. Narrow with `computers`,
> `project`, `state` or `spawned_by_me`. Open a hit with `thread_show`
> `around` its item id. Read the recent end of a thread with
> `thread_show` (default: last 20 turns). Results that stop short give
> you `next`; pass it as `since` to keep reading. For a whole thread,
> ask for `to_file` and read the file with your own tools. A large
> tool output or diff shows clipped with its item id: use `thread_item`
> to search inside it with `query` and read the byte range you need.
>
> Starting work. `thread_spawn` opens a new thread and runs your
> `prompt` there; `thread_send` continues an existing thread as if the
> user typed your `message`; `thread_ask` asks a question of a hidden,
> read-only, throwaway copy of a thread, so the real thread is never
> touched and the copy cannot write or wait on a person. Use ask to
> consult a thread's context; use send to give it work. All three
> return a `token`.
>
> Waiting. Each of the three takes `wait_seconds` (up to 900). Ask
> waits 5 minutes unless you say otherwise; spawn and send return at
> once. If the answer arrives in time it is in the reply and you are
> done. If not, the reply says `backgrounded`: the work continues and
> the answer will arrive in this thread as a message, at your next turn
> boundary, badged with the thread it came from. You do not need to
> poll. `thread_status` with the token waits again or checks state;
> without a token it lists what you have started. Pass `notify: true`
> on a spawn or send with no wait if you still want the message when
> it finishes.
>
> Answering. When a message in your thread ends with a reply footer,
> another thread is waiting on you: call `thread_reply` with that
> token and your answer, once. If you finish your turn without
> replying, the sender receives your final text flagged as such, and a
> later `thread_reply` still reaches it.
>
> Stopping. `thread_cancel` interrupts a thread you spawned, sent to
> or asked, by token or thread id. It does not undo anything.
>
> Other computers. Spawning on another computer needs `computer_id`
> and one of its `project_id`s; leave `project_id` out once and the
> error lists them. Provider and model default to yours and are checked
> there. An offline computer appears as an error row in search results
> and as an error on a direct call; nothing runs somewhere else
> instead. Answers from another computer wait up to a day for you if
> the connection is down.
>
> Ids are UUIDs; a prefix of six or more characters works when it is
> unambiguous. Never guess an id: take it from a result.

## Waiting

`thread_spawn`, `thread_send` and `thread_ask` take `wait_seconds`.
The agent chooses per call. When it does not, ask waits five minutes
and spawn and send return at once (0). The cap is fifteen minutes,
matching `remote_run`, and the providers are already configured to
tolerate a call of that length. AO's wait, not the provider, decides
when the call returns.

A wait ends when the request settles (see Wakes for what settles it)
or the time runs out. Settled: the answer is in the reply, and the
request is done; the reply is the delivery. Timed out: the reply says
`backgrounded` with the token, the work continues, and the answer
arrives later as a message, exactly as a remote job's completion does.
Interrupting the caller's turn ends a parked wait the same way. A call
with `wait_seconds: 0` returns at once and delivers nothing later
unless `notify` is set.

`notify` (default false) asks for a message on settlement regardless of
waiting. A backgrounded wait implies it. Without either, a spawn or
send is fire-and-forget: the thread runs, and the caller finds it later
with `thread_search` or reads it with `thread_show`.

## Request identity and durability

Every spawn, send and ask creates a `thread_requests` row on the
caller's computer before anything else happens (token, caller thread,
target computer, target thread, kind, notify, wait, state, created,
settled-at, delivered-at). The token is minted by the source and doubles
as the idempotency key; the model never chooses or types request ids.
Locally the row is lineage for `thread_cancel`, the handle for
`thread_status`, and the wake binding.

Across computers the destination stores its own row for the request
(token, owning device, source computer, source thread, target thread,
state, answer) and deduplicates on it: a retry with the same token
returns the existing acceptance, and the source retries a lost reply
itself, with the same token, a bounded number of times. A spawn or ask
whose acceptance is still unknown after that returns
`thread_request_unconfirmed` with the token; the watcher (below)
reconciles it against the destination, and a destination holding no row
for it settles the request `refused` so the caller is never left
waiting for something that never started.

`ParentThreadID` is not reused for lineage, since it means "Codex
subagent child" and would mark the caller busy while its spawns run.

## Wakes (Q7, Q8, Q15, Q25)

A wake is a user-role row in the calling thread, badged with the source
thread (see Attribution), carrying a status line, the source thread's
computer, id and title as a link, and a body capped at 24KB with a
truncation notice pointing at `thread_show`. Delivered by the
queued-message path: queued at the turn boundary when the caller is
live, lazily starting a session otherwise, draft preserved, one durable
transaction inserting the row and marking the request delivered so a
repeated observation cannot inject it twice. A wake is a message, so an
idle caller wakes up and runs on its own when it lands, human present
or not; that is what waiting past the timeout or `notify` opts into. An
archived caller is unarchived by delivery. Caller deleted first:
dropped.

What settles a request, in order:

1. An explicit `thread_reply` settles it `replied` with the reply
   text.
2. If the responder's turn rests first (turn completed or errored; a
   pending approval or question is the human's business and does not
   count, Q7), it settles `finished` with the final assistant text,
   flagged as no explicit reply. A later `thread_reply` still delivers
   as a second wake, so a responder that ended its turn to wait on a
   command costs the caller nothing.
3. An errored turn settles `errored` with the error text.
4. `thread_cancel` settles `cancelled`; a restart of the responder's
   computer mid-turn settles `interrupted`.

A request settles once and its wake fires once. A spawned thread the
human keeps using afterwards never wakes the caller again; a new
`thread_send` with `notify` or a wait re-subscribes. For scratch threads
only, any non-running state counts as rest (there is no human to
unblock one), and the thread is deleted once its answer is stored.

### Wakes from another computer

Pairing is directional: the source holds a credential for the
destination, not the reverse, so the destination cannot push. The
source's remote watcher polls its outstanding cross-computer requests
through the same poller remote jobs use (outstanding rows only, four at
a time, slower retry after connection errors, resumed after a source
restart) and asks the destination for each request's settlement. A
settlement observed by any successful call (the poll, a parked wait, a
`thread_status`, a `thread_cancel`) is written immediately; the wake
then follows the local delivery rule above. Restart on either side
loses nothing: the source keeps polling, the destination keeps the
settlement until the source has taken it.

An answer waits for its caller for one day. A source that reconnects
within that window collects it and the wake lands then, its status line
naming the answer's age. After a day the destination drops the answer
and the source settles the request `expired` when it next asks, with a
wake that says so. Nothing is held indefinitely, and nothing needs a
lease: a scratch thread is deleted the moment its answer is stored,
whether or not the caller is around.

A target thread moved while a request is open: the old computer knows
the new owner, so the source re-addresses the request there and keeps
polling; the answer, when it comes, names the computer it came from. A
target deleted while a request is open settles `errored` with that
reason.

## Attribution (Q9, Q16)

A user row written by an agent carries `meta.origin = {computerId,
threadId, title}`. The UI renders a small "from <title>" chip on the
row, with "on <computer>" appended when the origin is another computer,
clickable through to that thread when the frontend is attached to its
computer and inert with the same label otherwise. It appears on a
spawn's first message, on every `thread_send`, and on every wake. Both
ends record it: the destination's thread shows where a message came
from, and the caller's wake shows where the answer came from.

## Availability and permissions (Q13, Q17, Q27)

Each computer has one switch, Settings → Agent thread tools, default on.
It means one thing: agents on this computer get the tools. It says
nothing about who may reach this computer's threads; pairing already
says which computers belong to the user, and a paired computer's agents
reach this one whether its switch is on or off.

The switch follows the browser-tools mechanism exactly. The server is
part of every interactive Claude and Codex session's MCP configuration,
and the switch turns it on or off inside the running provider: Claude
through the `mcp_toggle` control request, Codex through its MCP config
reload, the same calls `ApplyManagedServerEnabled` makes for
`ao-browser-tools` today. Off means the provider has no thread tools; on
means it has them; neither needs a session restart. The composer's MCP
menu disables the server for one conversation through the same call,
as it does for the other two servers (a revision of Q27's "no
per-thread toggle": there is no new mode, only the toggle the shared
transport already carries). A call from a session that races the flip
is refused with `thread_tools_disabled`.

Workflow phase sessions don't get it; their CLI stays grant-scoped.
Read-only sessions allowlist the server's tools by exact name so
`thread_reply` works under `dontAsk`. Every other runtime mode keeps its
normal behavior: approval-required prompts per call, auto reviews per
call (billed), full-access just runs. No spawn or wake-loop caps:
spawned threads are visible and stoppable from the sidebar, and
ephemeral ones are short.

### Reaching another computer

Cross-computer reach is own-device only: the computers in the user's
personal group, paired by the user, on the same trust the UI already
extends to them. It is never a team or federation peer; remote-access
§11 rules those read-only with no peer-triggered spawns, and this spec
does not change that.

A call to another computer succeeds when the caller's switch is on and
the pairing is live. The destination's own switch is not consulted.
Revoking the pairing ends everything between the two computers;
outstanding requests settle `errored` with that reason.

The peer methods are `ThreadToolCall` (tool name, arguments, and the
caller's thread identity) and `ThreadToolRequestStatus`, added to the
`CallAgentPeer` allowlist and advertised as a `thread-tools` capability
in the manifest so an older destination fails as `thread_unsupported`
("Update Agent Overflow on that computer") rather than with a confusing
method error. The destination authorizes every call with the
authenticated device of the originating computer (the same principal
that owns remote jobs there) and runs the handler with a caller scope
naming the source computer and thread. Read tools carry `threads:read`;
spawn, send, ask and cancel carry `terminal:operate`, the execute tier
an own-device peer session already holds. Each call rechecks all of
that; a tool-list omission is not an execution permission check.

Errors follow the remote-commands shape: stable codes, prose that names
the operation, the computer and the thread with the ids beside the
names so the model can retry with them, and raw causes kept in host
logs. A failed network reply does not establish whether a spawn or ask
happened; the token and `thread_status` do.

## Lifecycle interactions

- Interrupting the caller's turn ends its parked waits; the requests
  stay open and their answers arrive as messages.
- Deleting, archiving or moving the caller thread cancels the scratch
  asks it owns on every computer and drops their pending wakes. Spawned
  and sent threads are left alone; only the bindings are dropped. A copy
  waits for outstanding requests and wake handoff like the existing
  queued-work checks. The cancel is best-effort and logged when a
  destination cannot be reached; an unreachable scratch ask finishes on
  its own in read-only mode and its answer expires after a day.
- Forgetting a computer waits for outstanding requests and wake handoff;
  reconnect an offline computer and cancel first. Pairing credentials
  must remain available while a settlement is still to be collected.
- Flipping a computer's switch toggles the server in every live session
  on that computer except one holding an open request from another
  computer, which keeps it until that request settles. Its threads stay
  reachable from other computers either way.

## `/side-chat` (Q11, Q12, Q22)

A frontend action command (injects nothing), available whenever the
focused pane has a thread, including while that thread's turn is
running. Forks the thread at its tail, in-flight turn included, into a
`scratch` thread that keeps the source's provider, model, and runtime
mode (a human is present in this pane), and opens it in a companion pane
kind `side-chat` to the source's right. Modeled on `take-control`: never
persisted, added to `COMPANION_SHAPED_PANE_ID`, left out of
`isPersistedCompanionKind`. Closes with the source pane and when the
source pane's thread changes, like every companion; closing deletes the
thread. A Keep action in the pane header flips the thread to the
source's mode, which puts it in the sidebar, and converts the pane to a
normal thread pane in place. Works on any thread the frontend can show,
whichever computer owns it.

## Scratch lifecycle (Q10, Q12, Q21)

`scratch` joins `threadmode.hiddenModes` and the `mode` CHECK
constraint. Scratch threads are excluded from search, list, title
generation, and import. Deletion is `DeleteThread` on the DB rows;
provider session files are never touched (Q21: a discarded ask or side
chat leaves its forked provider file behind, accepted). Boot deletes
every scratch thread on that computer and settles any open request
bound to one as `interrupted`, since no side-chat pane persists and no
ask survives a restart of the computer running it; the caller's watcher
collects that settlement like any other.

## Key decisions

- MCP server, not a CLI or slash command; guide in `instructions` (Q1).
- FTS5 over settled summaries, background build, thinking unindexed
  (Q2, Q24).
- One `thread_search` for searching and listing; it covers every paired
  computer by default and `computers` narrows (Q3, Q14, Q20, Q23).
  Results group per computer.
- `thread_show` defaults to the prose tail; `include`, paging with
  `next`, and `to_file` reach everything stored, with no window the
  agent cannot finish. `thread_item` reads ranges and searches inside
  one large item (Q4, revised 2026-09-19).
- Spawn inherits everything, overrides each, can take a worktree (Q5,
  Q18). On another computer, project and checkout are explicit and the
  refusal lists the choices.
- No launch card; a spawn is a tool call, not a subagent (Q6).
- Completion means turn rest or error, never a pending approval (Q7).
- Calls may wait, up to fifteen minutes, then background; anything not
  collected inline arrives as a message that starts a turn (Q8 revised,
  Q10). `thread_status` re-attaches.
- Messages queue at the turn boundary, never steer (Q9).
- Reply tool with a flagged fallback at rest and late-reply delivery,
  because "busy" cannot distinguish a dev server from awaited work
  (Q25).
- Ephemeral asks run `read-only`; approval proxying to the caller is
  rejected (Q26).
- Ephemeral and side-chat forks take the tail, in-flight turn included.
- No caps of any kind (Q13, Q17). One switch per computer, default on,
  toggled inside live sessions like browser tools (Q27), plus the
  transport's existing per-conversation MCP toggle. A thread answering
  another computer's request has the server on regardless. All tools
  inside scratch threads (Q28).
- `thread_cancel` interrupts, never reverts, and is scoped to threads
  the caller spawned, sent to, or asked (Q19).
- Deletes touch AO rows only (Q21).
- Side chat follows companion rules, discards on close, opens mid-turn
  (Q12, Q22).
- A thread id is a complete address; resolution fans out on a local
  miss and follows a Move. Cross-computer reach is own-device only,
  gated by pairing and the caller's own switch. Source-minted tokens
  are the request identity; the source polls for settlement; answers
  wait one day for their caller.
- The handler runs where the thread lives; nothing about a thread is
  copied to another computer except a tool result, a wake body, or an
  export file the agent asked for.

## Non-goals

- A CLI or `/ao-tools` composer command for these tools.
- Launch cards, tray rows, or subagent treatment for spawned threads.
- Relaying approval requests to the calling agent.
- Spawn or wake-loop caps. A notify ping-pong in bypass mode runs until
  the human stops it from the sidebar; accepted.
- Deleting or editing provider session files.
- Indexing tool outputs, diffs, or thinking for search.
- A cross-computer search index or replica; each computer searches its
  own database.
- Changing the UI's global search semantics (`SearchThreadMessages` stays
  substring `LIKE`; switching it to FTS is a separate call).
- `open`/memory tools, workflow-phase access, team or federation peers.
- Moving or copying a thread between computers from these tools; that is
  the conversation-transfer protocol, from the UI.

## Spikes before building

- Claude honors `--allowedTools "mcp__ao-thread-tools__*"` (or the
  per-tool spelling) under `dontAsk`, and `thread_reply` runs without a
  prompt in a read-only session, including a scratch thread created by
  a request from another computer.
- Codex's read-only sandbox does not gate MCP tool calls.
- A fifteen-minute parked `thread_ask` returns cleanly on both
  providers under the existing call ceiling, as `remote_run` does.

## Success criteria

- [ ] Both providers list the nine tools in every interactive session,
      the server instructions read as a decision guide, and flipping the
      switch removes and restores them in a running session without a
      restart.
- [ ] `thread_search` finds a phrase from an imported Codex session and
      from an archived Claude thread in another project by default,
      never a scratch thread, and flags `indexing` while building;
      without a query it lists running threads with their state.
- [ ] `thread_search` returns rows from two isolated computers grouped
      per computer, `computers` narrows to one, and an offline third
      computer yields an `errors` row without failing the call.
- [ ] A bare thread id that lives on another computer resolves there;
      an ambiguous prefix is refused with candidates; a thread moved
      between computers resolves to its new owner.
- [ ] `thread_show` with `around` returns the surrounding turns within
      the byte budget on a 38k-item thread, locally and on another
      computer; `all` pages the same thread to its end through `next`;
      `to_file` with `include` everything writes the complete thread
      and returns a path the agent can read, from either computer.
- [ ] A multi-megabyte tool output shows clipped with its size in
      `thread_show`; `thread_item` finds a phrase inside it by `query`,
      reads the range around the match, and reads its last 16KB with a
      negative offset, locally and on another computer.
- [ ] `thread_spawn` with `worktree` and `notify` yields a sidebar thread
      on a new worktree whose first row carries the origin chip, and the
      caller receives a wake when it rests. The same on another computer
      with an explicit project, with the chip naming the computer on
      both ends; omitting the project lists that computer's projects.
- [ ] `thread_ask` with the default wait returns the answer inline when
      it arrives in time; a longer answer backgrounds and arrives as a
      message; `thread_status` on the token waits and returns it, and
      the queued message is dismissed.
- [ ] `thread_send` with notify into a mid-turn thread lands after the
      boundary with the draft intact; the responder's `thread_reply`
      wakes the caller; a rest-without-reply wake is flagged and a late
      reply still arrives, including a reply written while the caller's
      computer was unreachable.
- [ ] `thread_ask` on a full-access thread mid-turn produces a hidden
      read-only tail fork, `thread_reply` runs unprompted inside it, a
      write inside it is refused and reported, and the fork is gone
      once the answer is stored. The same against a thread on another
      computer, where the fork never leaves that computer.
- [ ] A lost peer reply to `thread_spawn` returns `unconfirmed` with the
      token; the retry with the same token does not spawn twice; a
      request the destination never accepted settles `refused`.
- [ ] An answer whose caller reconnects after an hour is delivered with
      its age; one whose caller stays away for a day expires on both
      sides.
- [ ] `thread_cancel` interrupts a spawned thread on either computer,
      settles `cancelled`, and refuses an unrelated thread.
- [ ] Deleting the caller cancels its scratch asks on another computer;
      a destination with its switch off still accepts spawns and asks,
      its responder can `thread_reply`, and the server is off again in
      that session once the request settles; revoking the pairing
      settles outstanding requests `errored`; an older destination
      fails as `thread_unsupported`.
- [ ] `/side-chat` opens a companion fork during a running turn,
      survives nothing across restart, closes with its source or a
      thread switch, and Keep promotes it to the sidebar in place.
- [ ] Boot removes every scratch thread and settles their requests.
- [ ] Streaming a long assistant message does no FTS work until the row
      settles.

## Testing strategy

- Store: FTS build, settle-time indexing (and none per append), the
  import table and its overrides, hidden and scratch exclusion, archived
  inclusion, `thread_requests` lifecycle on both the source and the
  destination side including token deduplication, settlement states and
  the one-day expiry.
- `internal/threadtools`: tool schemas, param parsing, result rendering,
  budgets and `next` paging, file export, item range reads and
  searches, per-computer grouping and
  error rows, id resolution and ambiguity, the instructions string,
  capability resolution, as unit tests against a fake app.
- App (kerneltest harness, mock providers, never a real CLI): server
  registration per session and its absence for phase sessions, the
  live switch on both providers, the read-only allowlist flags, spawn
  inheritance and overrides including worktree creation, send
  queue-versus-lazy-start, waits (inline settle, timeout to
  backgrounded, interrupt, `thread_status` re-attach and dismissal),
  reply token lifecycle (one reply, fallback at rest, late reply,
  errored turn, deleted caller, archived caller), scratch forcing
  `read-only`, tail fork mid-turn, deletion once stored, boot prune,
  cancel scoping.
- Two isolated computers over a real paired TLS connection, as the
  remote-command extended tests do: the caller's switch, a destination
  with its switch off still serving, ownership by device, id resolution
  fan-out, lost-reply retry with one token, unconfirmed reconciliation,
  expiry, settlement collection after a restart on either side,
  revocation, a moved target, and an older destination. No real
  providers, no real GPU or network hardware.
- Frontend: origin chip with and without a computer, inert chip when
  the computer is not attached, `side-chat` companion pane persistence
  and close rules, Keep promotion, the settings switch; Playwright in
  `e2e/` through the agent harness for the side-chat flow (including
  mid-turn) and a spawn/wake round trip, local and across two harness
  hosts.

## Open questions

None. Ruled 2026-09-19: one switch per computer that only governs what
that computer's agents start on their own, reach is pairing alone, and
a responder to another computer's request has the server on for that
request; search fans out to every paired computer by default and
`computers` narrows; `/side-chat` works on any thread; a thread id is a
complete address and requests follow a Move; answers wait a day for
their caller; calls may wait and then background like `remote_run`,
with the agent choosing the wait per call; `thread_cancel` stays scoped
to threads the caller started or asked (Q19); `thread_list`,
`thread_computers` and `thread_requests` are gone (listing folded into
`thread_search`, project discovery into the spawn refusal, request
recovery into `thread_status`).
