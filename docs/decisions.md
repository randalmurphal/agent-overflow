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

## Performance and memory

Do not trade rendering performance for lower memory usage. Preserve the visible
work of mounted panes. Do not
condition optimizations on a pane being hidden or off-screen; returning to a
pane must remain immediate. Forced garbage collection is a diagnostic, not an
active-memory optimization. Do not enable `NetworkServiceInProcess2` as a memory
workaround because it changes the sandbox boundary.

Measurement methods and interpretation belong in
[the performance reference](../.claude/skills/perf-investigation/REFERENCE.md).

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

## Sidebar, threads, drafts

- Changing a draft's project in the thread carries its composer content,
  attachments, captured terminal text and selected settings. The destination
  starts with that project's ordinary workspace defaults. The emptied source
  follows normal draft cleanup, including retaining an existing worktree draft.
  Sidebar navigation and New Thread remain separate actions. Moving to another
  computer clears the source-plan acceptance link; the prompt remains intact
  and the original plan stays unaccepted.

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
  queued messages return to the composer. Only a removal the app did not
  perform (Claude's, or one made outside) puts a warning notice on each
  moved thread, at its own timeline position, saying who removed the
  worktree and that it now runs in Base. Terminals opened in the worktree
  close, and one `worktree:removed` event moves draft placeholders on it to
  Base on every client. A project whose root is itself gone is left alone;
  there is nothing to reattach to.
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
  a restart like the layout; take-control and side chats last the session.
  Nothing is shared across clients. The browser pane follows its backend
  state instead (`frontend/src/lib/stores/companionStash.ts`).
- Thread groups, pins, and auto-pin rulings: `docs/specs/sidebar-thread-groups.md`
  and `internal/store/AGENTS.md`. Re-pin-to-bump is deliberately dead.
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
  bounded `command_output` payload.
- Monitor idle-wake: the CLI writes `<task-notification>` to the
  transcript only. A transcript-tail backfill was proposed and declined.
- A Claude Stop kills every running or parked background agent, from any
  turn, with the shells it owns. Stop and the Stop un-send therefore ask
  for confirmation while such an agent is live, and the backend refuses
  them until confirmed (ruling 2026-09-24). The app asks through one
  dialog at the root (`BackgroundKillConfirmationHost.svelte`) that names
  the agents; declining stops nothing, and the sent message stays either
  way. Agent thread requests, their cancels and workflow takeovers are
  not gated. Wire facts:
  [claude-wire.md §Background task ownership](references/claude-wire.md#background-task-ownership).
- Pre-existing dangling Codex child rows in old fork threads are left inert
  on purpose (`internal/store/AGENTS.md`).

## Providers and accounts

- Model, effort and fast-mode selections trust previously observed capabilities
  and remembered choices. Catalog expiry never blocks a selection. A refresh
  warns only when a probed or live catalog withdraws a part of the selection
  the previous probed or live catalog listed; the shipped list is a placeholder
  and never warns. Warnings are advisory toasts; keep the selection and allow
  sending. Provider rejections remain timeline errors without a duplicate toast.
  The Claude catalog learned by an account probe is persisted per account and
  seeded at boot while the binary is unchanged (`internal/claudemodels/AGENTS.md`).

- AO never calls Codex `thread/queue/add`; a mid-turn send is `turn/steer`
  (`internal/provider/codex/AGENTS.md`).
- Rollback refuses on an unpurgeable Codex queue.
- Cross-session OFF always writes `crossSessionInbound:"refuse"`, overruling
  the user's own `~/.claude/settings.json` accept. Per-thread gating for
  full-access threads is not built (global only).
- Sharing `~/.claude` between AO, terminal `claude`, and Claude Code is the
  designed mode; never "fix" it with per-account `CLAUDE_CONFIG_DIR`
  (`internal/provideraccounts/AGENTS.md` has the case table).
- A model that exists only as probe enrichment (`claude-fable-5-1`) is not
  added to the hand catalog; the point of enrichment is that a new model
  needs no release (see `internal/claudemodels/AGENTS.md`).
- Model inputs are full model IDs. AO keeps no alias table: a Claude alias
  (`opus`, `sonnet`) is refused as input with the offered IDs, and a Codex
  slug is passed as written for Codex to judge. An alias the CLI reports back
  is read only through its own model list (`internal/provider/AGENTS.md`).
- Claude 2.1.257 `rate_limit_info.unifiedWindows`: not parsed yet by
  ruling (revisit once the shape is stable; supporting it adds a visible
  overage row, its own decision).
- Binary upgrades under a running app: per-thread restart button, never an
  auto-recycle of sessions; no version ranking or auto-default.
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
- Cursor as a provider: `docs/specs/cursor-provider.md`.

## Workflows

Decisions D1..D73 are in `docs/specs/workflows-system-decisions.md`. Rulings
and anti-changes that live only here:

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
- PR comments live in the overview: Description (collapsed by default)
  and Conversation (also collapsed by default, and capped short enough
  that the diff stays visible under it), two collapsible sections that head
  the diff list as its first row and scroll off with the diff; once off
  screen the title bar offers peek buttons back to each. No Comments tab
  or separate list (ruling 2026-10-08, superseding the fixed header and
  the rail tab). The Conversation is one chronological feed (newest
  first) of thread cards on a timeline rail, review verdicts and commit
  pushes, mirroring the forge's overview; a card's edge and chip carry
  its state (warning unresolved, success resolved, dashed outdated) and
  the thread's row on the diff is the same card. A top-level comment is
  NEVER truncated or clamped; only settled threads' replies may fold.
  Reading is protected from updates: ordering freezes from the
  section's first render, arrivals wait behind an "N new" chip, and a
  remote resolve never moves an open card ("nothing worse than
  GitLab"). Each section fits its content up to a cap and is
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

## Miscellany

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
- Voice dictation: not built; the researched options and their auth
  constraints are in `docs/references/voice-dictation.md`.
- Wide blocks pan inside their own box on every layout: markdown tables,
  inline diff bodies and unwrapped fenced code scroll horizontally only
  when they overflow (the `pan-x` rules in `frontend/src/app.css`), with
  an edge fade as the touch affordance. Nothing is clipped by the pane and
  nothing changes for a block that fits. Fenced code wraps by default; the
  block overlay toggles unwrap per block, recorded by content identity so
  it survives retirement and remounts.
- Rejected from the t3-code survey, do not re-propose: hard steer, codex
  shadow homes, workspace file browser, changed-files card, global word
  wrap (per-block opt-in only), top-edge fade, favicon fetching, settled
  thread lifecycle, `iterations[-1]` context usage, prompt stash, claude.ai
  connectors, app-hosted MCP, classifier-row usage labeling, MCP toggle
  fan-out and per-thread pinning.
- Proposed skills the owner declined: a commit skill, a standalone light
  review skill, a standalone unslop skill, handoff riders, a wait-what
  micro-skill. Artifacts are never offered unprompted.

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
