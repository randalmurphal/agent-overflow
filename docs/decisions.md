# Product decisions

Product choices that code alone does not establish belong here or in their
owning spec. Mechanisms belong in architecture docs or code contracts, with
area guides routing readers to them. Keep only current decisions and the
reason needed to apply them; do not add work logs or verification history.

Where a spec already owns a decision, link to it instead of copying the rule.
A new user instruction can supersede a recorded decision. Existing behavior
alone does not establish intent.

## Working style (owner rulings that shape every change)

- Clear improvement with no negatives: proceed without asking. Any
  user-visible trade-off or product call: hold and ask.
- Restart of a provider session is a last resort. Never route a config
  change to the restart path when a live retry exists
  (`internal/provider/claude/AGENTS.md`).
- Before changing behavior whose purpose is unclear, inspect the current
  implementation, tests and relevant product decision. Use history to find
  intent, then verify whether it still applies.
- Codex second-opinion reviews are for large changes only (feature waves,
  subsystem reworks, wide refactors); routine fix waves get Claude-owned
  review.
- Never publish or offer a Claude Artifact (a hosted claude.ai page). A
  page for the owner to view is a local `.html` file.

## Performance and memory

Do not trade rendering performance for lower memory usage. Preserve the visible
work of mounted panes. Do not
condition optimizations on a pane being hidden or off-screen; returning to a
pane must remain immediate. Forced garbage collection is a diagnostic, not an
active-memory optimization. Do not enable `NetworkServiceInProcess2` as a memory
workaround because it changes the sandbox boundary.

Measurement methods and interpretation belong in
[the performance reference](../.claude/skills/perf-investigation/REFERENCE.md).

Chromium's vblank frame pacing stays the default on Windows. A machine
driving monitors at different refresh rates can present every slow glide
with a periodic hitch that no animation-frame trace shows; Settings >
Performance > Disable GPU vsync opts that machine into timer pacing
(`--disable-gpu-vsync` on the app window and the browser pane, applied on
the next launch). It is an opt-in, not a default or an automatic detection,
because vblank pacing is the better clock wherever it works. The scroll
spring is not the lever for this symptom: a CSS animation with no JavaScript
hitches the same way.

## Background maintenance

Nothing the app does to maintain its own storage may be noticeable. No visible
wait at boot, at quit or in use; no write stall beyond about 100 ms; no read
stall at all. Maintenance work is paced into chunks that fit that budget and
yields between them, and it is deferred until the app is settled rather than
run on a fixed timer after launch. Reclaiming disk space has no deadline:
freed pages are reused by later writes, so a pass that stops early or never
qualifies costs nothing.

