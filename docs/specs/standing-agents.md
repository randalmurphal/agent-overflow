# Standing agents

Status: design signed off 2026-10-10. Not implemented. The build is split
into the slices in [Slices and verification](#slices-and-verification);
each lands as its own pull request against the `personal-assistant`
branch and nothing merges without the user's review.

## Goal

A thread the user promotes becomes a standing agent: it keeps a home of
its own, wakes itself on a schedule or on events its own scripts detect,
spawns and steers worker threads, and brings the user decisions and
evidence rather than progress. The agent builds its own scripts, notes,
memory and skills and asks before widening what it is allowed to do. It
runs whenever the app runs, headless when no pane is open, and across
app restarts without losing a scheduled wake.

Agent Overflow provides the few things an agent cannot give itself:
identity that outlives a session, a wake supervisor, a home, a settings
page that installs a skills library, and a sidebar that does not drown
in worker rows. Everything else, including what the agent watches, what
it remembers and how it verifies, is instructions and scripts the agent
and the user maintain. The app knows nothing about Jira, GitLab or any
other source.

The skills library is a vendored copy of pstack ported to the thread
tools, so a Claude thread and a Codex thread behave the same. The app
works without it installed.

## Decisions

Rulings from the design session, 2026-10-10. Each is binding until the
user changes it.

1. **Instructions, not enforcement.** The agent's grants live in its
   own instructions with scope and date; the user grants by telling it;
   it asks before any new outward or destructive action. No approval
   proxy, no ask-mode, no tool allowlist. If slips recur, the agent
   writes its own PreToolUse hook; the app adds nothing.
2. **The agent builds its own systems.** The app never ships a Jira
   poller, an MR watcher, a memory format or a prescribed home layout.
   It runs what the agent registers and delivers the output.
3. **Subagents or threads, never a third thing.** Work that belongs to
   the thread's own task (a review panel over its diff, a verifier of
   its change, a swarm over its investigation) runs as the provider's
   native subagents, shown in the agent pane as today. Work that is
   someone else's job and outlives the turn (an owner carrying an MR, a
   review the user will talk to, a worker on the other provider) is a
   thread spawned under the agent. The sidebar shows only the second
   kind.
4. **The agent is the group.** A standing agent's row folds the threads
   it spawned, one header instead of a group row plus a pinned thread.
   Groups stay for gathering threads nobody spawned. An agent row never
   sits inside a group, and a group never contains an agent's children;
   promoting the MR lead replaces its "MR Reviews" group with the fold.
5. **Done folds away.** A worker the agent archives leaves the row list
   and counts in a "done" footer under the agent that lists every
   archived child, newest first. Archive is the done state; no new
   state is added.
6. **Only the user promotes.** A thread becomes a standing agent
   through a row action. Agents cannot promote themselves or each other,
   and a spawned thread is never an agent unless the user promotes it.
7. **Home outside the repo.** Each agent owns
   `<configDir>/agents/<thread-id>/`. Its working directory stays the
   project it serves. Nothing in the home is committed anywhere. An
   agent with no project (a cross-project coordinator) has a home and no
   workspace.
8. **Compaction is enough for working memory.** The thread keeps its
   conversation; the provider compacts it. Durable facts (rulings,
   grants, cursors, decision log, its skills) live in the home because
   the agent writes them there, not because the app extracts them. A
   degraded thread is replaced by spawning a new thread on the same home
   and promoting it; no replacement-session mechanism.
9. **Routine ticks are silent.** A loop or watch turn never raises an
   OS or push notification by default. Awaiting input, approval and
   error notify as today. A per-agent setting can turn tick
   notifications on.
10. **No budget guard.** The app enforces no spend cap on agents; usage
    stays visible in the existing footer and the agent's instructions
    carry any limit.
11. **Watches wake on events.** An agent registers a script of its own
    plus either an interval or a long-running mode where each stdout
    line is an event. The app supervises it and wakes the thread with
    the output. Polling on each tick is the first thing an agent will
    do; the long-running mode is what makes event-driven wakes possible
    with the same scripts.
12. **Watch output is data.** A watch or loop wake is delivered as a
    badged event wrapped as untrusted text, size-capped with the
    truncation stated, never as the user speaking and never able to
    carry an approval. The script runs as the agent already can, with no
    app-held credentials.
