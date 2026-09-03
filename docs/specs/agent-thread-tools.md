# Agent thread tools

Status: design, awaiting sign-off. Nothing implemented yet. Decisions
were settled in a brainstorm session; the `(Qn)` tags are that session's
ids and carry no other meaning.

## Goal

An agent in any interactive thread can search the whole thread history,
ask another thread a question (one-shot or continuing), and open new
threads, through the same CLI a human would use and with no restrictions
beyond what its own thread already has.

## Approach

One composer command, `/ao-tools`, expands at send time (the `/workflow`
mechanism, `app_composer_commands.go`) into a decision guide that points
the agent at `agent-overflow thread <verb>`. Each verb is an App method
on the scoped-token allow-list (`transport.ScopedTokenMethods`); the
caller's thread comes from the token via `CallerScopeFrom(ctx)`, never
from an argument. Every session already carries `AO_ENDPOINT`,
`AO_TOKEN`, and `AO_THREAD_ID`, so both providers get the verbs for free.

Answers and outputs travel back as messages: a queued row in the calling
thread, delivered at its next turn boundary through the same path
workflow wakes use (`deliverWorkflowWake`). Nothing blocks. An agent that
asks something ends its turn and reads the answer when it arrives.

Ephemeral asks are hidden forks (`scratch` thread mode) forced into the
`read-only` runtime mode. `/side-chat` is the human's version of the same
fork: a companion pane, discarded on close.

## Surface

### `/ao-tools` (Q1)

Composer entry in `composerCommands` and `SLASH_COMMANDS`. The expanded
block is a decision guide, not a verb list: when to `search` versus
`show`, `ask` (one question, read-only, answer arrives as a message)
versus `send` (continue a thread, optionally get its answer back) versus
`spawn` (new work, optionally notified), the reply obligation when a
message arrives with a reply token, and the fact that answers arrive at
the next turn boundary so the agent should finish its turn after asking.
`agent-overflow thread <verb> --help` carries flag-level detail.

### `thread search "query"` (Q2, Q3, Q23, Q24)

FTS5 over `items.summary` for `user_text`, `assistant_text`, and
`tool_call` rows, plus thread titles, plus the imported-history table
that backs the `timeline_items` view (import overrides honored). Kept in
sync by triggers; built in the background on first boot after the
migration, and `search` reports that state until the build completes.
Thinking is not indexed: `items.summary` holds only its 400-rune tail,
and the full text is one `show` away once a thread is found.

Defaults: every thread in the DB, all projects, archived included,
workflow-mode threads included, scratch threads excluded, 20 hits.
Filters: `--thread <id>`, `--kind user|assistant|tool|title`,
`--project <slug>`, `--provider`, `--since`, `--limit`. A hit prints
thread id, title, project, provider, updated-at, matched item id, and a
snippet.

### `thread show <id>` (Q4)

Plain transcript, turn-delimited, each row prefixed with role and item
id. Default content: user text, assistant text, and full thinking (from
`payloads.data`). Tool rows collapse to one line. `--include
tools,outputs,diffs,subagents` widens. Windowing: `--turns N` (last N),
`--since <itemId|ts>`, `--around <itemId> --context N` (pairs with a
search hit). A 64KB byte budget (`--max-bytes`) truncates with a notice
naming the window flags; 38k-item threads exist and this budget is
load-bearing. Any thread by id, hidden workflow threads included.

### `thread list` (Q14)

Columns: id, title, project, provider, model, state, last activity,
branch. State is the UI's enum (idle / running / awaiting-input /
pending-approval / plan-ready / error / interrupted), reachable because
the RPC executes inside the app. Defaults: active threads, all projects,
by last activity, 30 rows. Filters: `--project`, `--provider`,
`--running`, `--archived`, `--spawned-by-me`, `--limit`. Thread ids
accept an unambiguous prefix of six or more characters everywhere.

### `thread spawn` (Q5, Q6, Q18)

`thread spawn "prompt"` (stdin for long prompts). Creates a normal
sidebar thread, sends the prompt as its first user message, starts the
turn, prints the thread id. Inherits the caller's project, workspace,
provider, model, mode, and runtime mode; each has an override flag.
`--worktree [branch]` creates a fresh worktree through the existing
draft-worktree path instead of inheriting the workspace. `--notify`
(default off) appends the reply footer and binds a wake. No launch card:
the call renders as the Bash call it is.

