# Agent thread tools

Status: design, signed off 2026-09-04. Nothing implemented yet.
Decisions were settled in a brainstorm session; the `(Qn)` tags are
that session's ids and carry no other meaning.

## Goal

An agent in any interactive thread can search the whole thread history,
ask another thread a question (one-shot or continuing), and open new
threads, as seamlessly as if the human were coordinating those threads
by hand, with no restrictions beyond what its own thread already has.

## Approach

An in-process MCP server, `ao-thread-tools`, built the way
`ao-browser-tools` is (`internal/browser/mcp.go`): loopback HTTP, a
per-thread token in the URL, wired into Claude via `--mcp-config` and
Codex via `mcp_servers` at session start, with the decision guide in the
server's `instructions` string. Handlers resolve the token to the
calling thread and call the app in-process; nothing new goes on the
wire. Both providers get the tools identically.

Answers and outputs travel back as messages: a queued row in the calling
thread, delivered at its next turn boundary through the path workflow
wakes use (`deliverWorkflowWake`). Nothing blocks. An agent that asks
something ends its turn and reads the answer when it arrives.

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

## Tools

Eight tools. Long text (prompts, replies) travels as string params.
Thread ids accept an unambiguous prefix of six or more characters.

### `thread_search` (Q2, Q3, Q23, Q24)

FTS5 over `items.summary` for `user_text`, `assistant_text`, and
`tool_call` rows, plus thread titles, plus the imported-history table
behind the `timeline_items` view (import overrides honored). Rows enter
the index when they settle, never per streamed append: an update
trigger would re-tokenize the whole row on every chunk. Built in the
background on first boot after the migration; until it completes,
results carry `indexing: true` and cover what is indexed so far.
Thinking is not indexed: `items.summary` holds only its 400-rune tail,
and the full text is one `thread_show` away.

Defaults: every thread in the DB, all projects, archived included,
workflow-mode threads included, scratch threads excluded, 20 hits.
Params: `query`, `thread_id`, `kind` (user | assistant | tool | title),
`project`, `provider`, `since`, `limit`. A hit carries thread id, title,
project, provider, updated-at, matched item id, and a snippet.

### `thread_show` (Q4)

Plain transcript, turn-delimited, each row prefixed with role and item
id. Default content: user text, assistant text, and full thinking (from
`payloads.data`). Tool rows collapse to one line. `include` widens
(tools, outputs, diffs, subagents). Windowing: `turns` (last N), `since`
(item id or timestamp), `around` + `context` (pairs with a search hit).
A 64KB byte budget (`max_bytes`) truncates with a notice naming the
window params; it sits under Claude's MCP result cap, and 38k-item
threads exist, so it is load-bearing. Any thread by id, hidden workflow
threads included.

### `thread_list` (Q14)

Fields: id, title, project, provider, model, state, last activity,
branch. State is the UI's enum (idle / running / awaiting-input /
pending-approval / plan-ready / error / interrupted), reachable because
the handler runs inside the app. Defaults: active threads, all projects,
by last activity, 30 rows. Params: `project`, `provider`, `running`,
`archived`, `spawned_by_me`, `limit`.

### `thread_spawn` (Q5, Q6, Q18)

Creates a normal sidebar thread, sends `prompt` as its first user
message, starts the turn, returns the thread id. Inherits the caller's
project, workspace, provider, model, mode, and runtime mode; each has an
override param, plus `title`. `worktree` (optional branch name) creates
a fresh worktree through the existing draft-worktree path instead of
inheriting the workspace. `notify` (default false) appends the reply
footer and binds a wake. No launch card: the call renders as the tool
call it is, and the spawned thread is a normal thread with no sidebar
marking (Q16).

### `thread_send` (Q9, Q15)

Continues an existing thread as if the human had typed `message`.
Mid-turn, it queues for the turn boundary with the draft preserved;
idle, it sends now and lazily starts the session. `notify` appends the
reply footer and binds a wake. Works across projects.

### `thread_ask` (Q10, Q25, Q26, Q28)