13. **One clock.** The wake supervisor is the app's only scheduler. The
    workflow scheduler and the `automations` table are removed. A
    scheduled workflow run is an agent whose loop starts the run.
14. **Reminder semantics, not cron semantics.** Loops and watches are
    store rows, rebuilt on boot. A fire missed while the app was closed
    replays once on boot and says so; a tick cancelled by quit is
    recorded and rerun; a script whose output never reached the queue is
    treated as not run.
15. **Discussions are removed.** The discussion feature (its packages,
    tables, thread mode, settings page, keybindings and the sidebar
    nesting on `parent_thread_id`) has no place in the product. Agent
    nesting is derived from spawn records, not from a parent column.
16. **Skills are optional and user-installed.** The app ships the
    vendored skills tree; a settings page installs or removes it into
    both provider homes. Nothing installs automatically, and every
    feature above works with no skills installed.
17. **Workflows move to an MCP tool.** The workflow verbs leave the
    `agent-overflow` CLI for an MCP tool the app manages. `service`,
    `pair`, `remote` and `serve` stay as CLI. This reverses the
    CLI-first ruling in `workflows-system-decisions.md` D15; the slice
    that lands it records the superseding decision there.
18. **Verification is e2e or evidence.** A slice is verified by a
    Playwright or harness spec, a screenshot on the pull request, or
    described proof against the real app. Unit tests may exist for the
    few things that are unit-shaped and never stand as a feature's
    verification.

## Non-goals

- A chief-of-staff agent as a distinct kind. A coordinator over several
  domains is a standing agent with a home and no workspace, added when
  addressing domain agents by hand becomes a chore.
- Inbound webhooks, Slack, email or calendar sources.
- Cross-computer agents. An agent lives on one computer; its workers may
  be remote through the existing thread tools.
- A general memory subsystem, an inbox pane, action-rule enforcement, or
  a Jira or Confluence client in Go.
- Lanes, hidden spawns or any subagent surface beyond what the
  providers have.
- Running while the app is closed. `agent-overflow serve` already covers
  the always-on case.
- Replacing the provider's compaction.

## Design

### Standing agent

A standing agent is a thread with a row in `standing_agents`:

```sql
CREATE TABLE standing_agents (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    notify_on_tick   INTEGER NOT NULL DEFAULT 0 CHECK(notify_on_tick IN (0,1)),
    promoted_at      INTEGER NOT NULL
);
```

Promotion (`PromoteStandingAgent`) creates the home directory, writes a
`README.md` naming the agent, its thread id, its project and the tools
it can register with, and records the row. Demotion removes the row and
leaves the home on disk; the row action says so. Promotion is refused
for an archived thread, a thread with a running workflow binding, or a
thread on another computer.

The home path and the agent's standing instructions reach the provider
through the existing per-thread instruction injection used by the
thread tools server (the `instructions` string for Claude, developer
instructions on thread start for Codex). The injected block is short: the
home path, the thread id, and the sentence that the agent owns the
directory and keeps its durable state there. No layout is prescribed.

Deleting the thread deletes the agent row, its loops and watches; the
home is removed only through the row action that asks.

### Sidebar

The agent row is a new row kind rendered by `ThreadRow` with an agent
variant, not a new component. Differences from an ordinary thread row:

- Leading person glyph after the chevron. The chevron is the only fold
  target; name, dot and state line open the thread. Keyboard: Enter
  opens, Right and Left fold and unfold, the same as a project row.
- A state line under the name, 10px, one line, truncating from the
  right: `until 10:30 · watching 4 MRs · 2 need you`,
  `loop board check · next 12m · 1 missed`, or nothing when the agent
  is idle with no loop or watch. The text comes from the supervisor
  (next fire, missed count) and from the agent's own registered labels
  (what a watch is called). The dot keeps the live status; the state
  line never repeats it.
- Children: the threads this agent spawned, derived from
  `thread_requests` rows of kind spawn whose `caller_thread_id` is the
  agent, joined to the resulting thread id. A thread the agent did not
  spawn never nests under it. A spawned thread that is itself promoted
  leaves the fold and becomes a top-level agent row.