### `thread send <id> "message"` (Q9, Q15)

Continues an existing thread as if the human had typed it. Mid-turn, it
queues for the turn boundary with the draft preserved; idle, it sends
now and lazily starts the session. `--notify` appends the reply footer
and binds a wake. Works across projects.

### `thread ask <id> "question"` (Q10, Q25, Q26)

One-shot. Forks the target at its last settled turn boundary (the
existing pinned-cut fork; mid-turn targets are fine) into a `scratch`
thread: hidden from the sidebar, runtime mode forced to `read-only`
regardless of the source's mode, no title generation. Sends the question
with the reply footer and returns immediately with the token. The
answer arrives as a wake. The scratch thread is deleted (DB rows only,
Q21) once its wake is delivered. If the answer is a clarifying question,
the caller re-asks or switches to `send` on the real thread.

`read-only` exists for unattended work: writes and mutating commands are
refused immediately and the refusal goes straight back to the model on
both providers, so an ephemeral thread never waits on a human. If it
needed a write to answer, it says so in its reply.

### `thread reply <token> "answer"` (Q25)

The responder's half. The footer on every `ask`, `send --notify`, and
`spawn --notify` message names the sender thread and this command; stdin
for long answers. A token resolves to one pending request; one reply per
token, a second is refused with the reason. Unknown token: loud error.

### `thread cancel <id>` (Q19)

Interrupts the running turn of a thread the caller spawned or asked.
Refused for any other thread with the reason.

### Wakes (Q7, Q8, Q15, Q25)

A wake is a user-role row in the calling thread, badged with the source
thread (see Attribution), carrying a status line, the source thread's
id and title as a link, and a body capped at 24KB with a truncation
notice pointing at `thread show`. Delivered by the workflow-wake path:
queued at the turn boundary when the caller is live, lazily starting a
session otherwise. Caller deleted first: dropped.

Per request, in order:

1. An explicit `thread reply` delivers a `replied` wake with the reply
   text.
2. If the responder's turn rests first (turn completed or errored; a
   pending approval or question is the human's business and does not
   count, Q7), a `finished without reply` wake carries the final
   assistant text, flagged as such. A later `reply` still delivers as a
   second wake, so a responder that ended its turn to wait on a command
   costs the caller nothing.
3. An errored turn delivers an `errored` wake with the error text.

For scratch threads only, any non-running state counts as rest (there
is no human to unblock one), and the thread is deleted after its wake.

Bindings persist in a `thread_requests` table (token, caller thread,
target thread, kind, created, replied-at, fallback-delivered-at) so a
restart loses nothing.

### Attribution (Q9, Q16)

A user row written by an agent carries `meta.origin = {threadId,
title}`. The UI renders a small "from <title>" chip on the row, clickable
through to that thread. It appears on a spawn's first message, on every
`send`, and on every wake. Spawned threads are otherwise normal threads
with no sidebar marking.

### `/side-chat` (Q11, Q12, Q22)

A frontend action command (injects nothing). Forks the focused pane's
thread at its last settled turn boundary into a `scratch` thread that
keeps the source's provider, model, and runtime mode (a human is present
in this pane), and opens it in a companion pane kind `side-chat` to the
source's right. Modeled on `take-control`: never persisted, added to
`COMPANION_SHAPED_PANE_ID`, left out of `isPersistedCompanionKind`.
Closes with the source pane and when the source pane's thread changes,
like every companion; closing deletes the thread. A Keep action in the
pane header flips the thread to the source's mode, which puts it in the
sidebar, and converts the pane to a normal thread pane in place.
Unavailable when the focused pane has no thread.

### Scratch lifecycle (Q10, Q12, Q21)

`scratch` joins `threadmode.hiddenModes` and the `mode` CHECK
constraint. Scratch threads are excluded from search, list, title
generation, and import. Deletion is `DeleteThread` on the DB rows;
provider session files are never touched (Q21: a discarded ask or side
chat leaves its forked provider file behind, accepted). Boot deletes
every scratch thread, since no side-chat pane persists and no ask is in
flight across a restart.