One-shot. Forks the target at its tail, in-flight turn included (the
existing mid-turn tail fork, settled as interrupted), into a `scratch`
thread: hidden from the sidebar, runtime mode forced to `read-only`
regardless of the source's mode, no title generation, all eight tools
available. Sends `question` with the reply footer and returns the token
immediately. The answer arrives as a wake. The scratch thread is deleted
(DB rows only, Q21) once its wake is delivered. If the answer is a
clarifying question, the caller re-asks or switches to `thread_send` on
the real thread.

`read-only` exists for unattended work: writes and mutating commands are
refused immediately and the refusal goes straight back to the model on
both providers, so an ephemeral thread never waits on a human. If it
needed a write to answer, it says so in its reply.

### `thread_reply` (Q25)

The responder's half. The footer on every ask, `send` with notify, and
`spawn` with notify names the sender thread and this tool. A `token`
resolves to one pending request; one reply per token, a second is
refused with the reason. Unknown token: loud error.

### `thread_cancel` (Q19)

Interrupts the running turn of a thread the caller spawned or asked
(lineage from `thread_requests`, notify or not). Refused for any other
thread with the reason.

## Wakes (Q7, Q8, Q15, Q25)

A wake is a user-role row in the calling thread, badged with the source
thread (see Attribution), carrying a status line, the source thread's
id and title as a link, and a body capped at 24KB with a truncation
notice pointing at `thread_show`. Delivered by the workflow-wake path:
queued at the turn boundary when the caller is live, lazily starting a
session otherwise. A wake is a message, so an idle caller wakes up and
runs on its own when it lands, human present or not; that is what
`notify` opts into. An archived caller is unarchived by delivery. Caller
deleted first: dropped.

Per request, in order:

1. An explicit `thread_reply` delivers a `replied` wake with the reply
   text.
2. If the responder's turn rests first (turn completed or errored; a
   pending approval or question is the human's business and does not
   count, Q7), a `finished without reply` wake carries the final
   assistant text, flagged as such. A later `thread_reply` still
   delivers as a second wake, so a responder that ended its turn to wait
   on a command costs the caller nothing.
3. An errored turn delivers an `errored` wake with the error text.

A binding fires once. A spawned thread the human keeps using afterwards
never wakes the caller again; a new `thread_send` with notify
re-subscribes. For scratch threads only, any non-running state counts as
rest (there is no human to unblock one), and the thread is deleted after
its wake.

Bindings persist in a `thread_requests` table (token, caller thread,
target thread, kind, created, replied-at, fallback-delivered-at) so a
restart loses nothing. That table is also the spawn/ask lineage;
`ParentThreadID` is not reused, since it means "Codex subagent child"
and would mark the caller busy while its spawns run.

## Attribution (Q9, Q16)

A user row written by an agent carries `meta.origin = {threadId,
title}`. The UI renders a small "from <title>" chip on the row, clickable
through to that thread. It appears on a spawn's first message, on every
`thread_send`, and on every wake.

## Availability and permissions (Q13, Q17, Q27)

The server is on for every interactive session, behind one global
settings toggle (default on) beside the browser-tools toggle; no
per-thread toggle. Workflow phase sessions don't get it; their CLI stays
grant-scoped. Read-only sessions allowlist the server's tools by exact
name so `thread_reply` works under `dontAsk`. Every other runtime mode
keeps its normal behavior: approval-required prompts per call, auto
reviews per call (billed), full-access just runs. No spawn or wake-loop
caps: spawned threads are visible and stoppable from the sidebar, and
ephemeral ones are short.

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
normal thread pane in place.

## Scratch lifecycle (Q10, Q12, Q21)

`scratch` joins `threadmode.hiddenModes` and the `mode` CHECK
constraint. Scratch threads are excluded from search, list, title
generation, and import. Deletion is `DeleteThread` on the DB rows;
provider session files are never touched (Q21: a discarded ask or side
chat leaves its forked provider file behind, accepted). Boot deletes
every scratch thread, since no side-chat pane persists and no ask is in
flight across a restart.

## Key decisions

- MCP server, not a CLI or slash command; guide in `instructions` (Q1).
- FTS5 over settled summaries, background build, thinking unindexed
  (Q2, Q24).
- Search covers every thread in the DB by default; params narrow (Q3,
  Q20, Q23).
- `thread_show` defaults to prose plus thinking; `include` reaches
  everything (Q4).
- Spawn inherits everything, overrides each, can take a worktree (Q5,
  Q18).