- Children sort by the normal comparator (needs-you first, then running,
  then activity). Archived children do not render; their count renders
  as a footer row `N done` that expands to list them inline, newest
  first, styled as the existing show-more footer and paged by the same
  preview limit. Unarchiving a child returns it to the fold.
- Collapsed: the row shows the state line and bubbles status for sort
  exactly as a group does. The focused child is previewed beneath a
  collapsed agent, as a group does.
- Drag: a thread dropped on an agent row is not adopted (ruling 3 and
  the spawn-record source). The drop is a no-op with the same affordance
  as dropping on another project. Dragging an agent row onto a group is
  refused the same way.
- Groups: an agent row renders in the groups section, above the pin
  blocks, sorted among groups by the same bubbled status. It cannot join
  a group, and its children are not group members. Promoting a thread
  that is in a group removes it from that group; if that leaves the
  group holding only the agent's own children, the group is deleted and
  those children stay under the agent.
- Context menu: Open, Rename, Demote, Loops and watches (opens the
  activity rail of that thread), Archive done children, the normal pin
  items.

Discussions are removed first (slice 1), and the tree code that nests
on `parent_thread_id` goes with them. The agent tree is new code in
`sidebarTree.ts` with its own tests against spawn records.

### Wake supervisor

The thread request sweep (`app_thread_tools_poll.go`) already owns the
reminder clock: a `thread_requests` row of kind remind, fired by `due_at`,
replayed at boot when missed. It grows into the wake supervisor by
driving one more table on the same sweep, and the workflow scheduler is
deleted (ruling 13). Reminders stay on `thread_requests`; their contract,
tests and the transfer guard are untouched.

```sql
CREATE TABLE agent_wakes (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK(kind IN ('loop','watch')),
    label            TEXT NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    command          TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    interval_seconds INTEGER,
    long_running     INTEGER NOT NULL DEFAULT 0 CHECK(long_running IN (0,1)),
    next_at          INTEGER,
    last_run_at      INTEGER,
    last_outcome     TEXT NOT NULL DEFAULT '',
    missed           INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK(state IN ('armed','running','cancelled')),
    created_at       INTEGER NOT NULL
);
CREATE INDEX idx_agent_wakes_due ON agent_wakes(state, next_at);
```

Kinds:

- `loop`: fires every `interval_seconds` with `note`. Any thread may
  register one; it is not limited to standing agents.
