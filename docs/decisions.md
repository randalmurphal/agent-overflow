# Product rulings and rejected proposals

Decisions the code cannot tell you: what the owner ruled, what was built
and then torn out, and what must not be proposed again. Mechanisms belong
in the nearest `AGENTS.md`; this file holds only the ruling and the reason.
Add a line when a decision is made that a future change would otherwise
re-open; never add status, dates of verification, or numbers.

Where a spec already records its rulings (the `docs/specs/*.md` Decisions
and Non-goals sections, `workflows-system-decisions.md`), read the spec;
this file only points there.

## Working style (owner rulings that shape every change)

- Clear improvement with no negatives: proceed without asking. Any
  user-visible trade-off or product call: hold and ask.
- Restart of a provider session is a last resort. Never route a config
  change to the restart path when a live retry exists
  (`internal/provider/claude/AGENTS.md`).
- Existing behavior is usually a recorded decision. Before "fixing" a
  guard or permission as a hole, `git log -S` the symbol and look for
  `*Allowed*` / `*Rejects*` test names pinning it.
- Codex second-opinion reviews are for large changes only (feature waves,
  subsystem reworks, wide refactors); routine fix waves get Claude-owned
  review.

## Performance and memory

Rulings and the closed-cause list live in
`.claude/skills/perf-investigation/REFERENCE.md`. The standing ones:
never trade performance for memory; any optimization conditioned on a
pane or element being off-view, hidden, or scrolled out is banned (rejected
repeatedly, the common case is bouncing between panes); forced GC is not a
fix; `NetworkServiceInProcess2` rejected.

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
- History the reader explicitly loaded (Load older) is never taken back by
  an automatic prune; memory is the cheaper cost.

## Sidebar, threads, drafts

- Multi-computer ownership and UI follow [Connected computers](specs/connected-computers.md): frontend-local preferences, selectable host configuration, portable conversations, optional peer tools, and machine names in the existing metadata row. No additional sidebar attention feed or artifact dashboard.

- Message nav rail: one position claim at all times (one current tick, the
  dot only when no user message is visible, and the dot does not track the
  fisheye); ticks never compress (8px), overflow is a clipped sliding
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
  unchanged except the open-pane door; every detached launch gets one card
  at its completion; the card body is an allowlist; approvals show only in
  the composer, never as a card or tray pill.
- A Codex child's answer is a normal message; never a special final-answer
  block. Its token figure is the child's cumulative spend
  (`childAgentTokenSpend`); Claude's `task_progress` total is latest input
  plus cumulative output by the CLI's own construction, so the two agree
  until a compaction.
- A finished background task's launch row stays `running` by design
  (invariant 24); the completion sibling is its terminal. Never read the
  launch flag as a verdict; the completion decides.
- Monitor idle-wake: the CLI writes `<task-notification>` to the
  transcript only. A transcript-tail backfill was proposed and declined.
- Pre-existing dangling Codex child rows in old fork threads are left inert
  on purpose (`internal/store/AGENTS.md`).

## Providers and accounts

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
  needs no release (`internal/claudemodels/AGENTS.md` merge rule 6).
- Claude 2.1.257 `rate_limit_info.unifiedWindows`: not parsed yet by
  ruling (revisit once the shape is stable; supporting it adds a visible
  overage row, its own decision).
- Binary upgrades under a running app: per-thread restart button, never an
  auto-recycle of sessions; no version ranking or auto-default.
- Session import (`internal/sessionimport/AGENTS.md`): one AO thread per
  Claude leaf; dedup mandatory; non-active branches materialize lazily at
  first send; no "imported" badge; no auto-sync; an imported historic model
  never becomes the composer default.
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
- Every thread/workspace event reaches any client with visibility; channel
  audience is by data class, loopback-only is for host directives only. A
  mutation that persists without emitting is a bug.

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
- Voice dictation: not built; the researched options and their auth
  constraints are in `docs/references/voice-dictation.md`.
- Rejected from the t3-code survey, do not re-propose: hard steer, codex
  shadow homes, workspace file browser, changed-files card, global word
  wrap (per-block opt-in only), top-edge fade, favicon fetching, settled
  thread lifecycle, `iterations[-1]` context usage, prompt stash, claude.ai
  connectors, app-hosted MCP, classifier-row usage labeling, MCP toggle
  fan-out and per-thread pinning.
- Proposed skills the owner declined: a commit skill, a standalone light
  review skill, a standalone unslop skill, handoff riders, a wait-what
  micro-skill. Artifacts are never offered unprompted.
