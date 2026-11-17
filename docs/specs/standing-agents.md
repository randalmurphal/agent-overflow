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
   thread it spawns, its worker (ruling 36).
4. **An assistant is one row.** A standing agent's row holds no other
   thread: its workers have no sidebar row unless placed (ruling 36),
   and a placed one is a plain or grouped row of its project. An agent
   row never sits inside a group (ruling 2026-11-14, amending
   2026-10-10).
5. **Done is archive.** Archive is the only done state; nothing folds
   under a row (ruling 2026-11-14, replacing the done fold of
   2026-10-16).
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
9. **Routine ticks are quiet.** A loop or watch turn that ends cleanly
   leaves its thread read and where it was in the sidebar and raises
   no notification; a failed one notifies as an error. Awaiting input
   and approval notify as today. The agent reaches the user from a
   tick only by flagging its thread (ruling 41). This holds for every
   thread's loops, not only an assistant's.
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
    nesting on `parent_thread_id`) has no place in the product. A
    worker's owner is derived from spawn records, not from a parent
    column.
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
19. **A worker's question goes to the thread that spawned it.** A
    thread another thread spawned on this computer does not put a
    question or approval to the user while its spawner is not archived,
    whoever the spawner is: an assistant, an ordinary thread or another
    worker, whose own worker then asks it. Its awaiting-input and
    approval stops are delivered to the spawner as a badged wake, the
    worker shows `waiting on <spawner>` rather than needs-you, and the
    spawner answers through `thread_reply` or escalates by asking the
    user itself, as one question with the context. The user can still
    open the worker and answer directly, which settles the wake. A
    worker a thread on a paired computer spawned and owns (its spawn's
    link) asks that spawn's caller there, named with this computer; a
    prompt that computer does not say it holds for two minutes, or
    whose link ends, goes back to the user here. A worker whose spawner
    is archived, or that another computer spawned without owning it,
    asks the user, and a prompt left open when its spawner is
    archived or deleted goes back to the user, whether or not the
    spawner had read it; one whose spawner was unarchived again before
    that cleanup ran stays with it. A prompt open when the worker's
    session ends closes, and its wake leaves the spawner. A prompt's
    token answers only that prompt; the worker asking again under the same provider id, in the
    same session or a later one, gets a new token. This supersedes the thread tools non-goal
    of not relaying approval requests, for spawned threads only.