- No launch card; a spawn is a tool call, not a subagent (Q6).
- Completion means turn rest or error, never a pending approval (Q7).
- Nothing blocks; everything returns as a message that starts a turn
  (Q8, Q10).
- Messages queue at the turn boundary, never steer (Q9).
- Reply tool with a flagged fallback at rest and late-reply delivery,
  because "busy" cannot distinguish a dev server from awaited work
  (Q25).
- Ephemeral asks run `read-only`; approval proxying to the caller is
  rejected (Q26).
- Ephemeral and side-chat forks take the tail, in-flight turn included.
- No caps of any kind (Q13, Q17). Global toggle, default on (Q27). All
  eight tools inside scratch threads (Q28).
- `thread_cancel` scoped to threads the caller spawned or asked (Q19).
- Deletes touch AO rows only (Q21).
- Side chat follows companion rules, discards on close, opens mid-turn
  (Q12, Q22).

## Non-goals

- A CLI or `/ao-tools` composer command for these tools.
- Blocking waits, launch cards, or subagent treatment for spawned
  threads.
- Relaying approval requests to the calling agent.
- Spawn or wake-loop caps. A notify ping-pong in bypass mode runs until
  the human stops it from the sidebar; accepted.
- Deleting or editing provider session files.
- Indexing tool outputs, diffs, or thinking for search.
- Changing the UI's global search semantics (`SearchThreadMessages` stays
  substring `LIKE`; switching it to FTS is a separate call).
- Per-thread toggles, `notify`/`open`/memory tools, workflow-phase
  access.

## Spikes before building

- Claude honors `--allowedTools "mcp__ao-thread-tools__*"` (or the
  per-tool spelling) under `dontAsk`, and `thread_reply` runs without a
  prompt in a read-only session.
- Codex's read-only sandbox does not gate MCP tool calls.

## Success criteria

- [ ] Both providers list the eight tools in every interactive session,
      the server instructions read as a decision guide, and the global
      toggle removes them.
- [ ] `thread_search` finds a phrase from an imported Codex session and
      from an archived Claude thread in another project by default,
      never a scratch thread, and flags `indexing` while building.
- [ ] `thread_show` with `around` returns the surrounding turns within
      the byte budget on a 38k-item thread.
- [ ] `thread_spawn` with `worktree` and `notify` yields a sidebar thread
      on a new worktree whose first row carries the origin chip, and the
      caller receives a wake when it rests.
- [ ] `thread_send` with notify into a mid-turn thread lands after the
      boundary with the draft intact; the responder's `thread_reply`
      wakes the caller; a rest-without-reply wake is flagged and a late
      reply still arrives.
- [ ] `thread_ask` on a full-access thread mid-turn produces a hidden
      read-only tail fork, `thread_reply` runs unprompted inside it, a
      write inside it is refused and reported, and the fork is gone
      after the wake.
- [ ] `thread_cancel` stops a spawned thread and refuses an unrelated
      one.
- [ ] `/side-chat` opens a companion fork during a running turn,
      survives nothing across restart, closes with its source or a
      thread switch, and Keep promotes it to the sidebar in place.
- [ ] Boot removes every scratch thread.
- [ ] Streaming a long assistant message does no FTS work until the row
      settles.

## Testing strategy

- Store: FTS build, settle-time indexing (and none per append), the
  import table and its overrides, hidden and scratch exclusion, archived
  inclusion, `thread_requests` lifecycle.
- `internal/threadtools` (the MCP server): tool schemas, param parsing,
  result rendering and budgets, the instructions string, token
  resolution, as unit tests against a fake app.
- App (kerneltest harness, mock providers, never a real CLI): server
  registration per session and its absence for phase sessions, the
  read-only allowlist flags on both providers, spawn inheritance and
  overrides including worktree creation, send queue-versus-lazy-start,
  reply token lifecycle (one reply, fallback at rest, late reply,
  errored turn, deleted caller, archived caller), scratch forcing
  `read-only`, tail fork mid-turn, deletion after wake, boot prune,
  cancel scoping.
- Frontend: origin chip, `side-chat` companion pane persistence and
  close rules, Keep promotion, the settings toggle; Playwright in `e2e/`
  through the agent harness for the side-chat flow (including mid-turn)
  and a spawn/wake round trip.