- `watch`: runs `command` in `cwd` (default: the thread's workspace)
  with the agent's home in `AO_AGENT_HOME`. Interval mode runs it every
  `interval_seconds` and wakes the thread when stdout is non-empty.
  Long-running mode keeps it running and wakes the thread once per
  stdout line, batching lines that arrive within one second. Stderr is
  captured and shown with the wake; a non-zero exit is an outcome, not
  a wake, and shows as an error on the row. Watches are limited to
  standing agents.

Delivery queues the wake into the thread's message queue through the
path `thread_remind` uses today, so a thread with no live session starts
one headless. The queued message is badged with the wake's kind and
label, wrapped as untrusted text with a stated byte cap (64 KiB, with
the dropped byte count named when it truncates), and carries no
approval or user authority. A wake delivered while a turn is open waits
for the turn, as a `thread_send` does.

Restart rules:

- On boot the sweep loads every `armed` or `running` row.
- An interval `loop` or `watch` whose `next_at` is in the past fires
  once with `missed` set to how many fires elapsed, and the wake says
  `missed N while the app was closed`. A reminder keeps its existing
  fire-at-boot behavior.
- A row in `running` at boot was cut by quit: its outcome is recorded as
  `cancelled-by-quit` and it runs again now.
- A long-running `watch` is restarted from scratch. Its cursor is the
  script's own business in the home; the app holds none.
- Output that was produced but not queued before quit is lost by
  design, and the row's outcome says `not delivered`; the script's own
  cursor decides whether the next run repeats it.
- `agent-overflow serve` runs the sweep the same way; the window never
  owns it.

The idle-session reaper gains nothing: a wake outside the session does
not need the session alive. The existing protection for an in-process
Claude `ScheduleWakeup` stays, and `CronCreate` acks are recognized the
same way (slice 4) so a provider-side loop cannot be reaped either.

Thread tools:

- `thread_remind` is unchanged.
- `thread_loop {label, note, every_seconds | stop}`: one loop per label
  per thread; registering the same label replaces the interval; `stop`
  cancels. Returns the next fire time.
- `thread_watch {label, command, every_seconds | long_running, cwd?,
  stop}`: standing agents only; an ordinary thread is refused with the
  reason. One watch per label.
- `thread_status` lists the caller's loops and watches with next fire,
  last outcome and missed count alongside its open requests.
- `thread_cancel` by label cancels a loop or watch.

An outside poke, `agent-overflow thread send <thread-id> <text>`, queues
a message to a local thread through the same badged, untrusted path, for
scripts run by anything other than the supervisor. It is a CLI verb
because its caller is not a session.

### Activity rail

Loops, watches and reminders of the open thread render in the composer's
existing background tray (`ActivityRailBackgroundBody`), one row each:
kind glyph, label, `next 12m` or `running`, last outcome, a `Wake now`
action that fires it immediately and a `Cancel` action. The tray toggle
counts them with the other background work. Expanding a watch row shows
its command, cwd and the tail of its last stderr. The tool call that
registered it renders in the timeline like any thread tool call, with
the next fire time in the summary line; today it renders nothing.

### Skills library

The vendored tree lives in the repository at `assets/skills/pstack/`
with `UPSTREAM.md` pinning the upstream commit and listing every edit
made for the port. Syncing is a manual diff against upstream; nothing
pulls.

The port rewrites every host primitive to a thread tool so both
providers run identical skill text:

| pstack names | Port uses |
|---|---|
| `/loop`, heartbeat, `/loop 1h` | `thread_loop`; a watcher subagent becomes `thread_watch` |
| Task-tool model roles, cloud agents | native subagents for the thread's own work; `thread_spawn` with provider, model, effort, `worktree` for work that outlives the turn (ruling 3) |
| transcripts under the agent store | `thread_show` and `thread_search` |
| Cursor dashboard liveness | `thread_status`, `thread_ask` |
| `control-ui`, `control-cli`, verification live lane | `ao-browser-tools` |
| `gh` in `watch-pr`, Babysit, Shipping, the autopilots | one `forge` script choosing `gh` or `glab` from the remote |
| `.mdc` always-apply rule, SessionStart hook | the agent's own instructions file |
| `make-bot-ui`, Benny's Slack triggers | not ported; Benny's triage and reproduce skills are ported as skills an agent runs from a watch |

The settings page `Skills` lists the shipped tree with its upstream
version, shows whether it is installed in each provider home
(`<claude home>/skills/pstack`, `<codex home>/skills/pstack`), and
offers Install, Update and Remove per provider. Install copies; it never
symlinks into the repository. Remove deletes only what Install wrote,
by manifest.

### Workflows to MCP

The `workflow`, `run`, `notes`, `memory` and `schedule` CLI verbs are
replaced by an `ao-workflow-tools` MCP server on the shared
`internal/threadmcp` transport, scoped the way the thread tools are. The
phase runner's injected credentials become the per-session capability
URL, and phase grants become the tool allowlist for that session. The
`schedule` verb has no replacement (ruling 13). `aocli` keeps `service`,
`pair`, `remote` and the serve entry. Starters, docs and the runner
guidance that mention `agent-overflow run` are rewritten. The decisions
log records the reversal of D15 with the user's reasoning: app-managed
tooling, one surface for agents, no separate binary to keep in sync.

### Claude wire gaps

Two defects observed in the wire reference that an agent's loop would
hit:

- `CronCreate` acks are not classified, so the idle reaper can close a
  session holding a provider-side fixed-interval loop. Classify them as
  `EventSessionWakeup` with the next fire time, mirroring
  `ScheduleWakeup`.
- A background completion that lands on an idle session renders a
  response starting from nothing (claude-wire.md E7). The reference
  says a transcript-tail backfill is not built; the scoped fix is to
  render the completion as a badged system row carrying the task's
  result and name, so the pane shows what woke the session rather than
  an unexplained response. A full backfill stays an open question.

The `CronCreate` ack shape has no captured fixture. Slice 4 starts with
a spike against the real CLI under the isolated spike policy that
records the ack into `docs/references/fixtures/claude/`, and the
classification is written against that capture.

## Slices and verification

Each slice is one pull request. Its owner never verifies its own work:
a fresh verifier re-runs the named proofs on the exact head and attaches
the evidence before review. Evidence means a Playwright or harness spec
by name with its run output, screenshots copied out of
`e2e/test-results` into the pull request, or a described proof against
the real app with the commands run. Unit tests are not evidence.

| # | Slice | Depends on | Done means | Evidence |
|---|---|---|---|---|
| 1 | Remove discussions | none | No reference to discussions in Go, frontend, docs, keybindings or schema; the sidebar renders every existing thread flat; `make go-test`, `pnpm run check`, `make e2e` green | Sidebar e2e run; screenshot of the sidebar with the user's current database restored into a harness root; migration output |
| 2 | Agent row | 1, 3a | Promote, demote, fold, state line, done footer, keyboard, drag no-op, collapsed preview, group nesting all behave as specified | New `standing-agent-sidebar.spec.ts` covering each interaction; screenshots of promoted, folded, collapsed-with-preview and done-footer states |
| 3a | Wake supervisor | none | `thread_loop`, `thread_watch` and `thread send` work for Claude and Codex mock providers; a wake starts a headless session; boot replays a missed fire once with the missed count in the wake; quit mid-tick reruns; long-running watch lines arrive as separate wakes; a non-zero exit is an outcome, not a wake; `thread_status` lists loops and watches; `thread_watch` on a non-agent is refused (the agent row lands in 2, so 3a carries the `standing_agents` table and the refusal, and 2 builds the UI on it) | Harness spec that registers each kind, restarts the backend, and asserts the delivered wakes, their badges and the untrusted framing; the same spec on both mock providers |
| 3b | Wake rows in the pane | 3a | Loops, watches and reminders render in the background tray with next fire, last outcome, wake now and cancel; expanding a watch shows command, cwd and stderr tail; the registering tool call renders in the timeline with the next fire time | Screenshots of the tray with each kind, expanded and collapsed, on desktop and compact; the wake-now and cancel paths in a Playwright spec |
| 3c | Delete the workflow scheduler | 3a | `internal/workflow/scheduler`, the `automations` and `automation_cursors` tables, `WorkflowCreateAutomation`, the `schedule` CLI verb and the overlay's automation rows are gone; the workflow suites pass | Workflow e2e suite run; migration output on the user's database copy showing the tables dropped |
| 4 | Claude wire gaps | spike | `CronCreate` ack keeps the session alive past the reap threshold; an E7 completion renders as a badged system row naming the task | Fixture replay through the harness with the reap threshold shortened; before and after screenshots of the E7 pane |
| 5 | Agent homes and settings | 2, 3a | Promotion creates the home and injects its path into both providers; the Skills page installs, updates and removes per provider | Settings e2e with temp provider homes showing the installed tree; harness proof that a Claude and a Codex mock session received the home path; screenshots of the Skills page |
| 6 | pstack vendored and ported | 3a, 5 | Every primitive in the port table is rewritten; the `forge` script passes against the fake `gh` and `glab`; `poteto-mode` completes a small real task in a Claude thread and in a Codex thread with the same steps | Manual provider smoke on a throwaway repo with the transcripts attached; forge script run log against the harness fake |
| 7 | Workflows to MCP | 5 | No workflow verb in `aocli`; every workflow e2e spec passes through the MCP tool; starters and docs name the tool | Workflow e2e suite run; a screenshot of a run started and watched from a thread through the tool |
| 8 | MR lead migration | 2, 3b, 5, 6 | The lead's notes live in its home, it is promoted, it set its own loop and watches, and it ran a week | The user |

Slices 1 and 3a start together, with the slice 4 spike beside them.
Slice 7 is independent of 6 and runs after 5 so the tool lands on the
settled capability plumbing.

## Open questions

- Whether `thread_loop` for ordinary threads should notify on tick by
  default. Ruling 9 says no for agents; ordinary threads follow the
  same default.
- A full transcript-tail backfill for E7 completions, beyond the badged
  row slice 4 ships.