20. **Other threads' work is an Agents segment, not Background.** The
    requests a thread is owed (spawns, and sends and asks it waits on or
    asked to be told about) list in their own rail segment after
    Background, nested by spawn as provider subagents nest, with its own
    count, Stop All and digest; reminders stay wakes. A settled row
    lingers 8 s from when the pane saw it settle, longer than a
    Background row because it is a click target, then leaves for the
    segment's Past list, which pages every settled row. Stop
    cancels for the user with the caller's wake kept armed and worded as
    the user's stop; a thread the caller spawned is brought to rest, on
    any computer, with the wakes inside that lineage disarmed
    ([At rest](#at-rest)), and a send to someone else's thread cancels
    only the request. An older paired computer degrades to the request
    and the turn, said on the row and in the toast. Background's Stop
    All and the thread's own Stop do not touch the requests ([Agents segment](#agents-segment)).
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
    it, so the child is marked read. A child whose session never started
    has no turn that answered: it keeps its Failed pill. A pinned child is exempt: the pin says the user
    wants to look at it, so its dot stays. A later turn the user caused
    still shows as unread.
31. **Mark All Read.** The context menu of a group marks every member
    read in one call.
32. **Containers are bounded.** A group shows the
    project's preview limit of member rows, then a show-more footer that
    reveals the project's increment. Pinned members always show and do
    not count toward the limit; a member open in a pane is never hidden
    by it. The footer's dot rolls up the hidden rows, an unread one
    included, as a collapsed container's does (ruling 33); the project's
    own show-more footer does the same.
33. **A collapsed container carries its members' dot.** A collapsed
    group shows the bubbled status of its members (one dot, gone
    once every member is read and idle); expanded, each row shows its
    own.
34. **Order inside a container.** Needs-you, then running, then unread,
    then idle by last activity, below the pinned members. The top level
    keeps its order.
35. **The capability note.** Every thread's tool guide carries one
    descriptive account of pinning, groups, archive, mark read, the done
    fold, unread dots and that a thread it spawns is its worker, out of
    the sidebar unless placed ([Organizing](agent-thread-tools.md)). It says what each does, never
    when to use it.

Ruling of 2026-11-14 (owned workers, amending spawn placement of
2026-10-17):

36. **A spawned thread is its spawner's worker.** A thread any thread
    spawns on this computer is owned by its spawner, the caller of its
    spawn link (the one local spawn reaching it that is not
    `superseded`), unless the spawn places it standalone (a plain
    row of its project) or in a named group. An owned thread has no
    sidebar row and is otherwise an ordinary thread: search and
    `thread_search` find it, it opens in a pane, keeps its session,
    history and retention, and raises no unread mark and no completion
    notification of its own; a prompt handed back to the user still
    notifies. The spawner moves it with `thread_update` `placement`;
    the user with Show in Sidebar on its Agents row and Remove from
    Sidebar on its sidebar row, and a drop on a group makes it grouped.
    Owned is refused for a thread no local thread spawned and for an
    assistant's own thread, and a helper is always owned. Taking a
    thread out of a group leaves it standalone. A placement change
    unpins the thread, as a group move does. Archiving a thread
    archives its owned workers down the lineage, each as an ordinary
    archive; unarchiving it leaves them archived. Deleting it deletes
    its owned workers down the lineage in the same delete, and its
    confirmation counts them; anything under it that refuses the delete
    refuses all of it, and a worker whose delete fails after the delete
    went ahead is named and finished by deleting again
    ([Owned workers](#owned-workers)).

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
    assistant's home. Both are its workers (ruling 36), and a helper
    cannot be placed. A spawn into a project the assistant does not
    serve is refused.
39. **Deleting an assistant deletes what lives under it:** its current
    and past threads, its helpers, the project work it owns (ruling 36)
    and its home. Project work given a sidebar row stays in the
    projects, as plain threads there.
40. **Removing a project unassigns it.** An assistant is never deleted
    with a project; its work in that project goes with the project.

Rulings of 2026-10-22 (attention):

41. **An agent flags its thread for the user.** `thread_attention`
    marks the calling thread with a kind, a decision to make, work to
    review or a problem, and a one-line reason. The row shows both in
    the sidebar's status colours and reads as unread until the user
    reads the thread, which clears the flag as it clears Completed. A
    turn that ends with the flag up notifies once, as its kind. Turns
    the user started behave as before. Any thread may flag itself.

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
CREATE INDEX idx_agent_past_threads_agent ON agent_past_threads(agent_id, superseded_at DESC, thread_id DESC);
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
assistant's. Recording an assistant (`CreateStandingAgent`) places its
thread in the home outside every project in the same transaction.

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
   workspace.
2. One store transaction (`HandOffAssistant`) refuses an assistant
   archived by then, and a successor that fails the checks creation
   makes (home, no project, group, worktree or branch, no assistant's
   thread). It titles the successor with the name and gives it the
   current thread's provider, model, effort, fast mode, context window,
   auto-compact thresholds and runtime mode, all read in that
   transaction, so a rename or settings change made while the successor
   was being created is kept. It makes the successor the record's
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
   successor's queue; every open worker prompt routed to the old thread,
   its wake queued, still being delivered or already taken in, to the
   successor under a new token, since its spawn row moved, the successor
   answering it, the old token answering nothing, and a withdrawal or the
   user's answer acting on the new registration alone; a loop or watch wake, taken back before the queue is
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
   successor (`handOffTo`); that marker moves the panes that showed
   the old thread, and a hidden agent thread companion, to the successor.
   Any other unlisted frame closes them. A client that missed the frame
   moves them when a thread catalog answer names another current thread
   for the assistant whose current thread they showed
   (`reconcileThreadRows`); one opened on a past thread stays.

The old thread is then a past thread: it keeps its history, lists in
its assistant's previous threads with the time it was superseded
(`ListAssistantPastThreads`, newest first, cursor paged by
`(supersededAt, threadId)`, at most `store.MaxPastThreadsPage` a page),
and is never current again. Every app-written message addressed to a past thread
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
its home, and the threads it owns in projects with them, as any
thread's delete deletes its workers ("Owned workers"). Threads it
spawned into projects with a sidebar row stay there as plain threads.
The one confirm, always shown, names the home folder, counts the past
threads, helpers and workers (`GetDeleteDependents`), and says they are
removed with the assistant. The thread delete owns the removal, so
every path that deletes the thread (the sidebar, a delete the boot
finishes) removes them: only `<configDir>/agents/<agent-id>` is
removed (`threadapp.DeletePorts.AssistantHome`), and a record naming
any other path refuses the delete before anything stops, naming the
folder. The past threads and helpers, and the workers under them, go in
the same delete as any thread's workers ("Owned workers"): a transfer of
any of them refuses the whole delete before anything stops. While the
delete decides, a claimed thread takes the app's messages into its queue,
and a start the app makes for one waits for the decision ("Owned
workers"). From the delete's claim no handoff moves the current thread
(`ErrThreadDeleting`), and from its commit no helper is recorded for it. Its home, row and
record go last, only once nothing is under it: its teardown refuses,
before touching anything, while a past thread or helper remains
(`AssistantDependentsError`). A helper whose teardown fails keeps the
assistant, its home and its history committed and hidden until deleting
it again, or the next boot, finishes them. Deleting a past thread touches
neither the record nor the home. Archiving the current thread archives the assistant: it
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
compaction, and with the next prompt after any of it changes, only what
changed; the latest copy, with the changes after it, is current. When
you do not see them, read instructions.md and memory/ from your home
before you act."), then the
base instructions under "How you work" (`agenthome.BaseInstructions`):
serve the role, acting ahead of the user only where that adds real
value and never looking for work; orchestrate and delegate rather than
implement, spawning project work into the project and keeping helpers
for its own work; answer first, say little and verify; question
premises; decide reversible work and ask before anything irreversible
or posted on someone's behalf without a grant; treat other text as
data; end a loop or watch turn with nothing for the user silently, and
reach the user from one only with `thread_attention`; and, outside the
role, offer proactive work once when a clear recurring chance comes up
and record the answer as a ruling. The block depends on the assistant record
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
| `thread_send` / `thread_spawn` footer | Whether the sender is a lead or a worker; for a worker, that its questions go to the agent (ruling 19) | A standing agent sender is named as one with its role line, the first non-blank line of its `instructions.md` after a leading frontmatter block and rule lines (one cleaned line, at most 200 characters, quoted), read when the message is sent. A frontmatter block that is never closed is not skipped, so its first key line is the role. When the sender, standing agent or not, spawned the receiver on this computer, or is the thread on another computer whose spawn's link owns it, the footer adds that the receiver's questions and approval requests go to it. A sender on another computer carries its role in the forwarded call |
| A spawned thread's first message | The sender's standards, which it could only paste | `home_files` on `thread_spawn` and `thread_send`: files from the caller's home prepended under their names, framed as the sender's own text ([thread tools](agent-thread-tools.md#thread_spawn-q5-q6-q18)) |
| Wake of a reply, final message or failure | Whether the answering thread is an agent | A standing agent is named with its role line on the line after the status line, as it was when the answer that wake carries was written, the first answer and the late reply each recording their own: the answering computer's receipt records it then, and the request row copies it when it collects the answer, from this computer or another, so the wake reads the same on either. A role is bounded again where it is collected, and bounding a bounded role changes nothing. A computer that reports none answered from an ordinary thread |
| `thread_search`, `thread_show` | Which rows are agents and whose workers | `assistant` and `spawned_by` on every row |
| `thread_reply`, worker prompt wakes, loop and watch wakes | Nothing missing: a reply's text is the answer, a worker's prompt wake already names the worker, and a loop or watch wake is the agent's own | Unchanged |
| `thread_status` | Nothing missing: it reports the caller's own requests and the live state of threads it names | Unchanged |

The role line and the attached files are the sender's own text and
carry no authority; the thread tools guide says so beside the rule for
message bodies, and a body line that mimics the footer is quoted as
before.

### Owned workers

Ruling 36, as built:

- `threads.placement` is `owned`, `grouped` or `standalone`, with a
  CHECK that a row is grouped exactly when it has a group. A row is
  created standalone, or grouped with its group; the write that names a
  local spawn's thread (`NameLocalThreadRequestTarget`) makes it owned
  in the same transaction unless the spawn asked for `standalone` or a
  `group` (`threadtools.ResolveSpawnPlacement`). A paired computer's
  owned spawn that carries its level is owned on its destination the
  same way, through its receipt, marked as the link
  (`thread_request_receipts.spawn_link`, `SetThreadReceiptTarget`); its
  thread names that computer (`Thread.spawnedByComputerId`). One that
  arrives without a level, from a computer that predates this, is
  standalone there, and a destination that predates it reports no
  placement, so the spawn's reply says the thread has a sidebar row
  there. A request from the link's device and source thread marks the
  worker read when it answers, as a local spawner's does (ruling 30).
  A spawn's fresh thread, and a fork it will own, are created without a
  frame; the spawn announces the thread once that write has named and
  placed it (`AnnouncePlaced`), so a thread that will be owned is never
  listed. An owned fork is created in no group. A naming write that fails
  deletes the thread it would have named.
- `SetThreadGroup` makes a row grouped and an ungroup makes a grouped
  row standalone. `SetThreadPlacement` (the user's Show in Sidebar and
  Remove from Sidebar) and `thread_update` `placement` write owned or
  standalone; owned is refused for a thread with no spawn link
  (`ErrThreadNotSpawned`), and both are refused for an assistant's own
  thread (`ErrStandingAgentPlaced`) and a thread outside every project
  (`ErrPlacementOutsideProjects`). An owned thread's spawner deletes it
  first, so the link of an owned project thread is deleted while it
  lives only by the cleanup of a spawner moved to another computer; the
  trigger `trg_thread_placement_spawn_gone` then makes it standalone,
  whatever `superseded` rows still name it. A helper stays owned.
- `catalogThreadClause` is the one predicate for what the sidebar
  lists: the thread catalog, the project thread counts and the archived
  threads list leave hidden-mode and owned threads out. Search,
  `thread_search` and retention do not use it.
- An owned thread's turn rest raises no notification
  (`notify.MapTurnRest` on `ThreadRef.Owned`) unless an attention flag
  is up; prompts still notify.
- A placement or group change is announced with
  `ThreadAnnouncer.AnnouncePlaced`: `listed` while the row is unarchived
  and not owned, else `full`. Never `unlisted`, the archive frame that
  closes the panes showing the thread.
- Archiving a thread, through `ArchiveThread` or `thread_update`, then
  archives each unarchived owned thread whose spawn link names it as
  caller, at any depth through workers already archived
  (`ListOwnedWorkersToArchive`), through the same archive, so the
  cascade runs down the lineage and an already archived worker's own
  archive is not repeated. The workers are read under the
  owner's action lock once its archive committed and its session
  stopped, and each worker's flip holds only while the owner is still
  archived and still owns it (`ArchiveOwnedWorker`), so a cleanup that
  runs late leaves alone a worker moved since and every worker of an
  owner reopened since. The owner's archive succeeds once committed: a
  worker that cannot be archived is named beside it
  (`ArchiveThreadResult.WorkersNotArchived`, `workers_not_archived`),
  the others are still archived, and archiving the worker or the
  owner again retries it, however deep, without repeating any archived
  thread's own cleanup.
  The sidebar's archive says so in a toast that archives them again.
  The archive's flip marks it unfinished in the same transaction, the
  walk's end clears the mark, and each boot finishes the archives still
  marked through the same path (`startPendingArchives`): what the thread
  still has of the work an archive stops is stopped, a remote command's
  cancel owed when its computer does not take it, and its workers are
  archived. A thread reopened before the resume reaches it is left alone.
- Deleting a thread deletes what lives under it in one delete
  (`familyDelete`): each owned thread whose spawn link names it as
  caller, archived or not, down the lineage, and for an assistant's
  current thread its past threads and helpers and their workers. One
  store projection lists them (`ListDeleteFamily`); the delete and its
  confirmation's counts (`GetDeleteDependents`) both read it, and archive's walk takes the same
  owned edge (`ownedStepSQL`). Every door that deletes runs it: the
  sidebar's delete and bulk delete, the archived threads list, a project
  removal, a delete the boot finishes, the retention sweep, an ended
  terminal thread, an assistant's delete and the cleanup of an ask's
  copy. No agent tool deletes a thread.
- The delete runs in four phases (`app_thread_delete.go`), each phase's
  write the store's (`store/thread_delete.go`). Claim: one transaction
  claims the whole family for the delete (`threads.delete_claim`,
  `ClaimDeleteFamily`), and refuses, claiming nothing, when another
  delete holds a claim in it (`DeleteClaimedError`). A worker's claim
  holds only while its owner still owns it (`ErrWorkerNotOwned`). A
  claimed thread stays listed and read, takes what the app queues for
  it, and starts nothing. A person's or another agent's door refuses it
  (`ErrThreadDeleting`): execution (`CheckThreadExecutionAccess`), work
  admission, edits, a send, a steer or an outside `thread send`, a
  placement or group move, a handoff and an outgoing transfer. A
  message the app writes for it (an answer, a reminder, a loop or watch
  wake, a worker's prompt, answer or stop notice, a remote completion,
  a continue after a usage limit, a wake a handoff moves, a workflow
  run's wake) is written to
  its queue and withdrawn from it as on a stopped thread
  (`CheckThreadDeliverable`, `threadapp.LockDeliverable`), so the claim
  changes nothing that is due, spent or recorded. What would start
  something waits for the decision, with no failure recorded: a notice's
  session start (`ErrThreadDeleteClaimed`) and a queue dispatch, to a live
  session or with the session start it makes (`lockFlushDispatch`),
  release the thread's action lock and wait for the decision of the delete
  the thread's claim names (`deleteDecisions`), which the delete registers
  before it writes a claim and makes once its claims are released or
  committed. No
  wait there holds a thread's lock: a teardown that gives a moved wake
  back runs the start it tries again as notice work. The
  dispatch keeps its batch as the one it is sending meanwhile, so the
  queue shows it, a withdrawal reaches it and a teardown discards it. A
  refused delete's starts then go on as if the claim had never stood; a
  committed thread's start nothing, and the destroy's teardown gives the
  queue back to its owners. A shutdown ends the wait, and a fire's run
  waiting there ends with its cancel. A queued message's durable row
  says it is still wanted: a withdrawal deletes it, and the dispatch
  reads the rows of each group under the lock the withdrawal takes, right
  before writing it (`commitFlushGroup`), so a message withdrawn before
  then is never sent and one withdrawn after is reported dispatched. A
  failed write's requeue and a session end's restore keep only messages
  whose rows stand. The boot releases every claim no delete committed
  before anything starts, so no claim stands without a delete to decide
  it, and gives every queue back to its owners before any session
  starts. A committed thread's loops and watches do not fire. A fire never claims
  a wake whose previous run is still registered, and a run retires and
  publishes its row in one step, writing nothing once a cancel took its
  registration. Verify: the claimed threads have their work
  admission closed and drained (`threadRestFence.close`), so a spawn
  already admitted names its thread; then the family is claimed again
  once, which takes such a worker, and each thread's refusals run under
  its action lock alone: a transfer, an assistant home the delete may
  not remove, an ended terminal's own check, and for a project removal a
  running thread inside the project or out. The threads that claim
  added go the same way, so the family is read once per pass whatever
  its size. A family that keeps growing refuses the delete. Nothing is
  stopped. A refusal releases every claim in one transaction
  (`ReleaseDeleteClaim`), so a refused delete changes nothing. Commit:
  one transaction marks every claimed thread deleting
  (`threads.deleting`, `CommitDeleteClaim`). Clients are told the rows
  are gone. From here the delete only goes forward. Destroy: each
  committed thread, once every thread under it is gone, is torn down and
  its row dropped under its own action lock (`threadapp.Destroy`). No
  delete holds one action lock while it takes another.
- A thread whose destroy fails keeps its commit with the threads above
  it, and is never rolled back: the named thread's own failure is the
  delete's error; another's is named beside it
  (`DeleteThreadResult.WorkersNotDeleted`) and the sidebar's toast
  deletes the named threads again, which joins the committed threads
  and finishes them. A project removal with any such thread keeps the
  project and says to remove it again; a door with no one waiting logs
  it. The boot releases the claims no delete committed and finishes
  every committed family, dependents first
  (`startPendingThreadDeletes`).
- A delete never removes, checks or waits for a worktree
  ([decisions](../decisions.md)): every worktree its threads were on stays on disk
  with its work, for Remove Worktree to remove. The sidebar, a
  selection, the archived threads list and the compact layout delete
  through `DeleteThreads` (`threads:operate`); threads selected together
  are one delete per computer. The window stops no session for a delete.
  The cleanup of a thread moved to another computer deletes none of the
  workers it left here.
- Archiving and deleting reach the owned workers the threads spawned on
  paired computers. The write that decides it owes each remote spawn one
  operation, in its own transaction (`thread_owner_ops`): the archive's
  end an `archive` (`OweRemoteArchives`), the delete's commit a `delete`,
  or a `release` for a moved thread's cache, and an unarchive a
  `supersede` of an archive already sent. The sidebar's doors and
  `thread_update` make one attempt, all of them within 5 s, before they
  return: a worker whose computer has not taken it is named pending with
  its computer (`WorkerFailure.pending`, `computer`), and one on a
  computer that predates this as running an older version, which is not
  asked again. The request sweep sends the rest, 5 s after an attempt
  doubling to 10 min, until that computer answers
  (`deliverOwedOwnerOps`); a refusal or a computer that does not answer
  never undoes the archive or delete here. That computer admits each
  through its own doors, fenced by the spawn's link (`agent_owner_op`):
  an archive through the archive's path, taking each operation's
  sequence once (`ArchiveLinkedWorker`), so one admitted after its
  supersede changes nothing; a delete through its own claim, verify,
  commit and destroy, the claim holding only while the link owns the
  worker (`ClaimLinkedDelete`), so its refusals and its gate are that
  computer's; a release ends the link. A worker given a sidebar row there
  is kept, and the delete ends its link. Every write that ends links
  announces the workers it released (`announceReleasedWorkers`): a
  release, that delete, the end of the owner device's last session there,
  and the retention floor. The delete's confirmation says when the
  threads it takes spawned workers on other computers
  (`DeleteDependents.remoteWorkers`).
- A spawn's Agents row follows its worker's lifetime, not only its
  request's: it is open while its request is open or the thread the spawn
  made is not at rest, whatever its placement. A thread is at rest when it
  has no turn, no queued message, no open prompt (`threadWaitingOn`:
  the session's prompts, and a Codex async question not yet answered or
  dismissed, which reads `blocked` while nothing else runs or is queued,
  as `thread_status` reads it awaiting input), no running background work
  (`runningBackgroundWorkForThread`: Claude tasks and background agents,
  Codex subagents and terminals, remote commands), no armed loop or watch
  (`agent_wakes`), no reminder that has not fired, no workflow run that
  reports to it running or owing it its report (from the run's rest until
  that rest's wake job ends, whatever it did with the report:
  `workflowapp.Service.ReportingRuns`), no answer on its way to it (a request it made
  settled with its answer owed as a message, not yet echoed by the turn
  that reads it: `ThreadAnswerDue`), and no provider timer
  the idle reaper keeps a session for (`PendingWakeupAt`: a scheduled
  wakeup or an armed cron job), and no open work below it at any depth:
  a thread whose own work is done is not at rest while a worker below it
  is not, so every spawn above a live worker is open up to the root,
  whatever the listing has room to show. One place answers a thread's own
  state (`workerUnrest`), and one the work below it: the listing's reads
  (`ThreadAgentRequest.TargetOpen`, `agentTargetOpenSQL`), whose answer
  the row's state, the listing's membership, Past and the summaries all
  take. A fresh read of both, `threadRest`, is what a spawn's settlement,
  `thread_status` with `thread_ids` and a paired computer's answers take
  ([agent-thread-tools.md](agent-thread-tools.md#wakes-q7-q8-q15-q25));
  every rest pass reads it for each held request's thread, whatever the
  memory record below says, which only draws rows.
  The app keeps the spawned threads not at rest in memory (`liveWorkers`),
  a thread a paired computer's spawn made here included,
  each with its spawn link and what its durable sources last said, and
  re-evaluates a thread only on the events that change a source: its turn,
  prompts, queue and provider timers (`noteWorkerLiveness`, memory reads),
  and its background tray, wake, question, request and workflow frames,
  a workflow run's binding, which names the thread it left and the one
  it reports to now, both re-evaluated, a workflow run's owed report
  ending, and its session's end (`noteWorkerWaiting`,
  which reads its durable sources again: its unanswered questions, the
  answers on their way to it, the background inventory, its workflow
  runs, its loops and watches, and its reminders). A changed request
  re-evaluates its caller when that caller is a worker, named by the same
  read that names the request's frames (`ThreadAgentRowFrames`), and a
  workflow run's state change the thread it reports to. A session's end
  settles the
  background work it owned and ends its timers. At boot one indexed read
  per durable source (armed loops and watches, unfired reminders, pending
  remote commands, answers on their way, running workflow runs,
  unanswered questions, queued messages) names the threads to record,
  each re-evaluated before any request held for a thread's rest may
  settle (`recoverRestAtBoot`); the memory sources report themselves
  once they change. A deleted thread is forgotten. A
  thread only sent to is no worker: a send's row follows its request.
- An open row whose request settled, or is held until its thread rests
  ([agent-thread-tools.md](agent-thread-tools.md#wakes-q7-q8-q15-q25)),
  shows its worker's state (`blocked`,
  `running`, `queued`, then `waiting`, in that order;
  `ThreadAgentRow.worker`), its blocked or activity line and Stop, and
  keeps its answer in its digest. A worker with nothing of its own that
  waits on its own workers shows `waiting` with no activity line: the
  open rows nested under it, or at the depth bound its "nest deeper"
  line, say what it waits on, and its Stop reaches them. A waiting row's
  activity line is what it waits on when the worker shows no activity of
  its own: the background work's summary, or the loop, watch or reminder
  named as its wake rows name it (`Loop · <label>`, `Watch · <label>`,
  `Reminder · <note's first line>`); a timer adds no line. A waiting worker never needs the user. It is counted once
  per worker however many requests went to that thread, on the toggle and
  in the worker summaries, kept past the linger and the settled window,
  left out of Past, and framed to its spawner and the threads above it
  when it changes, every spawn above it framed for its own caller up to
  the root. Once the worker rests the row is in Past: at once when
  its request settled outside the settled window, after the linger
  otherwise. A paired computer's `agent_rows` answer carries why the
  spawn's thread there is not at rest, and whether it is open: not at
  rest, or with open work at any depth below it, read there as a spawn of
  it would read it, whatever rows the answer has room for. It carries the
  thread's placement and archive state there and the prompt it waits on
  (`blockedOn`, `blockedDetail`, `blockedRoutedTo`), with `routedToAsker`
  when that prompt went to the asking device's thread whose link owns it. The sweep keeps
  asking while it is (`thread_requests.nested_open`). A remote spawn at
  the listing's last level asks for that state alone, which lists no rows
  and asks no computer further on, so a chain of computers is asked no
  deeper than the listing shows; its row says when that thread has rows.
  A computer that predates the open state is read from the rows it
  returns, as far as they reach. A failed call is no news of rest: the
  row keeps the state the last answer gave, in a pane's read and in an
  answer a computer gives for its own spawn's computer further on alike,
  says that computer did not answer, and stays open, counted and out of
  Past; with no answer remembered, as after a restart, its recorded open
  work keeps it open, waiting. Only an answer
  closes it: rest there, or a refusal of the thread as not the spawn's.
  Nothing on that computer tells this one when a thread that rested is
  continued there, so the sweep also asks about the settled remote
  spawns at rest, one page of 32 every six poll intervals (30 s) from a
  cursor (`idx_thread_requests_remote_rest`), each once a cycle, through
  the same in-flight, per-computer and backoff bounds; whether a pass is
  due is read from the ledger on that index (`NextThreadRequestWork`), so
  the first spawn to come to rest wakes the sweep for it: with n spawns at
  rest each is asked every ceil(n/32) × 30 s, and the pass makes at most
  about one call a second however many there are. An answer that finds
  one still at rest, or none, is kept nowhere and frames nothing; one
  that finds it working records it, and its row reopens. A computer that
  runs an older version answers that it cannot list, which records
  nothing. Past the retention floor a remote spawn is kept here as its
  lineage, as a local one is: its answer, late reply and wake cleared,
  its poll stopped and its token unknown, and still its thread's link, so
  the rest pass keeps asking about it and its row lists, counts and stops
  as before. It goes once that computer refuses the thread as gone, the
  spawner is deleted, or the computer is forgotten. What it keeps is one
  row per such spawn, read only by the rest pass's pages and by the
  Agents reads that reach it: a person with n old remote spawns whose
  threads there still exist keeps n rows, each asked about every
  ceil(n/32) × 30 s, at most one call a second in all.
- Owned workers nest at most Settings, Agent profiles, Sub-thread levels
  deep (1 to 4, default 2). The level is counted along the owned spawn
  links (`Store.SubThreadLevel`): an assistant's own thread is 0 and its
  helper 1, and a worker the person gives a sidebar row is 0, with its
  own workers counted from it. A paired computer's owned worker counts
  the level its spawn carried while it is owned: Show in Sidebar ends it
  and Remove from Sidebar restores it. A session at the limit starts without
  `thread_spawn` and without native subagents
  ([Sub-thread depth](agent-thread-tools.md#sub-thread-depth)). The limit
  applies to owned workers and an agent's ask copies: a thread that is
  not owned starts the count again.

### At rest

A thread is at rest when it has no turn, no queued message, no open
prompt, no background work, no armed loop, watch or reminder, no
workflow run that reports to it running or owing it its report, no answer
on its way to it, and no open work below it. A Stop of a thread a thread spawned brings it to
rest: the Agents row's Stop of a spawn and Stop All
([Agents segment](#agents-segment)), and an agent's `thread_cancel` by
thread id of a thread it spawned
([agent-thread-tools.md](agent-thread-tools.md#thread_cancel-q19)). An
archive and the Stop share the capture, the session teardown and the
release (`app_thread_rest.go`); an archive then hides the thread and
archives its owned workers ([Owned workers](#owned-workers)), and
leaves the threads it does not own as they are. Besides hiding, the one
deliberate difference is that an archive leaves the workflow runs that
report to the thread running: the next wake of each finds the thread
archived, clears its binding and reports to the workflows overlay
instead. A request open on the thread is not settled by an archive as
step 4 settles it: the session's end settles it `interrupted`, as it
does for any session that ends. The Stop, on the computer that holds the
thread:

1. Fences the thread until the Stop ends: an app-written message for it
   is refused (`the thread is being stopped`) and no session is started
   for one, as for an archived thread. A person's message is taken. The
   thread starts no work of its own either: a spawn, send, ask or
   reminder, a new assistant, a loop or watch, a remote command, and a
   workflow run's start, binding, resume, rerun, retry or answer are
   refused (`This thread is being stopped and cannot start new work
   now.`). The Stop first waits for the ones already being admitted, so
   the steps below reach everything the thread started. An archive
   closes the thread to new work the same way, from before its capture
   until its session is stopped. Work an agent's call starts belongs to
   the session the call authenticated as (thread, workflow and remote
   tools, and the `agent-overflow remote` credential): once that session
   has ended, a call that was already past authentication starts nothing,
   however late it reaches admission (`The agent session that made this
   call has ended, so it cannot start new work.`), and a call from the
   session started afterwards is admitted.
2. Under its action and mutation locks, ends its parked calls, disarms
   the wakes of its requests, cancels its loops and watches with the
   outcome `cancelled: the user stopped the thread` (an agent's stop:
   `cancelled: thread "<title>" stopped the thread`) and dismisses the
   remote completions its agent has not taken in, queued or not.
3. Pauses every workflow run that reports to it (bound to it, whoever
   started the run and whatever its source), with Pause and End Sessions ([Interrupt and end session](#interrupt-and-end-session)):
   the run reads paused, its members' sessions end, and it stays bound
   to the thread and resumable from the workflows view. The run's wakes
   to the thread are held from before anything moves it, so the rest the
   pause leaves, or a parked run's own, does not wake the thread,
   whenever it arrives, even when a scheduled resume set the run running
   after the Stop read it parked; a rest after somebody resumes the run
   does. A run already paused or parked keeps its state and reason and
   has its members' sessions ended too; a finished run is left as it is;
   a running run beneath a parked one is paused. A resume scheduled on any run in the tree is
   cancelled first, after one already firing has finished, so neither its
   timer nor the next boot resumes the run; Resume from the workflows
   view still does. A run the thread started that is bound to another
   thread or to none is left as it is.
4. Settles every open request on it, from any thread on any computer,
   cancelled as the user's Stop (`The user stopped it from the Agents
   tray; you did not cancel it.`) or the agent's: its message is taken
   back, a turn it started is cut, and its sender is woken as it asked.
   None is queued again.
5. Under the action lock every send and start takes, cuts its turn with
   the stop row (`Stopped by user`, or `Stopped by thread "<title>"`)
   and ends its session as End Session does, a thread with no session
   included: the provider process and everything it runs (background
   shells, subagents, terminals and provider timers) end, its background
   items read as ended, its open prompts close and its unanswered async
   questions are dismissed, their wakes routed to its spawner withdrawn,
   and the messages the person queued in its composer are dropped with
   the queue.
6. Withdraws what the app queued for it and cancels its asks and the
   commands it started on paired computers, as an archive does. A cancel
   the computer does not take is named in the failures and sent again
   until it is ([remote commands](../architecture/remote-commands.md)).
7. Stops every request it made: a reminder is dropped, a send or ask is
   cancelled with its wake disarmed and only the turn it started cut,
   and a spawn's thread, in any placement and settled or not, is
   brought to rest by this rule, here or through `agent_stop` on its
   computer, to any depth the ledger holds, a cycle ended by the
   threads already reached.
8. Sends a prompt routed to it from a worker it left running to the
   user.

A thread a running workflow run works in, as a phase or a fan-out unit, is
that run's, and a Stop leaves it and the run as they are and names the run
(`it works in workflow run <id>, which it did not start; pause that run from
the workflows view`). The runner makes every member thread, so a spawn
reaches one only through a ledger row nothing writes.

The thread is not archived, moved or regrouped and keeps its history and
its browser pages; a later message from anyone starts a session that
resumes it. A step that fails is named in the Stop's failures (a run it
could not pause: `pause its workflow run <id>: <error>`), and the others
still run. A
repeated Stop finds the thread at rest.

### Sidebar

The sidebar has two sections, Assistants above Projects, in one scroll
area, each under a header that collapses the whole section (persisted
per client in `sidebar:collapsedSections`). A search shows its matches
in a collapsed section. The Assistants header carries New Assistant.

**Assistant row.** Each assistant on every attached computer is one
row in the Assistants section (ruling 4), ordered by the top-level
comparator. The first line is the person glyph in the pin column, the
name, the worker count and badge, and the dot; the second is the state
line; the third names its projects (`All projects` for a chief of
staff), truncating from the right. Both lines start under the name. The
row holds nothing: no chevron, no children, no done fold. It is the
agent variant of `ThreadRow`. Differences from an ordinary thread row:

- The name is the title. When an assistant on another attached computer
  has the same name (compared as the store compares), both rows show
  `<name>-<computer>`, using each computer's display name, until one is
  renamed; the suffix is never written to a title.
- The state line, 10px, one line, truncating from the right:
  `until 10:30 · watching 4 MRs · 2 need you`,
  `loop board check · next 12m · 1 missed`, or nothing when the agent
  is idle with no loop or watch. The text comes from the supervisor
  (next fire, missed count) and from the agent's own registered labels
  (what a watch is called). The dot keeps the live status; the state
  line never repeats it.
- Drag: an assistant row cannot join a group and takes no drop.
- New Thread, Same Assistant: an icon-only hover action on the row
  (`aria-label` "New Thread, Same Assistant"), shown at rest under the
  compact layout, and the same item in its menu
  ([Next thread](#next-thread)). Without `threads:autonomy`, which the
  call needs, the action, the menu item and the header button are
  disabled with the reason.
- Context menu: Open, Rename, New Thread Same Assistant, Projects…,
  Previous Threads…, Loops and Watches (opens the thread with its
  background tray open), Fork, Mark Unread, Copy, Archive and Delete (the
  confirm described under [Standing agent](#standing-agent)). No pin
  item: an assistant row ignores its thread's pin.

**Previous Threads.** Previous Threads… on an assistant's menu opens a
dialog listing the threads its handoffs superseded, newest first, each
with `Superseded <time>`, 20 a page with Show more…, and No previous
threads. when it has none. Picking one opens that thread in the pane the
menu came from and closes the dialog. A page that answers after the
dialog closed or opened again is dropped, and the dialog closes when its
assistant leaves the list.

**Workers on a row.** Any thread row with open workers shows how many in
its count slot, and a warning `CountBadge` with how many are waiting on
the user, each titled with what it counts, at every density. A row
whose thread has no open worker shows neither.

**Placed workers.** An owned thread has no row (ruling 36). A standalone
or grouped thread a local thread spawned into a project is a plain or
grouped row there, and its menu adds Remove from Sidebar, which makes it
owned again; its panes stay open. A placement change unpins the row, as
a group move does. Helpers and past threads have no project, so no row
and no placement item.

New Assistant is on the Assistants header (a person-plus icon,
`aria-label` "New Assistant") and in each project's context menu under
New Thread, desktop and compact. It opens the New Assistant dialog: a
name field and a projects checklist with All projects above it (the
project it was opened from preselected), and Create, enabled once a
name and a project are chosen. A name in use is refused inline with the
assistant that holds it. Create opens the new thread in the pane the
action came from, or a new pane under the same modifier a new thread
takes; from the Assistants header it creates on the computer selected
when the dialog opened. The assistant row's menu adds Projects, which opens the
same checklist for that assistant. A Create or Save that answers after
Cancel still lands (the assistant is listed, its row updated), but
only the opening that started it closes, shows its error or opens the
new thread.

Mechanics, as built:

- The thread row carries the lineage: `agentId` on an assistant's
  current thread, with `agentAllProjects` and `agentProjectIds` (the
  projects it serves, sorted); `pastAgentId` and `agentSupersededAt` on a
  past thread; `helperOfAgentId` on a helper; `spawnedByThreadId` (the
  caller of its spawn link) and `placement`
  ([Owned workers](#owned-workers)). They are derived in
  `threadColumns`, so one `thread:updated` frame moves the row. A
  spawn's accept re-broadcasts the spawned thread; a handoff
  re-broadcasts the old thread, the successor and every live thread
  the predecessors spawned, whose `spawnedByThreadId` it changes. The
  partial index `idx_thread_requests_spawn_target` serves the lookup,
  and while the thread it created exists the thread-request retention
  sweep keeps a spawn row past the floor as `lineage_only`, local or
  remote ("Owned workers"), its answer, late reply and wake cleared, once
  no open work is under it:
  a spawn the Agents segment lists for open work below it, local or
  remote, waits for that work to settle, so the floor never takes rows
  out of a tree. The ledger tools treat that
  token as unknown; the lineage reads, a worker's prompt routing
  (`SpawnOwnerOf`) and the Agents tree still find the row, so work a
  reused worker opens after the sweep lists, counts and stops under
  its owner as before it. On the computer a remote spawn reached, the
  same sweep keeps the spawn's receipt as `lineage_only` while the
  thread it created exists, its answer and late reply cleared: that
  receipt is what lets the spawner's computer list and stop the
  thread's work, so a worker continued there after the floor still
  lists and stops under its owner. Its token is unknown to the ledger
  there as well.
- `SetThreadGroup` refuses an assistant's thread
  (`ErrStandingAgentGrouped`). The drag payload marks only assistant
  rows `agentBound`, so a group target accepts a placed worker.
- `ListStandingAgentWakes` (route all) and `thread:wakes_changed` feed
  `stores/threadWakes.svelte.ts`, one key per thread, so a frame reruns
  only the row of the thread it names; the state line
  (`utils/agentStateLine.ts`) shows the running row, else the soonest
  loop or watch, with `+N more`. A row the bulk read missed asks
  `ListThreadWakes` once on mount. Each computer's bulk read settles
  when that computer answers, however late, with the frames that computer
  sent meanwhile replayed over its answer; a newer read of the computer
  supersedes it. The bulk read and a thread's own reads, the sidebar's
  and a tray's, are ordered by when each started: a thread's loops take
  an answer, and its replayed frames, only from a read no later read has
  written them after, and its reminders, which the bulk read does not
  carry, are ordered the same way among its own reads. A thread's own
  read still out when the bulk answer lands is read again.
- Worker counts: `ListThreadWorkerSummaries` (route all, one page of
  `{summaries, next}` after a thread id) and `GetThreadWorkerSummary`
  (route thread) answer `{threadId, open, needYou}`, counted as the
  Agents list counts its open rows, and feed
  `stores/threadWorkerSummaries.svelte.ts`, one key per thread with open
  workers. A bulk read per computer at boot and on resync reads every
  page; the pages together list only threads with an open worker, so
  once the last has answered, one they leave out has none, and not
  before. Each `thread:agent_rows_changed` frame reads the thread it
  names again, coalesced through one scheduler; a gap reads that
  computer in bulk. A read lands on a thread only when no read that
  started later has landed there, and a computer's completed bulk read
  is that computer's watermark: no answer from a read of it that
  started earlier lands after it, absence included. An answer lands
  only while the computer that gave it is the one attached when its
  read started. A read that fails is read again, through the same
  scheduler, on a backoff that grows from 3 to 30 seconds, until one
  answers or a newer read covers it; meanwhile the row keeps what it
  showed, no badge before the first answer, and carries no error mark.
  The failure goes to the diagnostics log. A read the connection
  dropped is left to the reconnect, whose resync reads the computer
  again. A thread whose count reaches zero, and a detached computer's
  counts, are dropped. A deleted thread's count is dropped with its
  row, and no read of it still out lands or is retried.
- `ListAssistantPastThreads(agentID, after, limit)` (route home, sent to
  the assistant's computer) pages `agent_past_threads` newest superseded
  first by the `{supersededAt, threadId}` cursor and feeds
  `stores/assistantPastThreads.svelte.ts`, which notes each row's
  computer so the pane it opens in routes there.
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
- Bucketing (`sidebarBuckets.ts`): `sidebarListsThread` is the one rule
  for which threads have a row: unarchived and not owned. An
  assistant's current thread goes to the Assistants section while its
  name matches a search; every other listed thread goes to its own
  project, and a thread outside every project has no row.
- The harness seeds lineage with `HarnessSeedThread.spawnedBy`, the
  title of a thread seeded earlier in the same project, or
  `spawnedById`, the id of one seeded before in any project, and an
  assistant with `HarnessSeedThread.assistant`.

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
  error on the row. Cancelling a watch, by any door, stops a run in
  flight with its process group, one whose fire has just claimed the
  row included. Watches are limited to an assistant's current
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
([agent-thread-tools.md](agent-thread-tools.md)). An Interrupt stops the turn and leaves the queue, which is delivered
after it. An End Session or a revert drops the whole queue, and returns
these two kinds the same way once the queue is gone: an answer or reminder to its request, owed and
disarmed, so `thread_status` still returns it; a remote job's
completion to its watch, whose watcher delivers it again and starts the
session for it, as for a completion that finishes after the session ended. Ending the
session does not quiet later wakes. An End Session or an archive that
ends a session holding such an answer or completion, handed to the provider
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
leaves a message the agent holds reading unconfirmed. An End Session or
an archive drops the unconfirmed sends before it closes the provider, which
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
and `remote_jobs` still returns the result. One that an End Session, an
archive, a revert or a restart returned is delivered at the watcher's
next poll. A session teardown and a revert drop the queue under the
thread's mutation lock, which every queue write takes, so a message queued
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

A turn is a tick when every message the provider delivered into it was
a wake, whichever turn their rows show in: a Claude wake the CLI runs
as a turn of its own keeps its row in the turn it was queued during.
Each delivered message names its members, so a person's text Claude
joined to a wake makes the turn the user's. A turn that took no message
in is not a tick. A reminder, a request, an answer and a remote job's
completion are not wakes: a turn they start is the user's. A tick that
ends cleanly is quiet ([Attention](#attention)).

A wake that never reaches the agent is not put in the person's
composer: a loop or watch wake still queued at boot, or left
unconfirmed by a session that ended, is discarded and its row records
`not delivered`. A watch keeps that wake's data as `last_output`
(capped at 64 KiB), which `thread_status` shows, so the agent can
recover the event at its next wake; a loop needs nothing beyond the
row. An outside `thread send` message is handled the same way, with a
`not delivered` row in the thread's timeline; so is one an End Session, an
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
  `cancelled-by-quit` and it runs again now. An interval row is armed due
  at the fire the cut run was for, so the rerun stays owed until a fire
  claims it, through another quit or a pause, and counts no missed fire
  for the run it replaces.
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

### Turning assistants off

The Assistants setting (`assistantsEnabled`, user tier, on by default)
is in Settings under Agents, between Thread tools and Agent profiles,
beside the other settings about what agents get. Off pauses and hides;
it never deletes ([decisions](../decisions.md#sidebar-threads-drafts)).

**One state per computer.** Each computer's setting decides its own
assistants, in every window: they are shown and running, or hidden and
paused. A window attached to a computer with them off and another with
them on shows and runs the second one's and not the first one's. The
Settings page follows the computer picker at the top of Settings, so a
window changes the setting of the computer it picks. A standalone
frontend's own home has no assistants: its switch is disabled and the
page never writes there.

**Hidden.** Every surface decides an assistant by the computer it lives
on: the Assistants section (shown while any attached computer has them
on), its New Assistant (disabled for the selected computer while it has
them off), New Assistant in a project's menu (by the project's
computer), New Thread, Same Assistant, the Projects and Previous Threads
dialogs (one that is open closes, and does not come back when the
setting is turned on), the threads under an assistant in the sidebar,
the thread picker and archived threads, and the memory page's sentence
about assistants (by the computer the page shows). Message search leaves
them out on the computer that searches, by its own store rule; an open
search asks again when any computer changes the setting. A pane already
showing a thread under an assistant stays, readable: an edit or
confirmation open in it closes, keeping the edited text as the draft,
its composer gives way to "Assistants are turned off in Settings. Turn
them on to continue this thread." (naming the computer when the window
shows more than one) and its messages offer no fork, edit or run.
Closing it would lose the layout the person comes back to.

**Paused.** The computer whose setting it is decides what fires, under
the lock that owns each start, so a fire selected before the pause and
claimed after it does not run: a loop or watch fire and Wake now are
refused where the row is claimed, a long-running watch's process where
it is started (a refused restart arms the row again), and a reminder
where it settles. Wake now on a paused loop, watch or reminder is
refused with "Assistants are turned off in Settings on this computer."
and changes nothing. While `store.PauseAssistants` is set, the store's
due reads (the due loops, watches and reminders, the sweep's next moment
and the live watch list) also leave out every thread that lives under an
assistant, its current thread, its past threads and its helpers; that
is scheduling only, not the admission. No session starts for those
threads: an app-written message waits in its queue, and any other start
and any new work is refused with the same message. A worker's question
goes to the user. The pause is set from the setting at boot, before the
sweep's first pass, so a restart keeps it.

**Refused to a person.** A person's send, steer, edit and resend, fork,
fork from a message, side chat, move or copy to another computer,
shell command or workflow run on a thread under a paused assistant is
refused before anything is staged, stopped or cut, under the thread's
action lock. One the computer admitted before the pause finishes. A
message queued in the composer waits until the setting is on.

Turning it off also stops what is running, thread by thread: watch
scripts and long-running watches end and their rows are armed again
with the interrupted fire still due; a request open on the thread
settles as a Stop of it does, saying "Assistants were turned off in
Settings before it answered."; a running turn is cut ("Stopped:
assistants were turned off") and the session ends, with the messages
queued behind it back in the composer. A thread that could not be
stopped completely says so in its pane. While the pause holds, a request
held for such a thread's rest never settles at rest; the pause settles
it. A boot that finds the setting off settles the same way, before any
held request may settle at rest, what a quit left open before the pause
reached it, and leaves the loops and watches as they are.

Turning it on first recovers, while the pause still holds, what a quit
left running: a run with no live process is settled, and a run still
registered as live is left alone. A long-running watch whose restart the
pause refused gives up its registration in the same step, so it is
recovered, not taken for live. Then the pause is released, if the
setting is still on, in the same step as the recovery, and only the runs
it recovered are run again; a recovered interval run turned off before
the release stays due and runs once when assistants are next on: the
long-running watches start, the sweep serves each loop and watch that
came due meanwhile once, saying how many fires it missed since the
previous wake, fires the reminders that came due, and starts the session
of each thread with messages waiting. Toggling quickly ends in the state
the setting ends in, with no run started twice and none left stopped.

**Not offered to agents.** A session's tools leave out
`thread_new_assistant`, `thread_watch`, `home_files` and the `agents`
search filter, and the guide says nothing of assistants. A session that
started before the change keeps its tool list; a call that needs
assistants is refused with the same message. On the computer with them
off, an assistant's name, or the id of a thread under one, is no
address, and `thread_search` lists none of their threads. Each computer
decides for its own assistants: a caller on a paired computer reaches
the assistants of a computer that has them on, whatever the caller's
computer has, and is told which computer has them off when it does.

**Notifications.** A moment of a thread under a paused assistant is a
hidden thread's, raised only where `notifyHiddenThreads` is on.

**Kept.** Every assistant, thread, past thread, helper, home, memory
folder, wake registration, draft and unread state is untouched. An
assistant's home memory is neither read nor written while it is
paused. Project and user memory ([memory](memory.md)) are not affected:
threads that read them with shared memory on still do, and nobody
receives the maintenance warnings, which only assistants get.

### Attention

A thread's attention flag is three columns of `threads`:
`attention_kind` (`decision`, `review` or `problem`), `attention_reason`
and `attention_at`. `thread_attention` writes them
(`SetThreadAttention`), replacing any flag the thread had, and moves the
thread's activity time to the same moment, so the row moves up as it
does for new activity; kind `none` clears them (`ClearThreadAttention`).
The flag is pending while `attention_at` is past the read marker, or
while the thread has no read marker. Reading the thread clears it
because `attention_at` is one of the read targets
(`threadReadTargetColumns`); the columns stay as the thread's last flag,
so Mark Unread shows it again.

A pending flag reads as unread: the title is bold, the status dot and
ring take the kind's colour (decision info, review accent, problem
warning) with the label `<phrase>: <reason>`, where the phrase is the
one its notification says, and a line under the title gives the kind's
icon and name and the reason, cut to the row's width with the whole
reason in its tooltip (`ThreadAttentionLine.svelte`; the kinds are
`ATTENTION_KINDS` in `threadStatusPill.ts`). A running, approval or
question state outranks it, as each outranks Completed. An unfocused
pane's header says the phrase in the kind's colour, except that a review
takes the success colour there, since accent marks the focused pane. The
row sorts with the unread rows.

A turn is quiet when it is a tick ([Wake supervisor](#wake-supervisor))
that ended cleanly: no error, not cut short, not interrupted. Triage
gives a quiet turn no activity bump, and its wake rows none when they
are written. It moves the read marker to the turn's completion when the
user had read everything before it: every other completed turn, the
thread's newest turn error and the flag (`KeepThreadReadPast`). A
thread marked unread, or never read, keeps its marker. The row is sent
before `provider:turn_completed`, which carries `quiet`, so a client
never shows the turn as Completed; every round of the turn is quiet,
unless an earlier round of it failed, which leaves the whole turn the
user's to read. A turn the Claude CLI starts on its own after a tick (a
background task finishing, a scheduled wakeup) is a turn of its own
([Rounds and turns](../architecture/turn-lifecycle.md#rounds-and-turns))
and is never quiet: nothing says a wake started the work it reports,
even when wakes were waiting as it started and a later turn takes them
in. A tick's start withdraws no notification.

A top-level turn that ends with the flag pending, a tick or not,
notifies as `thread-attention` in place of Completed, with the kind's
phrase as its body (`Needs your decision`, `Ready for your review`,
`Hit a problem that needs you`); the reason is the agent's own text and
never reaches a notification. A failed turn still notifies as an error.
The notification has the thread's rest identifier, so a flag and a
completion are one moment, said once, and lowering a pending flag
withdraws it. A quiet turn with no flag notifies nothing. Its toggle is
`notifyThreadAttention`, on by default, and it plays the input-needed
cue.

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
  sidebar's assistant rows. `GetThreadWakeDetail` reads an expanded
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
Background: the `users-round` icon, `Agents`, a count of the workers its
open rows go to, each once, and incoming messages, and its own `Stop All`. Every thread with rows, open
or past, shows it, a standing agent's pane included, and so does a
thread whose read of either failed. With nothing open the toggle shows
no count, as Background, which hides at zero, never shows one. It
renders at every density of the
rail; the densest rung (`tight`) drops the segments' running pulses and
closes up their spacing so the row holds at a phone width.

- **Rows.** One row per request the thread made through the thread tools
  and is still owed: every spawn, and a send or ask with notify armed or a
  call parked on it. Reminders are wakes, not rows. A row shows the
  target's provider (a paired computer's from its acknowledgement), the
  kind, its title, its computer when it is not this
  one (an ask on this computer shows the thread asked, its title and
  provider, while its copy runs and after the copy is deleted), its state (queued, running, blocked, replied, finished, failed,
  stopped, interrupted, expired, refused, a late reply marked, or a
  settled spawn's waiting worker), how long
  since the request, and what a blocked row waits on: the approval with
  its tool and detail, or the question, and who it was routed to; a
  paired computer's target reports the same in its `agent_rows` answer,
  and its request's poll only that it waits and on which thread. A
  prompt that computer routed back to the spawner here names the spawner
  as this computer does (`blockedRoutedHere`) and does not count as
  needing the person. A
  spawn's row says what its thread did as a subagent's row says what its
  agent did, from the one formatter both use (`AgentMetrics.svelte`),
  read with the thread's name (`store.ThreadTally`, one probe per
  thread):
  - Tool calls: each top-level tool call the thread executed, once. A
    fork's copy of its source's history, an import and a restore count
    none of theirs again.
  - Tokens: what the thread spent itself, counted as a subagent's are
    (fresh input, cache writes and output; a cached prompt read again is
    no spend), the running turn's included: the same meaning as a native
    agent row's tokens and the same scope as its tool calls. Its native
    agents are rows of their own with their own counts, and the threads
    it spawned count their own. The figure is the one the provider
    reports as the thread's own with each settled turn
    (`WireTurnCompleteMeta.OwnUsage`): Claude's flat `result.usage`,
    which leaves its subagents out, not its subagent-inclusive
    `modelUsage`; a Codex turn's usage, which never carries its
    children's. The billing ledger is unchanged and keeps the inclusive
    figure.
  - Runtime: the time its turns ran, each ended turn's start to end plus
    the open turn's time so far (`RuntimeMs`, `TurnOpen`); the time
    between turns is not runtime, nor is the time the app was down: a
    turn a crash left open ends at the last activity it recorded. The row
    adds the open turn's time from its own clock since the read and,
    within that open turn, never shows less than it showed. The read
    names the open turn by its start on the computer that holds the
    thread (`TurnStartedAt`), compared only with a later read's, so a read
    of the next turn, or one after the turn has ended, shows the server's
    figure as it is. A Past row's settled age is a separate fact, its
    runtime the tooltip.
  The counts are live while it works and kept, final, once it rests and
  under Past. A copy of the thread to another computer starts at zero; a
  move takes the counts with it, and a thread that moves back brings the
  work it did there. Its row is framed again, at most every 2 s, for each new
  tool call, one whose line reads as the last one did included, and for
  its usage's progress. A running target's
  latest top-level tool call is its activity, beside the counts on a
  second line; its messages are not activity, its last one is in the
  digest. A waiting worker's line is what it waits on instead, and a
  blocked one's what blocks it. A paired computer reports its spawn's
  counts and line with its `agent_rows` answer
  (`ThreadAgentRows.ToolCalls`, `Tokens`, `RuntimeMs`, `TurnOpen`,
  `TurnStartedAt`, `Activity`), refreshed as that answer is, its open row and its Past
  row projected alike; a Past row reads the last answer and asks again,
  a page's worth at most, and one whose computer did not answer or is
  no longer paired says why (`NestedUnavailable`). One that predates
  them shows the row without them. A request settled `finished` by a
  paired computer that cannot report rest says so on its row
  (`restUnreported`). The
  chevron opens the row's digest: the activity, what it is blocked on in full, the last
  message in the thread (a request without the footer the app appended)
  and the answer once it settled. State comes from the thread tools' own
  derivation (`requestStateFrom`, which `requestStateView` runs for
  `thread_status`); reading never delivers an answer.
- **Nesting.** A spawned thread's own rows nest under its row, as provider
  subagents nest under theirs, four levels deep; a row at the bound whose
  thread has agents of its own says so. A settled spawn stays listed while
  its worker or anything nested under it at any depth is open, here or, by the last
  `agent_rows` answer, on a paired computer. The listing holds at most 128
  rows and says when it left some out.
- **Lifetime.** An open row stays: its request is open, or its spawn's
  worker is not at rest ([Owned workers](#owned-workers)). Any other row
  stays for 8 s
  (`AGENT_ROW_SETTLED_LINGER_MS`) from when the pane first saw it settled,
  then leaves the open rows; it is in Past from when it settled, and
  while Past is expanded and lists it, it is listed there alone, unless
  a row open under it keeps it above. The backend keeps listing settled rows for 30 s so a pane
  that reconnects still sees them. The linger is longer than Background's
  because a settled row is a click target: clicking it takes it out early
  and scrolls to its wake item when the answer landed in this thread, or
  opens the thread it went to otherwise, in the companion pane below; a
  settled ask whose answer did not land here opens its digest with the
  answer instead and stays. Clicking an open row opens its target in that
  pane. A pane remembers which settled rows it saw and which
  were clicked for the last 64 threads it showed, so a row that left does
  not come back to the open rows when the pane returns.
- **Row actions.** Every row, open or past, has Open in Agent Pane
  (`panel-right-open`), which opens the thread it went to in the
  companion pane below, settled or archived alike. An ask's worker is its
  hidden copy: while the ask runs its row, click and icon alike, opens
  the copy (`ThreadAgentRow.openThreadId`), whose composer takes no
  message and shows no pickers or attach, whose workspace strip and
  compact header show no machine, branch or worktree picker (they would
  move the copy or check out the workspace it shares), whose messages offer no edit
  or fork, and which offers no Keep, only Stop (`Thread.askCopy`; the
  backend refuses a person's message and Keep there too). The copy is deleted
  once it answers, its pane with it; then the icon opens the thread
  asked (`askedThreadId`), labeled as the thread asked, never as the
  worker. A click that lands after the delete and before the row's next
  read opens nothing and says the copy could not be opened. A spawn row
  whose thread is owned, on the listing computer and not archived has
  Show in Sidebar (`panel-left-open`); an assistant's helper, outside
  every project, takes no placement and has none. It is `SetThreadPlacement` standalone on
  that computer, after which the row's placement is standalone and the
  button is gone; a refusal is an error toast. Both are icon buttons with
  a title and a matching label, as Background's Open in Agent Pane. Stop
  and Stop All stay text buttons.
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
  the user, the spawner's answer to a worker's async questions puts them
  back to unanswered and the user's, as a hand-back does, a wake's row
  records it as not delivered, and a workflow run's wake is dropped. A completion is dismissed on its watch with the reason
  instead of going back to it, which would queue it again on the next
  check; `remote_jobs` and `remote_status` still return the result.
- **Stop** (`StopThreadAgentRow`) cancels the request as `thread_cancel`
  does, with the caller's notify left armed: its agent is woken with the
  request cancelled by the user, not by itself. An agent's own
  `thread_cancel` still disarms it. A thread the caller spawned is
  brought to rest ([At rest](#at-rest)), down the lineage. A request
  inside the lineage is cancelled with its wake disarmed: the thread
  that made it was stopped too, and a wake would start it again.
  A Stop of a row on this computer works from the ledger, without reading
  the listing. A send to a thread the caller does not own cancels only the
  request. A settled row reports it had already finished, unless it is a
  spawn whose worker, here or on a paired computer, was not at rest or
  whose rest stopped anything of it or below it (a turn, background work,
  an agent of its own): that Stop reports the worker stopped, and the
  spawner is told, by an app-written message attributed to the worker
  (send id `worker-stopped:<token>:<n>`), since no settlement tells it:
  once for each time a Stop stopped the worker, so a worker continued and
  stopped again is told of again, and Stops racing on one stopping of it
  make one message. The message says the user stopped it, not the
  spawner, and that it is at rest, or names each part the Stop reported it
  could not stop, and that a later message starts it again. The settled
  request and its answer stay as they were; a Stop that finds the worker
  and everything below it at rest, a cascade and an agent's own
  `thread_cancel` tell nobody. Each layer's result, what it stopped and
  what failed, is folded into one toast, and a failure in one layer
  leaves the rest to run. `Stop All`
  (`StopThreadAgentRows`) does the same for every row the ledger held
  when it was pressed, past the listing's 128 rows too, less a send or
  ask already settled, and withdraws the incoming messages listed then;
  a rest reads every request a spawned thread made, every spawn of it
  settled or not, once its session has ended. A request made after that
  read is not stopped. The Stops of rows
  here start at once, beside those that wait on a paired computer.
  Background's `Stop All` and Stop background work leave Agents rows
  alone. The thread's own Interrupt leaves its requests and its queue,
  the incoming rows with it; End Session clears the incoming rows with
  the rest of the queue, given back as a queue drop does.
- **Other computers.** A spawn on a paired computer lists and stops as a
  local one: the forwarded `agent_rows` and `agent_stop` calls
  ([reaching another computer](agent-thread-tools.md#reaching-another-computer)) nest that
  thread's rows under the spawn, named by their computer, and bring it
  to rest there. A read never waits on another computer: it nests the
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
  until it lists none or refuses the thread as not the spawn's, which it
  does once that thread is deleted there and never takes back. A computer
  that does not answer is said on the row
  (`<computer> did not answer; its agents are not shown`). A computer
  that predates the calls has the request cancelled and the turn
  interrupted, and the row and the Stop's toast say its own work was not
  shown or stopped. One that answers `agent_stop` but predates the rest
  stops the thread's Backgrounded items, its turn and its own rows, and
  leaves its session up.
- **Live updates.** The backend sends `thread:agent_rows_changed` after
  every change to a request, to the target a running request works in,
  to a wait parked on one, and to a worker not at rest, through its spawn
  link, coalesced off the emit path: one statement
  names the callers of a batch. The frame names the caller and the
  request and carries no row, and is sent again, with the same request, to
  each thread up to three spawns above the caller, whose listing nests
  the caller's rows. `ListThreadAgentRows` is read when the
  thread opens, after a reconnect or a lost frame, and on a frame naming
  the thread or a thread its list nests that is not about a local row the
  pane lists: a new row, a frame about every request of a caller, a row a
  paired computer listed or a spawn on one. Frames about local rows the
  pane lists read those rows alone, up to 32 per read
  (`GetThreadAgentRows`), and the pane keeps each row's place, depth and
  nesting marks; past 32, or when the read says a row is no longer
  listed, the pane reads the list. Reads go through the rate-bounded
  refresh scheduler, so a burst of frames is one read. A frame that
  lands during a list read, about the thread or a thread that read's
  answer nests, reads the list again after it. A failed list read keeps
  the last list and is said at the top of the body, with the app's row
  error and a Retry icon button, until a read succeeds; the segment shows
  while it stands, so a failed first read is not taken for no agents.
- **Reads.** A listing is a constant number of statements whatever the
  tree's size: the tree's shape with each row's depth in one recursive
  read, the listed rows in one, the targets' titles and providers in one,
  their last messages in one. Each thread's rows are read by caller in
  five arms, open, settled inside the window, local spawns kept for
  stored open work, local spawns kept only for a parked call or a worker
  not at rest at or below their targets,
  and remote spawns with open work, each through its own index and
  each taking at most the row limit, and the recursion stops at the limit,
  so the cost follows the listed rows and the depth, not the threads'
  settled history, their open spawns or the open work of threads outside
  the tree, whatever statistics the database holds. A local spawn is kept
  for open work by counts it carries of the open work up to four spawns
  below its target, which the ledger keeps in the transaction of every
  write to a request; for work deeper than that, each read climbs from
  the spawns whose counts hold work three spawns down
  (`idx_thread_requests_open_deep`, which holds only those) to the root. Open work is what a thread's own listing shows
  open: a spawn, and a send or ask with its wake armed or a call parked
  on it. A parked call lives in memory and arms its wake only when its
  wait ends, so the counts hold the armed part and each read climbs the
  spawns above the parked requests it is given, a spawn a step: a worker's
  quiet wait lists, and counts, under its owner as under itself. The
  workers not at rest are memory too, and each read climbs from their
  spawn links the same way. Each climb goes to the root, and a link
  enters it once however many seeds lie below it: a read pays one search
  per parked call, worker not at rest and deep spawn, and one per
  distinct spawn above them. A thread is reached by at most one local spawn, so
  a read of named rows climbs from each row to the listed thread, one
  spawn a step, and gives each row the listing's place, wherever it falls
  against the row limit; the rows and their targets are read for those
  rows alone. A row
  carries the head of its answer (what the row shows), never the whole
  answer or the late reply, and names its wake item from the request
  (`thread_requests.wake_item_id`, `late_wake_item_id`, recorded when the
  echo confirms the wake), not by a send-id lookup. A tool row's state
  read (`GetThreadRequestStates`) is one statement for all its tokens.
  A spawn row carries its thread's `placement`, empty for a thread that
  takes none (outside every project, or a standing agent's own), and
  whether it is `archived`, as the computer that holds the thread
  reported it: this computer's store, or a paired computer's
  `agent_rows` answer, for the rows nested under it too. Every other row
  carries neither, nor does a spawn on a computer that does not report
  them.
- **Past.** Below the open and incoming rows, one Past row with the
  count of the thread's settled rows, less spawns whose workers are not
  at rest; expanding it lists them newest
  first, 8 and then 20 more per "Show N more…", each with the open
  row's layout and digest, no Stop, and when it settled in place of the
  elapsed time. Collapsed, the pane holds only the count, read as a page
  of one row when the thread opens; it reads again after a reconnect or
  a lost frame, on a frame naming the thread and no row the list holds,
  when a row of the thread's own the list shows settles or opens again,
  so a row that settles or whose worker rests is counted and first in
  Past without a reload, and one whose worker is continued leaves it, and
  on a frame about a settled row the list holds that the open list's next
  read does not supply. A row of the thread's own that an open-list read
  supplies is taken into Past from that read when settled (a late reply
  while it lingers), and out of it, with the count, when open again, so
  no row shows in both lists. Reads and Show in Sidebar are ordered by
  when each started: a page read never shows a row older than an
  open-list row or a placement that arrived while it was out, the first
  page included, nor a row an open-list read found open while it was
  out, and an open-list read never undoes a placement made while it was
  out.
  Expanded, those cues read every row shown again, from the newest
  through cursor pages down to the oldest row shown, so rows that
  settled since go first and none shown is dropped. The rows are held
  only while Past is expanded and dropped when it collapses, the
  segment's body closes or the pane shows another thread; each expansion
  is its own lifetime, and a read or Show more that answers after it
  ended lands nothing. Its reads go through their own refresh scheduler,
  so a newer read supersedes an older one and a cue during a read reads
  again after it; a failed read, collapsed or expanded, is said below
  Past with a Retry until one succeeds, and the segment shows while it
  stands.
  `ListThreadPastAgentRows` pages the rows,
  flat and newest settled first, ties by token: every spawn and ask it
  made and its sends with notify armed, with the total. Each row is
  derived as an open one is. A page is at most 50 rows, read from a
  cursor (settled time and token) through
  `idx_thread_requests_caller_settled` in index order, never offset or
  sorted; the total counts the caller's settled rows, which the
  retention floor bounds, less the spawns whose threads are not at rest,
  by the listing's own answer. A spawn past the floor is listed as its lineage
  row, with no answer.
- **Worker summaries.** `GetThreadWorkerSummary` counts the workers the
  open rows of the thread's listing go to, nested ones included, read as
  a pane reads it, each thread once however many of its requests are
  open, and those whose blocked fields say they wait on the user: the
  row's own open and needs-you rule (`ThreadAgentRow.open`, `needsYou`). The
  count reads each row's state and what it is blocked on, and none of
  the text a row shows: no answer head, target names or last messages.
  `ListThreadWorkerSummaries` summarizes one page of 64 of the threads
  whose listing may hold an open row, in id order after a cursor, found
  from the open work only: callers of open requests the listing shows,
  local spawns whose counts hold open work below them or that sit above
  a parked call or a worker not at rest, and remote spawns that last
  reported open work. Its
  `next` is the page's last thread, empty on the last page. A frame
  naming a thread is the cue to read its summary again.

#### Agent thread companion

A row's thread opens in a companion pane of kind `agent-thread` beside
the pane whose Agents segment it came from, the way the subagent pane
opens beside its thread ([agent visibility](agent-visibility.md)).
Clicking another row swaps the thread in the same pane; a source pane
has at most one. A companion that fails to open its first thread closes
with a toast. The pane is an ordinary thread view of that thread: its
own timeline, its own composer and its own Stop, which interrupts only
that thread, while the row's Stop brings a spawned thread to rest. Its
header shows the thread's title, or the assistant's name with the clash suffix for an
assistant's thread, and an Open as Pane action that turns it into a
normal pane in the same slot, which keeps it. It lasts the session: it
is stashed and reopened with its source thread like a side chat
(`companionStash.ts`), not restored after a restart, and closing it
never deletes or archives the thread. A row whose thread is already
shown in another pane reveals that pane instead, since a thread is in
one pane at a time. A thread the client must read first opens beside
the pane as it was when the row was clicked: if that pane closed or
moved to another thread during the read, nothing opens and a toast says
so. An agent message's sender chip opens in the focused pane under the
same rule. Under the compact layout the pane is a screen of the
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
also ends the session of every member of the tree that has not ended,
running or already parked, a running descendant of a parked root
included, as the engine names them once the pause is made, and Resume
starts them again on the same provider threads. A Stop that brings a thread to rest
makes the same Pause and End Sessions of the runs that report to it. The Agents row's Stop on a spawned
thread brings it to rest ([At rest](#at-rest)), which ends its session.

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
| 2 | Agent row | 1, 3a | One assistant row with its state line, no owned worker row, Remove from Sidebar, Previous Threads and the worker count and badge behave as specified (ruling 2026-11-14 replaced the fold and done footer) | `owned-workers-sidebar.spec.ts` and its compact twin, `owned-workers-need-you.spec.ts`; screenshots of the sections with counts, the row menu and Previous Threads |
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
| 10 | Agents segment | 3b, 9 | The Agents segment lists a thread's spawns and owed sends and asks with title, provider, state, elapsed and what blocks them, nested by spawn to four levels and across paired computers; settled rows linger 8 s and leave for Past, which counts them and pages them 8 and then 20 at a time, a click opens the target or the answer, Open in Agent Pane opens any row's thread beside and Show in Sidebar gives an owned worker its row; Stop cancels the request for the user with the caller's wake armed and brings a spawned thread to rest, here or on a paired computer, and Stop All does the same; incoming agent messages list until delivered and withdraw; Background's Stop All and the thread's own Stop leave the rows; the segment fits every rail density | `agents-tray.spec.ts`, `agents-tray-past.spec.ts` and `compact-agents-tray.spec.ts` on both mock providers (Past, its paging and an ask in it, Open in Agent Pane on a past and an archived worker, Show in Sidebar, a worker's prompt routed to an ordinary spawner, rows, nesting, a blocked approval, settle and linger, click, per-row Stop cascade with the caller's user-cancelled wake and the nested wakes disarmed, Stop All, Background's Stop All and the thread's own Stop leaving the rows); the cascade across computers in `thread-tools-paired.spec.ts`; screenshots of running, blocked, nested and settled rows and the digest, desktop and compact |
| 14 | Assistant identity and creation | 9, 10, 13 | Standing agents are records (`agents`, `agent_past_threads`, v154); promotion and demotion are gone from the app, the bindings and the docs; New Assistant (desktop and compact, sidebar and project menu) creates the record, the home and a thread whose app-written opening starts the role discussion; the name is the title, unique per computer, a clash across paired computers shows `<name>-<computer>`; New Thread, Same Assistant moves loops, watches, reminders, requests and workers to the successor and opens it with the handoff block; app-written messages to a past thread reach the current one; `thread_search` `agents` and name addressing; `thread_new_assistant` for assistants only; Agents rows open in the `agent-thread` companion pane; Interrupt and End Session are separate for assistant threads and workflow pauses | `assistant-creation.spec.ts`, `assistant-handoff.spec.ts`, `assistant-companion.spec.ts` and their compact twins on both mock providers; the name clash and name addressing in `thread-tools-paired.spec.ts`; `thread_new_assistant` in `thread-tools.spec.ts`; regression on the standing-agent, agents-tray, thread-tools and upgrade-corpus specs; screenshots of the dialog, the opening discussion, the handoff, the companion pane and the Stop menu, desktop and compact |
| 15 | Memory and assistants above projects | 9, 14 | No built-in skills or Skills page; memory scopes, facts, the built index, a whole delivery per start and compaction with the write rules, what changed at each prompt, nothing cut past the size limit, and maintenance warnings for assistants only ([memory](memory.md)); shared memory and the provider memory settings on Claude, claude-tui and Codex; assistants with projects and no project of their own, running in their home, helpers under the assistant and project work under its row in the project, delete and project removal as ruled (rulings 37 to 40); the Assistants and Projects sidebar sections | Unit tests named in [memory](memory.md#verification); `standing-agent-home-contents.spec.ts`, the standing-agent sidebar and spawn-placement specs and the assistant specs on both mock providers, desktop and compact; screenshots of the Assistants section, an assistant's row in a project and the Projects checklist |

Slices 1 and 3a start together, with the slice 4 spike beside them.
Slice 7 is independent of 6 and runs after 5 so the tool lands on the
settled capability plumbing.

## Open questions

- A full transcript-tail backfill for E7 completions, beyond the badged
  row slice 4 ships.
