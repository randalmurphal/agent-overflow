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
- Draft worktree and branch operations are DISK state, not thread state:
  project-scoped RPCs, bound to the thread at send or creation. Accepted
  consequences: an abandoned draft's worktree stays in pickers; a restart
  loses unbound setup runs and staged intent.
- Thread groups, pins, and auto-pin rulings: `docs/specs/sidebar-thread-groups.md`
  and `internal/store/AGENTS.md`. Re-pin-to-bump is deliberately dead.
- Thread content search matches title and workspace path only. Searching
  message text server-side (t3-code does) is undecided, not rejected.

## Subagents and background work

- Rulings: `docs/specs/agent-visibility.md`. In short: the launch row is
  unchanged except the open-pane door; every detached execution gets one card
  at its completion; the card body is an allowlist; approvals show only in
  the composer, never as a card or tray pill.
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
- Monitor idle-wake: the CLI writes `<task-notification>` to the
  transcript only. A transcript-tail backfill was proposed and declined.
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
- PR comments live in the header's collapsible Conversation section
  beside Description, not a separate view or tab. The section is one
  chronological feed (newest first) of thread cards, review verdicts and
  commit pushes — mirror the forge's proven overview presentation, no
  invented triage layouts (ruling 2026-09-04, superseding the
  unresolved-first ordering). A top-level comment is NEVER truncated or
  clamped; only settled threads' replies may fold. Reading is protected
  from updates: ordering freezes while the section is open, arrivals
  wait behind an "N new" chip, and a remote resolve never moves an open
  card ("nothing worse than GitLab"). Both header sections are
  user-resizable (bottom drag handle, remembered height). Mechanism:
  `frontend/src/lib/components/review/AGENTS.md`.

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
