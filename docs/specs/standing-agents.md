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
   It runs what the agent registers and delivers the output. Refined in
   slice 9: the app reads two well-known files from the home when they
   exist and prescribes nothing else; the optional `agent-home` skill
   teaches a layout ([Agent home contents](#agent-home-contents)).
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
    verification. The repository is public: evidence never carries the
    user's real thread titles, project names, branch names, costs or
    ticket keys. Evidence comes from harness fixtures, or from a clone
    of the real database with every such string replaced in the DOM
    before the capture.
19. **A worker's question goes to its agent.** A thread spawned by a
    standing agent never puts a question or approval to the user. Its
    awaiting-input and approval stops are delivered to the spawning
    agent as a badged wake, the worker shows `waiting on <agent>` rather
    than needs-you, and the agent answers through `thread_reply` or
    escalates by asking the user itself, as one question with the
    context. The user can still open the worker and answer directly,
    which settles the wake. This supersedes the thread tools non-goal
    of not relaying approval requests, for agent-spawned threads only.
20. **Other threads' work is an Agents segment, not Background.** The
    requests a thread is owed (spawns, and sends and asks it waits on or
    asked to be told about) list in their own rail segment after
    Background, nested by spawn as provider subagents nest, with its own
    count, Stop All and digest; reminders stay wakes. A settled row
    lingers 8 s from when the pane saw it settle, longer than a
    Background row because it is a click target, then leaves. Stop
    cancels for the user with the caller's wake kept armed and worded as
    the user's stop; for a thread the caller spawned it cascades into
    that thread's background work, turn and own rows, on any computer,
    with the wakes inside that lineage disarmed, and a send to someone
    else's thread cancels only the request. An older paired computer
    degrades to the request and the turn, said on the row and in the
    toast. Background's Stop All and the thread's own Stop do not touch
    the requests ([Agents segment](#agents-segment)).
21. **Agent messages are not the user's.** A message another agent, a
    wake or a worker queued for a thread lists in its Agents segment as
    incoming until delivered, and its Stop withdraws it back to its owner
    ([Agents segment](#agents-segment)). Leaving it out of the composer's
    queue lands with slice 13.

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
leaves the home on disk; the row action says so. Its confirm also offers
Demote and Delete Home (`DemoteStandingAgentAndDeleteHome`), which
demotes and then deletes the row's recorded home, only when that is
`<configDir>/agents/<thread-id>` (a row that records no home or any
other path is refused); a deletion that fails or is refused leaves the
demotion in place and names the folder. A promotion finds a kept home
and keeps its README. Promotion is refused for an archived thread, a
thread with a running workflow binding, or a thread on another
computer.

The home path and the agent's standing instructions reach the provider
beside the app guide: Claude's appended system prompt (claude-tui's
too) and Codex's developer instructions, read at every session start
and resume whatever the guide setting says (`standingAgentPrompt`). A
session live at promotion gets it at its next start or resume. What the
block carries is in [Agent home contents](#agent-home-contents).

Deleting the thread deletes the agent row, its loops and watches; the
home is removed only through the row action that asks. Archiving it
cancels its loops and watches, and unarchiving re-arms none. A
conversation transfer leaves the home on its computer: a move cancels
the loops and watches when it begins and drops the agent row when it
commits, so the conversation arrives as an ordinary thread, and a copy
is an ordinary thread while the original keeps its row and schedules.
The transfer dialog says so before the user commits (`GetStandingAgent`).

### Agent home contents

A standing agent has to persist and improve across sessions: what it
verifies and how, the rulings the user gave it, its grants, product
context and lessons. It keeps them in its home; the app reads two files
from there and prescribes nothing else (ruling 2). The `agent-home`
skill, an Agent Overflow skill in the vendored tree
([Skills library](#skills-library)), teaches the layout and the
discipline; the app works the same whether or not it is installed.

Layout the skill teaches (`assets/skills/pstack/skills/agent-home`):

| Path | Holds | Written by |
|---|---|---|
| `instructions.md` | Line 1 the role (the role line is its first non-blank line after a leading frontmatter block and rule lines); then grants with scope and date (ruling 1), standing rules, who it reports to | The agent, by hand, on a ruling |
| `rulings.md` | Append-only dated rulings, each with the thread and item it came from | The agent |
| `verification.md` | What the agent checks before calling work done and how, extended when a miss is found | The agent |
| `memory/<slug>.md` | One fact per file; frontmatter `name`, `description`, `type` (user, feedback, project, reference), `source`, `last_verified` | The agent |
| `MEMORY.md` | The index of `memory/`, one line per memory grouped by type, then an archive | The hygiene loop only |

The skill creates what is missing on first load and keeps whatever the
home already holds, the README promotion wrote included. Promotion
seeds nothing beyond that README.

**Injection.** At every session start and resume of a standing agent,
on Claude, claude-tui and Codex, the block `standingAgentPrompt` joins
to the app guide is: the home sentence (home path, thread id, that the
agent owns the directory) and "Load the agent-home skill before any
work. If it is not installed, tell the user to install the skills
library from Settings, Agents, Skills, and keep your home files by hand
until it is."; then, for each file that exists and is not empty,
`instructions.md` under "Your standing instructions" and `MEMORY.md`
under "Your memory index". Each file is cleaned first, the way an
attached file is (`agenthome.CleanText`: invalid UTF-8 replaced, line
ends made `\n`, every Unicode control and format character removed
except tab and line break, the line and paragraph separators too), and
everything after cleaning counts on the cleaned text. Only the part of
`MEMORY.md` above the archive marker is injected: a line holding
`<!-- agent-home:archive -->` exactly, case-sensitive, with only white
space around it. A mention inside a line does not count; a marker line
inside a code fence does. Caps: 8 KiB of `instructions.md` and 4 KiB of
`MEMORY.md`; past a cap the head is injected, cut after the last line
break inside the cap (on a character boundary when one line is longer),
with a notice naming the file, the line and the byte of the cleaned text
it stopped at and the line to read on from, and the agent reads the
rest itself. Reads go through an `os.Root` on the home, so a link out of
it is not followed, and only a regular file is read: the open does not
wait (`O_NONBLOCK` on Unix) and the mode is checked on the opened file,
so a FIFO cannot hang the start. A read takes at most the first 1 MiB
of a file (`agenthome.MaxRawRead`), so a file of characters cleaning
removes is not read without end; when that bound stops a read with the
cleaned text inside the cap, what was read is injected with a notice
that the rest past 1 MiB was not read and the line to read on from, and
`home_files` refuses such a file. The literals live in
`internal/agenthome`, and a test holds the skill to them.

The files are read when the session starts and never watched, so a
session's prompt is fixed for its life and two starts over unchanged
files carry the same bytes, which keeps the provider's prompt cache. An
edit, the hygiene loop's rewrite included, reaches the next start or
resume. A missing file injects nothing. A file that cannot be read for
another reason injects a line saying so in its place, is logged with
its path, and puts a warning row on the thread once the start has
succeeded or failed; the start proceeds. A demoted thread's next session carries
none of it.

**Hygiene loop.** The skill has the agent register one `thread_loop` on
itself, labeled `agent-home hygiene`, every six hours unless it chooses
otherwise. Each fire runs one pass: rebuild `MEMORY.md` from the
frontmatter; for each memory, check its source still exists
(`thread_show` around the item for a thread source, opening what any
other source names) and that no later ruling contradicts it, mark a
failing one `stale: <reason>` and drop it from the index; set
`last_verified` only on a memory that passed; merge duplicates; mark a
memory with no source, or one it cannot check, unverified in its line
and leave its `last_verified` alone; keep the part above the archive
marker under 4 KiB by moving lines below it in order of the
`last_verified` each memory held before the pass, oldest first, slug
breaking a tie. It writes nothing else and ends the turn stating what changed,
which is the wake's outcome. Only this pass writes `MEMORY.md`, never at
the moment a memory is written, so the injected bytes change on the
loop's schedule rather than on every write.

**Agent-to-agent context.** What a receiver needs from the surfaces a
standing agent reads and writes:

| Surface | What the receiver lacked | Now |
|---|---|---|
| `thread_send` / `thread_spawn` footer | Whether the sender is a lead or a worker; for a worker, that its questions go to the agent (ruling 19) | A standing agent sender is named as one with its role line, the first non-blank line of its `instructions.md` after a leading frontmatter block and rule lines (one cleaned line, at most 200 characters, quoted), read when the message is sent. A frontmatter block that is never closed is not skipped, so its first key line is the role. When it spawned the receiver on this computer, the footer adds that the receiver's questions and approval requests go to it. A sender on another computer carries its role in the forwarded call and never routes. An ordinary sender's footer is unchanged |
| A spawned thread's first message | The sender's standards, which it could only paste | `home_files` on `thread_spawn` and `thread_send`: files from the caller's home prepended under their names, framed as the sender's own text ([thread tools](agent-thread-tools.md#thread_spawn-q5-q6-q18)). The skill passes `verification.md` to every worker and reviewer |
| Wake of a reply, final message or failure | Whether the answering thread is an agent | A standing agent is named with its role line on the line after the status line, as it was when the answer or the late reply was written: the answering computer's receipt records it then, and the request row copies it when it collects the answer, from this computer or another, so the wake reads the same on either. A role is bounded again where it is collected, and bounding a bounded role changes nothing. A computer that reports none answered from an ordinary thread |
| `thread_search`, `thread_show` | Which rows are agents and whose workers | `standing_agent` and `spawned_by` on every row |
| `thread_reply`, worker prompt wakes, loop and watch wakes | Nothing missing: a reply's text is the answer, a worker's prompt wake already names the worker, and a loop or watch wake is the agent's own | Unchanged |
| `thread_status` | Nothing missing: it reports the caller's own requests and the live state of threads it names | Unchanged |

The role line and the attached files are the sender's own text and
carry no authority; the thread tools guide says so beside the rule for
message bodies, and a body line that mimics the footer is quoted as
before.

### Sidebar

The agent row is a new row kind rendered by `ThreadRow` with an agent
variant, not a new component. Differences from an ordinary thread row:

- Leading person glyph after the chevron. The chevron is the only fold
  target; name, dot and state line open the thread. Keyboard: Enter
  opens, Right and Left fold and unfold (project rows have no arrow
  keys; the agent row is the one foldable thread row).
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
- Context menu: Open, Rename, Loops and Watches (opens the thread with
  its background tray open), Archive Done Children (the spawned threads
  whose effective status is idle, behind a confirm), Demote (behind a
  confirm that reads "This thread becomes an ordinary thread. Its
  watches are cancelled; its loops stay as they are. Demote keeps its
  home directory; Demote and Delete Home removes it.", with Demote and
  Demote and Delete Home as its two choices). An archived child listed
  by the done footer offers Open, Rename, Unarchive, Copy and Delete:
  no pin, group or archive item. No pin
  item: an agent row sits in the groups section whatever pin its thread
  holds, and its pin is left alone. Promote to Standing Agent appears on
  a chat or plan thread that is not archived, a draft or an agent; the
  backend refuses every other mode.

Mechanics, as built:

- The thread row carries the lineage: `agentPromotedAt`,
  `spawnedByThreadId` (the newest local spawn request naming it) and
  `archivedChildCount` (an agent's archived spawned threads), derived
  in `threadColumns`, so one `thread:updated` frame moves the row. A
  spawn's accept re-broadcasts the spawned thread; a child's archive,
  unarchive and delete re-broadcast the child and its spawner, whose
  `archivedChildCount` they change. The
  partial index `idx_thread_requests_spawn_target` serves the lookup,
  and while the thread it created exists the thread-request retention
  sweep keeps a local spawn row past the floor as `lineage_only`, its
  answer, late reply and wake cleared. The ledger tools treat that
  token as unknown; the lineage reads, and a worker's prompt routing
  (`StandingAgentSpawnerOf`), still find the row.
- `PromoteStandingAgent` ungroups the agent and its local spawned
  threads in the same transaction and deletes a group that is left
  empty. `SetThreadGroup` refuses an agent (`ErrStandingAgentGrouped`)
  and a thread an agent spawned (`ErrSpawnedChildGrouped`), and
  `thread_spawn` with `group` is refused for an agent caller. The
  frontend never offers the group items on those rows and the drag
  payload marks them `agentBound`, so no group target accepts them.
- `ListStandingAgentWakes` (route all) and `thread:wakes_changed` feed
  `stores/agentWakes.svelte.ts`; the state line
  (`utils/agentStateLine.ts`) shows the running row, else the soonest
  loop or watch, with `+N more`. A row the bulk read missed asks
  `ListThreadWakes` once on mount.
- `ListArchivedSpawnedThreads` (newest first) feeds the done footer
  through `stores/agentArchivedChildren.svelte.ts`, keyed by the row's
  `archivedChildCount` so a changed count reads again. The fold state
  lives in `sidebar:collapsedAgents`; an open done footer and its
  paging are session state, and both reset when the count reaches
  zero, so a footer that returns starts closed. A thread that stops
  being an agent in the list (demoted, archived, deleted) drops its
  fold, footer and cached list, so a later promotion starts fresh.
- A spawned thread's row carries `spawnedByAgent` beside
  `spawnedByThreadId`: the group refusal holds while the agent is
  archived (its own row is then absent), and the row's menu and drag
  payload read the bond from the row, not from a lookup of the agent.
  The live spawned rows go out again whenever that bond changes:
  promotion, demotion, the agent's delete (which takes the spawn rows
  with it) and a committed move.
- The harness seeds lineage with `HarnessSeedThread.spawnedBy`, the
  title of a thread seeded earlier in the same project.

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
    timeout_seconds  INTEGER,
    long_running     INTEGER NOT NULL DEFAULT 0 CHECK(long_running IN (0,1)),
    next_at          INTEGER,
    last_run_at      INTEGER,
    last_outcome     TEXT NOT NULL DEFAULT '',
    last_output      TEXT NOT NULL DEFAULT '',
    undelivered      TEXT NOT NULL DEFAULT '',
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
  `interval_seconds` and wakes the thread when stdout is non-empty; a
  run longer than the watch's `timeout_seconds` is stopped with its
  process group and recorded as timed out. Long-running mode keeps it
  running and wakes the thread once per stdout line, batching lines
  that arrive within one second. Stderr is captured and shown with the
  wake; a non-zero exit is an outcome, not a wake, and shows as an
  error on the row. Watches are limited to standing agents. Demotion
  cancels a thread's watches and keeps its loops.

Limits, enforced at registration:

- An interval is at least 60 seconds and at most 30 days.
- A thread keeps at most 64 live loops and watches together.
- An interval watch's `timeout_seconds` defaults to 600 and is at most
  3600; a long-running watch has none.
- A wake carries at most 64 KiB of data and names the bytes it dropped.

Delivery queues the wake into the thread's message queue through the
path `thread_remind` uses today, so a thread with no live session starts
one headless. A wake is delivered once it is queued: the queue write
never waits on the thread's action lock or on a session start, and the
start runs after it. A start that fails leaves an error row and the
wake queued for the next start, and raises an error notification
naming the thread (`session-start:<thread>`), which the thread's next
successful session start withdraws, after a restart too: the thread is
marked in `session_start_failures` until then. Every message the app queues for someone else
and starts a session for (a wake, an outside send, a reminder or an
answer, a remote job's completion, a worker's prompt) does the same.
The notification is raised once per run of failed starts. A failed
start does not stall a loop or watch: each later fire that finds its
previous wake still queued with no live session tries the start again,
no sooner than half the 60-second floor after the last try, and writes
no further row while the start keeps failing. When the start succeeds,
the waiting wake and the new one are both delivered. Archiving a thread
takes its queued wakes, outside sends and workers' prompts back out of
the queue, and cancelling a loop or watch (by label, by demotion or by
a move) takes back its queued wakes, whether or not a session is live,
so no later start delivers them; a message a session has already taken
is the session's. A message that could not be taken back is reported to
the caller. A remote job's queued completion goes back to its watch
instead and arrives once the thread is unarchived
([remote-commands.md](../architecture/remote-commands.md)). A thread
request's queued answer or reminder goes back to its request, owed and
disarmed, as the archive leaves the thread's undelivered requests
([agent-thread-tools.md](agent-thread-tools.md)). A person's Stop or a
revert drops the whole queue, and returns these two kinds the same way
once the queue is gone: an answer or reminder to its request, owed and
disarmed, so `thread_status` still returns it; a remote job's
completion to its watch, whose watcher delivers it again and starts the
session for it, as for a completion that finishes after the Stop. A
Stop does not quiet later wakes. A Stop or an archive that ends a
session holding such an answer or completion, handed to the provider
and not confirmed, returns it too; a person's text joined with it stays
dropped. A session that ends, or a Codex resend that fails, without
taking in such an answer or completion, and a restart that finds one
still queued or handed to the provider and never confirmed, return it
the same way; none goes to the composer. Delivery is marked in two
phases: the queue write marks the request or watch `queued`, and the
provider's echo of the message confirms it, so only a message the
provider has not confirmed goes back. On either provider, a message
given back may already be in the conversation the next session resumes,
so it can reach the agent twice. The confirmation is the app's own write
after the provider's echo, while the provider records the message in its
session file on its own, so a crash or a session death between the two
leaves a message the agent holds reading unconfirmed. A Stop or an
archive drops the unconfirmed sends before it closes the provider, which
has three seconds to exit once its input closes, and an echo the
provider sends in that window arrives after the drop and is discarded.
That cost is accepted: a request that reads owed and a watch that reads
pending, with nothing lost, outrank a line the agent may already have
seen. A completion a
session end or a failed resend returned is delivered again on the
ladder a request's undelivered wake is retried on, doubling from half
a minute towards ten minutes for eight attempts, so a session that keeps
ending before it takes the completion in is not restarted every few
seconds; after the last attempt the watch is dismissed with the reason,
and `remote_jobs` still returns the result. One that a Stop, an
archive, a revert or a restart returned is delivered at the watcher's
next poll. A Stop and a revert drop the queue under the thread's
mutation lock, which every queue write takes, so a message queued
during the drop is either dropped with it or queued after it.
An archived thread takes none of these messages and starts no session
for them: one that arrives after the archive is refused back to what
sent it, and one queued just before it waits, without a start, for the
archive to take it back. A person's answers to an agent's questions are
the exception, as anything they type into an archived thread is.
The queued message is badged with the wake's kind and
label, wrapped as untrusted text with the byte cap above, and carries
no approval or user authority. The timeline and the send queue show its
quoted data decoded; the agent reads it escaped. A wake that arrives
while a turn is open is delivered like a reminder: on Claude it is
queued to the CLI, which takes it in at its next step or as a turn of
its own after the current one, and on Codex it is steered into the
running turn.

A turn is a tick when only wakes started it. Whether a turn was a tick
is read from the rows of the turn that ended; a Claude wake the CLI
runs as a turn of its own keeps its row in the turn it was queued
during, so that pickup turn is a tick when everything it was waiting
on was a wake. A tick raises no notification at rest unless the agent's
`notify_on_tick` is on (`SetStandingAgentNotifyOnTick`); a failed tick
notifies.

A wake that never reaches the agent is not put in the person's
composer: a loop or watch wake still queued at boot, or left
unconfirmed by a session that ended, is discarded and its row records
`not delivered`. A watch keeps that wake's data as `last_output`
(capped at 64 KiB), which `thread_status` shows, so the agent can
recover the event at its next wake; a loop needs nothing beyond the
row. An outside `thread send` message is handled the same way, with a
`not delivered` row in the thread's timeline; so is one a Stop, an
archive or a revert takes out of the queue. When Claude
received a wake or an outside send joined with a person's queued
message and the session ended before taking it in, the join is split:
the person's own text goes back to the composer, and the wake and send
parts are discarded with their `not delivered` records, the watch
keeping only its own wake's data. The dispatch that joins the messages
records each one's length, which splits the join exactly; an answer's
or a completion's text can hold the separator the join puts between
messages, so the first-line split older joins use refuses those and
restores all of the text, giving the answer and completion back too.
An outside send's and a wake's text then go to the composer whole with
the person's, and the send leaves no `not delivered` notice: its text is
in front of the person, and a notice would invite sending it twice.
These records, notices and give-backs are written once the person's text
is back in the composer; a composer write that fails queues every
message again whole, and none of them is recorded.

Other than that retry, a fire whose previous wake is still queued is
skipped and counted missed, and its output is kept the same way. The outcome of the latest
wake that did not reach the agent stays on the row as `undelivered`,
with the fires a skip missed, until a later wake of the row reaches
the queue: that wake says the previous one did not reach the agent
and counts the missed fires. A run that wakes nothing, such as a watch
run with no output or a failed one, records its own `last_outcome`
and leaves them.

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
  timeout_seconds?, stop}`: standing agents only; an ordinary thread
  is refused with the reason. One watch per label.
- `thread_status` lists the caller's loops and watches with next fire,
  last outcome, the outcome and output of a wake that has not reached
  the thread, and missed count alongside its open requests.
- `thread_cancel` by label cancels a loop or watch.

An outside poke, `agent-overflow thread send <thread-id> <text>`, queues
a message to a local thread through the same badged, untrusted path, for
scripts run by anything other than the supervisor. It is a CLI verb
because its caller is not a session. It returns once the message is
queued, printing its send id, and never waits for a session start. It
refuses an archived thread and a thread that is not on this computer.

### Activity rail

Loops, watches and reminders of the open thread render in the composer's
existing background tray (`ActivityRailBackgroundBody`), one row each:
kind glyph, label, `next 12m` or `running`, last outcome, a `Wake now`
action that fires it immediately and a `Cancel` action. The tray toggle
counts them with the other background work. Expanding a watch row shows
its command, cwd and the tail of its last stderr. Under the compact
layout a row takes two lines, so the label is never cut to fit the
actions: the kind and the label across the row (an ellipsis only past
two lines), then `next 12m` and the two actions. The tool call that
registered it renders in the timeline like any thread tool call, with
the next fire time in the summary line: a time alone when it falls on
the day of the call, and dated otherwise. The form is fixed by the
call's own stamp, so the settled row reads the same on any later day.

- **Reading.** `ListThreadWakes` returns the thread's live loops and
  watches and its 64 soonest reminders that have not fired; the tray
  says how many more there are. `ListStandingAgentWakes` returns the live
  loops and watches of every standing agent on a computer, for the
  sidebar's agent rows. `GetThreadWakeDetail` reads an expanded
  watch on demand. A watch keeps the last 8 KiB of its last run's stderr
  (`agent_wakes.last_stderr`); a long-running watch keeps its latest
  non-empty batch.
- **Live updates.** Every write to a loop, watch or reminder (register,
  fire, run, outcome, undelivered wake, boot recovery, cancel, demote,
  archive) emits `thread:wakes_changed` for that row: the thread id, the
  row id, and the row as it stands or `removed` when it left the listing.
  A frame with neither means the row could not be read, and a reader
  reads the thread again, as it does after a lost frame or a reconnect.
  The tray reads its list again through the rate-bounded refresh
  scheduler, since the order and the reminder bound are the backend's.
  Nothing polls.
- **Outcome.** A wake that did not reach the thread is shown as a warning,
  with its missed fires, until a later wake does. An `error` outcome is
  shown as an error.
- **Wake now** (`WakeThreadWakeNow`) fires a loop or watch through the
  sweep's own fire path and keeps its schedule; a reminder settles as if
  it came due. It is refused, with the reason, for a cancelled row, a run
  already in flight, a long-running watch (its process is the schedule)
  and an archived thread. A watch's Wake now runs a script on the
  computer, so it also needs `threads:autonomy`.
- **Cancel** (`CancelThreadWake`) takes `thread_cancel`'s path. A row
  already gone is not an error.
- **Next fire in the timeline.** Triage stamps the `next_fire` of a
  successful `thread_loop`, `thread_watch` or `thread_remind` answer on
  its row's meta. History imported from a provider file has no stamp and
  shows the summary without a time.

#### Agents segment

The other threads a thread is waiting on get a segment of their own after
Background: the `users-round` icon, `Agents`, a count of open rows and
incoming messages, and its own `Stop All`. Every thread with rows shows
it, a standing agent's pane included. It renders at every density of the
rail; the densest rung (`tight`) drops the segments' running pulses and
closes up their spacing so the row holds at a phone width.

- **Rows.** One row per request the thread made through the thread tools
  and is still owed: every spawn, and a send or ask with notify armed or a
  call parked on it. Reminders are wakes, not rows. A row shows the
  target's provider (a paired computer's from its acknowledgement), the
  kind, its title, its computer when it is not this
  one, its state (queued, running, blocked, replied, finished, failed,
  stopped, interrupted, expired, refused, a late reply marked), how long
  since the request, and what a blocked row waits on: the approval with
  its tool and detail, or the question, and who it was routed to. A
  running target's latest activity is a second line; the chevron opens the
  row's digest: the activity, what it is blocked on in full, the last
  message in the thread (a request without the footer the app appended)
  and the answer once it settled. State comes from
  the thread tools' own derivation (`requestStateView`); reading never
  delivers an answer.
- **Nesting.** A spawned thread's own rows nest under its row, as provider
  subagents nest under theirs, four levels deep; a row at the bound whose
  thread has agents of its own says so. A settled spawn stays listed while
  anything nested under it within the bound is open, here or, by the last
  `agent_rows` answer, on a paired computer. The listing holds at most 128
  rows and says when it left some out.
- **Lifetime.** An open row stays. A settled row stays for 8 s
  (`AGENT_ROW_SETTLED_LINGER_MS`) from when the pane first saw it settled,
  then leaves; the backend keeps listing settled rows for 30 s so a pane
  that reconnects still sees them. The linger is longer than Background's
  because a settled row is a click target: clicking it takes it out early
  and scrolls to its wake item when the answer landed in this thread, or
  opens the thread it went to otherwise. Clicking an open row opens the
  target in a pane. A pane remembers which settled rows it saw and which
  were clicked for the last 64 threads it showed, so a row that left does
  not come back when the pane returns.
- **Incoming.** Messages other threads' agents, wakes and workers queued
  for this thread are rows too, labelled incoming with the sender and the
  kind (request, reply, wake, question), until they leave the queue. A
  live session takes a queued message at once, so one waits while the
  thread has no session that can take it: its start is pending or failed.
  Their Stop withdraws the message and gives it back the way a queue drop
  does: a request's sender is told the user withdrew it, an answer is owed
  again and disarmed for `thread_status`, a worker's question goes back to
  the user, a wake's row records it as not delivered.
- **Stop** (`StopThreadAgentRow`) cancels the request as `thread_cancel`
  does, with the caller's notify left armed: its agent is woken with the
  request cancelled by the user, not by itself. An agent's own
  `thread_cancel` still disarms it. For a thread the caller spawned the
  Stop cascades as a provider subagent's does: that thread's Backgrounded
  items, its running turn, and its own Agents rows, down the lineage. A
  request inside the lineage is cancelled with its wake disarmed: the
  thread that made it was stopped too, and a wake would start it again.
  A Stop of a row on this computer works from the ledger, without reading
  the listing. A send to a thread the caller does not own cancels only the
  request. A settled row reports it had already finished. Each layer's
  result, what it stopped and what failed, is folded into one toast, and a
  failure in one layer leaves the rest to run. `Stop All`
  (`StopThreadAgentRows`) does the same for every row and withdraws the
  incoming messages listed when it was pressed; the Stops of rows here
  start at once, beside those that wait on a paired computer.
  Background's `Stop All` and Stop background work leave Agents rows
  alone. The thread's own Stop leaves its requests, and clears the
  incoming rows with the rest of its queue, given back as a queue drop
  does.
- **Other computers.** A spawn on a paired computer lists and stops as a
  local one: the forwarded `agent_rows` and `agent_stop` calls
  ([reaching another computer](agent-thread-tools.md#reaching-another-computer)) nest that
  thread's rows under the spawn, named by their computer, and run the
  cascade there. A read never waits on another computer: it nests the
  last answer for each remote spawn and asks again in the background, and
  an answer that changes the rows sends a `thread:agent_rows_changed`
  frame. The asking is bounded: at most 256 spawns wait to be asked, each
  once, the oldest dropped with its caller told to read again; a spawn
  asked about again while its call runs is asked once more after it; at most
  eight calls run, two of them to one computer; a computer whose call
  timed out is not asked again for 5 s, doubling to a minute, until it
  answers; at most 256 answers are kept. A computer no longer paired,
  however its pairing ended, is forgotten with its spawns' open work. Each poll of an open remote spawn
  asks again; so does the sweep, at the poll's pace, for a settled one
  whose last answer listed open work (`thread_requests.nested_open`),
  until it lists none. A computer that does not answer is said on the row
  (`<computer> did not answer; its agents are not shown`). A computer
  that predates the calls has the request cancelled and the turn
  interrupted, and the row and the Stop's toast say its own work was not
  shown or stopped.
- **Live updates.** `ListThreadAgentRows` is read when the thread opens,
  after a reconnect or a lost frame, and on each
  `thread:agent_rows_changed` frame naming the thread or a thread its list
  nests. The backend sends one after every change to a request, to the
  target a running request works in, and to a wait parked on one,
  coalesced off the emit path. Reads go through the rate-bounded refresh
  scheduler.

### Skills library

The vendored tree lives in the repository at `assets/skills/pstack/`
with `UPSTREAM.md` pinning the upstream commit and listing every edit
made for the port. One skill in it, `agent-home`, is authored for Agent
Overflow rather than vendored ([Agent home contents](#agent-home-contents));
`UPSTREAM.md` lists it as added. Syncing is a manual diff against upstream; nothing
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
| `gh` in `watch-pr`, Babysit, Shipping, the autopilots | agents run `forge which` once, then `gh` or `glab` directly; the scripts read the forge through the `forge` script, which chooses `gh` or `glab` from the remote |
| `.mdc` always-apply rule, SessionStart hook | the agent's own instructions file |
| `make-bot-ui`, Benny's Slack triggers | not ported; Benny's triage and reproduce skills are ported as skills an agent runs from a watch |
| Graphite `gt` stack reads in `orch` and the orchestrate playbook | `forge stack`: the chain of pull requests from the current branch to the default branch, read from each request's base |

The settings page `Skills` lists the shipped tree with its upstream
version, shows whether it is installed in each provider home, and
offers Install, Update and Remove per provider. Install copies each
skill directory `assets/skills/pstack/skills/<name>` to
`<provider home>/skills/<name>`: both providers discover a skill one
level under `skills/`, so the tree is flattened rather than nested
under a `pstack` directory. Install never symlinks into the
repository. Remove deletes only what Install wrote, by manifest.

As built (`internal/skillsinstall`, `app_skills.go`): the manifest is
`<provider home>/skills/.agent-overflow-pstack.json`, so every app
profile sharing a home sees one install. It names every file and skill
directory written; before the first write it names everything the run
may leave, so a failure part way shows as an unfinished install that
Update finishes and Remove clears. Install refuses the whole
install, writing nothing in `skills/`, when a skill directory or file it
would write is there and the manifest does not name it, and names them. Update
replaces what the manifest names and deletes the named files the tree
no longer has; it is refused for a home with no install. Remove deletes
the named files and the directories they leave empty; a directory
holding a file the user added stays. Update and Remove treat every file
the manifest names as the app's, a user's edit to one included, and
Remove asks no confirmation ([decisions](../decisions.md#miscellany)).
A manifest naming anything outside its own skill directories (a `..`
element, an absolute path, a file of a skill it does not list) is read
as an error and nothing is deleted. Every read, write and delete in
`skills/` goes through an `os.Root` opened on it (a `skills` link itself
is followed), so nothing reaches through a link to outside it. A link or
file where a directory of an owned skill should be refuses Install and
Update as a named conflict, wherever it points, and Remove deletes the
link, never through it. Install, Update and Remove hold an exclusive
advisory lock, and a status read a shared one, on
`<provider home>/.agent-overflow-pstack-skills.lock` from reading the
manifest to writing it, so two processes sharing a home cannot
interleave and the page never shows a change part way. The lock file
sits beside `skills/`; the first Install creates it (readable only by
the user, as is a provider home it has to create), a read of a home
with nothing installed does not, and it stays after Remove, since
removing a lock file races its users. A home is current when its
manifest's digest of the tree's paths and content matches the
shipped one, since the port edits files without moving the upstream
pin. An embedded file has no mode, so a file starting with `#!` is
installed executable.

### Workflows to MCP

The `workflow`, `run` and `memory` CLI verbs are replaced by an
`ao-workflow-tools` MCP server on the shared `internal/threadmcp`
transport, scoped the way the thread tools are and registered on Claude,
claude-tui and Codex. The phase runner's injected credentials become the
per-session capability URL, and phase grants become the tool allowlist for
that session. `aocli` keeps `service`, `pair`, `remote`, `thread` and the
serve entry. Starters, docs and the runner guidance name the tools. The
decisions log records the reversal of D15 with the user's reasoning:
app-managed tooling, one surface for agents, no separate binary to keep in
sync (D77). The contract is
[agent-workflow-tools.md](agent-workflow-tools.md).

Memory and `workflow_list`/`workflow_validate`/`workflow_schema` are
grant-free because a phase could already reach them through the offline CLI
and the memory verb, so nothing a phase can do widened. Grant-free tools
never widen what a phase can do, and a read-only phase keeps exactly the
envelope memory channel it had: `memory_add` and `workflow_new` are withheld
from read-only phases. Grants take effect on read-only phases; the author
declared them and validation already accepted them, so their old
unreachability was a mechanism accident, not a rule.

claude-tui hosts `ao-workflow-tools` and no other app server; whether it
hosts the thread tools is a separate decision
([decisions](../decisions.md)).

### Claude wire gaps

Two defects observed in the wire reference that an agent's loop would
hit:

- `CronCreate` acks are not classified, so the idle reaper can close a
  session holding a provider-side fixed-interval loop. Jobs are
  session-only on the provider side (the ack says so), so the reaper
  must keep a session alive while it holds an armed recurring job,
  protecting until the next fire computed from the job's cron
  expression plus the existing grace, re-evaluated on each fire and
  cleared by `CronDelete` or a one-shot fire. Acks are classified by
  tool name (`CronCreate`, `CronList`, `CronDelete`) with the key shape
  as a check; the spike capture is the fixture. A recurring job never
  holds a service update or a conversation transfer: both end the
  session and the job with it, and the durable loop is `thread_loop`.
  The transfer dialog names the recurring jobs it ends. One-shot jobs
  and ScheduleWakeup hold both until their deadline plus the grace.
- A cron fire reaches the wire as a `command_lifecycle` bracket with a
  CLI-created uuid and no user envelope, so the parser stamps the
  bracket with the peer origin and the prompt never renders. Its
  transcript-mirror user row carries `turnOrigin:"scheduled"`
  and `isMeta:true`; classify the turn as scheduled from that row and
  render the injected prompt as the turn's badged user row, as a
  `thread_remind` wake renders, instead of dropping it.
- A background completion that lands on an idle session renders a
  response starting from nothing (claude-wire.md E7). The reference
  says a transcript-tail backfill is not built; the scoped fix is to
  render the completion as a badged system row carrying the task's
  result and name, so the pane shows what woke the session rather than
  an unexplained response. A full backfill stays an open question.

The ack and fire shapes were captured on claude 2.1.284 under the
isolated spike policy (2026-10-10); slice 4 commits the sanitized
captures to `docs/references/fixtures/claude/` and documents them in
`claude-wire.md` as E11 (E10 was already `EnterWorktree`).

## Slices and verification

Each slice is one pull request. Its owner never verifies its own work:
a fresh verifier re-runs the named proofs on the exact head and attaches
the evidence before review. Evidence means a Playwright or harness spec
by name with its run output, screenshots copied out of
`e2e/test-results`, or a described proof against the real app with the
commands run. Unit tests are not evidence. Evidence files are never
committed (`.evidence/` is ignored): they go to a secret gist on the
author's account and the pull request body links their raw URLs. Nothing
in them may carry local paths, host names, tokens, real thread titles or
ticket keys.

| # | Slice | Depends on | Done means | Evidence |
|---|---|---|---|---|
| 1 | Remove discussions | none | No reference to discussions in Go, frontend, docs, keybindings or schema; the sidebar renders every existing thread flat; `make go-test`, `pnpm run check`, `make e2e` green | Sidebar e2e run; screenshot of the sidebar with the user's current database restored into a harness root; migration output |
| 2 | Agent row | 1, 3a | Promote, demote, fold, state line, done footer, keyboard, drag no-op, collapsed preview, group nesting all behave as specified | New `standing-agent-sidebar.spec.ts` covering each interaction; screenshots of promoted, folded, collapsed-with-preview and done-footer states |
| 3a | Wake supervisor | none | `thread_loop`, `thread_watch` and `thread send` work for Claude and Codex mock providers; a wake starts a headless session; boot replays a missed fire once with the missed count in the wake; quit mid-tick reruns; long-running watch lines arrive as separate wakes; a non-zero exit is an outcome, not a wake; `thread_status` lists loops and watches; `thread_watch` on a non-agent is refused (the agent row lands in 2, so 3a carries the `standing_agents` table and the refusal, and 2 builds the UI on it) | Harness spec that registers each kind, restarts the backend, and asserts the delivered wakes, their badges and the untrusted framing; the same spec on both mock providers |
| 3b | Wake rows in the pane | 3a | Loops, watches and reminders render in the background tray with next fire, last outcome, wake now and cancel; expanding a watch shows command, cwd and stderr tail; the registering tool call renders in the timeline with the next fire time | Screenshots of the tray with each kind, expanded and collapsed, on desktop and compact; the wake-now and cancel paths in a Playwright spec |
| 3c | Delete the workflow scheduler | 3a | `internal/workflow/scheduler`, the `automations` and `automation_cursors` tables, `WorkflowCreateAutomation`, the `schedule` CLI verb and the overlay's automation rows are gone; the workflow suites pass | Workflow e2e suite run; migration output on the user's database copy showing the tables dropped |
| 4 | Claude wire gaps | none | An armed recurring cron keeps the session alive past the reap threshold and `CronDelete` releases it; a cron fire renders as a scheduled turn with its prompt, not as a peer turn; an E7 completion renders as a badged system row naming the task | Fixture replay through the harness with the reap threshold shortened; screenshots of a cron fire turn and of the E7 pane before and after |
| 4a | Stale session frames after a stalled drain | 4 | A dead session's frames still queued when the provider event drain hits its stall bound (`providerEventDrainStall`) never reach triage after a replacement session starts: frames carry their session token and `triage.Handle` refuses a token that is no longer the thread's; `MarkThreadActive`'s claim that no stale frame passes its gate becomes true | Harness spec that stalls a worker across a replacement start with the dead session's frames queued and asserts nothing reaches the replacement, on both mock providers: on Claude a ScheduleWakeup and a CronCreate ack plus the reply, and the replacement holds no schedule; on Codex, which has no session schedule, the reply and the turn completion, and the replacement holds no stale turn or row. A frame already being handled when the replacement commits is proven by unit tests at every process-state writer and in the host |
| 5 | Agent homes and settings | 2, 3a | Promotion creates the home and injects its path into both providers; the Skills page installs, updates and removes per provider | Settings e2e with temp provider homes showing the installed tree; harness proof that a Claude and a Codex mock session received the home path; screenshots of the Skills page |
| 6 | pstack vendored and ported | 3a, 5 | Every primitive in the port table is rewritten; the `forge` script passes against the fake `gh` and `glab`; `poteto-mode` completes a small real task in a Claude thread and in a Codex thread with the same steps | Manual provider smoke on a throwaway repo with the transcripts attached; forge script run log against the harness fake |
| 7 | Workflows to MCP | 5 | No workflow verb in `aocli`; every workflow e2e spec passes through the MCP tool; starters and docs name the tool | Workflow e2e suite run; a screenshot of a run started and watched from a thread through the tool. No mock speaks claude-tui, so the claude-tui leg is proven at the argv level and at the app level through the real threadmcp transport, and the only unproven step is the real Claude TUI binary reading --mcp-config and --allowedTools, which the manual provider smoke covers. |
| 8 | MR lead migration | 2, 3b, 5, 6 | The lead's notes live in its home, it is promoted, it set its own loop and watches, and it ran a week | The user |
| 9 | Agent home contents | 5, 6 | The `agent-home` skill ships in the installed tree; a standing agent's sessions on Claude, claude-tui and Codex carry its `instructions.md` and the non-archive part of `MEMORY.md` within their caps, byte-identical over unchanged files, with a rewrite reaching the next start and nothing after demotion; an unreadable file starts the session with a warning row; footers and wakes name a standing agent with its role line and tell a worker its questions go to its agent; `home_files` attaches files on spawn and send and is refused for a non-agent, an escaping path, a missing file and a total past 32 KiB; thread rows carry `standing_agent` and `spawned_by` | `standing-agent-home-contents.spec.ts` on both mock providers (injected prompt, hygiene wake through `thread_loop` reaching the next start, a worker's first message carrying `home_files`, the footer role line between an agent and an ordinary thread); app tests reading the claude-tui argv and append file, since no mock speaks claude-tui; screenshots of the agent's session, the worker's first message and the hygiene wake, desktop and compact |
| 10 | Agents segment | 3b, 9 | The Agents segment lists a thread's spawns and owed sends and asks with title, provider, state, elapsed and what blocks them, nested by spawn to four levels and across paired computers; settled rows linger 8 s and leave, a click opens the target or the answer; Stop cancels the request for the user with the caller's wake armed and cascades into a spawned thread's background work, turn and own rows, here or on a paired computer, and Stop All does the same; incoming agent messages list until delivered and withdraw; Background's Stop All and the thread's own Stop leave the rows; the segment fits every rail density | `agents-tray.spec.ts` and `compact-agents-tray.spec.ts` on both mock providers (rows, nesting, a blocked approval, settle and linger, click, per-row Stop cascade with the caller's user-cancelled wake and the nested wakes disarmed, Stop All, Background's Stop All and the thread's own Stop leaving the rows); the cascade across computers in `thread-tools-paired.spec.ts`; screenshots of running, blocked, nested and settled rows and the digest, desktop and compact |

Slices 1 and 3a start together, with the slice 4 spike beside them.
Slice 7 is independent of 6 and runs after 5 so the tool lands on the
settled capability plumbing.

## Open questions

- Whether `thread_loop` for ordinary threads should notify on tick by
  default. Ruling 9 says no for agents; ordinary threads follow the
  same default.
- A full transcript-tail backfill for E7 completions, beyond the badged
  row slice 4 ships.