`VACUUM` is not run by the application. Mechanisms and measurements are in
[the SQLite store document](architecture/sqlite-store.md#free-space).

One-time data fixes are migrations in the chain, run once and version-gated
(ruling 2026-09-23). Work too long for a synchronous migration gets a deferred
paced phase after open, and only then. No sweep, timer or standing job may
exist to fix a one-time state; a sweep only does work that recurs by design
(retention, reclaim). A phase never abandons an item: a run skips a failing
item so it can finish, but the phase is not recorded as done, the failure
count and first error persist beside the watermark, the user gets a notice,
and every later start retries, with no attempt cap (ruling 2026-09-23).
Mechanism in
[the SQLite store document](architecture/sqlite-store.md#deferred-phases).

## Startup

- A boot never shows an empty catalog. Until a computer's threads and
  projects have answered, the sidebar shows a loading row for it, naming its
  boot phase while it is starting, or its error with Retry. The empty-list
  message appears only once both have loaded; rows already held stay visible.
- Readiness reports progress. A starting backend names its phase, step and
  elapsed time, and a boot right after an in-app update reads as finishing
  that update. The launcher's loading page, the startup screen and the
  sidebar show the same sentence. The update record is the only source of
  that version, so a boot the record does not name shows an ordinary start.
- An in-app update migrates the database in a trial over a snapshot on every
  platform: macOS, the Linux desktop, Windows and a supervised serve host.
  The new version is kept only once it has booted fully. A failure, 30 s
  without observed progress, or the 30 minute ceiling restores the snapshot,
  keeps the previous version and reports the reason. No boot migrates an
  existing database live: one that would, outside an update, runs the same
  snapshot and trial first. An update to a version that predates the trial
  keeps the framework's swap. Mechanism in
  [the update spec](specs/app-update.md).
- A migration or update trial that failed does not run again on its own.
  The next launch of the same build over the same database schema version
  shows the stored reason and phase with Retry; a different build or schema
  version runs normally, and a trial that succeeds forgets the failure
  (ruling 2026-09-24). Mechanism in
  [the update spec](specs/app-update.md#failure-memory).
- The Windows launcher fails a boot only when it makes no observed progress
  for 30 s, and names the stalled phase. A heartbeat alone is not progress;
  a new step, a database or WAL size change, or the process doing CPU or
  storage work is. A slow boot that keeps working is never torn down; one
  blocked on a lock is.
- Heavy post-boot scans, such as the search index build, wait for the first
  client's `ListThreads` and `ListProjects` answers, or 15 s after the backend
  starts answering when no page reads.

## Streaming and reveal

- Nothing skips, rushes, or pops the readable reveal drain. A backlog-skip
  was built and rejected outright; the queue self-corrects through wire
  gaps (`PerItemSmoother` header pins it). Ceiling for a reasoning ticker
  under an overspeed wire: about 2x.
- Static `will-change` on the controller-owned content elements. Never
  reintroduce conditional layer promotion (a promotion transition on a
  mounted element is a raster flash; `docs/architecture/frontend-scroll.md`).
- A resume or stall discontinuity with more than a viewport of backlog
  snaps fully; "jump to one viewport short, glide the rest" was rejected.
- Nothing force-snaps hidden panes in the background; visible-again must
  simply already be right.
- The loaded timeline window is a bounded range around the reader, not a
  tail. A window cut never drops a row the viewport shows, at any count;
  anything outside the window is one page away and scrolling toward it
  always loads (an upward gesture pages older even at the very top and in
  a window too short for the scroll geometry to express direction).
- Text Claude discards after a stalled response (`abandoned_blocks`,
  [claude-wire.md §abandoned_blocks](references/claude-wire.md#abandoned_blocks-a-response-cut-short-21300)) stays in
  the timeline as it streamed, muted, with a note after it; the reply
  continues below. Nothing disappears and nothing re-streams in its place
  (ruling 2026-10-16). The rows are not rewritten: the note is its own row
  naming them, and a read of a row it names serves the row marked
  (`Item.discardedBy`) whether or not the note is in the loaded window.
  The note joins the activity run of discarded reasoning or tool rows like
  any notification, and stands alone after discarded prose (ruling
  2026-10-21).

## Sidebar, threads, drafts

- Changing a draft's project in the thread carries its composer content,
  attachments, captured terminal text and selected settings. The destination
  starts with that project's ordinary workspace defaults. The emptied source
  follows normal draft cleanup, including retaining an existing worktree draft.
  Sidebar navigation and New Thread remain separate actions. Moving to another
  computer clears the source-plan acceptance link; the prompt remains intact
  and the original plan stays unaccepted.

- The sidebar lists a never-sent thread only while it is real: it has draft
  content, a bound worktree or an item (an app notice or error counts), or
  is a terminal or a fork being prepared (owner ruling 2026-11-15). A thread
  with a spawn relation is real too: one a spawn made, here or from another
  computer, and one that spawned a thread. A screen
  opened later and a screen already open list the same threads. A staged
  workspace choice is that screen's own state and lists nothing until the
  worktree is bound. The screen typing into a new thread lists it as soon as
  it holds text. A never-sent thread that loses what made it real leaves
  every sidebar and stays in the panes showing it, and empty-draft cleanup
  never deletes a thread the sidebar lists. The rule is `threadRealClause`
  in `internal/store/threads.go`.

- Sidebar status tiers run needs-attention, running, completed (unread),
  then read (owner ruling 2026-11-16). Completed sits below running so
  that opening a completed row, which reads it, never moves it past a
  running row. A pin orders rows and is never a status: a pinned parent
  shows and sorts by its busiest child. An expanded group cuts its members
  with the project's preview rules. The order is in
  `frontend/src/lib/utils/sidebarTree.ts`.

- Multi-computer ownership and UI follow [Connected computers](specs/connected-computers.md): frontend-local preferences, selectable host configuration, portable conversations, optional peer tools, and machine names in the existing metadata row. No additional sidebar attention feed or artifact dashboard.

- Message nav rail: one position claim at all times (one current tick, the
  dot only when no user message is visible, and the dot does not track the
  fisheye); ticks never compress, overflow is a clipped sliding
  window, arrows exist only while their end tick is clipped out (a
  position-based alternative was reviewed and rejected); the bottom arrow
  jumps to the latest message, not to bottom; thread-edge overrides force
  the edge tick only at the thread's edge.
- `ThreadTitleContextItems` takes the literal first user-role row,
  `wire_only` included; do not add a reader-authored filter there.
- A thread deliberately renamed "New Thread" re-heals its title; there is
  no "user named this" bit, and that is intended.
- A thread's workspace follows a move the PROVIDER makes mid-session
  (Claude's `EnterWorktree` / `ExitWorktree`): the row updates from the
  tool's structured result, the live session is not restarted, and the
  transcript follows the CLI's own relocation (settled at the next
  session start only when row and file disagree).
  A directory the row cannot represent (outside the project's worktrees)
  is refused with an error on the thread, not recorded.
  See `docs/references/claude-wire.md` §E10.
- A removed worktree's threads move to the project root (Base) and their
  sessions stop; nothing restarts them, and the next send starts them
  there. Neither CLI exits when its cwd is deleted, and a restart would run
  work nobody sent. This holds for every removal: in-app, a workflow's,
  Claude's `ExitWorktree remove` in another thread, and one made OUTSIDE
  the app (a terminal's `git worktree remove`, `rm -rf`, another tool),
  detected by a watch on git's worktree registry (`internal/worktreewatch`,
  `internal/app/app_worktree_watch.go`) that runs for every project whether
  or not a pane is open. The thread running `ExitWorktree` keeps its
  session from the tool call on, since it moves itself. The in-app removal
  refuses busy threads; a workflow's cleanup keeps its own force rules. A
  removal that did not wait for idle threads stops a running turn as
  interrupted, its background tasks show as died like an app restart, and
  queued messages return to the composer. A removal the app did not
  perform (Claude's, or one made outside) puts a warning notice on each
  moved thread, at its own timeline position, saying who removed the
  worktree and that it now runs in Base; so does the merged-PR cleanup
  below, with a neutral notice. A removal the user asked for says nothing.
  Notices name the worktree by its directory and the project root as Base,
  never by path. Terminals opened in the worktree
  close, and one `worktree:removed` event moves draft placeholders on it to
  Base on every client. A project whose root is itself gone is left alone;
  there is nothing to reattach to.
- A worktree git still registers whose directory is gone stays in the
  workspace picker, marked missing. The app never prunes it: a directory
  that is only unreachable (an unmounted drive) would lose its
  registration. No thread can enter it, and its Remove deletes git's
  registration alone, without the uncommitted-changes check a present
  directory gets.
- When a thread's pull request merges, its linked worktree is removed and
  its threads move to Base (user ruling 2026-10-13). The app tracks the PR
  a thread's worktree branch has OPEN when a lookup finds it (git status,
  the app's own create, an agent's `gh pr create` / `glab mr create`), and
  never adopts a merged or closed PR found by branch name. Only a PR whose
  head is the origin repository itself counts, here and in the header
  badge: a fork's PR from a branch of the same name is someone else's. A
  merge is the forge's merged state; a PR closed without merging changes
  nothing until a lookup sees it open again. It acts once per PR, and only
  when the thread runs in a linked worktree (never the project root), no
  user message in it is newer than the merge, the branch has no open PR
  now, and nothing runs in the worktree: every thread in it is idle, and
  no composer shell command, open terminal or setup recipe runs there (it
  waits while one does); the branch, the user's messages and the threads in the worktree
  are checked again with the threads locked, immediately before the
  removal. Unpushed commits do not block, since the local branch keeps
  them; uncommitted changes, untracked files included, keep the worktree
  with one warning notice and are never forced. Files git ignores are not
  counted and are deleted with the worktree (user ruling 2026-11-15). A
  `git worktree lock` with no reason (Claude Code's `EnterWorktree`) or
  with one Agent Overflow wrote is lifted; a lock with any other reason is
  a person's, and keeps the worktree with one warning notice ("Kept
  worktree fix-pr-diff-fetch: it is locked.") (user ruling 2026-11-15). A
  removal the user asks for lifts any lock. Moved threads get "PR #90
  merged. Removed worktree fix-pr-diff-fetch; this thread now runs in
  Base." (GitLab "MR !12"); a kept worktree gets "PR #90 merged. Kept
  worktree fix-pr-diff-fetch: 3 uncommitted files." once. A removal git
  refuses is told once ("Kept worktree fix-pr-diff-fetch: removing it
  failed.") and retried with a growing wait; the removal notice follows if
  a retry succeeds, and after five tries the worktree is kept. A worktree
  with initialized submodules, which git removes only by force, is kept at
  once ("Kept worktree fix-pr-diff-fetch: it has submodules."). No card, banner, toast or button. A worktree already
  gone counts as done; the worktree registry watch reconciles it. A
  failure to read a thread or the forge is retried later, never taken as
  a reason to skip. The periodic state read follows "Fetch remotes in the
  background": off, the app reads no PR state on its own, and a turned-on
  setting picks up what merged meanwhile at its next pass, without a
  restart. A lookup of a thread on screen that stops finding its PR still
  reads that PR once, and the create paths still look the branch up.
  Mechanism:
  [forge transport](architecture/forge-transport.md#merged-pr-worktree-cleanup).
- A branch's PR/MR stays in the header badge, the git menu and the review
  pane until the forge answers that the branch has no open PR (user ruling
  2026-10-14). Opening a thread asks the forge again but keeps showing the
  last known PR meanwhile; a failed lookup keeps it, with the error in the
  git menu item's tooltip, and is retried on its backoff while the thread
  is open. Only
  an origin re-pointed at another repository drops it without an answer.
- Draft worktree and branch operations are DISK state, not thread state:
  project-scoped RPCs, bound to the thread at send or creation. Accepted
  consequences: an abandoned draft's worktree stays in pickers; a restart
  loses unbound setup runs and staged intent.
- A terminal thread lives as long as its shells. When its last terminal
  exits, its computer deletes it whether or not a client is connected, and
  it leaves the sidebar and every pane; a restart or crash ends every
  terminal thread the same way. Nothing reopens or restores one: the user
  opens a new terminal. A chat thread's drawer terminals are only tabs; a
  dead one is removed and the drawer collapses with the last. Restoring
  terminals across a restart would be a separate feature
  (`internal/app/app_terminal_threads.go`).
- Companion panes belong to their thread, per client. A pane leaving a
  thread (switching, closing, starting a draft) hides the thread's
  companions, and the next pane to show that thread reopens them where they
  were: order, widths, an agent pane's scope, a side chat's conversation.
  Closing a companion forgets it; deleting or archiving the thread forgets
  all of them and deletes a hidden side chat. Plan, review and agent survive
  a restart like the layout; take-control, side chats and agent-thread
  companions last the session. An agent-thread companion shows a real
  thread an Agents row opened; closing it never deletes the thread.
  Nothing is shared across clients. The browser pane follows its backend
  state instead (`frontend/src/lib/stores/companionStash.ts`).
- Thread groups, pins, and auto-pin rulings: `docs/specs/sidebar-thread-groups.md`
  and `internal/store/AGENTS.md`. Re-pin-to-bump is deliberately dead.
- Assistants (standing agents) are records with a name, a home and one
  current thread, created by New Assistant or an assistant's
  `thread_new_assistant` and never promoted; promotion and demotion are
  gone. New Thread, Same Assistant carries loops, watches, reminders,
  requests and workers to the successor. The name is the current
  thread's title, unique per computer case-insensitively; a clash
  across paired computers shows `<name>-<computer>`, display only.
  Deleting the current thread deletes the assistant and its home, after
  one confirm naming the folder; a home outside
  `<config>/agents/<agent-id>` refuses the delete. Deleting a past thread
  touches neither. An assistant stays on its computer: its current
  thread is copied to another computer, never moved, and the copy is an
  ordinary thread (ruling 2026-10-21). The retention sweep never removes
  an assistant's current or past threads. A session the provider cleaned
  up moves the assistant to a new thread on its own, its opening block
  naming the previous thread (ruling 2026-10-15,
  [standing agents](specs/standing-agents.md) rulings 22 to 29,
  "Retention" and "Session loss").
- Turning assistants off pauses and hides them; it never deletes. Every assistant, thread, home, memory folder, wake
  registration and draft stays as it was, and turning them on resumes
  each loop and watch, serving the fires it missed once with their
  count. The setting is the computer's: its assistants are shown and
  running, or hidden and paused, in every window, by that computer's
  setting alone, and Settings changes the computer its picker shows
  ([standing agents](specs/standing-agents.md), "Turning assistants off").
- Multi-agent discussions are removed. No thread nests under another in
  the sidebar; a thread renders flat under its project or group, and a
  spawned thread is its spawner's worker with no row unless placed
  ([standing agents](specs/standing-agents.md) rulings 15 and 36).
- Agent rows and groups (ruling 2026-10-16,
  [standing agents](specs/standing-agents.md) rulings 4, 5 and 30 to
  35): a spawned thread's answer to its spawner, an error included,
  marks it read unless it is pinned; archive is the only done state
  (the done fold went with nesting, ruling 2026-11-14); a group shows
  the project's preview limit of members, pinned members exempt; a
  collapsed group shows its members' bubbled dot, replacing the
  2026-09-02 rule that a collapsed group renders none of it; every
  show-more footer (project, group) and a collapsed project's dot rank
  their hidden rows the same way, so an unread one lights it; running
  sorts above unread inside a container. Mark All Read on a group marks
  every member in one call. An assistant is one row holding nothing
  (ruling 2026-11-14).
- Owned workers (ruling 2026-11-14, amending 2026-10-17,
  [standing agents](specs/standing-agents.md) ruling 36): a thread any
  thread spawns is owned by its spawner, with no sidebar row and no
  completion notification of its own, unless placed standalone or in a
  group; the spawner or the user moves it either way later. Archiving a
  thread archives its owned workers, and deleting it deletes them, down
  the lineage (ruling 2026-11-16); a worker with a sidebar row is the
  person's and stays. A delete is one backend operation over the whole
  family: anything under the thread that refuses refuses it, with
  nothing stopped or deleted; a family another delete holds is refused.
  A delete that went ahead never rolls back: what fails after it stays
  committed and hidden, and deleting again or the next boot finishes
  it. Threads selected together are one delete per computer. Placement
  is organization only: the spawn record, the reply path and the read
  mark on an answer do not change. An assistant row never joins a group.
- Ownership crosses paired computers (owner rulings 2026-11-17,
  [standing agents](specs/standing-agents.md#owned-workers)): a worker a
  thread spawns on another computer is owned there through the receipt's
  link, with no sidebar row in any window. Its prompts go to the spawner,
  pulled by the spawner's computer, and reach the person there when the
  spawner's computer has not said it holds them for two minutes. The
  spawner's computer owes it the spawner's archive and delete, and that
  computer admits them through its own archive and delete. An archive or
  delete that computer refuses or cannot receive stands here, is named as
  pending there and is retried; an older computer is named once. No new
  indicator is drawn: "on <computer>" is the only mark, and a window not
  attached to that computer cannot open or place the worker.
- Deleting a thread never removes a worktree (owner ruling 2026-11-17):
  no delete removes, checks or is refused by a worktree, whatever work it
  holds or whoever else is on it. The worktree stays for Remove Worktree.
- Thread content search matches title and workspace path only. Searching
  message text server-side (t3-code does) is undecided, not rejected.
- A fork is a snapshot of its source at the cut. Nothing the source does
  afterwards (continuing, reverting, being deleted or moved) changes what
  the fork shows, so a fork is never told about its source's writes. A
  provider report that arrives for the source after the fork was made is
  the source's fact: the source records it and the fork keeps the row as
  it was. A
  deleted source's history stays, hidden, until the last fork that shows
  it is deleted. A fork taken while its source is still running a turn
  owns that turn as it stands. The fork's timeline holds no origin row;
  the thread row records where it came from
  ([pointer forks](architecture/sqlite-store.md#pointer-forks)).
- A tag is picked like a branch: in place, as a new worktree, or as the base
  of a new branch. Picking a tag checks out the tag itself, which leaves the
  workspace on no branch; no branch is made for it. The workspace then reads
  the tag's name, or "detached at" the short commit once HEAD is at no tag,
  never "No branch". There, Push gives way to "Create branch from here", and
  removing a worktree counts commits no branch holds as unsaved work.
- The branch picker has no tags section. With no search text, tags not
  recently picked follow every branch; while searching, branches and tags
  share the branch order (default branch, recent picks, newest first). Tags
  are rare, so they never take list space from branches.
- Tags are never force-fetched or pruned automatically. "Refresh tags from
  origin", the last row of the picker's list, force-fetches them without
  pruning when clicked: a forced fetch replaces a local tag of the same name,
  and an unforced one stores no tags at all once any tag has moved upstream.
- Workflows take branches only: a run merges its work back into its base.

## Subagents and background work

- Rulings: `docs/specs/agent-visibility.md`. In short: the launch row is
  unchanged except the open-pane door and the indicator rule below; every
  detached execution gets one card at its completion; the card body is an
  allowlist; approvals show only in the composer, never as a card or tray
  pill.
- Rows above the write head never change on screen (ruling 2026-09-25). The
  one exception: a Claude background launch row's `backgrounded` indicator
  turns off once, when the launch settles as its stored
  `live_background_active` bit records, and never turns on again; its box
  stays, so nothing on the row moves; a parked
  stop does not settle it, and a later run is a new row with its own
  indicator. Nothing else on an older row reads live state. A revert that
  removes an agent's result leaves its launch with no result card; the
  store owns that case. Every Claude agent card is the agent as of the stop
  it sits at: the numbers reported there, the duration from the launch to
  that stop, and every row up to it. A Codex card keeps its execution: its
  saved bounds, or without them the rows since the previous completion.
  Card headers, the depth-cap marker and the tray show tools and tokens,
  never a row count. Mechanism:
  [immutable agent history](specs/agent-visibility.md#immutable-agent-history)
  and [agent runs and stops](specs/agent-visibility.md#agent-runs-and-stops).
- A Codex child's answer is a normal message; never a special final-answer
  block. Its token figure is the child's cumulative spend
  (`childAgentTokenSpend`); Claude's `task_progress` total is latest input
  plus cumulative output by the CLI's own construction, so the two agree
  until a compaction.
- A finished background task's launch row keeps its launch state; the
  completion sibling carries everything about the execution: terminal
  result, final tool and token counts, descendant count and the answer
  preview that is the card's collapsed line. This holds for Claude and
  Codex alike (ruling 2026-09-10). See
  [Tool, task and turn lifecycle](architecture/turn-lifecycle.md).
- Completion never reads or replays an agent's sidechain transcript
  (ruling 2026-09-23). An agent's rows come from the live stream and the
  session mirror; `transcript_mirror_degraded` is the only degraded
  behavior. The completion preview is the report in the notification
  `summary`, written with the card; a later notification of the same stop
  can give an answerless card its report but never replaces or clears one
  (ruling 2026-09-25). A command's `output_file` is still read into the
  bounded `command_output` payload, and followed live in the tray while
  the command runs: the app tails the file itself, never through the
  CLI's `TaskOutput`.
- Monitor idle-wake: the CLI writes `<task-notification>` to the
  transcript only. A transcript-tail backfill was proposed and declined.
- A Claude Stop kills every running or parked background agent, from any
  turn, with the shells it owns. Stop therefore asks for confirmation
  while such an agent is live, and the backend refuses it until
  confirmed (ruling 2026-09-24). The app asks through one
  dialog at the root (`BackgroundKillConfirmationHost.svelte`) that names
  the agents; declining stops nothing, and the sent message stays either
  way. Agent thread requests, their cancels and workflow takeovers are
  not gated. Wire facts:
  [claude-wire.md §Background task ownership](references/claude-wire.md#background-task-ownership).
- Work the Claude CLI starts on its own (a background-task wake, a
  scheduled wakeup, a peer session's message) is a turn of its own, with
  its own timer and Stop and no user row; the completion notice that woke
  it stays above it. A scheduled prompt is the exception: the turn opens
  with it as a row badged Scheduled. `/effort`, `/fast` and `/rename` open no turn. A
  turn ends when the model's message ends, so a thread waiting on a
  running `local_agent` is idle; it runs again only if the model
  continues that turn (ruling 2026-10-24).
  Mechanism:
  [turn-lifecycle.md §Rounds and turns](architecture/turn-lifecycle.md#rounds-and-turns).
- A forked skill is an awaited agent with a different label. Its card has
  no Stop and no background action; its agent pane's Stop ends the fork
  alone (the main turn goes on after a fork the model launched, and a
  typed command's turn ends with its fork); the turn's Stop ends
  everything (ruling 2026-10-14). The CLI refuses to background a fork
  ([claude-wire.md §E9](references/claude-wire.md#e9-forked-skill-skill-tool-context-fork)).
- Pre-existing dangling Codex child rows in old fork threads are left inert
  on purpose (`internal/store/AGENTS.md`).
- A standing agent's home: the app reads `instructions.md` and the facts
  in `memory/` from it and prescribes nothing else
  ([agent home contents](specs/standing-agents.md#agent-home-contents)).
  The app ships no skills; what an agent needs to keep memory arrives with
  its memory (ruling 2026-10-21). A file that cannot be read is a warning
  row on the thread, not a failed start. A standing agent is named with its role line on the footers it
  sends and on the wakes of its replies, on either computer: a thread on
  another computer behaves as a local one does.
- An assistant's session block depends on its record alone, so a home
  file edit keeps the provider's prompt cache. Memory reaches the model
  whole through a SessionStart hook at a start and at each compaction,
  never at a resume, which would stack another copy per resume (ruling
  2026-10-18), and an edit reaches a running thread with its next
  prompt: a UserPromptSubmit hook delivers only what changed since that
  session last received its memory, from the session's own record,
  nothing when nothing did, with no version control and no state shared
  between sessions. Nothing is cut: a delivery past what a provider
  shows the model whole opens with its size, the agent reads the file
  the provider saved, and the assistant is warned (rulings 2026-10-21).
  The app builds each memory index from the facts' frontmatter, so no
  agent keeps one (ruling 2026-10-21). A fact follows an event and names
  its evidence, the user's words or a check; what an agent only judged
  worth remembering is not saved, and a fact whose source is neither
  form is left out of the index (ruling 2026-11-18). On Codex the thread's config
  trusts only the hook commands it registers. A delivery that did not
  arrive is a non-fatal error row, not a failed turn
  ([memory](specs/memory.md#delivery)).
- Assistants live above projects: an assistant serves projects it is
  assigned, runs in its home, keeps helpers to its own work under it
  and puts project work in the project; deleting it deletes what lives
  under it, and removing a project only unassigns it
  ([standing agents](specs/standing-agents.md) rulings 37 to 40).
- Messages between agents are not the person's messages (ruling
  2026-10-13). A thread tool's spawn prompt, request or question, the
  answer that comes back, a loop, watch, outside send or worker wake, a
  remote job's completion, a workflow run's wake, and a provider's own
  agent message stay user-role on the wire and
  `user_text` rows in the store, and render as a folded agent row naming
  the sender, never as the user bubble; their footer is shown as facts,
  never raw. Such a row is the thread tools' traffic and an activity-run
  member as a tool row is, wherever it lands, alone a run of one; the
  person's messages end a run (ruling 2026-11-19). They are not navigator ticks, composer recall or send-queue
  preview entries, and the thread tools label them `agent` with their
  sender. A Claude join holding the person's composer message stays the
  person's, and so do a workflow phase prompt, which is a phase thread's
  only user message, and the messages the person starts from a button
  (answers, review comments, triage and PR discussion). A person's Stop
  on a turn a request started reaches the sender as the person's Stop,
  never as a restart (ruling 2026-10-14). On the wire every such message opens with a line naming
  its sender and closes with an end mark, and the person's own messages
  are the only turns without them (ruling 2026-10-21). Mechanism:
  [how agent messages appear](specs/agent-thread-tools.md#how-agent-messages-appear).
- A loop or watch turn that ends cleanly leaves its thread read and in
  place and notifies nothing; there is no per-assistant switch to make
  ticks notify. An agent reaches the user from such a turn only with
  `thread_attention`: its kind and one-line reason show on the row until
  the user reads the thread, and the turn's notification says the kind,
  never the agent's reason. Any thread may flag itself (ruling
  2026-10-22, [standing agents](specs/standing-agents.md) rulings 9 and
  41, [attention](specs/standing-agents.md#attention)).
- A watch that keeps failing notifies the person once per failure
  episode, at once when its process cannot start and otherwise at a
  failed run five minutes after the episode's first, and its next clean
  run withdraws it; it never wakes the agent and adds no retry. The
  assistant instructions say workers implement project changes while the
  assistant may build tools in its home, that a worker's evidence is
  checked before work is reported done, that a working procedure is saved
  and reused, and that the opening settles a first task before any
  recurring work. An Instructions action on the assistant is declined
  for now (owner ruling 2026-11-17,
  [wake supervisor](specs/standing-agents.md#wake-supervisor)).
- In an assistant's thread, Stop is two actions: Interrupt (stop the
  turn, keep the session) and End Session (end the process and its
  background items); a workflow run's Pause keeps its members' sessions
  and Pause and End Sessions ends them; the Agents row's Stop on a
  spawned thread is a rest, not one of these (ruling 2026-10-15,
  [interrupt and end session](specs/standing-agents.md#interrupt-and-end-session)).
- Requests to other threads and agent messages queued for a thread list
  in the activity rail's Agents segment, apart from Background, with a
  user Stop that keeps the caller's wake armed and brings a spawned
  thread to rest on any computer: its session and everything it waits on
  or started end, a workflow run that reports to it is paused with its
  members' sessions ended and stays resumable, and it stays where it was
  ([at rest](specs/standing-agents.md#at-rest)); settled ones page under Past;
  incoming agent messages list there instead of the composer's queue
  (standing agents
  rulings 20 and 21,
  [Agents segment](specs/standing-agents.md#agents-segment)).

## Providers and accounts

- Model, effort and fast-mode selections trust previously observed capabilities
  and remembered choices. Catalog expiry never blocks a selection. A refresh
  warns only when a probed or live catalog withdraws a part of the selection
  the previous probed or live catalog listed; the shipped list is a placeholder
  and never warns. Warnings are advisory toasts; keep the selection and allow
  sending. Provider rejections remain timeline errors without a duplicate toast.
  The Claude catalog learned by an account probe is persisted per account and
  seeded at boot while the binary is unchanged (`internal/claudemodels/AGENTS.md`).
  Claude fast mode is the exception: only the CLI's report states it, so it is
  offered only on a model a report marks fast. Once a report exists, a saved
  fast flag on any other model is cleared before launch; before one, it stands.

- AO never calls Codex `thread/queue/add`; a mid-turn send is `turn/steer`
  (`internal/provider/codex/AGENTS.md`).
- Rollback refuses on an unpurgeable Codex queue.
- Cross-session OFF always writes `crossSessionInbound:"refuse"`, overruling
  the user's own `~/.claude/settings.json` accept. Per-thread gating for
  full-access threads is not built (global only).
- Sharing `~/.claude` between AO, terminal `claude`, and Claude Code is the
  designed mode; never "fix" it with per-account `CLAUDE_CONFIG_DIR`
  (`internal/provideraccounts/AGENTS.md` has the case table).
- The retention window lengthens Claude Code's own transcript cleanup for
  the Claude processes AO starts and never shortens it: the
  `cleanupPeriodDays` override is sent only when the window is longer than
  the period the process resolves from `~/.claude/settings.json` and its
  workspace's `.claude/settings.json` and `settings.local.json`, and
  disabled cleanup sends the cap. AO does not write those files; the storage
  settings page names the value to set for `claude` run outside AO. Codex
  keeps its sessions indefinitely and AO leaves them.
- A model that exists only as probe enrichment (`claude-fable-5-1`) is not
  added to the hand catalog; the point of enrichment is that a new model
  needs no release (see `internal/claudemodels/AGENTS.md`). The exception is
  a model whose context windows the nearest catalog family gets wrong, since
  the CLI reports none (`claude-haiku-5-5`, 1M against Haiku 4.5's 200k).
- Model pickers offer what the provider's latest model list offers, plus any
  model a thread ran in the last week, and start new threads on the
  provider's own default. Before the first answer they offer the shipped
  list. Rows the list omits stay for capability lookup, so threads already on
  them keep running; a model the provider truly withdrew fails in the
  timeline (ruling 2026-10-15).
- A new thread starts on 1M on every model that offers it, Claude and Codex,
  including models where a long prompt costs more (Haiku 5.5 over 100k
  tokens); 200k stays selectable (ruling 2026-10-15).
- Model inputs are full model IDs. AO keeps no alias table: a Claude alias
  (`opus`, `sonnet`) is refused as input with the offered IDs, and a Codex
  slug is passed as written for Codex to judge. An alias the CLI reports back
  is read only through its own model list (`internal/provider/AGENTS.md`).
- A sign-in in Agent Overflow never switches accounts: it renews the saved
  account its login belongs to or adds a new one at the bottom. Only the
  first account on a machine becomes active by signing in. Saved accounts
  stay in the order the user drags them into; being active does not move a
  card.
- The composer shows one usage meter per duration the account has a
  provider-wide limit of, so a plan without a 5h window has no 5h ring. An
  account with no such limit yet shows the weekly meter alone. A scoped limit
  of the same duration (a per-model weekly quota) is the meter's inner ring,
  colored by its own threshold; with several, the most-used one. The
  provider-wide limit keeps the outer ring so a scoped quota never reads as
  the account being exhausted.
- Account auto-switch ([spec](specs/account-auto-switch.md)): every saved
  account is in the pool until its Auto-switch checkbox is cleared, with no
  master switch.
  It changes the one provider-wide selection, never per thread and never
  through a proxy. A selection a person or an outside login made holds until
  that account can no longer serve the running models. Continue after a
  usage limit is off by default, kept in memory only, and never sent to a
  workflow-owned or hidden thread; a workflow run parked on a usage limit
  stays parked until resumed.
- Claude usage meters read only `/api/oauth/usage`. The stdio
  `rate_limit_event` (including 2.1.257 `unifiedWindows`) is not a quota
  source: it names no account, and responses to requests sent before an
  account switch land after it.
- Binary upgrades under a running app: per-thread restart button, never an
  auto-recycle of sessions; no version ranking or auto-default.
- Provider CLI updates (`internal/providerupdate`): the app never updates
  a provider CLI on its own. It offers an update only when the version the
  provider's own updater would install is newer than the installed one,
  because `claude update` on the stable channel installs an older
  version; it runs that updater and nothing of its own. A Codex install
  that npm, Homebrew or the Codex installer does not own has no updater,
  so its check fails saying so and nothing is offered. A provider whose
  update command could not be stopped is refused further runs until the
  app restarts. One toast per provider version per client, a dot on the
  provider's settings entry, and nothing in the sidebar footer.
- Session import (`internal/sessionimport/AGENTS.md`): one AO thread per
  Claude leaf; dedup mandatory; non-active branches materialize lazily at
  first send; no "imported" badge; no auto-sync; an imported historic model
  never becomes the composer default.
- If Claude Code treats a queued batch as one message, AO does too, including
  on revert: a flush drain of N>=2 messages for a headless Claude session is
  dispatched as one envelope and recorded as one row carrying every member's
  send id, and a batch the CLI merged across separate AO drains is folded into
  one row when the survivor's echo proves the merge (`claude-wire.md`
  §Queued-message consumption, boundary-drain merge). The fold requires an
  exact block-sequence decomposition; an echo that differs any other way is
  logged and left alone rather than guessed at.
- A Claude rollback whose stamped provider uuid is missing from a transcript
  that continues past it FAILS loudly instead of falling back to the ordinal
  walk; only a transcript ending before that turn is recoverable by cloning.
- The app guide appended to Claude, claude-tui and Codex sessions is on by
  default and user-toggleable, spawn-only, names only the app's tool servers
  that are on for the session, and is never added to a workflow-mode thread
  (phases and units; `docs/specs/prompt-tool-overrides.md` §App guide).
- Claude and claude-tui always pass `--system-prompt-snapshot off` on a binary
  known to be 2.1.267 or newer, guide or not, so a resume runs the current
  prompt; the default body is stable across resumes of one build, so the
  prompt cache is unaffected (same spec §Claude prompt snapshot).
- System prompt override text lives only in its file under
  `<configDir>/prompts`, which the user or an agent edits; Settings has no
  text editor and its row holds id, enabled and models (decided with the user
  2026-10-11, text-only-in-file ruling 2026-10-14; same spec §Prompt files).
- A prompt change never applies to a running session: no live swap and no
  automatic restart. It applies at the thread's next session start. Each live
  thread whose next start would use a different prompt shows a dismissible
  "system prompt changed" banner with Restart; a later change shows it again,
  and a restart or the session's death withdraws it. The stale-binary banner
  is dismissible the same way (2026-10-14; same spec §Reconcile semantics).
- When Claude Code leaves auto on a model without auto mode, the thread's
  runtime mode follows what the CLI runs, with a warning notice. No other
  mode the CLI moves to on its own changes the thread's access. Switching
  back to a model whose remembered settings carry Auto restores Auto with a
  notice of its own (rulings 2026-10-15 and 2026-10-16; `claude-wire.md`
  §system/status).
- Claude offers no thinking-off setting: current models reject disabled
  thinking and the CLI runs adaptive thinking instead (ruling 2026-10-15).
- Cursor as a provider: `docs/specs/cursor-provider.md`.

## Workflows

Decisions D1..D77 are in `docs/specs/workflows-system-decisions.md`. Rulings
and anti-changes that live only here:

- claude-tui hosts `ao-workflow-tools`; thread tools there are a separate
  decision ([agent workflow tools](specs/agent-workflow-tools.md), D77).
- Workflows keep no clock. A scheduled or triggered run is started by a
  standing agent whose loop or watch wakes it
  ([standing agents](specs/standing-agents.md) ruling 13, D76).
- Maximum autonomy with full authoring flexibility; intervene only when
  there is no good way to proceed. Free-running default, notify-not-gate at
  wave boundaries, soft-stop is the brake.
- No spend-based self-repair allowance ("if I worried about costs I'd set
  budgets"). Park only for environmental issues or structurally impossible
  or ambiguous asks. Quality first, efficiency second.
- Normal thread and chat behavior never changes to protect workflows. A
  workflow-special account path was built and fully unwound; protection is
  the bounded start plus honest failure reporting.
- AO never auto-commits (prompt-side responsibility) and never pushes during
  a run (only the manual item-PR verb). Direct prompt units may declare
  custom `resources:`.
- Deliberately not built: series or campaign primitive (derived ordinal
  suffices); standing supervisor phase (born cold fails); writable budget;
  arithmetic in predicates; script-based definitions; native fan-out unit
  outputs to later phases (join is the contract; jq through the join);
  engine-computed checkpoint parity; failed-unit null-filter stays a human
  verb; `capabilities:` / `mcp:` on Phase left unrefused until they have a
  runtime consumer.
- Dismissed from the orc investigation: initiative-to-task hierarchy,
  weight classification, auto/ai/human/skip gates, strictness profiles,
  heavy knowledge infrastructure, Ralph loop, bench harness, token pools,
  team mode, rewind, retry_map.

## Remote access, browser pane, phone

- `docs/specs/remote-access.md` §18 carries the rulings. Never re-propose
  public exposure of the personal backend (no tunnel, no public session
  class); release signing is cut (sha256 sidecar over HTTPS is the trust
  line).
- A Windows/WSL release without remote access ships for company machines
  (`noremote` tag). Remote features cannot be turned on in it; everything
  else, including history, is unchanged. It updates only to that build, from
  a private GitLab project through the user's `glab` login, with no stored
  token. It does not clean up remote state, since its users have none.
  `docs/architecture/noremote-build.md`.
- Repository identity exists only for GitHub and GitLab checkouts. A Git
  repository with no origin or another forge's origin is a plain folder:
  no identity warning on its project, and on another computer it becomes
  its own project ([project identity](specs/remote-access.md#7d-project-identity)).
- Cross-device advisory toasts are not worth fixing; only sticky
  misattributed banners get connection attribution.
- Browser pane: an embedded real engine per platform, never a streamed
  simulation, and no remote fallback for it. Clipboard file paste for Teams
  is impossible (Teams refuses every pasted file object); the toolbar button
  is "Show in folder". `docs/specs/embedded-browser.md`.
- A browser page fills the pane at 1:1 and reflows with it, like a tab in
  any browser. A fixed page size is an explicit `browser_viewport set`,
  never a default the pane scales down.
- Every thread/workspace event reaches any client with visibility; channel
  audience is by data class, loopback-only is for host directives only. A
  mutation that persists without emitting is a bug.
- Error detail is not redacted by origin (2026-10-08). A paired session that
  may call a method receives its full error text and wrap chain, and a
  session granted `threads:operate` may read the backend log lines behind a
  failure: that grant already lets it run an agent on the machine. Panic
  values stay in the log. `docs/architecture/transport.md#rpc-failures`.
- Agent thread tools are admitted in every runtime mode on both providers
  with no per-call prompt (`docs/specs/agent-thread-tools.md`, Availability
  and permissions). `ao-browser-tools` and `ao-remote-tools` stay denied in
  read-only sessions: both act outside the thread, and read-only is the
  mode for unattended work.
- Agent profiles (2026-11-16): the person's roster of named agents, each
  with a use-for line, is how a thread picks another agent. A spawn
  still inherits by default. The guide names every profile whose
  provider this computer offers and checks no model, because it is fixed
  when the session starts; `thread_options` and the spawn check the model
  and refuse with the reason. Use-for lines and choosing notes are in
  `thread_options`, paid for only by a thread that is choosing. A
  profile, note or listing never points at a provider the computer does
  not run: availability is the provider switch plus a binary that
  resolves, and notes that need both families are conditional notes the
  person can edit. The seeds are the owner's, ordered by tier within
  each family, with the comparison across families in a note shown only
  when both run; an edited roster is never replaced. One resolver
  decides what a profile runs with for the listing, the settings page
  and the spawn, so a profile is listed only when it runs as listed.
  `docs/specs/agent-thread-tools.md`, Agent profiles.
- Spawn instructions (2026-11-16): `thread_spawn` `instructions`, after a
  profile's, are appended last to the thread's system prompt for its
  whole life, stored with the thread, on every provider; a provider or
  paired computer that cannot carry them refuses the spawn, and one that
  cannot receive them refuses the transfer, rather than dropping them.
  The spawn row shows them from the spawn's own reply, never from the
  live thread. `docs/specs/agent-thread-tools.md`, Spawn instructions.
- Sub-thread depth is enforced by removing tools, not by refusing calls
  (2026-11-16): a session at the limit (Settings, 1 to 4, default 2) has
  no `thread_spawn` and no native subagents, and its guide says so. The
  tool list is fixed at session start; a spawn that reaches a thread at
  the limit after a change is refused naming it. The level counts
  delegation: an owned worker sits below its spawner and an agent's ask
  copy below its asker. A thread outside an agent's delegation (a
  sidebar row, including a spawn given one; a person's side chat; a
  workflow phase, which its definition and engine bound) starts again
  at 0, and an agent cannot move a thread on its own chain there. The
  level travels with an owned spawn or ask to a paired computer and
  ends when that thread leaves the delegation (grouped, made standalone,
  copied or moved): it counts only while the thread is owned there or
  is the ask copy, so Remove from Sidebar restores it. `docs/specs/agent-thread-tools.md`, Sub-thread
  depth.
- AO tool rows (thread, workflow, browser, remote) present as sentences
  with structured results, never raw JSON (2026-10-13). The header says
  what the call did in a person's words, ids read as names, and the body
  decodes the reply into an outcome, links, facts and folded text; a
  failed call shows its message and code. Text no decoder reads is
  reported as undecoded and shown only on request. `docs/specs/agent-thread-tools.md`,
  How calls surface in the timeline.
- Phone chat chrome (2026-09-13): header row one is back, title, diff
  badge, menu; row two is the full-width `machine · project · branch ·
  worktree` line with every segment a direct tap into its picker. The
  title fades and swipes rather than wrapping or truncating; only the
  branch ellipsizes; the worktree is an icon that never scrolls away
  (it is the way to New worktree). No workspace strip on the phone; the
  token count sits right-aligned on the activity rail, with cost in its
  popover. The rail keeps one fixed-height row: Checklist and SendToBack
  icons with counts replace disclosure arrows and labels under compact;
  wide spinner art scales proportionally within its fixed-height slot.
  The rail shows whenever there is usage to report. Desktop shows the project
  as `project / title` in the header, and its strip is machine, branch,
  worktree, cost. The desktop crumb and every segment of the phone's facts
  line wear the secondary text color, one visible tier under the title in
  every built-in theme; they are not dimmed to the hint tier, and the
  title is not enlarged. Opening a thread on the phone never focuses the
  composer; the keyboard rises only on a tap into the input.
- Phone menus (2026-09-13): every popover and context menu opens where it
  was tapped, anchored to its control or to the pressed point and clamped
  into the viewport. No bottom-sheet menus; the earlier sheet default is
  reversed.
  `docs/specs/remote-access.md` (6f).
- Phone touch pass (2026-09-13): every action a mouse reveals on hover is
  visible on the phone; shared primitives carry a compact hit size; the
  timeline's nested output boxes have no height cap under compact so a swipe
  never latches inside a tool row; a browser's Back and Android Back run one
  ladder, and a context menu or modal claims the press before anything under
  it; a long press on the terminal no longer pastes, the key row has Paste;
  a remote client sees an inert, labelled control for anything its grants
  refuse, never a live one that fails afterwards. Follow-ups the same day:
  the facts line shows a linked worktree's name and truncates it before the
  branch; the compact header menu carries Search messages; pin, multi-select
  and project reorder stay off the phone (hold-only, not offered, not built);
  Settings opens with focus on the card, not the search field; editing a past
  message on the phone happens in the composer's slot with the bubble
  outlined, because the timeline row sits behind the keyboard; a touch tap
  on Send keeps the input focused, so the keyboard's relayout cannot swallow
  the tap and the keyboard stays up for the next message.

## Review pane

- Icon buttons with hovertext, never text buttons, for thread actions
  (owner preference, stated for the comments overhaul; applies to new
  review chrome).
- The PR title bar reads one fact per line (title; author and state;
  base ← head, conflicts and verdicts; CI with its refresh wrapping among
  the chips) instead of one wrapped meta line; the toolbar keeps the diff's
  own counts and folds whitespace, wrap and split into a view-options
  menu; the file-tree toggle sits with the tree (a thin strip when hidden,
  full width over the diff on compact); the conversation refresh sits on
  the Conversation header. Files auto-collapse only past 5000 rows (or
  lockfiles): a normal source file is never folded by default. The tree's
  filter can hide test files by naming convention, accepting that helpers
  without a test name are missed (ruling 2026-10-19).
- The toolbar's selectors are one dropdown style (the app's menu), and the
  commit picker's trigger is fixed-width content that never clips: `All
  commits`, or the commit's ordinal and short hash once one is picked. The
  subject, author, time and message belong where there is room: one row
  per commit in the menu (with a previous/next stepper in its header), and
  a commit card that leads the diff list and scrolls away with it; in
  `All commits` there is no card. No always-present row for the picked
  commit (a height shift with nothing to show in most states) and no
  toolbar comment tally (ruling 2026-10-20).
- Merge actions have one home: the Merge section that leads the overview
  row (readiness pill, Approve or Unapprove, `Merge ▾`), identical in a
  thread's companion pane and on the Pull requests page. The title bar's
  refs line carries the readiness pill and never an action. Merge has no
  confirmation step (the forge refuses a stale head; gh and glab do not
  confirm either); the button is never disabled, offering auto-merge while
  blocked with "Merge now" greyed and naming what it waits for. The forge's
  events (approved, revoked, merged, closed, draft, ready, auto-merge,
  force push) are rows of the Conversation feed. Pipelines on the page
  default to every status; the review pane's CI line stays the head
  pipeline only (rulings 2026-10-20, `docs/specs/pull-requests.md`).
- Tools (Workflows, Pull requests, later Issues) are pane kinds behind
  one sidebar tools row above Settings: the active tool carries its label,
  the others their icon with hover text; only Workflows carries a count
  (ruling 2026-11-11: no Pull requests count in the sidebar, and the
  page's `Needs you` count follows its project picker). A click
  opens the tool in the focused pane like a thread; ctrl+click opens a new
  pane, so two tools sit side by side with no special case; a drag places
  it in the pane strip like a thread row (2026-11-11). Workflows
  loses its modal. The row's label follows the focused pane when it is a
  tool, else the tool used last (ruling 2026-10-20).
- The Pull requests page never records which PRs the user opened. A row's
  attention comes from the forge's own record of the viewer's activity
  (GitHub's latest review, GitLab's reviewer state and the viewer's notes
  and approvals in its events feed), so it reads the same on every
  computer and in the forge's own UI. The list opens on its stored copy
  and never reads one PR on its own (ruling 2026-10-21,
  `docs/specs/pull-requests.md`).
- PR comments live in the overview: Description (collapsed by default)
  and Conversation (also collapsed by default, and capped short enough
  that the diff stays visible under it), two collapsible sections that head
  the diff list as its first row and scroll off with the diff; once off
  screen the title bar offers peek buttons back to each. No Comments tab
  or separate list (ruling 2026-10-08, superseding the fixed header and
  the rail tab). The Conversation is one chronological feed (newest
  first) of thread cards on a timeline rail, review verdicts and commit
  pushes, mirroring the forge's overview; a card's edge and chip carry
  its state (warning unresolved, success resolved); outdated is a marker
  on either state (dashed edge, "outdated" chip, the line it was written
  at), never a third state: a thread on moved code is still open or
  settled, still counted, stepped and resolvable (ruling 2026-10-19). The
  thread's row on the diff is the same card. A top-level comment is
  NEVER truncated or clamped; only settled threads' replies may fold.
  Reading is protected from updates without hiding them: a card keeps
  its place once shown and never moves or disappears for a remote update
  (a resolve, a reply), and every arrival joins the feed at its
  chronological place on its own, never behind a press. An explicit
  refresh or reload shows everything it read. While the reader is
  scrolled into the feed or has focus in it, the card at the top of the
  viewport holds its position to the pixel and an "N new" chip points
  up at the arrivals, already in the feed; at the top, an arrival simply
  pushes the cards down. "Hide resolved threads" is remembered on the
  computer so it need not be ticked per PR, and hides only what was
  resolved at the last decision point and never a thread the reader is
  using; between decision points the hidden set only shrinks, and a
  review already open takes up a change made elsewhere only at its own
  refresh or reload, so nothing vanishes under the cursor. A thread
  read never removes a card a push brought while it ran. Each section fits its content up to a cap and is
  user-resizable (bottom drag handle, remembered height). Mechanism:
  `frontend/src/lib/components/review/AGENTS.md`.
- Forge attachments referenced by PR/MR bodies and comments (images,
  video, audio and files) render or download in the review pane through
  the user's `gh`/`glab` login, the same access the browser has, on the
  computer that owns the pull request, including media a forge wrote
  inside an HTML wrapper (`<p align="center">`, `<a>`, a table cell, a
  `<details>` body), which PR templates use routinely. Do not
  reintroduce direct third-party `<img>` fetches for private forge
  assets, and do not hide a reference the browser could open.
- Forge reads and writes go directly over HTTPS with the token of the
  user's `gh`/`glab` login, read from the CLI at runtime and held in memory
  only: never on disk, in settings, in logs, in argv or environment, in
  diagnostics or on the wire. `gh`/`glab` stay the login, the token
  handoff and `pr create`/`mr create`. No token of our own, no stored
  token, no OAuth app. `docs/architecture/forge-transport.md`.
- Issue trackers are a scoped exception to "no stored token": the Issues
  pane uses the app's own Jira Cloud and Linear client with the user's API
  token in an owner-only key file on the owning computer, never sent over
  the app connection (rulings 2026-11-15, `docs/specs/issues.md`).

## Miscellany

- Composer shell commands (`!command`, Run on a shell code block): the app
  runs the command for every provider and always sends the output; Stop
  discards it. Codex's native shell command is not used because it never
  starts a reply. Run on a code block runs on click. Claude TUI `!` history
  is not converted. See
  [composer-shell-commands.md](specs/composer-shell-commands.md).
- Commit dialog: the changed files are listed with none selected, and a
  commit records exactly the selected files, whole. Files that appear
  while it is open arrive unselected. It stays open on what is left and
  closes after the commit that leaves nothing. Generate describes the
  selection. Neither touches what the user staged outside the selection.
  A selected file that changed after it was listed fails the commit, so
  nothing is recorded that the dialog did not show. A commit, hooks and
  signing included, gets 10 minutes, long enough for a project's checks in
  a pre-commit hook; one stopped there ends its hooks and leaves the index
  unlocked. Every other git command that runs a user's hook (push and the
  forge CLIs' create, which pushes, pull, checkout, branch creation,
  worktree add, merge) gets the same 10 minutes and the same stop. A fetch
  runs no hook and keeps the 45 second bound. During a merge,
  cherry-pick or revert a commit takes every change, so every file is
  selected and locked. Not built: hunk staging, discard, amend, push
  after commit.
- Same-pane outside-click dismissal leaving focus on body (Enter-to-send
  after a background click stops working) is accepted; type-to-focus
  covers typing.
- Keep-awake: no input-simulation tier (a GPO-lock jiggler was investigated
  and not built); persists across restarts.
- Spinner sprites: no constant animation (no GIF, no CSS animation); JS
  timer at native cadence, frame-0 freeze on reduced motion.
- Markdown path links: rewriting happens only on a surface that passes a
  workspace path; directories are refused everywhere; never pass
  `defaultOrigin` to Streamdown.
- Markdown URLs: nothing an agent shows is withheld unless following it
  would run something. Links render for every scheme except the deny-list
  in `markdown/render/elements/urlSchemes.ts` (mirrored in
  `internal/externalurl`); path-shaped image srcs load from the thread's
  machine on every surface with a workspace, including paired browsers.
  Do not reintroduce an http(s)-only allowlist.
- Images in chat (local paths and forge attachments): the timeline paints a
  derivative at the display's own density (the box width times the device
  pixel ratio, rounded up to a fixed width ladder), never upscaled, lossless
  PNG for lossless sources and JPEG only for sources that were already
  lossy, so a screenshot loses nothing visible. The `<img>` box is the
  original's pixel size whichever bytes are painted, so a resize or the
  original's arrival never moves a row. A derivative is made only when it
  is under two thirds of the original's width (closer than that it costs a
  full decode for little), except that a derivative never exceeds 16
  megapixels and an original over that is always reduced, stepping down
  the ladder as needed, because the cap bounds what a client decodes. A
  click opens the one app-level lightbox on what the timeline shows and
  loads the original behind a visible "Loading full size (N MB)" line,
  with wheel, drag, pinch, keys and a double-click between fit and 1:1;
  the same lightbox serves message attachments, which open on their
  thumbnail. On a compact layout the lightbox and Copy Image stop at the
  widest ladder tier (so a phone's webview never decodes a file past the
  cap) and the line reads "Loading sharper image"; Save always takes the
  file. A right-click (long press on compact) offers Copy Image (the
  original bytes, or that tier on compact), Copy Path and Copy Markdown
  (the text the agent wrote, written back as CommonMark that parses) and
  Save Image (the owning computer's Downloads, or a browser download).
  Bytes cross on the ticketed byte routes, never inside a WebSocket frame.
  Local derivatives are files under the data directory's `cache/images`
  (256 MiB, least recently used out), reused across restarts, so a
  screenshot is reduced once per tier, not once per pane or boot; memory
  holds a derivative only while the directory cannot take it. Deleting the
  directory, even while the app runs, is the whole cleanup.
- Voice dictation: a mic in Send's grid, an outlined circle of Send's
  diameter. On the desktop it is in the toolbar immediately left of Send;
  on the phone it sits in the input row in Send's column, beside the
  attachment picker, centered on the last text line, so the toolbar keeps
  the model label's room. It records on the device and
  transcribes on the thread's computer through its Codex sign-in with
  ChatGPT. It inserts at the caret and never sends; Escape or switching
  the pane's thread abandons a recording. Dictation is off until Settings
  > Voice turns it on for that computer, and with no dictation source on
  there is no mic. The endpoint and its 30-day retention are in
  `docs/references/voice-dictation.md`.
- Live dictation: a second source that streams the microphone to OpenAI
  with an API key while the user speaks and shows the text heard so far.
  The key lives only in an owner-only file under the config root, never in
  settings, the environment or a client. Where both sources are on, live
  wins; there is no per-device source picker. An engine that cannot
  stream uses the subscription when it is on and otherwise shows no mic.
- Read aloud: a play control beside Copy on finished replies, in Copy's
  grid, that pauses while playing and offers a restart while paused. While
  a reply is read the row adds previous section, back (10 seconds, or the
  sentence for a device voice) and next section, a rule marks the section
  being read, and a "read from here" button shows beside another section
  on hover (always on the compact layout). A reply that is not being read
  shows nothing new on hover. One reply is read at a time across the app;
  a device voice resumes at the start of its sentence, an OpenAI voice
  where it paused. Code, tables and URLs are skipped silently. It is off
  until this device turns it on in Settings > Voice, and a device with no
  voice shows no control unless the reply's computer offers OpenAI voices.
- Read aloud in OpenAI voices: a computer whose host switch is on (off by
  default) makes each section's audio with the key in its key file, one
  request per section so pace and intonation carry across its sentences,
  and the voice picker lists those voices with a preview for each. The choice
  of voice stays per device; the reply's computer makes the request, so
  the key never reaches a client. A reply from a computer that does not
  offer the saved OpenAI voice is read in the device's voice, and a device
  with no voice reads through the computer.
- Wide blocks pan inside their own box on every layout: markdown tables,
  inline diff bodies and unwrapped fenced code scroll horizontally only
  when they overflow (the `pan-x` rules in `frontend/src/app.css`), with
  an edge fade as the touch affordance. Nothing is clipped by the pane and
  nothing changes for a block that fits. Fenced code wraps by default; the
  block overlay toggles unwrap per block, recorded by content identity so
  it survives retirement and remounts.
- Markdown tables take their content width: one that fits does not stretch
  across the pane. In a cell, inline code of 32 characters or fewer stays on
  one line and longer code wraps; a table whose short code cannot fit pans.
- Rejected from the t3-code survey, do not re-propose: hard steer, codex
  shadow homes, workspace file browser, changed-files card, global word
  wrap (per-block opt-in only), top-edge fade, favicon fetching, settled
  thread lifecycle, `iterations[-1]` context usage, prompt stash, claude.ai
  connectors, app-hosted MCP, classifier-row usage labeling, MCP toggle
  fan-out and per-thread pinning.

## Decisions still required

- Mid-turn correction workflow: a dedicated correction-needed mechanic is
  outside the current scope. It requires its own product design rather than
  being inferred from a provider wire event.
- Computer nicknames: retain both the Go profile nickname (`RenameBackend`)
  and the per-frontend `computer-nicknames` preference until the owner decides
  whether existing profile names must remain visible to connected windows
  after an upgrade. The spec requires both frontend-local names and readable
  legacy desktop names. Removing one system, showing Device name only during
  access approval, and relabeling each row's local nickname to Rename depend
  on resolving that compatibility requirement.
