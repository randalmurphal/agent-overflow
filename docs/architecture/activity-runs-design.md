# Activity Runs: In-Place Scrolling + Collapsible Tool/Think Runs

Status: designed 2026-07-20, agreed with Randy. Not yet implemented.

## Goal

Bound the vertical space long runs of tool calls and thinking take in
the thread: past a threshold a run scrolls in place inside a
height-capped window with the same spring physics as the main pane,
and any run can collapse to a one-line per-tool count.

## Motivation

- An upstream Anthropic server bug frequently emits Fable prose as
  `thinking` items, so threads degenerate into long tool/think runs
  with sparse real prose. Reading the conversation means scrolling
  through screens of activity spam. This feature is a
  presentation-side mitigation, not a fix for the upstream bug.
- During tool spam the main pane churns continuously; the last prose
  block flies off screen while nothing the user needs to read is
  arriving. Capping the live run freezes the main pane: prose stays
  put, activity streams in place beneath it.
- Collapsed runs make prose-skimming possible: a tool-heavy thread
  reads as prose plus one-line count chips.

## Approach

A new final projection pass wraps every maximal sequence of
consecutive rail-kind nodes into an `activity_run` group node. Past a
row-count threshold the run renders inside a max-height clip element
driven by its own `createUseStickToBottomController` instance — the
identical spring, fusion floor, and sub-pixel glide compositing as the
main pane. A tail-window mounts only the newest K rows so DOM stays
O(K) regardless of run length. Clicking the rail collapses the run to
a count chip; an arrow on the chip expands it back. Two user settings
control the default state and K.

## Success Criteria

- [ ] While a windowed run streams, the outer pane's `scrollTop` does
      not move: the last prose block is stationary while items chase
      inside the clip with the main-pane spring feel.
- [ ] Mounted DOM inside a run never exceeds K + one chunk of rows,
      live or historical, regardless of run length.
- [ ] Rail click collapses any run to a chip whose per-tool counts are
      correct (completion pairing, read-group members, subagent
      launches); chip click/arrow expands it back.
- [ ] Collapse overrides and inner scroll state survive virtualizer
      remount, lazy older-paging backfill, and live-window pruning.
- [ ] Wheel-up inside any nested scroller (including existing
      `CommandOutput` / `SubagentGroup` boxes) no longer escapes the
      outer bottom-follow while the nested box consumes the delta; at
      the nested boundary the wheel chains to the outer pane.
- [ ] Expanding an item inside a windowed run grows the row by the
      expanded body's height — no scroll-within-scroll to read it.
- [ ] `make go-build`, `make go-test`, `pnpm run check`, `pnpm test`,
      and `pnpm run build` pass.

## Design

### Membership and projection

Membership is exactly the existing rail predicate
(`timelineRowProjection.svelte.ts`): leaf kinds `tool_call`,
`tool_completion`, `thinking` plus group kinds `group` (subagent),
`wait_group`, `read_group`. Rail continuity and run continuity are the
same property — that is what makes the rail coherent as the run's
collapse control. Prose, user messages, errors, notifications, and
every other non-rail kind break runs.

`groupActivityRuns` (new pure pass in `utils/activityRunGrouping.ts`)
runs **last** in the projection pipeline, after `sliceRevealedNodes`.
Placing it after the reveal gate keeps the gate untouched: items
reveal one by one and the run node is simply rebuilt each projection
pass (projection is derived and cheap). It wraps each maximal rail
sequence into:

```ts
type ActivityRunNode = {
  kind: 'activity_run'
  key: string            // current first member's item id
  children: TimelineNode[]
  counts: RunCounts      // per-tool aggregation, derived
}
```

**Every** run gets a node regardless of length. The threshold gates
*windowing* only, never *existence* — even a 3-item run needs a place
to hold collapse state and counts, because the rail is a control on
all runs.

### Render states

A run renders in one of three states, decided by
`(collapsed, childRowCount >= WINDOW_THRESHOLD_ROWS)`:

- **Flat** — expanded, below threshold. Children render exactly as
  today; the node contributes only the rail element and its hit
  strip. Zero new chrome.
- **Windowed** — expanded, at/above threshold. Children render inside
  the capped clip described below. No visible box: the rail continues
  down the clip's left edge; it is a window onto the run, not a card.
- **Chip** — collapsed, any length. One line at rail indent: an arrow
  (`▸`) plus per-tool counts. Clicking anywhere on the line expands
  back to flat or windowed per the threshold.

The threshold counts **child rows** (a `read_group` or subagent group
is one row), since rows are the height proxy. We never measure a
run's natural height — tail-windowing means the full run is never
mounted.

### Counts

`RunCounts` aggregates by tool display name, rendered
comma-separated, count-descending, thinking last:
`14 Bash, 6 Read, 3 Edit, 9 thinking`.

- `tool_call` counts under its tool's display name;
  `tool_completion` pairs with its call and is not counted separately
  (orphan completions count under their tool).
- A `read_group` contributes its member count to Read.
- A subagent group counts as one launch under its tool name; its
  nested children are not counted (they are inside the group).
- A `wait_group` counts its carrier tool once.
- `thinking` items count as thinking.

A collapsed live run's counts tick as items stream in — counts are a
cheap `$derived` over children already in pane memory.

### Run identity and state migration

Runs are not stored entities; they are recomputed every projection
pass from whichever items are loaded in the pane's window. Per-run
state (collapse override, inner scroll, mounted-row count) is keyed
by the run's **current first member id**, and that key is unstable at
the window edge: lazy older-paging can extend a run backward (new
first member), and live-window pruning can trim it.

There is no stable key available — the last member changes on every
streamed item, and "the prose item before the run" may itself be
outside the loaded window. So the registry migrates by membership:
when the projection builds a run, a stored entry whose key matches
**any current member's id** applies to that run and is re-keyed to
the current first member. One Map lookup per member per pass. Without
this, a user's explicit collapse silently reverts when scrolling up
loads older items into the same run.

### Windowed state: clip and inner controller

The clip element:

- `max-height: min(50vh, 32rem)` (tunable). `max-height`, not
  `height`, so a run that doesn't fill the cap shrinks to content.
- `overflow-y: auto`; `overscroll-behavior` stays **auto** (default).
  Chaining at the inner boundary is *wanted*: wheel-up at the inner
  top must scroll the outer pane. (The outer scroller keeps its own
  `overscroll-behavior-y: contain` against the document.) Correctness
  of outer intent under nested wheel is handled by attribution, below
  — not by blocking chaining.