## Key decisions

- `/ao-tools` is one command carrying a decision guide (Q1).
- FTS5 over summaries, background build, thinking unindexed (Q2, Q24).
- Search covers every thread in the DB by default; filters narrow (Q3,
  Q20, Q23).
- `show` defaults to prose plus thinking; flags reach everything (Q4).
- Spawn inherits everything, overrides each, can take a worktree (Q5,
  Q18).
- No launch card; a spawn is a Bash call, not a subagent (Q6).
- Completion means turn rest or error, never a pending approval (Q7).
- No blocking verbs; everything returns as a message (Q8, Q10).
- Messages queue at the turn boundary, never steer (Q9).
- Reply tool with a flagged fallback at rest and late-reply delivery,
  because "busy" cannot distinguish a dev server from awaited work
  (Q25).
- Ephemeral asks run `read-only`; approval proxying to the caller is
  rejected (Q26).
- No caps of any kind; spawned threads are visible and stoppable,
  ephemeral ones are short (Q13, Q17).
- `cancel` scoped to threads the caller spawned or asked (Q19).
- Deletes touch AO rows only (Q21).
- Side chat follows companion rules and discards on close (Q12, Q22).
- Verbs are interactive-scope only; workflow phase sessions keep their
  grant-scoped surface and get `method_not_found` as today.

## Non-goals

- Blocking waits, launch cards, or subagent treatment for spawned
  threads.
- Relaying approval requests to the calling agent.
- Spawn or wake-loop caps. A `--notify` ping-pong in bypass mode runs
  until the human stops it from the sidebar; accepted.
- Deleting or editing provider session files.
- Indexing tool outputs, diffs, or thinking for search.
- Changing the UI's global search semantics (`SearchThreadMessages` stays
  substring `LIKE`; switching it to FTS is a separate call).
- `notify`, `open`, or memory verbs; workflow-phase access.

## Success criteria

- [ ] `/ao-tools` appears in both providers' slash menus, expands once
      per message, and the block reads as a decision guide.
- [ ] `thread search` finds a phrase from an imported Codex session and
      from an archived Claude thread in another project by default,
      never a scratch thread, and reports the index state while
      building.
- [ ] `thread show --around <hit>` returns the surrounding turns within
      the byte budget on a 38k-item thread.
- [ ] `thread spawn --worktree feature-x --notify` yields a sidebar
      thread on a new worktree whose first row carries the origin chip,
      and the caller receives a wake when it rests.
- [ ] `thread send --notify` into a mid-turn thread lands after the
      boundary with the draft intact; the responder's `thread reply`
      wakes the caller; a rest-without-reply wake is flagged and a late
      reply still arrives.
- [ ] `thread ask` on a full-access thread produces a hidden read-only
      fork, a write inside it is refused and reported in the reply, and
      the fork is gone after the wake.
- [ ] `thread cancel` stops a spawned thread and refuses an unrelated
      one.
- [ ] `/side-chat` opens a companion fork, survives nothing across
      restart, closes with its source or a thread switch, and Keep
      promotes it to the sidebar in place.
- [ ] Boot removes every scratch thread.
- [ ] Every new App method is in `ScopedTokenMethods`, and a remote
      client or phase token gets `method_not_found`.

## Testing strategy

- Store: FTS build and trigger sync over `items` and the import table,
  override honoring, hidden and scratch exclusion, archived inclusion,
  `thread_requests` lifecycle.
- `internal/aocli`: verb and flag parsing, output rendering, `--help`
  and the `/ao-tools` block as snapshot tests.
- App (kerneltest harness, mock providers, never a real CLI): scoped
  authorization per method, spawn inheritance and overrides including
  worktree creation, send queue-versus-lazy-start, reply token
  lifecycle (one reply, fallback at rest, late reply, errored turn,
  deleted caller), scratch forcing `read-only`, deletion after wake,
  boot prune, cancel scoping.
- Frontend: slash-menu entry, origin chip, `side-chat` companion pane
  persistence and close rules, Keep promotion; Playwright in `e2e/`
  through the agent harness for the side-chat flow and a spawn/wake
  round trip.
