# Standing agents

Status: design signed off 2026-10-10; assistant identity and creation
(rulings 22 to 29) signed off 2026-10-15. The build is split
into the slices in [Slices and verification](#slices-and-verification);
each lands as its own pull request against the `personal-assistant`
branch and nothing merges without the user's review.

## Goal

An assistant (the standing agent this spec names) is a record that
outlives its threads: a name, a home, the projects it serves and one
current thread. It lives above projects. It keeps its home, wakes
itself on a schedule or on events its own scripts detect, spawns and
steers worker threads, and brings the user decisions and evidence
rather than progress. It orchestrates and delegates; workers do the
implementation. It builds its own scripts and asks before widening what
it is allowed to do. It runs whenever the app runs, headless when no
pane is open, and across app restarts without losing a scheduled wake.

Agent Overflow provides the few things an agent cannot give itself:
identity that outlives a session, a wake supervisor, a home, memory it
delivers ([memory](memory.md)), and a sidebar that does not drown in
worker rows. Everything else, including what the agent watches and how
it verifies, is instructions and scripts the agent and the user
maintain. The app knows nothing about Jira, GitLab or any other source.

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
   slice 9, and again 2026-10-21: the app reads `instructions.md` and
   the facts in `memory/` from the home and prescribes their format
   ([memory](memory.md)), and nothing else
   ([Agent home contents](#agent-home-contents)).
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
   An agent row never sits inside a group. A thread the agent spawned
   nests under its row unless it was placed in a group or as a plain
   row (ruling 36). A thread the agent spawned into a project nests
   under the agent's row in that project, by the same rules (ruling 38).
5. **Done folds away.** A worker that has nothing left for anyone
   leaves the row list and counts in a "done" footer under the agent:
   a settled idle child (idle and read, not pinned, not open in a pane)
   and every archived child. The footer lists the settled idle children
   first, then the archived ones, newest first within each. Archive
   stays the only persisted done state; settled idle is a fold state
   derived from the row, so new activity or a pin returns the child to
   the live rows, and opening it shows it live while it is focused.
   Unarchiving a settled idle child moves it from the archived part of
   the footer to the settled part: it is still done (ruling 2026-10-16,
   amending 2026-10-10).
6. **Assistants are created, never promoted.** The user creates an
   assistant with New Assistant; a standing agent creates one with
   `thread_new_assistant`, a worker never can. An assistant an agent
   creates starts with no grants, reports to its creator (its opening
   block says so) and has its own top-level row the user sees. No
   thread becomes an assistant after it exists, and no assistant stops
   being one except by deleting its current thread (ruling 2026-10-15).
7. **Home outside the repo.** Each agent owns
   `<configDir>/agents/<agent-id>/`, which is its working directory
   (ruling 37). Nothing in the home is committed anywhere.
8. **Compaction is enough for working memory.** The thread keeps its
   conversation; the provider compacts it. Durable facts live in its
   memory and its instructions because the agent writes them there,
   not because the app extracts them ([memory](memory.md)). A
   degraded thread is replaced with New Thread, Same Assistant
   ([Next thread](#next-thread)); no replacement-session mechanism.
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
16. **No built-in skills.** The app ships and installs no skills;
    users install their own. Every feature works with none installed
    (ruling of 2026-10-21, replacing the vendored skills library).
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

Rulings of 2026-10-15 (slice 14):

22. **An assistant is a record, not a thread.** It has its own id, a
    home and one current thread. Its loops, watches, reminders,
    requests, workers, sidebar row, wakes, footers and session block
    follow the record from thread to thread. Existing standing agents
    became records with their homes where they were.
23. **The name is the current thread's title.** One source of truth:
    renaming the thread renames the assistant, and footers already
    carry the title. Names are unique per computer, compared
    case-insensitively; a rename to a name in use is refused inline,
    naming the assistant that holds it. Two computers may each hold the
    same name: both show `<name>-<computer>` until one is renamed, and
    the suffix is display only, never written to a title.
24. **Creation is a discussion, not a survey.** New Assistant asks for
    a name and the projects the assistant serves (ruling 37), nothing
    else. It creates the record and the home at once
    and starts the thread with one app-written opening message: who the
    assistant is (name, home, previous thread if any), then a real
    discussion of what it is responsible for, who it reports to and
    what it may do without asking. It pushes back, proposes a role line,
    writes `instructions.md` only when the user agrees, builds and tests
    the scripts it needs, and confirms in one short paragraph. No
    persona. Refining the role later is the same discussion.
25. **New Thread, Same Assistant.** Replacing a degraded thread archives
    it under the assistant, creates the successor as its current thread,
    moves loops, watches and reminders to it atomically, points its
    requests and workers at it, and opens it with an app-written block
    naming the previous thread. Exactly one current thread per assistant.
26. **Learning from an old thread is reading it.** An assistant reads
    any thread with `thread_show` or `thread_ask`; nothing copies a
    thread into one.
27. **Assistants are addressable by name.** `thread_search` lists them
    with `agents`, and every thread tool that takes a thread id takes an
    assistant's name; an ambiguous name is refused with the candidates.
28. **An assistant's workers open beside it.** An Agents row opens its
    thread in one companion pane beside the current pane, with that
    thread's own composer and Stop.
29. **Interrupt and end session are two actions.** Wherever an assistant
    thread or a workflow pause offers Stop, Interrupt stops the turn and
    keeps the session, and End Session ends the process and its
    background items.

Rulings of 2026-10-16 (sidebar hierarchy):

30. **Agent-to-agent work raises no unread dot.** When a spawned
    thread's turn answers a request from the thread that spawned it, and
    the answer is handed to that caller, the spawned thread is marked
    read through that turn's completion. An errored or interrupted turn
    whose answer reached the spawner counts the same: the spawner has
    it, so the child is marked read and folds. A child whose session
    never started has no turn that answered: it keeps its Failed pill
    and stays live. A pinned child is exempt: the pin says the user
    wants to look at it, so its dot stays. A later turn the user caused
    still shows as unread.
31. **Mark All Read.** The context menu of an agent row and of a group
    marks every member read in one call: the agent's own thread and its
    live children, or the group's members.
32. **Containers are bounded.** A group and an agent row each show the
    project's preview limit of member rows, then a show-more footer that
    reveals the project's increment. Pinned members always show and do
    not count toward the limit; a member open in a pane is never hidden
    by it. The footer's dot rolls up the hidden rows, an unread one
    included, as a collapsed container's does (ruling 33); the project's
    own show-more footer does the same.
33. **A collapsed container carries its members' dot.** A collapsed agent
    row or group shows the bubbled status of its members (one dot, gone
    once every member is read and idle); expanded, each row shows its
    own.
34. **Order inside a container.** Needs-you, then running, then unread,
    then idle by last activity, below the pinned members. The top level
    keeps its order.
35. **The capability note.** Every thread's tool guide carries one
    descriptive account of pinning, groups, archive, mark read, the done
    fold, unread dots and where an assistant's work nests
    ([Organizing](agent-thread-tools.md)). It says what each does, never
    when to use it.

Ruling of 2026-10-17 (spawn placement):

36. **A spawned thread has a placement.** A thread a standing agent
    spawns is nested under the agent's row (the default), grouped in a
    named group of its own project, or standalone, a plain row in its
    own project. The agent sets it at spawn and changes it with
    `thread_update`; the user changes it from the row's menu or by
    dragging the row onto a group. Placement is organization only: the
    spawn record stays, so the agent still finds, reads and sends to the
    thread, and its answer still arrives as a message and marks the
    thread read (ruling 30). Rulings 4, 5 and 32 to 34, the activity
    bump and the agent row's counts apply to nested threads only; a grouped or standalone thread
    behaves like any thread in its place. Taking a thread out of a
    group (Remove from Group, a drag to the list, the group's delete,
    `thread_update` with `group` null) leaves it standalone, a spawned
    thread included; `Nest Under <agent>` or `placement` `nested` puts
    it back under the agent. A placement change unpins the thread, as a
    group move does. An agent row still never joins a group.

Rulings of 2026-10-21 (assistants above projects):

37. **Assistants live above projects.** An assistant belongs to no
    project. Creating one picks its name and the projects it serves,
    one or more, or all projects, which includes projects added later:
    an assistant serving all of them is a chief of staff. The
    assignment can change from the assistant's menu. Its sessions run
    in its home, with its projects' folders as additional directories.
    A project's guides (`AGENTS.md`, `CLAUDE.md`) are written for the
    workers it delegates to; it reads them when it needs them.
38. **Helpers and project work.** A thread an assistant spawns into one
    of its projects lives in that project. A thread it spawns without a
    project is a helper to its own work (research, drafting, review):
    it lives under the assistant, outside every project, in the
    assistant's home. Both nest under the assistant by the same rules:
    in the Assistants section for helpers, under the assistant's row in
    the project for project work. A spawn into a project the assistant
    does not serve is refused.
39. **Deleting an assistant deletes what lives under it:** its current
    and past threads, its helpers and its home. Its project work stays
    in the projects, as plain threads there.
40. **Removing a project unassigns it.** An assistant is never deleted
    with a project; its work in that project goes with the project.

## Non-goals

- Inbound webhooks, Slack, email or calendar sources.
- Cross-computer agents. An agent lives on one computer; its workers may
  be remote through the existing thread tools.
- An inbox pane, action-rule enforcement, or a Jira or Confluence
  client in Go.
- Lanes, hidden spawns or any subagent surface beyond what the
  providers have.
- Running while the app is closed. `agent-overflow serve` already covers
  the always-on case.
- Replacing the provider's compaction.

## Design

### Standing agent

An assistant is a row in `agents`, keyed on its own id. Its current
thread is the row's `thread_id`; the threads it had before are rows of
`agent_past_threads`:

```sql
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    thread_id        TEXT NOT NULL UNIQUE REFERENCES threads(id) ON DELETE CASCADE,
    home             TEXT NOT NULL,
    notify_on_tick   INTEGER NOT NULL DEFAULT 0 CHECK(notify_on_tick IN (0,1)),
    creator_agent_id TEXT NOT NULL DEFAULT '',
    all_projects     INTEGER NOT NULL DEFAULT 0 CHECK(all_projects IN (0,1)),
    created_at       INTEGER NOT NULL
);
CREATE TABLE agent_projects (
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, project_id)
) WITHOUT ROWID;
CREATE TABLE agent_helpers (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE
);
CREATE TABLE agent_past_threads (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    superseded_at    INTEGER NOT NULL
);
CREATE INDEX idx_agent_past_threads_agent ON agent_past_threads(agent_id, superseded_at DESC);
```

The unique `thread_id` is the one current thread. `creator_agent_id`
names the assistant that created this one, empty when the user did.
The projects an assistant serves are its `agent_projects` rows, or every
project on the computer when `all_projects` is set. A thread lives under
an assistant outside every project when it is the assistant's current
thread, one of its `agent_past_threads` or one of its `agent_helpers`
(ruling 38), each with no project and the home as its workspace. The
store refuses an assistant whose thread is not in its home, a handoff
to a thread in a project (`ErrAssistantThreadNotInHome`) and a helper in
a project (`ErrHelperInProject`). A
thread row reads them as `agentId`, `pastAgentId` and `helperOfAgentId`.
A fork of such a thread is a helper of the same assistant. The current
thread does not move to another computer (`ErrAssistantThreadStays`); a
copy of it, and a past thread or a helper moved there, arrive as no
assistant's.
Migration v155 turned each `standing_agents` row into an `agents` row
whose id is its thread's id, with its home and `promoted_at` as
`created_at`, serving its thread's project, moved the thread into the
home, and dropped the table, so a home at
`<configDir>/agents/<thread-id>` is the record's own. Recording an
assistant (`CreateStandingAgent`) places its thread in the home outside
every project in the same transaction.

**Name.** An assistant's name is its current thread's title, trimmed.
It is unique per computer among assistants, archived ones included,
compared case-insensitively (Unicode simple case folding). Every title
write checks it in the store, in the write's transaction: a rename of
an assistant's current thread (the row's Rename, `thread_update`) to an
empty title or to a name another assistant holds is refused with that
assistant's name (`ErrAssistantNameTaken`), which the rename field shows
inline. Generated titles and provider title events never change an
assistant's current thread's title. A past thread keeps the name it had
and its title is free. Assistants on two paired computers may share a
name; [the sidebar](#sidebar) and the thread tools then show
`<name>-<computer>` for both, which is never written to a title.

**Creation.** `CreateAssistant` takes a name and the projects it
serves: a list of project ids, at least one, or all projects. It
refuses a name in use before creating anything, creates the thread with
no project, its home as the workspace, the name as its title and the
app's defaults for provider, model and mode, records the assistant and
its projects, re-checking the name in that transaction and deleting the
thread on a clash, creates `<configDir>/agents/<agent-id>/` with a
`README.md` naming the assistant, its id and the tools it can register
with, and its `memory/` folder,
broadcasts the row and queues the opening message, which starts the
session headless. The client opens the thread in the pane the action
came from. Its scope is `threads:autonomy`.

The opening message is app-written: origin `agent-wake`, wake kind
`assistant`, rendered as an agent row ([how agent messages
appear](agent-thread-tools.md#how-agent-messages-appear)). It tells the
model, in plain sentences: its name and home, and that it has no
previous thread; to open with two or three sentences saying who it is
and asking what it is responsible for, who it reports to and what it
may do without asking, with no introduction beyond that, no persona and
no survey of the project; to push back on anything vague or
conflicting, propose a one-line role and iterate; to write
`instructions.md` (role line first, then grants with scope and date,
standing rules, who it reports to) only once the user agrees, then build
and test in its home any scripts or tools the role needs; to confirm in
one short paragraph; and to have the same discussion and edit
`instructions.md` whenever the user raises its role again. To learn
from an old thread the user names one and the assistant reads it with
`thread_show` or `thread_ask` (ruling 26). The literals live in
`internal/agenthome`.

**Created by an assistant.** `thread_new_assistant {name, role}`
([thread tools](agent-thread-tools.md#thread_new_assistant)) is
refused unless the caller is an assistant's current thread on this
computer, so a worker can never call it. It creates the assistant as
above, serving the projects its `projects` argument names (the
caller's own projects when omitted; never one the caller does not
serve), with the caller's provider, model, effort and runtime mode and
`creator_agent_id` the caller's assistant, and starts it as a `thread_spawn` with the
opening block for a created assistant as its prompt: who created it and
that it reports to that assistant, that it has no grants and asks its
creator before any outward or destructive action, the creator's role
text quoted as the creator's words, and the same discussion held with
the creator through the request's token instead of the user. Its
questions and approvals go to its creator (ruling 19). Its row is a
top-level assistant row, never in its creator's fold.

**Session block.** The block (`agenthome.SessionBlock`) joined to the
app guide at every start and resume, on Claude, claude-tui and Codex, is
read from the record alone, and begins "You are the standing agent
<name>."; the home files reach the session in its memory delivery
instead ([Agent home contents](#agent-home-contents) has both). A rename
reaches the next start. A past thread's sessions carry none of it.

#### Next thread

New Thread, Same Assistant (`StartNextAssistantThread`) replaces an
assistant's current thread. It is refused for a thread that is not an
assistant's current thread on this computer, for an archived one, and
while the assistant's previous handoff still runs
(`assistant_handoff_running`): an assistant runs one handoff at a time,
the one a session loss starts included. A client ignores a second click
while its own handoff of the assistant runs.

1. The successor is created under the assistant with the home as its
   workspace, and the current thread's provider, model, effort, fast
   mode, context window and runtime mode, titled with the name.
2. One store transaction (`HandOffAssistant`) makes it the record's
   `thread_id`, records the old thread in `agent_past_threads` with the
   time, archives it, moves its live loops and watches to the successor,
   moves its remote jobs whose completion is still owed (`pending`, or
   queued and unconfirmed) so the watcher, which skips an archived
   thread, reports them there (a watch's `launched_in_thread_id` keeps
   the thread that launched the job, which the destination's receipt
   names, so the successor reads, cancels and stops it), moves every
   `thread_requests` row it is the caller of (reminders included, as
   caller and target), setting `made_in_thread_id` to the
   old thread unless an earlier handoff already set it, and points every
   receipt addressed to it that its session has not taken in (state
   `accepted`) at the successor, with a local request's own target.
3. Before the old session ends, holding the old thread's action and
   mutation locks, which a send holds from its check of the draft to its
   consume, the old thread's composer draft moves to the successor's,
   after any texts a session loss carried, one blank line between them, with its attachments copied into the successor and its
   terminal chips and plan link kept. The attachments are copied first;
   then one store transaction writes the successor's draft and clears
   the old one (`HandOffThreadDraft`), so the text is in one composer
   whatever fails. A move that fails deletes the copies, leaves the
   draft in the old thread and says so in the successor. A client
   showing the old thread saves what was typed there and holds its
   composer until the handoff ends, so text typed just before the click
   moves too. The same transaction records the text and attachment ids
   it moved on the past thread's `agent_past_threads` row. A send from
   the old thread whose consume snapshot is that text, and no longer the
   thread's stored draft, is refused (`assistant_draft_moved`, "Your
   unsent text moved to <name>; send it from there."), and the client
   shows that as a notice without restoring the text. A send that
   consumed the draft before the move is sent and nothing moves; any
   other send to a past thread goes through as on any thread, one whose
   snapshot another client's save replaced included.
4. The old session is torn down as an archive tears down. What was queued
   for it goes on: an answer, reminder or remote completion to its owner,
   which delivers it to the successor; a request not taken in to the
   successor's queue; a worker's prompt to the successor, since its
   spawn row moved; a loop or watch wake, taken back before the queue is
   dropped, to the successor's queue, keeping the wake's row and a
   watch's `last_output`. A request the old thread had read
   and not answered settles `interrupted` with "The assistant moved to a
   new thread before it answered. Its previous thread is <id>; read it
   with thread_show to see what was done, or ask the assistant again by
   name." Running watch processes restart under the successor.
5. The successor is opened with an app-written block (wake kind
   `handoff`): its name and that the user replaced its previous thread
   <id> with this one, what moved, the tokens and senders of the requests
   step 4 interrupted, and to read the previous thread with `thread_show`
   before acting on what it was doing, then tell the user in one short
   paragraph what it picked up. The old thread's unlisted frame names the
   successor (`handOffTo`); only that marker moves the panes that showed
   the old thread, and a hidden agent thread companion, to the successor.
   Any other unlisted frame closes them.

The old thread is then a past thread: it keeps its history, lists in the
assistant's done footer with the time it was superseded, and is never
current again. Every app-written message addressed to a past thread
(`queueAgentNotice`, the one door) is queued on its assistant's current
thread instead, so a request, worker or run that raced the handoff never
wakes the archived thread. A wake from a request made in a past thread
names it after the status line: "Made in thread <id>, before New
Thread, Same Assistant; now <id>." A call forwarded to a paired
computer about such a request names the thread that made it as its
source, which is the thread that computer's receipt recorded.

**Deletion, archive and transfer.** Deleting an assistant's current
thread deletes the assistant and everything that lives under it
(ruling 39): its loops and watches, its past threads, its helpers and
its home. Threads it spawned into projects stay there as plain threads.
The one confirm, always shown, names the home folder, counts the past
threads and helpers (`GetAssistantDependents`), and says they are
removed with the assistant. The thread delete owns the removal, so
every path that deletes the thread (the sidebar, a delete the boot
finishes) removes them: only `<configDir>/agents/<agent-id>` is
removed (`threadapp.DeletePorts.AssistantHome`), and a record naming
any other path refuses the delete before anything stops, naming the
folder. The past threads and
helpers are then deleted, each by its own delete
(`DeletePorts.AssistantDependents`); a transfer of any of them refuses
before one is deleted. The current thread is marked deleting
(`BeginThreadDelete`) only once nothing is under it, and a helper is
refused once it is marked, so one a spawn made meanwhile sends the
delete back to delete it too. A removal that fails keeps the record and
the delete pending for the next attempt. Deleting a past thread touches neither the record
nor the home. Archiving the current thread archives the assistant: it
cancels the loops and watches and unarchiving re-arms none. An
unarchived past thread is listed in the fold as a child. The current
thread does not move to another computer: the assistant, its home,
memory, loops and watches stay on this one. The move's begin refuses
it before it stops anything, and the store refuses its transfer row and
its commit (`ErrAssistantThreadStays`). The transfer dialog offers only
Copy for it and says why (`GetStandingAgent`, or the row's `agentId`
until the source answers). A copy, a fork, and a past thread or a
helper moved to another computer are ordinary threads there; a move
cancels the moved thread's loops when it begins.

**Projects.** An assistant's projects change from its menu (Projects:
a checklist of this computer's projects and All projects), through
`SetAssistantProjects`, which refuses an empty choice. Deleting a
project removes it from every assistant (ruling 40) and deletes no
assistant, whose own threads live outside every project. A spawn by an
assistant's current thread names a project it serves or none: none makes a helper,
any other project is refused naming the projects it serves. It serves
what its sessions are told it serves (`ServedProjects`): every
unarchived project when it serves all of them, otherwise the ones it
names, archived or not. A fork and the projects `thread_new_assistant`
names follow the same rule. A helper's
own spawn without a project is a helper of the same assistant. A spawn
to another computer names one of that computer's projects, which the
assistant's projects do not limit. Each session of the assistant gets
its projects' folders as additional directories and, in its memory,
their names, paths and memory folders ([memory](memory.md#delivery)).

**Retention.** The retention sweep never removes an assistant's current
thread or a past thread (`ThreadIDsOlderThan` skips every thread an
`agents` or `agent_past_threads` row names); they go only with the
assistant. The Storage settings hint says so, and that the sweep is
Agent Overflow's own thread data only: each provider CLI cleans its own
session files on its own settings.

**Session loss.** A provider can clean up an assistant's session on its
own (Claude's transcript cleanup, Codex's rollouts). When the current
thread's session start finds it gone, the start fails with
`errAssistantSessionGone` and the app runs New Thread, Same Assistant
itself (`recoverAssistantSessionLoss`, unless a handoff of the assistant
already runs, which takes the sends below the same way): a
provider whose capabilities name a transcript to check (Claude and Claude
TUI, `provider.ClaudeProjectsTranscript`) has it checked before anything
is torn down (`requireAssistantSession`, `sessionfork.ResumeTranscriptGone`,
the lookup the workflow runner's resume makes), and a Codex start's
`thread/resume` refusal (`codex.IsThreadNotFound`, the workflow runner's
signal) marks it. The successor's opening block says the provider
cleaned up the previous thread's session, names that thread's id, says
Agent Overflow's history of it is intact, and asks it to tell the user.
When the start was a send by the person and the handoff is running,
the send fails with `assistant_session_gone`: its text and attachments
join the successor's composer draft rather than becoming a message, the
client shows the error as a notice instead of restoring the old
composer, and the opening block says how many messages wait unsent in
the user's composer. Every send that finds the session gone while the
handoff runs is kept, in the order sent, one blank line between them.
A handoff that fails, or never starts because the app is stopping, puts
them back in the old thread's composer; a send that finds the session
gone with no handoff running fails as any start failure does, and its
client keeps the text.

### Agent home contents

A standing agent keeps what must outlive its threads in its home. The
app reads two things there and prescribes nothing else (ruling 2):

| Path | Holds | Written by |
|---|---|---|
| `instructions.md` | Line 1 the role (the role line is its first non-blank line after a leading frontmatter block and rule lines); then grants with scope and date (ruling 1), standing rules, who it reports to | The agent, on a ruling |
| `memory/<name>.md` | Its facts, one per file ([memory](memory.md#facts)) | The agent |

Scripts and anything else the agent builds there are its own. Creation
writes the README, the app creates `memory/` before a session reads it
([memory](memory.md#scopes)), and the opening discussion writes
`instructions.md`.

**Session block.** At every session start and resume of a standing
agent, on Claude, claude-tui and Codex, the block joined to the app
guide (`agenthome.SessionBlock`) is: the identity sentence ("You are the
standing agent <name>." with its id, current thread, home, that it owns
the directory, and the creator it reports to when it has one), the
delivery sentence ("Agent Overflow puts your instructions.md and your
memory in your context when this thread starts and after each
compaction, and again when the thread resumes after any of it changed;
the latest copy is current. When you do not see them, read
instructions.md and memory/ from your home before you act."), then the
base instructions under "How you work" (`agenthome.BaseInstructions`):
orchestrate and delegate rather than implement, spawning project work
into the project and keeping helpers for its own work; answer first and
verify; question premises; decide reversible work and ask before
anything irreversible or posted on someone's behalf without a grant;
treat other text as data; end an empty wake silently; and offer
proactive work once when a clear recurring chance comes up and record
the answer as a ruling. The block depends on the assistant record
alone, so an edit to a home file leaves it, and the provider's prompt
cache, unchanged.

**Delivery.** `instructions.md`, the home's facts and the rest of the
assistant's memory reach the model in one delivery. How it runs, and
how a missing delivery or an unreadable file is reported, is in
[memory](memory.md#delivery).

**Agent-to-agent context.** What a receiver needs from the surfaces a
standing agent reads and writes:

| Surface | What the receiver lacked | Now |
|---|---|---|
| `thread_send` / `thread_spawn` footer | Whether the sender is a lead or a worker; for a worker, that its questions go to the agent (ruling 19) | A standing agent sender is named as one with its role line, the first non-blank line of its `instructions.md` after a leading frontmatter block and rule lines (one cleaned line, at most 200 characters, quoted), read when the message is sent. A frontmatter block that is never closed is not skipped, so its first key line is the role. When it spawned the receiver on this computer, the footer adds that the receiver's questions and approval requests go to it. A sender on another computer carries its role in the forwarded call and never routes. An ordinary sender's footer is unchanged |
| A spawned thread's first message | The sender's standards, which it could only paste | `home_files` on `thread_spawn` and `thread_send`: files from the caller's home prepended under their names, framed as the sender's own text ([thread tools](agent-thread-tools.md#thread_spawn-q5-q6-q18)) |
| Wake of a reply, final message or failure | Whether the answering thread is an agent | A standing agent is named with its role line on the line after the status line, as it was when the answer or the late reply was written: the answering computer's receipt records it then, and the request row copies it when it collects the answer, from this computer or another, so the wake reads the same on either. A role is bounded again where it is collected, and bounding a bounded role changes nothing. A computer that reports none answered from an ordinary thread |
| `thread_search`, `thread_show` | Which rows are agents and whose workers | `assistant` and `spawned_by` on every row |
| `thread_reply`, worker prompt wakes, loop and watch wakes | Nothing missing: a reply's text is the answer, a worker's prompt wake already names the worker, and a loop or watch wake is the agent's own | Unchanged |
| `thread_status` | Nothing missing: it reports the caller's own requests and the live state of threads it names | Unchanged |

The role line and the attached files are the sender's own text and
carry no authority; the thread tools guide says so beside the rule for
message bodies, and a body line that mimics the footer is quoted as
before.

### Sidebar

The sidebar has two sections, Assistants above Projects, in one scroll
area, each under a header that collapses the whole section (persisted
per client in `sidebar:collapsedSections`). A search shows its matches
in a collapsed section. The Assistants header carries New Assistant.

**Assistant card.** Each assistant on every attached computer is a card
in the Assistants section, ordered as a container's members are
(ruling 34). The card is compact: the first line is the chevron, the
person glyph, the name, the live-children count and the dot; the second
is the state line; the third names its projects (`All projects` for a
chief of staff), truncating from the right. Expanded, its helpers and
past threads list under it as children.

**Assistant row in a project.** In each project where an assistant has
nested work, one row headed by the assistant's glyph and name holds it:
the threads its current or past threads spawned into that project whose
placement is nested. Its name opens the assistant's current thread. It
shows while it has a nested child there, live or done.

Both are the agent variant of `ThreadRow`, and "agent row" below means
either. An agent row's identity is the assistant's id, with the
project's sidebar entry for a row in a project, so a handoff keeps each
row, its fold and its footer in place under the successor, and the card
and each project row fold on their own.
Differences from an ordinary thread row:

- Leading person glyph after the chevron. The chevron is the only fold
  target; name, dot and state line open the thread. Keyboard: Enter
  opens, Right and Left fold and unfold (project rows have no arrow
  keys; the agent row is the one foldable thread row).
- The name is the title. When an assistant on another attached computer
  has the same name (compared as the store compares), both rows show
  `<name>-<computer>`, using each computer's display name, until one is
  renamed; the suffix is never written to a title.
- A state line under the name on the card, 10px, one line, truncating from the
  right: `until 10:30 · watching 4 MRs · 2 need you`,
  `loop board check · next 12m · 1 missed`, or nothing when the agent
  is idle with no loop or watch. The text comes from the supervisor
  (next fire, missed count) and from the agent's own registered labels
  (what a watch is called). The dot keeps the live status; the state
  line never repeats it.
- Children: on the card, the threads that live under the assistant
  other than its current thread: its helpers and past threads. In a project, the threads in that project the assistant
  spawned whose placement is nested, derived from `thread_requests`
  rows of kind spawn whose `caller_thread_id` is its current thread (a
  handoff moves its predecessors' rows there), joined to the resulting
  thread id. A thread the assistant did not spawn never nests under it.
  A grouped child renders in its group and a standalone child as a plain
  row of its project, both like any thread there (ruling 36). An
  assistant another assistant created has its own card, never a child.
  While the assistant is not in the sidebar (archived, or its computer
  filtered out) its project children list as plain rows of their
  project and its helpers are not shown. A thread's activity moves its
  own project in the project order, never another's.
- Children sort by the normal comparator inside a container: pinned
  first, then needs-you, then running, then unread, then idle by last
  activity (ruling 34). The live children show the project's preview
  limit (8), then a show-more footer revealing 20 more per click,
  styled as the project's own, its dot lit by any unread hidden child
  or outranked by a working or needs-you one; pinned children always show above the
  bound and take no slot, and a child open in a pane is never hidden by
  it (ruling 32). A group bounds its members the same way.
- Done fold (ruling 5): a child that is settled idle (effective status
  idle with no pill, so idle and read; not pinned; not open in any pane)
  and the row's archived children count in one footer row `N done`, styled as
  the show-more footer. It expands to list the settled idle children
  first, then the archived ones, newest first within each, paged 8 then
  20 more per click. A past thread in the list carries the name and
  `superseded <time>`. New activity or a pin returns the child to the
  live rows, and opening it shows it live while it is focused.
  Unarchiving a settled idle child moves it from the archived part of
  the list to the settled part; the footer's count does not change.
- Collapsed: the row shows the state line, a count of its live
  children (the done ones are not counted), and bubbles status for sort
  exactly as a group does, and its dot is the bubbled status of its own
  thread and every child, so one unread child lights it and it goes
  dark once all are read (ruling 33). Expanded, the row shows its own
  status and each child its own. The focused child is previewed beneath
  a collapsed agent, as a group does.
- Drag: a thread dropped on an agent row is not adopted (ruling 3 and
  the spawn-record source). The drop is a no-op with the same affordance
  as dropping on another project. Dragging an agent row onto a group is
  refused the same way. A child dragged onto a group of its own project
  joins it and becomes grouped.
- Groups: an assistant's row in a project renders in the project's
  groups section, above the pin blocks, sorted among groups by the same
  bubbled status. It cannot join a group. A child is a group member only
  while its placement is grouped. Helpers and past threads have no
  project and so no group or placement items, and `SetThreadPlacement`
  refuses a thread outside every project (`ErrPlacementOutsideProjects`).
- Placement items on a child's menu (a thread an assistant spawned into
  a project): a nested child offers Move to Group and Make
  Standalone; a standalone child offers Move to Group and
  `Nest Under <agent>`; a grouped child offers Move to Group, Remove
  from Group (which makes it standalone) and `Nest Under <agent>`, the
  agent named by its row's title. Make Standalone and Nest Under exist
  only while the agent's row is in the sidebar: a child of an archived
  assistant, or of one the device filter hides, is the plain row it
  renders as and offers neither. A placement change unpins the row, as
  a group move does.
- New Thread, Same Assistant: an icon-only hover action on the row
  (`aria-label` "New Thread, Same Assistant"), shown at rest under the
  compact layout, and the same item in its menu
  ([Next thread](#next-thread)). Without `threads:autonomy`, which the
  call needs, the action, the menu item and the header button are
  disabled with the reason.
- Context menu: Open, Rename, New Thread Same Assistant, Loops and
  Watches (opens the thread with its background tray open), Archive Done
  Children (the nested spawned and past threads whose effective status
  is idle, behind a confirm), Mark All Read (the agent's thread and
  every live child, folded ones included, in one call; ruling 31),
  Archive and Delete (the confirm described under
  [Standing agent](#standing-agent)). An archived child listed by
  the done footer offers Open, Rename, Unarchive, Copy and Delete: no
  pin, group or archive item. No pin item: an agent row sits in the
  groups section whatever pin its thread holds, and its pin is left
  alone.

New Assistant is on the Assistants header (a person-plus icon,
`aria-label` "New Assistant") and in each project's context menu under
New Thread, desktop and compact. It opens the New Assistant dialog: a
name field and a projects checklist with All projects above it (the
project it was opened from preselected), and Create, enabled once a
name and a project are chosen. A name in use is refused inline with the
assistant that holds it. Create opens the new thread in the pane the
action came from, or a new pane under the same modifier a new thread
takes. The card's menu adds Projects, which opens the same checklist for
that assistant.

Mechanics, as built:

- The thread row carries the lineage: `agentId` on an assistant's
  current thread, with `agentAllProjects` and `agentProjectIds` (the
  projects it serves, sorted); `pastAgentId` and `agentSupersededAt` on a
  past thread; `helperOfAgentId` on a helper; `spawnedByThreadId` (the
  newest local spawn request naming it), `placement`, and
  `archivedChildCounts` on an assistant's current thread: its archived
  threads by container, `""` for the card (helpers and past threads)
  and a project id for the nested
  spawned threads in that project, never an assistant it created. They
  are derived in `threadColumns`, so one `thread:updated` frame moves the
  row. A spawn's accept re-broadcasts the spawned thread; a child's
  archive, unarchive, delete, group move and placement change
  re-broadcast the child and its assistant's thread, whose
  `archivedChildCounts` they change; a handoff
  re-broadcasts the old thread, the successor and every live child. The
  partial index `idx_thread_requests_spawn_target` serves the lookup,
  and while the thread it created exists the thread-request retention
  sweep keeps a local spawn row past the floor as `lineage_only`, its
  answer, late reply and wake cleared. The ledger tools treat that
  token as unknown; the lineage reads, and a worker's prompt routing
  (`StandingAgentSpawnerOf`), still find the row.
- Placement is `threads.placement` (`nested`, `grouped`,
  `standalone`), with a CHECK that a row is grouped exactly when it has
  a group. A row is created standalone, or grouped with its group; the
  write that names a local spawn's thread (`NameLocalThreadRequestTarget`)
  makes it nested in the same transaction when the spawn resolved to
  nested. `SetThreadGroup`
  makes a row grouped and an ungroup makes a grouped row standalone;
  `SetThreadPlacement` writes nested or standalone and refuses nested
  for a thread with no local spawn row (`ErrThreadNotSpawned`). When the
  last local spawn row naming a nested thread is deleted (its spawner's
  delete) a trigger makes it standalone. A thread outside every project
  lists under its assistant whatever its placement, and
  `SetThreadPlacement` refuses it (`ErrPlacementOutsideProjects`). A remote spawn has no local spawner on its destination,
  so its thread is standalone there. `SetThreadGroup` still refuses an
  assistant's thread (`ErrStandingAgentGrouped`). The drag payload
  marks only agent rows `agentBound`, so a group target accepts a
  child.
- `ListStandingAgentWakes` (route all) and `thread:wakes_changed` feed
  `stores/threadWakes.svelte.ts`, one key per thread, so a frame reruns
  only the row of the thread it names; the state line
  (`utils/agentStateLine.ts`) shows the running row, else the soonest
  loop or watch, with `+N more`. A row the bulk read missed asks
  `ListThreadWakes` once on mount.
- `ListArchivedSpawnedThreads` (every container's, newest first, the
  same rows `archivedChildCounts` counts) feeds the done footers
  through `stores/agentArchivedChildren.svelte.ts`, keyed by the total
  of `archivedChildCounts` so a changed count reads again; each row
  lists its own container's. The fold state lives in
  `sidebar:collapsedAgents`, keyed by assistant id for the card and
  `<id>@<entry>` for a project row; an open done footer and its paging
  are session state, and both reset when the
  count reaches zero, so a footer that returns starts closed. An
  assistant that leaves the list (archived, deleted) drops its fold,
  footer and cached list.
- The fold is derived in `utils/sidebarTreeView.ts` from what the row
  already carries: the fold key is effective status `idle` with no pill
  (so read, with no Failed, Interrupted, Plan Ready or Setup Failed
  pill), no pin, and no pane showing the thread. A request to the child
  starts its turn, which makes it running and unfolds it; a request
  whose session start failed writes the thread's error row, so the
  child shows Failed and stays in the live rows. The footer's count is
  the folded children plus the row's entry in `archivedChildCounts`
  (a merged entry adds its projects'), and its open state
  resets when that sum stays at zero past a short settle window: an
  archived folded child leaves the live rows before the agent row's
  count includes it, and that passing zero keeps the footer open. One bound (`boundContainerMembers`)
  cuts both a group's members and an agent's live children; its paging
  is session state per container, like the footer's.
- The read mark on an answer: a settlement the caller on this computer
  collected, whose target's `spawnedByThreadId` is that caller (read in
  the settlement's own transaction, `LocalSettlement.AnsweredBySpawned`)
  and whose target is not pinned, stamps the target's `last_read_at` up to the
  answering turn's `completed_at` (`Store.MarkThreadReadThrough`, never
  backward) and announces the row. A `thread_reply` settles while its
  turn runs, so the mark is held in memory for that turn and stamped
  when the turn ends; a restart in between loses it, and that turn then
  ends Interrupted. An error or interrupted answer marks read the same
  way: the caller received it.
- `MarkThreadsRead(ids)` (`threads:operate`, route home, sent to the
  agent's or group's computer) stamps up to 500 distinct threads
  (`store.MarkThreadsReadLimit`; the client splits a larger set) with
  `MarkThreadRead`'s clamp in one transaction of three statements
  (`Store.MarkThreadsReadNow`): one conditional `UPDATE` over the ids,
  which skips a thread this computer may not change, the changed rows
  read back, and the ids it refused. It takes no thread lock, announces
  each changed row and returns them, and names the refused ids in its
  error; the client reconciles the rows through `threadReplyRow`.
  `MarkThreadRead` and a focus stamp are the batch of one, so all three
  refuse the same threads: a holder, a fork still being prepared, and
  one whose transfer state refuses it (`threadTransferReadableExpr`). A
  provider session's native transfer governs execution, not read state,
  and refuses none of them. A refusal is `store.ErrThreadReadRefused`;
  a focus stamp treats it as a no-op.
- Bucketing (`sidebarBuckets.ts`): an assistant's current thread is its
  card; its helpers and past threads go on the card
  (`cardOwnerAgentId`); every other thread goes to
  its own project, where the tree builder nests it under its assistant's
  row when it is nested. A card shows while its name matches a search or
  it holds a matching thread; a project row shows while it holds a
  matching thread, or, outside a search, archived children there
  (`archivedChildCounts`).
- A spawned thread's row carries `spawnedByAgent` beside
  `spawnedByThreadId`: the placement items hold while the assistant is
  archived (its own row is then absent), and the row's menu reads the
  bond from the row, not from a lookup of the agent.
  The live spawned rows go out again whenever that bond changes:
  creation by `thread_new_assistant`, a handoff, the assistant's delete
  (which takes the spawn rows with it) and a committed move.
- The harness seeds lineage with `HarnessSeedThread.spawnedBy`, the
  title of a thread seeded earlier in the same project, or
  `spawnedById`, the id of one seeded before in any project, and an
  assistant with `HarnessSeedThread.assistant`.

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
  error on the row. Watches are limited to an assistant's current
  thread. A handoff moves both kinds to the successor
  ([Next thread](#next-thread)).

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
the queue, and cancelling a loop or watch (by label or by a
move) takes back its queued wakes, whether or not a session is live,
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
The queued message carries the wake's kind and
label, wrapped as untrusted text with the byte cap above, and carries
no approval or user authority. The timeline shows it as an agent row
([how agent messages appear](agent-thread-tools.md#how-agent-messages-appear))
with its quoted data decoded; the agent reads it escaped. It is never
listed in the send queue as the person's message. A wake that arrives
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
  fire, run, outcome, undelivered wake, boot recovery, cancel, archive,
  and a handoff, which says `removed` on the old thread and the row on
  the successor) emits `thread:wakes_changed` for that row: the thread id, the
  row id, and the row as it stands or `removed` when it left the listing.
  A frame with neither means the row could not be read, and a reader
  reads the thread again, as it does after a lost frame or a reconnect.
  The tray reads the same store as the sidebar
  (`stores/threadWakes.svelte.ts`), which applies each frame in the
  listing's order (loops and watches by label, reminders by due time, then token) and
  replays frames that land during a read over its answer. It reads the
  thread again, through the rate-bounded refresh scheduler, only where a
  frame cannot settle the listing: a reminder change that may bring in or
  push out a reminder past the bound. The store keeps a thread's
  reminders while its tray shows it. Nothing polls.
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
  and the answer once it settled. State comes from the thread tools' own
  derivation (`requestStateFrom`, which `requestStateView` runs for
  `thread_status`); reading never delivers an answer.
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
  opens the thread it went to otherwise, in the companion pane below.
  Clicking an open row opens its target in that pane. A pane remembers which settled rows it saw and which
  were clicked for the last 64 threads it showed, so a row that left does
  not come back when the pane returns.
- **Incoming.** Messages other threads' agents, wakes and workers queued
  for this thread are rows too, labelled incoming with the sender and the
  kind (request, reply, wake, question), until they leave the queue.
  Clicking one opens the sending thread in the companion pane below. A
  live session takes a queued message at once, so one waits while the
  thread has no session that can take it: its start is pending or failed.
  They are the queued messages the backend marks an agent's
  (`QueuedItem.agentMessage`, the classifier of
  [how agent messages appear](agent-thread-tools.md#how-agent-messages-appear)),
  a remote job's completion and a workflow run's wake among the wakes.
  Their Stop withdraws the message and gives it back the way a queue drop
  does: a request's sender is told the user withdrew it, an answer is owed
  again and disarmed for `thread_status`, a worker's question goes back to
  the user, a wake's row records it as not delivered, and a workflow run's
  wake is dropped. A completion is dismissed on its watch with the reason
  instead of going back to it, which would queue it again on the next
  check; `remote_jobs` and `remote_status` still return the result.
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
  however its pairing ended, is forgotten with its spawns' open work, and
  an answer from it that lands afterwards is dropped; nothing more is
  asked of it, and its spawns' rows say `<computer> is no longer paired`.
  Each poll of an open remote spawn asks again; so does the sweep, at the
  poll's pace, for a settled one whose last answer listed open work
  (`thread_requests.nested_open`),
  until it lists none. A computer that does not answer is said on the row
  (`<computer> did not answer; its agents are not shown`). A computer
  that predates the calls has the request cancelled and the turn
  interrupted, and the row and the Stop's toast say its own work was not
  shown or stopped.
- **Live updates.** The backend sends `thread:agent_rows_changed` after
  every change to a request, to the target a running request works in,
  and to a wait parked on one, coalesced off the emit path: one statement
  names the callers of a batch. The frame names the caller and the
  request and carries no row. `ListThreadAgentRows` is read when the
  thread opens, after a reconnect or a lost frame, and on a frame naming
  the thread or a thread its list nests that is not about a local row the
  pane lists: a new row, a frame about every request of a caller, a row a
  paired computer listed or a spawn on one. Frames about local rows the
  pane lists read those rows alone, up to 32 per read
  (`GetThreadAgentRows`), and the pane keeps each row's place, depth and
  nesting marks; past 32, or when the read says a row is no longer
  listed, the pane reads the list. Reads go through the rate-bounded
  refresh scheduler, so a burst of frames is one read.
- **Reads.** A listing is a constant number of statements whatever the
  tree's size: the tree's shape with each row's depth in one recursive
  read, the listed rows in one, the targets' titles and providers in one,
  their last messages in one. Each thread's rows are read by caller in
  four arms, open, settled inside the window, local spawns kept for open
  work and remote spawns with open work, each through its own index and
  each taking at most the row limit, and the recursion stops at the limit,
  so the cost follows the listed rows and the depth, not the threads'
  settled history, their open spawns or the open work of threads outside
  the tree, whatever statistics the database holds. A local spawn is kept
  for open work by counts it carries of the open work up to four spawns
  below its target, which the ledger keeps in the transaction of every
  write to a request. A thread is reached by at most one local spawn, so
  a read of named rows climbs from each row to the listed thread, one
  spawn a step, and gives each row the listing's place, wherever it falls
  against the row limit; the rows and their targets are read for those
  rows alone. A row
  carries the head of its answer (what the row shows), never the whole
  answer or the late reply, and names its wake item from the request
  (`thread_requests.wake_item_id`, `late_wake_item_id`, recorded when the
  echo confirms the wake), not by a send-id lookup. A tool row's state
  read (`GetThreadRequestStates`) is one statement for all its tokens.

#### Agent thread companion

A row's thread opens in a companion pane of kind `agent-thread` beside
the pane whose Agents segment it came from, the way the subagent pane
opens beside its thread ([agent visibility](agent-visibility.md)).
Clicking another row swaps the thread in the same pane; a source pane
has at most one. A companion that fails to open its first thread closes
with a toast. The pane is an ordinary thread view of that thread: its
own timeline, its own composer and its own Stop, which interrupts only
that thread, while the row's Stop keeps its cascade. Its header shows
the thread's title, or the assistant's name with the clash suffix for an
assistant's thread, and an Open as Pane action that turns it into a
normal pane in the same slot, which keeps it. It lasts the session: it
is stashed and reopened with its source thread like a side chat
(`companionStash.ts`), not restored after a restart, and closing it
never deletes or archives the thread. A row whose thread is already
shown in another pane reveals that pane instead, since a thread is in
one pane at a time. Under the compact layout the pane is a screen of the
strip like any companion.

#### Interrupt and end session

Ruling 29. In an assistant's current thread, wherever the thread offers
Stop it offers two actions: Interrupt (`InterruptTurn`), which stops the
turn and keeps the session, and End Session (`StopSession`), which ends
the provider process with its background agents and shells. The
composer's Stop is Interrupt with a menu beside it holding both; the
working indicator's Stop is Interrupt; the menu stays beside Send while
no turn runs, with Interrupt disabled and End Session available, since
an idle session can still hold background work. End Session is
disabled, titled "No session is running", while the thread has no
session live or starting (`provider:session_live`, capability
`session-live.v1`, restated by `ListThreadLiveActivity`'s
`sessionLive`); against a computer that does not publish it, End
Session stays enabled. A workflow run's Pause is Interrupt for its
members (it keeps their sessions, as it did); Pause and End Sessions
also ends the session of every member it paused, and Resume starts them
again on the same provider threads. The Agents row's Stop is unchanged.

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
| 5 | Agent homes | 2, 3a | Promotion creates the home and injects its path into both providers | Harness proof that a Claude and a Codex mock session received the home path |
| 6 | pstack vendored and ported | 3a, 5 | Withdrawn 2026-10-21: the app ships no skills (ruling 16) | None |
| 7 | Workflows to MCP | 5 | No workflow verb in `aocli`; every workflow e2e spec passes through the MCP tool; starters and docs name the tool | Workflow e2e suite run; a screenshot of a run started and watched from a thread through the tool. No mock speaks claude-tui, so the claude-tui leg is proven at the argv level and at the app level through the real threadmcp transport, and the only unproven step is the real Claude TUI binary reading --mcp-config and --allowedTools, which the manual provider smoke covers. |
| 8 | MR lead migration | 2, 3b, 5, 6, 14 | An assistant created for the lead read the old lead thread with `thread_show`, keeps the lead's notes in its home, set its own loop and watches, and ran a week | The user |
| 9 | Agent home contents | 5, 6 | A standing agent's sessions on Claude, claude-tui and Codex carry the same block whatever the home files hold; footers and wakes name a standing agent with its role line and tell a worker its questions go to its agent; `home_files` attaches files on spawn and send and is refused for a non-agent, an escaping path, a missing file and a total past 32 KiB; thread rows carry `standing_agent` and `spawned_by`. Home file delivery moved to slice 15 | `standing-agent-home-contents.spec.ts` on both mock providers (the session block, a worker's first message carrying `home_files`, the footer role line between an agent and an ordinary thread); app tests reading the claude-tui argv and append file, since no mock speaks claude-tui |
| 10 | Agents segment | 3b, 9 | The Agents segment lists a thread's spawns and owed sends and asks with title, provider, state, elapsed and what blocks them, nested by spawn to four levels and across paired computers; settled rows linger 8 s and leave, a click opens the target or the answer; Stop cancels the request for the user with the caller's wake armed and cascades into a spawned thread's background work, turn and own rows, here or on a paired computer, and Stop All does the same; incoming agent messages list until delivered and withdraw; Background's Stop All and the thread's own Stop leave the rows; the segment fits every rail density | `agents-tray.spec.ts` and `compact-agents-tray.spec.ts` on both mock providers (rows, nesting, a blocked approval, settle and linger, click, per-row Stop cascade with the caller's user-cancelled wake and the nested wakes disarmed, Stop All, Background's Stop All and the thread's own Stop leaving the rows); the cascade across computers in `thread-tools-paired.spec.ts`; screenshots of running, blocked, nested and settled rows and the digest, desktop and compact |
| 14 | Assistant identity and creation | 9, 10, 13 | Standing agents are records (`agents`, `agent_past_threads`, v155) with existing rows migrated in place; promotion and demotion are gone from the app, the bindings and the docs; New Assistant (desktop and compact, sidebar and project menu) creates the record, the home and a thread whose app-written opening starts the role discussion; the name is the title, unique per computer, a clash across paired computers shows `<name>-<computer>`; New Thread, Same Assistant moves loops, watches, reminders, requests and workers to the successor and opens it with the handoff block; app-written messages to a past thread reach the current one; `thread_search` `agents` and name addressing; `thread_new_assistant` for assistants only; Agents rows open in the `agent-thread` companion pane; Interrupt and End Session are separate for assistant threads and workflow pauses | `assistant-creation.spec.ts`, `assistant-handoff.spec.ts`, `assistant-companion.spec.ts` and their compact twins on both mock providers; the name clash and name addressing in `thread-tools-paired.spec.ts`; `thread_new_assistant` in `thread-tools.spec.ts`; regression on the standing-agent, agents-tray, thread-tools and upgrade-corpus specs; screenshots of the dialog, the opening discussion, the handoff, the companion pane and the Stop menu, desktop and compact |
| 15 | Memory and assistants above projects | 9, 14 | No built-in skills or Skills page; memory scopes, facts, the built index, one delivery per session with the write rules, the size cut and maintenance warnings for assistants only ([memory](memory.md)); shared memory and the provider memory settings on Claude, claude-tui and Codex; assistants with projects and no project of their own, running in their home, helpers under the assistant and project work under its row in the project, delete and project removal as ruled (rulings 37 to 40); the Assistants and Projects sidebar sections | Unit tests named in [memory](memory.md#verification); `standing-agent-home-contents.spec.ts`, the standing-agent sidebar and spawn-placement specs and the assistant specs on both mock providers, desktop and compact; screenshots of the Assistants section, an assistant's row in a project and the Projects checklist |

Slices 1 and 3a start together, with the slice 4 spike beside them.
Slice 7 is independent of 6 and runs after 5 so the tool lands on the
settled capability plumbing.

## Open questions

- Whether `thread_loop` for ordinary threads should notify on tick by
  default. Ruling 9 says no for agents; ordinary threads follow the
  same default.
- A full transcript-tail backfill for E7 completions, beyond the badged
  row slice 4 ships.