- Inside it, a content wrapper in plain flow layout carries the
  mounted rows and the inner controller's sub-pixel glide
  `translateY` (with `will-change: transform`, mirroring the main
  pane's contentEl contract).

Each windowed run instantiates its own
`createUseStickToBottomController` attached to (clip, content
wrapper) — the same factory, spring constants, fusion floor, and
glide compositing as the main pane. Like `ChannelView`, it leaves
`externalContentGeometry` unset and uses the controller's own
contentEl ResizeObserver. Its `animationMode` wires to the same
live-content latch (`pane.lastLiveContentAt`), so a streaming run
chases with identical feel. It never registers on
`pane.scrollController` — that slot stays the timeline's.

Outer-height stability is the load-bearing property: the row's outer
height changes only on explicit events — growth toward the cap while
the run is short, item expansion, collapse toggles — never from inner
streaming. While the live run is at its cap, the outer engine sees a
fixed-height row and the outer chase goes quiet.

Lifecycle: the controller is created on row mount and destroyed on
unmount. On unmount (virtualizer buffer eviction) the inner scroll
offset, sticky/escaped state, and mounted-row count snapshot into the
per-pane run registry; on remount they restore. Settled runs rest at
their tail.

### Tail-window mounting

An open run mounts only its newest K rows (K = the window-rows
setting, default 30, clamped ≥ 10 — sized to overfill the cap).

- At the top of the mounted window a thin boundary line —
  `· · · 170 earlier` — mounts one more chunk (25 rows) on click or
  on scrolling into it.
- Prepend compensation is manual: WebKit has no `overflow-anchor`,
  so a pre-paint effect adjusts the inner `scrollTop` by the
  scrollHeight delta after the chunk mounts (two reads and a write,
  before paint).
- While live and tail-following, the head trims back to K so a
  long-running turn doesn't accumulate DOM. Never trim while the
  user is scrolled up inside (escaped or non-sticky), and never
  unmount a row whose item-expansion lease is active — trimming an
  expanded row would snap the row height.

Item state is untouched by all of this: the timeline store window,
item expansion leases, and payload LRU behave exactly as today. The
tail-window governs DOM mounting only.

### Item expansion inside a windowed run

Expanding an item grows the row's outer height by the expanded body's
actual rendered height — expanded content never consumes cap budget,
so reading a diff never requires scroll-within-scroll.

Implementation: the run row keeps one ResizeObserver observing only
*expanded* child bodies (usually zero to two). The clip's style
becomes `max-height: calc(min(50vh, 32rem) + Npx)` where N is the
sum of observed heights. The outer virtualizer absorbs the row-height
change through its normal measure/compensate path; the inner
controller's content-geometry handling keeps the tail pinned if
following. Growth happens below the click point, so there are no
inner anchoring surprises.

### Rail control and chip

The run component owns the rail: one continuous border element plus
an invisible hit strip (~16px wide, spanning the run's full height in
flat and windowed states) over the rail gutter. Hover brightens the
rail and shows a pointer cursor; click collapses to the chip. The
strip is a real `<button>` with an aria-label
(`Collapse 23 tool calls`). It sits in the gutter margin, outside the
content flow, so text selection is unaffected.

Because every top-level rail row now lives inside a run node, the
per-row `isRail` border styling in `MessageTimeline`'s wrapper
retires; the rail is applied once per run. Nested groups
(subagent/wait/read) keep their own indented internal rails and all
their existing interactions — their rails sit further right, so
there is no hit-target conflict; only the run-level rail is a
control.

The chip: arrow + counts line, muted, one line, at rail indent.
Chip, flat, and windowed states all inherit the current tool↔text
boundary spacing (`isToolTextBoundary` → `mt-4`), applied at the run
node level, so the thread rhythm is unchanged; the run node becomes
the unit of tool↔text adjacency.

### Wheel-intent attribution (shared fix)

Today the outer intent machine (`utils/scroll/intent.ts`) treats any
upward wheel from any descendant as "escape bottom-follow", even when
a nested scroller consumed the delta and the outer pane never moved —
a latent bug already visible with `CommandOutput` and `SubagentGroup`
boxes.

Replacement: a shared attribution helper
(`utils/scroll/wheelAttribution.ts`). Nested scrollable containers
register themselves via a small Svelte action (`nestedScroll`)
applied to the run clip **and** the existing overflow boxes
(`CommandOutput`, `ToolResultCard`, `ExpandablePayloadBody`,
`AgentRow`, `SubagentGroup`, `WaitGroup`, etc.). On a wheel event the
helper walks target → boundary checking only registered elements
(keeping geometry reads to the one or two marked ancestors — wheel
handling must not force wide reflows mid-stream) and attributes the
event to the nearest registered scroller that can consume the delta
in that direction (`scrollTop > 0` for up;
`scrollTop + clientHeight < scrollHeight` for down).

An intent machine ignores wheel events attributed below its own
scroll element — no escape, no down-intent. At the nested boundary
the event attributes outward and native chaining scrolls the outer
pane, whose intent machine then reacts normally. Inner controllers'
intent machines use the same helper with their clip as boundary, so
a `CommandOutput` inside a run attributes correctly against both
levels.

### Settings

Two entries in the Go settings store alongside other user prefs
(names per existing settings-field conventions):

- **Activity run default** — `expanded` | `collapsed`; default
  `expanded` (preserves current visibility semantics). Applies to
  every run without a manual override, including the live run: with
  `collapsed`, a streaming run appears as a chip with counts ticking
  until opened. No live special case.
- **Activity run window rows** — K; default 30, clamped ≥ 10.
  Read at mount/trim time; changing it applies on the next
  mount/trim, no re-layout storm.

### Persistence

Per-run overrides and inner scroll state live in a per-pane registry
(new `threadActivityRuns.svelte.ts` sub-factory on `ThreadPane`),
session-only — deliberately matching item expansion leases, which
also don't survive restart. The durable layer is the setting. If
session-only chafes, promoting overrides into per-client `ui_state`
later is additive.

### Size priors and signatures

Run rows join the signature machinery
(`timelineStructureSignature.ts`) with deterministic heights:

- Chip: constant one-line height; stable signature regardless of
  counts.
- Windowed: the cap. The `50vh` term varies with window height;
  priors are estimates and tolerate that error. Run state and inner
  item expansion fold into the entry-level `expansionSig` so stale
  entries drop rather than mislead.
- Flat: composes children's signatures as today.

Windowed rows are better for priors than what they replace: one
stable height instead of N estimated ones.

## Key Decisions

- **Runs exist at every length; threshold gates windowing only** —
  the rail is a collapse control on all runs, so even short runs
  need a node for state and counts.
- **Grouping runs after the reveal gate** — the streaming smoother
  stays untouched; runs are rebuilt per pass from revealed nodes.
- **Same physics via a second controller instance, not a
  reimplementation** — the factory is element-agnostic
  (`ChannelView` precedent); identical feel comes free and stays in
  sync with future spring tuning.
- **Tail-window (option B) over mount-all and over a nested
  virtualizer** — mount-all regresses DOM versus today's flat
  windowing (a 200-call run ≈ 4–8k nodes in one row); a nested
  virtualizer is real complexity for fidelity the tail-window
  already delivers. O(K) DOM, constant expand cost.
- **Membership-keyed state migration** — no stable run key exists at
  the window edge; matching stored state by member id is the only
  rule that survives backfill and pruning.
- **Wheel attribution instead of `stopPropagation` or
  `overscroll: contain`** — attribution fixes the outer intent
  machine for *all* nested scrollers (including today's latent bug)
  while keeping native chaining at boundaries, which the
  browse-out-of-the-run UX depends on.
- **Registered-scroller walk over generic computed-style probing** —
  wheel handling runs while layout is dirty mid-stream; geometry
  reads stay confined to explicitly marked elements.
- **Uniform live behavior** — the default-state setting applies to
  the live run too; no "live always starts windowed" special case.
- **Session-only overrides** — matches item expansion leases; the
  setting is the durable layer.
- **`max-height`, not `height`, on the clip** — short-but-past-
  threshold runs and small K values shrink to content instead of
  leaving dead space.

## Non-Goals

- Not a fix for the upstream prose-as-thinking bug; mitigation only.
- No nested virtualizer inside runs.
- No changes to subagent/wait/read group internals, their rails, or
  their interactions.
- No durable (restart-surviving) per-run overrides in v1.
- No global "collapse all runs" affordance beyond the default
  setting.
- No changes to the timeline store window, item loading, or payload
  LRU — this is projection + mounting only.

## Constraints

- A windowed run row's outer height changes only on explicit events
  (growth to cap, item expansion, collapse toggle) — never from
  inner streaming. This is what keeps the outer engine quiet.
- Inner controllers never register on `pane.scrollController`.
- All per-run state survives virtualizer remount via the per-pane
  registry (row-local DOM state is lost outside the ~1800px buffer).
- WebKit has no `overflow-anchor`: every prepend compensates inner
  `scrollTop` manually, pre-paint.
- Accepted tradeoff (extends the existing virtualization one):
  content inside chips and beyond the tail-window is not in the DOM,
  so find-in-page and selection don't reach it.
- Svelte runes only; component files stay near the 300-line guide
  (`ActivityRun.svelte` + `ActivityRunChip.svelte` split).

## Migration/Removal

| Old Code | New Code | Action |
|----------|----------|--------|
| Per-row `isRail` border styling in `MessageTimeline`'s wrapper | Run-level rail element + hit strip in `ActivityRun.svelte` | MIGRATE |
| `intent.ts` any-descendant wheel escape (`targetIsInsideScrollEl` as sole gate) | Shared `wheelAttribution.ts` + `nestedScroll` action registry | MIGRATE |
| `RAIL_LEAF_KINDS` / `RAIL_GROUP_KINDS` / `timelineNodeHasRail` | Repurposed as the run-membership predicate | KEEP |
| Existing nested overflow boxes (`CommandOutput`, `SubagentGroup`, …) | Same boxes, marked with the `nestedScroll` action | MIGRATE |
| Tool↔text boundary spacing (`isToolTextBoundary` + `mt-4`) | Same rule applied at run-node granularity | MIGRATE |
| `sliceRevealedNodes`, subagent/read/wait grouping passes | Unchanged; `groupActivityRuns` appends after them | KEEP |

## Implementation Map

- `utils/activityRunGrouping.ts` — pure pass: run assembly, counts,
  membership key migration.
- `timelineRowProjection.svelte.ts` — pipeline insertion (final
  pass); rail-kind sets become the membership predicate.
- `components/chat/ActivityRun.svelte` — three states, clip, inner
  controller wiring, tail-window mount/trim, expanded-body observer,
  rail + hit strip. `ActivityRunChip.svelte` — chip line.
- `stores/threadActivityRuns.svelte.ts` — per-pane run registry
  (overrides, inner scroll snapshots, mounted-row counts).
- `utils/scroll/wheelAttribution.ts` + `nestedScroll` action —
  attribution helper; `utils/scroll/intent.ts` consumes it.
- `MessageTimeline.svelte` — `renderNode` dispatch for
  `activity_run`; retire per-row rail styling; boundary spacing at
  run granularity.
- `utils/timelineStructureSignature.ts` — run-node signatures.
- Go settings — two fields (default state, window rows) + bindings
  regeneration.
- `docs/architecture/frontend-scroll.md` — document the new row
  kind, inner-controller pattern, and wheel attribution as part of
  implementation.

## Testing Strategy

- **Projection unit tests** (`activityRunGrouping`): boundary rules
  (prose/user/error break runs), composition with read/subagent/wait
  groups, count aggregation (completion pairing, read-group members,
  subagent launches, orphan completions), threshold row counting,
  and key migration — backfill extends a run backward, pruning trims
  it, state carries over both ways.
- **Registry tests**: override + inner scroll snapshot/restore
  across simulated remount; setting-default vs override precedence.
- **Intent tests** (`intent.ts` / `wheelAttribution`): wheel-up
  consumed by a registered nested scroller does not escape outer
  follow; at the nested top it attributes outward and escapes;
  down-intent symmetric; unregistered descendants unchanged.
- **Component tests** (`ActivityRun`): three render states and
  transitions, chip counts line, rail click + chip expand,
  tail-window mount/trim with prepend `scrollTop` compensation
  (fake sizes), trim exemptions (escaped user, expanded rows),
  expanded-body growth adjusting clip max-height.
- **Controller**: existing suite covers multi-instance behavior; add
  a case pinning that an inner instance's writes never touch the
  outer element.
- **Feel tuning**: cap, threshold, and chunk size validated manually
  on real streams (165Hz setup) as with prior scroll work.

## Tunables (initial values)

| Constant | Initial | Notes |
|----------|---------|-------|
| `WINDOW_THRESHOLD_ROWS` | 10 | rows, not items; below → flat |
| Clip cap | `min(50vh, 32rem)` | via `max-height` |
| Window rows K | 30 (setting) | clamped ≥ 10 |
| Older-chunk size | 25 | mounted per boundary hit |
