# Run Map: spec and implementation plan

Replaces the recursive run-detail tree (`WorkflowRunTree.svelte`) with a
vertical **run map**: time flows down, filled is done, one marked "now",
a bare line is not-yet. Ground-up on the data plane where the current shape
can't carry a live view; surgical where existing machinery is already
right (action rows, evidence, sweep, thread links all stay).

Status: **implemented 2026-08-12.** Decisions recorded in §13; the two
standing defaults there hold unless countermanded. The §7 case matrix
is binding: a newly reachable case discovered during implementation
gets a row and a test before it gets code.

---

## 1. The design in one paragraph

A run renders as one vertical line. Completed waves of a self-call
campaign are one solid summary row each. The current wave is expanded in
place: its phases as nodes on the line, a fan-out phase as parallel
branch columns that split and rejoin, composition calls as chains inside
their branch. Everything the frozen definition says will happen but
hasn't yet is pre-rendered as quiet ghost lines below the action, ending
in the loop decision ("issues → wave N+1" / "clean → done") when the
definition tail-self-calls. The reading rule is the whole UI: **position
= progress, filled box = happened, marked node = now, bare line = not
yet.**
A workflow with no self-call is simply a map with one segment and no
loop affordance. That is the base case, not a special case.

**Only the live path is open, and it is open at every depth.** That is
one rule, applied to waves, to composition calls and to fan lanes alike:
anything settled is one compact line, anything not yet reached is one
ghost line, and what is left (the path from the root to where the run
actually is) is expanded and framed. Depth never decides; "is this
where the run IS" does. A campaign of three fan lanes, each a child
workflow with several adjudication laps behind it, is a short flow with
one open branch, not sixty rows. Every fold is one click from its
detail, and the click set is per-visit state, never persisted history.

Non-goals: the map is not a chat surface, not an envelope inspector
(R2), and not a replacement for the home list. Digest, evidence,
outputs, disposition, and the action row are untouched.

## 2. Visual vocabulary (R1-compliant)

The interactive mockup used cyan glow + pulse for "running". The app's
R1 rule (frontend/CLAUDE.md, `utils/workflowRunSignal.ts`) reserves
amber for human-blocked, red for failed, pulse/glow for amber only.
The map adopts R1 and adds two CLARITY HINTS on top (§13 fourth pass,
2026-08-15): the done GLYPH is `text-success` (glyph only, since the
label beside it stays neutral, so amber/red keep their monopoly on label
hues), and the `now ▸` row swaps its fill for a `bg-accent/10` tint.
Hierarchy comes from SURFACE, not border taxonomy: what happened is a
FILLED box, what has not happened is a bare line, so border style stops
being the only signal.

| Map state | Treatment |
|---|---|
| done (phase/unit/wave) | borderless fill `bg-surface-2/50`, glyph `✓` in `text-success` (the glyph, never the label), label `text-fg-muted` per `workflowNodeTone`. Settled work is quiet surface, not border ink |
| running | `border-border-strong` over `bg-surface-2/60`, `text-fg`, glyph = `SteppedSpinner` (the app's standing-spinner primitive). **No pulse, no new hue.** |
| the **now marker** | position + weight, plus an inline `now ▸` tag in `--accent` and a `bg-accent/10` fill on the row, the surface's one deliberate accent (decided: it marks position, not status; R1's hue meanings stay untouched) |
| pending / queued unit | hairline `border-border-subtle` over `bg-surface-1/40`, `text-fg-subtle`, glyph `◌`, meta `queued` |
| ghost (not yet reached) | **no box at all**, just a bare `text-fg-hint` line on the spine (`RUN_MAP_GHOST_ROW`). The future is quiet text; boxing it at full geometry made it weigh what the past weighs |
| parked / needs-human cause | amber chip/border over `bg-warning/10` via `workflowRunSignal` tones + existing `status-glow-warning` ring, the only glow on the surface |
| failed node / failed wave row | `text-error` / `border-error` over `bg-error/10` per `workflowNodeTone` |
| retry attempt | `·N` suffix on the label. That is the whole convention: the row keeps its own status's treatment, because a retried attempt is not a ghost and a second vocabulary for "again" would collide with the unboxed one that means "not yet" |
| soft-stop armed | note on the loop decision node: "stops after this wave" |

Colour decisions stay in `workflowRunSignal.ts` / `workflowNodeTone`.
Extend those, never inline hues in components. Do not use `animate-pulse`
for running: running is the `SteppedSpinner` glyph, and a second
"something is happening" signal would collide with it.

**Node geometry is part of the vocabulary, and lives in the same module
(`utils/workflowRunMapStyle.ts`, geometry section).** The hues say what a
node IS; the geometry is what makes the surface read as a flow rather
than as a list, and a per-component literal is how "the same box" stops
being true.

| Element | Treatment |
|---|---|
| a node on the spine | `RUN_MAP_NODE_BOX`, an INTRINSIC-width box, centered in its row, capped at the card edge with its label WRAPPING inside it. **CSS ellipsis is banned on this surface**: a map whose every node reads `Implement …` says nothing, so labels wrap and `RUN_MAP_LABEL_MAX` (96, applied with `truncateMiddle` in each label's renderer) is the runaway guard for a label that is effectively a paragraph, not a line budget. Never a full-width bar: a column of edge-to-edge bars reads as a list whatever the glyphs say. The box is `inline-block` TEXT FLOW, and a row's glyph + label + meta live inside ONE button as inline content: atomic siblings beside a label wrap as units when space runs out, stranding a lone glyph on the first line (§13 fourth pass) |
| a ghost row | `RUN_MAP_GHOST_ROW`: same intrinsic-width text flow, no border, no fill, one type step down. A ghost is a line, not a box (§2 table above) |
| the map's column | the overlay card's full width, with no wrapper and no cap. A 34rem column existed once for the look of the single-file mockup, and a real campaign paid for it twice (§6, width) |
| lane geometry | `RUN_MAP_LANE_MIN` / `RUN_MAP_LANE_MAX` (15rem / 26rem), an open lane's width band, plus `RUN_MAP_FOLDED_LABEL_MAX` (40), the folded lane title's hard budget, the one deliberately single-line text on the map. `app.css` re-declares the band on `:root` as the `var()` resolution floor; a test pins the two sources together |
| a frame around live flow | `RUN_MAP_CARD`: the current wave, and a live composition's sub-card. **The surface's one structural emphasis**: the live path is the thing with a box around it, and that contrast is what folds buy |
| a fan lane's name | `RUN_MAP_LANE_HEADER`, small caps above its column |
| connective tissue | pure CSS in `app.css` under `.run-map-*`: the spine between sequential nodes (`.run-map-spine`, whose gap and connector length are ONE custom property so a line can never be drawn across a distance the layout does not have), the fan's `.run-map-fork` / `.run-map-rejoin` bars and per-lane `.run-map-lane` drops, and the loop foot's dashed `.run-map-loop-fork`. **All of it is pseudo-elements**, so every box stays an ordinary block-level descendant of the scroller's row flow, which is what keeps §9.7's anchor descent able to find it. No SVG, no measurement JS, nothing absolutely positioned out of flow |

## 3. Wave semantics: what flattens, what nests

**Flatten only tail self-calls.** A run's definition tail-self-calls iff
its snapshot's last phase `IsCall()` and `CallTarget() == workflow.ID`
(`def/calls.go:21,26`). The chain root → child → grandchild along that
edge is the **wave chain**; wave ordinal = position in the chain
(equals `callDepth` relative to the chain root). Everything else
(non-tail self-calls, calls to other workflows, call-bound units)
renders as **composition**: a chain inside its parent's node/branch,
recursively.

Per wave segment:

- Phases render in **frozen-snapshot declared order** (not
  `started_at` order, because that ordering is what makes the current
  SQL counter lie). Attempts of one phase render as sequential nodes
  (`audit`, `fix`, `audit ·2`). `superseded` is a dead status: ignore it.
- The terminal tail-self-call phase does **not** render as a phase
  node; it renders as the **loop decision affordance** at the segment
  foot: two outcome stubs (loop → next wave / done), ghost until the
  gate resolves. Lap counter reads `lap N of ≤M` where M is the call
  edge's `maxDepth`; when `maxDepth < 1`, show `lap N` plus the budget
  line (the ceiling in force is then the only bound, so say so, don't
  imply unbounded). The strip states a ceiling whenever one exists,
  which is a superset of that rule, so no flag decides it.
- The strip's loop foot describes the **deepest LIVE wave**, falling
  back to the chain's last wave when nothing is live. The chain is
  level order, so its tail is the deepest, but a lap can hold two
  waves (a retried tail call), and which of those the walk reaches last
  is an accident of the parents' ordering. Taking the tail outright had
  the strip describing a dead-end sibling while the live wave beside it
  was the one the run is in. Pinned by `workflowRunMap.test.ts` "two
  waves at the deepest lap | the foot describes the LIVE one, not the
  tail".
- A fan-out phase's full unit list is persisted `pending` at expansion
  (`engine/units.go:443-457`), so branch columns are **known, real
  records** from the moment the phase starts, so pre-rendering queued
  branches requires no guessing. Before expansion (phase not yet
  reached) the fan-out renders as a single ghost node named from the
  skeleton ("ports — declared by plan").
- Completed waves fold to one summary row: `✓ wave N · duration ·
  <unit count summary> · <audit/gate outcome incl. retry count>`.
  Failed/cancelled waves keep their state colour on the row.
- **"Settled" is HANDED OFF, not `state != running`.** A wave that has
  called the next lap stays `running` in the engine (its call phase is
  open until the whole subtree rests), so folding on run state alone
  left every ancestor lap of a live campaign fully expanded, which is
  the wall this fold exists to prevent. The predicate is
  `waveIsSettled` = not live **or** has tail children, and both the fold
  and the row's glyph read it: `waveSignalOf` renders a handed-off lap
  `done` rather than drawing a live spinner beside the word "Looped".
  Attention still wins, because a lap that parked or failed keeps R1's hue even
  though it handed off, because that is the row a person acts on. Pinned
  by `workflowRunMap.test.ts` "a wave that handed off wears a settled
  glyph, not the engine's spinner" and "attention still wins: a lap that
  parked keeps its hue even though it handed off".
- **Composition collapses by default at EVERY depth, and opens only on
  the frontier path.** A called run off the frontier path renders as one
  summary node (glyph, workflow, duration, subtree counts) that expands
  on click; a called run ON the frontier path renders as a compact
  bordered sub-card holding its flow, with an amber blocker line when it
  is waiting on a person. Inside an open composition the same wave rule
  applies again: its finished laps fold to the same summary rows a
  top-level lap folds to, off the same expansion set. The one exception
  is a composition with a single lap: there is nothing to fold, and
  folding it hid the only content the sub-card exists to show. Pinned by
  `workflowRunMap.test.ts` §3 "composition collapse — only the live path
  is open".
- Chain edge cases: chain root restarted fresh has `callDepth 0` but
  authored wave numbering may continue, so the map shows **chain-local
  ordinals** ("wave 1..N of this run") and never trusts seeds like
  `next-wave-number` (untyped by design, D65).

## 4. Data plane (ground-up part)

### 4.1 Why not patch the current one

Today: whole-`WorkflowGetItem` refetch per event for cached details, one
RPC per child expand, durations frozen at derive time, list-cache
`phase N/M` frozen at overlay open, no `workflow:*` gap recovery, and
the snapshot (needed for ghosts + tailness) never leaves Go. None of
that survives contact with a live map; this is the "fits the
functionality ground up" portion.

### 4.2 New RPC: `WorkflowGetRunMap(rootItemID) → WorkflowRunMapView`

One call returns the **whole wave/composition tree** as metadata:

```
WorkflowRunMapView {
  runs: []RunMapRun          // root + all descendants, parent-linked
}
RunMapRun {
  // identity + linkage (from work_items / WorkItemNode)
  itemId, workflowId, parentItemId, parentPhaseId, parentUnitId,
  parentAttempt, callDepth
  // state
  state, reason, softStop, startedAt, endedAt, autoResumeAt
  // definition skeleton — projection of the FROZEN snapshot, not the
  // raw snapshot (R2: no envelopes/schemas cross the wire)
  skeleton: []SkeletonPhase { id, name, shape,        // single|fan-out|call
                              callTarget,             // "" unless call
                              isCheck, maxDepth }
  skeletonMissing, skeletonError   // absent snapshot vs. one that would
                                   // not decode: NOT the same answer
  tailSelfCall: bool          // last skeleton phase calls own workflow
  // records
  phases: []PhaseAttempt { phaseId, attempt, status, cause,
                           interventionKind,    // "" | take-over | by-hand | discard | pause
                           threadId, startedAt, endedAt }
  units:  []Unit { phaseId, attempt, unitId, unitIndex, kind, provider,
                   status, unitAttempt, threadId, startedAt, endedAt }
  // money (tree-rolled, on the root run only)
  spend:  WorkflowRunSpend         // costUsd = wireCostUsd + estimatedCostUsd,
                                   // plus unpricedRows: a total carrying them
                                   // is a LOWER BOUND and says so
  budget: WorkflowAgentRunBudget   // engine.ResolveBudget's own answer — kind,
                                   // ceiling, stand, estimated — so a ceiling
                                   // inherited from the project profile
                                   // (reliability.per_item_budget) is the map's
                                   // number exactly as it is the park's. null
                                   // when there is genuinely no ceiling.
}
WorkflowRunMapView.refusal { code, message }   // see below; null normally
```

Server side: TWO round trips, whatever the tree's size: the upward root
resolution, then `store.ReadWorkItemTree`, which runs SIX statements (runs,
attempts, units, auto-resumes, ledger sum, ledger split by model/cost source)
inside ONE read-pool transaction. The single transaction is load-bearing, not
tidiness: under WAL it pins one snapshot, so a run created between two reads
cannot contribute attempt rows belonging to no run the answer carries.
Membership is a recursive CTE over `work_items.parent_item_id`
(`internal/store/work_item_tree.go`), upward to resolve the root from
any member and downward for the run listing, and the phase/unit/auto-resume/
ledger reads narrow themselves with the same CTE
(`WHERE item_id IN (SELECT id FROM tree)`) rather than round-tripping an
id list into a bind array. The runs STREAM through a visitor rather than
materialising: each carries its own frozen snapshot (4MiB cap apiece), so
peak retention is one blob rather than a whole tree's worth. Each run's
snapshot is decoded once for its skeleton and dropped.

Every dimension is bounded, every bound REFUSES rather than truncates, and
each refusal is typed so the app can say which one happened: depth by
`MaxCallDepth` (256), size by `maxWorkflowRunMapMembers` (4096, that depth at
a realistic mean fan), plus a cyclic-linkage refusal that keeps the wire's
"a parent is always seen before its children" promise true by construction.
Payload is metadata-only (no envelopes, no narrative bodies, no diffs),
so a 40-wave campaign is a few hundred small rows: bounded, in line with
core principle 4.

**The refusals are DATA, and they are permanent.** `not-found` (a stale nav
entry or a discarded run), `too-large`, and `corrupt-linkage` come back as
`view.refusal` with the RPC succeeding, because the transport strips a method
error's text for every non-loopback caller (an error return cannot carry a
sentence to a remote client), and because the entity store answers a thrown
error with a retry ladder that would re-ask an unanswerable question forever.
Anything a retry could fix (a store read, a ledger group with an unknown cost
source) stays an error.

`WorkflowGetItem` stays for the root's evidence/digest/actions and
loses its map duties; child-expand fetching (`loadWorkflowDetail`
recursion) and `retainWorkflowDetails` child-walking are deleted, and so
is the detail view's `children` list itself, since call linkage is a tree
fact and the map answers for the whole tree instead of one level of it
(the per-child summary join parsed every child's frozen snapshot to find
a phase ordinal, on every detail fetch).

### 4.3 Event changes (small, backend)

1. `engine.PhaseEvent` gains `occurredAt int64` (event-time ms). A
   `running` transition patches `startedAt`; a terminal one patches
   `endedAt`. Without this the frontend must stamp client time, which
   drifts across reconnects. Emit sites already funnel through
   `emitUnitState` / the phase emit helpers: one field, all sites.
2. Frontend `WorkflowItemStateEvent` type adds `phaseId`/`attempt`.
   The Go payload already carries them (`engine/types.go:571-573`);
   the TS type at `types/workflow.ts:48-54` drops them today.
3. **New-run birth** needs no new event: a child's first
   `workflow:item-state` (`from: ""`) with an unknown `itemId` whose
   tree we watch triggers a map invalidate (wave birth is per-wave
   rare; a refetch is the right cost).
4. No narrative/resource-wait/cost events are added in v1 (default per
   §13: `pending`+provider renders as "queued"; honest wait-state
   events from the engine semaphores are a v2 addition).

### 4.4 Frontend store: entity-keyed, patch-first, refetch-reconciled

New `stores/workflowRunMap.svelte.ts` built on **`createEntityStore`**
(there is something to release: the staleable `WorkflowGetRunMap` source;
doctrine at `entityStore.svelte.ts:1-25`).

Keyed by **the item id the UI asked for**, the nav-stack run id, not by
the tree root it resolves to. The root is resolved server-side (§5.9), so
the frontend cannot know it before the first answer, and keying on it
would mean an attach that has to wait for a fetch to learn its own key.
The ANSWER covers the whole tree whichever member was named, so one entry
serves the wave, the root and every run below either. Two keys naming one
tree (a deep link to a child plus the root) are two entries holding two
copies, the shape `gitStatusStore` already carries for two spellings of
one directory, and it costs one extra RPC only when both surfaces are open
at once.

- `source()` = fetch map view, `apply()` it. Consumers attach from the
  run-detail component keyed on the entity id alone (getter-ctx rule,
  `ChatHeaderActions.svelte:66` shape).
- `eventsWorkflow.ts` routes `workflow:phase-state` /
  `workflow:item-state` / `workflow:soft-stop` into the store module,
  which resolves the event's `itemId` to a watched root (parent-index
  maintained on apply) and **patches the node in place** through
  `store.apply(key, next, { preserveError: true })`, the single write
  chokepoint, so any derived revision logic lives at the write site
  (the WebView2 lesson: bumps are a property of writing).
- **Patches are an optimization, never load-bearing for correctness.**
  Any event the patcher can't place precisely (unknown child, unknown
  phase id, attempt reopen it can't model) ⇒ `invalidate(rootKey)`,
  debounced 200ms. `source()` keeps the last value while refetching, so
  reconciliation never flickers.
- **A refusal ends the event-driven refetch, on every path.** A patch
  that lands while a fetch is in the air is marked, and the apply that
  may have buried it re-asks, unless the answer that landed was a
  refusal, in which case the mark is CONSUMED and nothing is re-asked.
  Every refusal code is permanent (§4.2), so the refetch could only
  produce the same refusal; this was the one path back into the loop the
  item-state path is explicitly guarded against.
- Transition cadence is phase/unit-level (not token-level), and a patch
  is O(tree-clone) at worst, well inside the 100ms occupancy contract
  without needing the quiet-work scheduler. If profiling ever says
  otherwise, the fix is keyed sub-maps, not skipping events.
- **Gap recovery**: add an explicit `workflow:` case to
  `eventsTransportGap.ts` (currently falls to the unknown-channel
  default): `workflowRunMapStore.invalidateAll()` +
  `refreshWorkflowRunsSoon()`. Edge-triggered channel ⇒ a dropped frame
  is terminal; blanket invalidate is the established recovery
  (`eventsTransportGap.ts:121-136` rationale).
- Reconnect: entityStore's `resetAll`/`suspend` handles re-source; the
  current workflows store has no reconnect story at all, and the map
  store must not inherit that.

### 4.5 Time

Durations tick client-side from `startedAt` via the existing shared 1Hz
clock: thread `createSharedNowClock(hasRunningNode).now` into the pure
projection as `nowMs` (its `workflowDuration(started, ended, nowMs)`
signature already accepts it; every current caller defaults it, which is
the bug). Keep `workflowDuration` formatting as-is (deliberately drops
seconds above a minute). Nothing time-related crosses the wire besides
timestamps.

## 5. Pure projection: `utils/workflowRunMap.ts`

`buildRunMap(view, nowMs, options?): RunMapModel` is pure, with no Svelte
imports, and exhaustively table-tested (mirrors the `buildWorkflowRunTree`
posture, which this replaces).

**The model's shape is stated in
[`utils/workflowRunMapTypes.ts`](../../frontend/src/lib/utils/workflowRunMapTypes.ts),
and only there.** This section used to restate it, and a restatement of a
type is a second declaration that cannot be checked: it drifted (a
`FrontierPath` that never existed, no `options` parameter, no refusal, no
spend/budget, no `lapSeq`) while every consumer compiled against the real
one. Read the module; each field carries the rule it encodes.

What is worth stating here, because it is design rather than shape:

- `options` is how LAZINESS enters the one walk: `expandedWaveIds` and
  `expandedCompositionIds` go IN, and `wave.segments` comes OUT as `null`
  for a folded wave nobody opened (§6, vertical scale). A per-wave
  builder made the tree index and the frontier collection per-wave too,
  once a second, off the shared clock.
- `segments === null` IS "this wave is closed". There is no second `open`
  flag anywhere (model field or component prop), because the surface only
  stays honest while two answers agree, and open-with-no-segments renders
  "Nothing recorded in this wave yet." over a wave full of records.
  `runMapWaveIsOpen` is the one spelling of the question.
- Segment nodes are a discriminated union on `kind`
  (`phase` | `fan` | `call` | `decision`), so a component switches and the
  compiler catches the case it forgot; statuses are unions too, never raw
  strings.
- Every display string (durations, labels, metas, the money line, the lap
  label) is precomputed. Rendering is a read, never a derivation.

Projection rules (each a table-test group):

1. **Skeleton ∪ records.** Every skeleton phase yields a node; records
   overlay status/timing; skeleton-only ⇒ ghost. Records whose phase id
   is missing from the skeleton (definition drift on rerun) render
   appended with a neutral "not in current definition" meta, never
   dropped, never crash.
2. **Wave chain extraction** per §3, including: broken chains (child
   failed ⇒ chain ends there, later runs impossible), cancelled
   mid-chain, a chain whose root is itself a child of a non-self call
   (campaign called by another workflow: the outer call renders as
   composition, the inner chain still flattens).
3. **Fan-out branches** from unit records; join unit renders as the
   merge node; `pending`+provider ⇒ "queued"; `dropped`/`taken-over`
   keep their glyphs; unit-bound child runs nest as the branch chain.
4. **Frontier extraction**: leaves with status `running`, or parked
   causes, ordered needs-human first, then deepest most-recent
   transition. Feeds the breadcrumb strip, the blocker chip, and the
   follow target.
5. **Ghost synthesis after the frontier only**: phases before the
   current one that never ran (loop-back re-entry) render as their
   recorded reality, not ghosts; the ordinal question ("how far") is
   answered by position, so no `phase N/M` arithmetic exists anywhere
   in the map.
6. **Ghosts exist only for live runs.** A terminal run (done / failed /
   cancelled) renders no future: ghosts and undecided loop stubs are
   exclusive to `running` / `needs-human` states; nothing further will
   happen, so nothing further is drawn.
7. **Each wave renders against its own run's frozen skeleton**, never
   the root's. A definition refresh between waves is reachable and the
   waves legitimately differ.
8. **Records-only degradation**: an empty or undecodable snapshot
   (pre-migration rows, oversized snapshot) yields an empty skeleton,
   and that wave renders its recorded phases in recorded order with no
   ghosts and no loop affordance. Never a crash, never a blank map.
9. **Root resolution**: `WorkflowGetRunMap` accepts any item id and
   resolves to the tree root server-side, returning `rootItemId`; a
   stale nav-stack entry or deep link pointing at a child normalizes
   instead of erroring.

## 6. Layout resilience: sizes, text, scale

The map must be correct at every reachable size and content shape. These
rules are deterministic: **there is no measurement-driven JS layout on
this surface at all**, and the "fan-tier decision" this section first
imagined does not exist: lanes wrap by flex rules and CSS decides, per
frame, how many ranks the row needs. Nothing measures a column to pick a
tier. Each rule below is testable without a layout engine.

**Width.**

- The map owns the **full card width**, with no wrapping column element
  at all. An earlier pass capped one at 34rem for the look of the
  single-file mockup, and a real campaign paid for the look twice: four
  fan lanes squeezed into half the card while the other half sat empty.
  Long phase names and parallel lanes are what the width is FOR; a
  single-file run centers its nodes inside the same span and loses
  nothing.
- The spine (wave summary rows, phase nodes, the loop foot) is a
  single centered column that **never scrolls horizontally**. Nodes size
  to content and their labels wrap; nothing is fixed-width.
- **NOTHING on the map scrolls sideways**, not the fan either. Branch
  columns have a readable floor and a cap (`RUN_MAP_LANE_MIN` /
  `RUN_MAP_LANE_MAX`, 15rem / 26rem, declared once in
  `workflowRunMapStyle.ts` and set on the fan container as
  `--run-map-lane-min` / `--run-map-lane-max`; the resting width and
  the enter keyframes are renderings of the same numbers, and `app.css`
  declares both on `:root` so a `var()` outside the fan container still
  resolves, and a test pins the two declarations together). When open
  lanes exceed the available width, the lane row **wraps into a second
  rank** (`.run-map-lane-row { flex-wrap: wrap }`). A horizontal
  scrollbar hid whole lanes off the right edge of a real campaign; a
  second rank hides nothing. A second-rank lane's drop line dangles
  from no fork bar; that is decoration degrading, and it beats content
  hiding. The row centers with **`justify-content: safe center`**: a
  rank that still overflows (one rigid folded lane wider than a narrow
  card) falls back to start alignment, so overflow is at worst a
  clipped tail, never an unreachable head bleeding out of both edges.
  **Nothing writes any `scrollLeft`**: §9's chokepoint owns exactly one
  number, the overlay body's `scrollTop` (§9.1).
- **Fans inside a lane render STACKED, never as columns**
  (`RunMapFan.layout`, decided by the model at build time: `stacked`
  once the walk has entered a lane, `columns` anywhere the card's full
  width is available, since a spine sub-card's own fan keeps columns, so
  depth alone is not the key). Columns inside a column can only
  subdivide a width that was already minimal. A nested fan is what put
  a horizontal scrollbar inside a 200px lane. A stacked fan draws no
  fork/rejoin bars and no lane row: each branch is a full-width block
  (header line, then its chain behind an indent guide), and the scalar
  groups (inline done chips, the oversized-done fold, the queued range
  node) and the join are wrapping rows in the same flow.

**Fan scale.** Fan width is engine-capped at `EffectiveMaxFanOutWidth`
(default 32); 32 uniform columns communicate nothing. A lane takes one
of three shapes, and the difference is **what the reader can DO**:

- **Actionable** (`running`, `failed`, `parked`, `taken-over`,
  `unknown`, or anything on the frontier path): an OPEN column carrying
  its chain, with a small-caps unit name above it.
- **Settled with structure under it**, a completed unit that CALLED a
  run: a column too, but COLLAPSED to its header alone (glyph, name,
  duration), with an inline toggle stating how much is behind it. One
  click puts the WHOLE subtree back: a lane whose unit made exactly one
  call **merges** with it (`merged`: the lane toggle is the one fold
  control, so the composition offers no second one), and that
  composition renders **headerless** with the workflow's name composed
  onto the lane's `title` in the model (`PORT-0 · go-port-slice`, shown
  folded and open alike). The alternative was the same duration twice,
  one line apart, with the workflow name truncated in between. The merge
  has guards: an ACTIONABLE lane (failed or taken-over, always open,
  no fold) never merges a settled child, because merging force-opens
  and that subtree would have no collapse anywhere on it; and a FAILED
  child keeps its own header row, because that row carries its red
  glyph and the lane header carries the UNIT's signal, which need not
  agree. A LIVE child merges under any lane, because live content is
  bounded and unpainting a running run is what the frontier rule exists
  to prevent. Sibling children keep their
  own rows: the merge exists because a sole child's header repeated the
  lane's, which is not true of two. Structure is a fact about the unit
  and not about its status: a group node renders a node and nothing
  else, so routing a finished call-bound unit into `done` deleted the
  child run and its whole composition subtree from the map (§7,
  "unit-bound call"). But painting that subtree unconditionally is what
  turned a three-lane campaign into sixty rows, so it is reachable
  rather than always-drawn.
- **Scalar**, nothing under it: arithmetic, and for the done side,
  chips. Queued lanes become ONE node named by the CONTIGUOUS RANGE they
  cover (`ports 2–4 · queued`, falling back to `14 units · queued` when
  a range label would claim lanes the group does not hold), and it is
  **non-interactive by construction**: the model carries no entries for
  it, because a queued lane has no record, no thread and no duration a
  click could reveal. Finished scalar units render **inline as chips, no
  click, up to `RUN_MAP_INLINE_DONE_MAX` (8)**: "what completed" is the
  first thing a reader asks of a finished fan, and a `done ·N` count
  chip made them click for the answer per lap, per composition. Past
  eight the group folds behind its labelled count (forty chips is the
  wall the fold exists to prevent), and it still expands, because
  `dropped` units live among the entries (struck styling) and nothing
  else states them.

A collapsed lane is `flex: none`: EXACTLY its header's content, and it
never gives any of it up. A folded lane has collapsed to one line, and
the unit's name is the only identity that line has left, so the name is
the LAST thing to yield rather than the first. An earlier pass let
folded lanes shrink first on the theory that a finished lane costs the
reader nothing, and what it actually cost them was `✓ POR… 2s` sitting
beside a fully-named open column. The **open columns are what flexes**
(15rem floor, 26rem cap, growing to share the row), and past that the
lane row wraps into a second rank, §6's declared escape for fan width,
and the right one, because a wrapped lane is still a lane the reader
can read; a scrolled-away one is not. An OPEN lane's name wraps in its
column for the same reason the folded one refuses to shrink: the name
is the lane's identity in BOTH states. The folded line takes a HARD
budget instead of the runaway guard (`RUN_MAP_FOLDED_LABEL_MAX`, 40,
middle-truncated with full text in `title`): it is `flex: none` under
the one deliberate `whitespace-nowrap`, so sheer length is the one way
it could still push the row past the card edge, and a folded lane is a
summary by definition. The queued group's range label
wraps too, because it is built from the phase's display name, and a real
phase name is a sentence. Elsewhere label length is bounded by
`truncateMiddle`'s 96-char cap (`RUN_MAP_LABEL_MAX`), applied in each
label's renderer, which is what
keeps "never ellipsizes" from meaning "unbounded".

A finishing branch folds into the done node; a starting queued unit
slides out into a column (§10 motion). The fan states **no tally of its
own**: the wave's summary row already carries the unit count, and a
second one under the same node was a number the reader had to reconcile
with the first. What the projection guarantees instead is the
PARTITION: every non-join unit is drawn or counted exactly once, and
the join is neither. This is information design, not just space: the
interesting subset gets geometry, the bulk gets arithmetic.

**Text.** No text length reachable from the engine may break layout,
and none of it ellipsizes: labels WRAP, because a phase name is the
node's whole meaning:

- Node labels: phase `name` (fallback `id`), wrapping in the node box,
  middle-truncated only at `RUN_MAP_LABEL_MAX` (96, the runaway guard,
  far past any real phase name) with full text in `title`.
- Unit ids are engine-stamped slugs but unbounded in principle, so the
  same rule applies. A FOLDED lane's one-line header is the single deliberate
  `whitespace-nowrap` (its intrinsic width is what makes it a summary
  node); `RUN_MAP_FOLDED_LABEL_MAX` (40) is what bounds it.
- Park causes / cause chips clamp to two lines with inline expand, the
  one deliberate clamp, and it is `line-clamp-2` with an expander, never
  `truncate`. Pinned by `WorkflowRunMap.test.ts` "parks amber, clamps
  the cause to two lines, and expands it inline"; the no-`truncate` half
  by "nothing on the map carries a CSS ellipsis", which sweeps every fan
  shape and the full surface (frontier strip, loop foot, blocker).
- Wave summary rows are one WRAPPING flex row; every part renders in
  full, each part is atomic (`whitespace-nowrap`) so the wrap point
  falls between facts, and only the outcome (the one part that can be
  a sentence) wraps internally. No JS measurement.
- Numerics use `tabular-nums`; durations keep `workflowDuration`'s
  shapes.

**Vertical scale.**

- Waves are one summary row each; a 40-wave campaign is 40 rows plus
  one expanded segment, with no virtualization in v1. The projection builds
  `segments` lazily per expanded wave, so folded history costs O(1) per
  wave and the adversarial ceiling (`MaxCallDepth` 256 chain) stays
  linear and small.
- **The frontier path is always fully expanded regardless of depth**:
  it is a path, so it costs O(depth) single nodes. **Everything off it
  is collapsed, at every depth**, to a summary node with inline expand:
  "no clicks to see what's running" holds, and no definition, however
  wide or however deep, can paint more than one path's worth of detail.
  Depth is no longer a UX lever (the old rule gave the first two levels
  away free, which is exactly the wall a real campaign hits); it is only
  a runaway bound, and `RUN_MAP_COMPOSITION_DEPTH` is deleted rather than
  left steering anything.
- **A composition the reader OPENED answers with content, not another
  fold.** An opened settled multi-lap chain defaults its FINAL lap open.
  The ending is the summary, and "final" is the lap that called no
  successor (a tail LEAF, not the last chain position: a retried tail
  puts a settled dead-end after the lap that carried the run forward,
  and every leaf defaults open). A click on any settled lap INVERTS its
  default, on the same `expandedWaveIds` a top-level lap uses, so the
  final lap can be re-closed and history reopened, one click each. A
  single-lap composition never folds its lap. Same doctrine one level
  down: a lane's sole composition arrives open with the lane click
  (`merged`). These are what killed "I have to click MULTIPLE times to
  even see that there were items in a phase that completed".
- **Collapsed means NOT BUILT**, everywhere, and it is the same
  convention throughout: `RunMapWave.segments === null`,
  `RunMapCompositionNode.waves[].segments === null`, and
  `RunMapBranch.chain === []`. A collapsed subtree costs the projection
  nothing to skip and the DOM nothing to hold, and there is no second
  `open` flag anywhere that could disagree with it.

**Rebuild cost: reviewed and ACCEPTED, recorded so it is not
re-litigated.** Every store write rebuilds the whole model in one bounded
walk: `buildRunMap` is one `buildIndex` plus one frontier collection, over
a tree the RPC refuses past `maxWorkflowRunMapMembers` (4096, §4.2). Three
things make that the right shape rather than a thing to memoise:

- The inputs are DISCRETE and LOW-RATE. A phase or unit transition is the
  event that moves the map, and those arrive at human-visible cadence, not
  per token. There is nothing to coalesce that the store's 200ms
  invalidate debounce does not already coalesce.
- The 1Hz clock, which is the one genuinely periodic input, gates on
  `runMapViewIsLive(view)`, an OPEN SPAN or a live `auto_resume_at`
  anywhere in the tree, not on run state. A tree parked on a human has no
  open span, so a person reading a stationary page rebuilds nothing at
  all. (Gating on `needs-human` rebuilt the whole model once a second for
  hours; that is the bug this clause exists to keep fixed.)
- The alternative is incremental invalidation keyed by run, which means a
  second source of truth about what changed, and the map's whole
  correctness posture (§4.4) is that patches are an optimization and the
  refetch is the truth. Per-key sub-maps stay the answer IF profiling ever
  says otherwise; skipping events never is.

## 7. Reachable case matrix

Every row is a projection test table and, where visual, a component
test. The matrix is binding (see Status note). Rows added after the first
implementation pass name the test that pins them, because a matrix row
with no test behind it is a claim, not a contract.

| Case | Rendering rule |
|---|---|
| single-shape phase, agent driver | plain node; thread link when `threadId` set |
| single-shape phase, tool driver | node with tool glyph; no thread link (none exists) |
| fan-out pre-expansion (static `fanOut` **or** dynamic `over`/`as`) | ONE rule, not two: a count-less ghost named from the skeleton phase, "units — declared by ports". The skeleton carries SHAPE and never a width, so a static list is no more countable here than a dynamic one, and a count "known from the skeleton" was never available to know. Pinned by `workflowRunMap.test.ts` "fan-out pre-expansion \| count-less ghost named from the skeleton phase" and "names a pre-expansion fan by where its units come from, never by a count" |
| fan-out expanded | three lane shapes per §6: open columns for the actionable, folded header-only columns for the settled-with-structure, inline chips or group nodes for the scalar; join unit = merge node. Pinned by `workflowRunMap.test.ts` "fan-out expanded \| columns for actionable branches, group nodes for the rest" and `WorkflowRunMap.test.ts` "gives columns to the actionable branches and arithmetic to the rest" |
| nested fan (any fan inside a lane) | STACKED, never columns (§6): `RunMapFan.layout` is the model's call by lane containment (a spine sub-card's fan keeps columns), and the component draws no fork/rejoin/lane-row for it: each branch is a full-width block in its parent's flow, scalar groups and join in the same flow. Pinned by `workflowRunMap.test.ts` "the top-level fan is columns, and a fan inside a lane is stacked" / "a fan on a spine sub-card keeps columns — only a lane forces stacking" and `WorkflowRunMap.test.ts` "stacks a nested fan instead of nesting columns" |
| lane whose unit made exactly ONE call | the lane MERGES with it (`merged`): the sole composition arrives OPEN with the lane click (the lane toggle is the one fold control) and renders headerless, the workflow name composed onto the lane's model-side `title` (`PORT-0 · go-port-slice`) folded and open alike. Sibling children keep their own rows. Pinned by `workflowRunMap.test.ts` "a sole child composition is headerless and opened by its lane", "a settled sole child still opens with the lane click alone", "sibling child compositions keep their own rows and folds", and `WorkflowRunMap.test.ts` "merges a sole child workflow into its lane header instead of repeating it" and "opens a settled lane's sole composition with the lane click alone" |
| sole child that FAILED, or a settled sole child under an ACTIONABLE lane | NOT merged (§6, merge guards): a failed child keeps the header row its red glyph lives on, and an actionable lane (no fold of its own) merging a settled child would force-paint a subtree with no collapse anywhere on it. Pinned by `workflowRunMap.test.ts` "a failed sole child keeps its own row instead of merging" and "an actionable lane leaves its settled sole child its own fold" |
| small done group (≤ `RUN_MAP_INLINE_DONE_MAX`, 8) | chips IN the flow, no click, because "what completed" is not behind an affordance, with dropped entries struck among them. Queued never inlines: its entries are empty by construction. Pinned by `workflowRunMap.test.ts` "a done group inlines at the bound and folds past it; queued never inlines" and `WorkflowRunMap.test.ts` "renders a small done group inline, dropped entries struck, no affordance" |
| oversized done group (> 8) | folds behind its labelled count and expands on click. Forty chips is the wall the fold exists to prevent. The BUTTON rides the lane row; the chips land beneath it as a full-width block, because chips inside a `flex-none` lane set the lane's intrinsic width to the whole chip-row and dragged the row past the card edge. A group that outgrows the bound mid-run seeds its fold OPEN, so the chips the reader was watching don't vanish behind a closed count. Pinned by `WorkflowRunMap.test.ts` "folds an oversized done group behind its count, and expands it on click", "lands the oversized done chips below the lane row, not inside it" and "seeds the fold open when an inline done group outgrows the bound" |
| unit-bound call | branch chain (composition), recursing per §5, **at every unit status, including terminal ones**, but REACHABLE rather than always-painted once the unit settles. Structure earns the lane (§6): a unit that called a run keeps its lane after it finishes, because the group node it would otherwise fold into renders a node and nothing else and the child run would vanish from the map. What changed is that a settled lane is FOLDED to its header, with the subtree one click away. Painting a finished child workflow's whole history in every lane is what turned a three-lane campaign into sixty rows. Pinned by `workflowRunMap.test.ts` "unit-bound call \| a COMPLETED call-bound unit keeps its lane, folded, and its subtree one click away" and `WorkflowRunMap.test.ts` "folds a settled lane to its header alone, with its subtree one click away" |
| settled lane's header | IS the summary: glyph, lane title in small caps (the sole child's workflow name composed in: `PORT-0 · porter`), duration, and an inline toggle stating how much is behind it (`1 run` / `N runs`). One line, and `chain === []` while folded, because collapsed means not built. **`flex: none`, and its label never CSS-truncates**: one line leaves the title as the lane's only identity, so the open columns flex and the row wraps instead (§6); the title's own bound is `RUN_MAP_FOLDED_LABEL_MAX` (40, middle-truncated, full text in the tooltip), because a rigid line is the one place sheer length can still overflow the row. Pinned by `WorkflowRunMap.test.ts` "heads every lane with its unit name, in the surface's small-caps treatment", "a folded lane never eats its own label — the open columns are what flex" and "budgets a folded lane's title, full text in the tooltip" |
| queued lanes | ONE group node labelled by the contiguous `unitIndex` range and the shared phase name (`ports 2–4 · queued`), falling back to a count when the group is not contiguous (`14 units · queued`). **Non-interactive by construction**: the model carries no entries, because a queued lane has no record, thread or duration a click could reveal. Its label WRAPS rather than clipping, because it is built from the phase's display name, and a real phase name is a sentence. Pinned by `workflowRunMap.test.ts` "queued lanes group into ONE node labelled by their contiguous range" and "a queued group whose lanes are not contiguous falls back to a count it can prove", and `WorkflowRunMap.test.ts` "offers no affordance on the queued group, because it stands for no records" and "the queued group states its whole range rather than ellipsising it" |
| fan unit accounting | the fan states no tally of its own; what holds is the partition: every non-join unit is drawn or counted exactly once across columns / `done.entries` / `queued.count`, and the join is neither. Pinned by `workflowRunMap.test.ts` "every unit lands in exactly one of column / done / queued, joins apart" |
| composition OFF the frontier path | one summary node (glyph, workflow, duration, subtree counts) with a click that opens it, **at every depth**. Depth is not the question; "is this where the run IS" is. Pinned by `workflowRunMap.test.ts` §3 "composition collapse — only the live path is open" ("collapses every composition off the frontier path, starting at the first level", "an expanded composition id opens exactly that row, and no other") and `WorkflowRunMap.test.ts` "collapses a composition off the frontier path to one row, and opens it on click" |
| composition ON the frontier path | a compact bordered sub-card (`RUN_MAP_CARD`) holding the flow, with an amber blocker line when it is waiting on a person, and NO fold affordance, because there is nothing to fold on the path the reader is watching. Pinned by `workflowRunMap.test.ts` "the frontier path is always expanded, and never offers a fold" and `WorkflowRunMap.test.ts` "frames the live composition and states its blocker where the reader is looking" |
| past laps INSIDE an open composition | folded to the same summary rows a top-level lap folds to, off the same expansion set. An open composition shows its live lap's flow, or its FINAL lap's when the chain is settled (the ending is the summary; an opened composition answers with content, not another fold). "Final" is the tail LEAF (the lap that called no successor, every one of them on a retried tail), not the last chain position. A click on a settled lap INVERTS its default: the final lap re-closes, history reopens, one click each. A composition with a SINGLE lap never folds it: there is nothing to fold, and folding it hid the only content the sub-card exists to show. Pinned by `workflowRunMap.test.ts` "an open composition folds its finished laps and opens the one the reader names", "an opened settled composition defaults its final lap open, and a click closes it again" and "the final lap is the tail LEAF, not the last chain position" |
| wave that handed off (called the next lap) | folded, and its row wears a settled glyph rather than the engine's spinner, because the lap's own work is over and what is running is the wave below it. Attention still wins for a lap that parked or failed. Pinned by `workflowRunMap.test.ts` "a wave that handed off wears a settled glyph, not the engine's spinner" and "attention still wins: a lap that parked keeps its hue even though it handed off" |
| call phase, other workflow | composition chain (CallNode) |
| self-call, **not** tail | composition: explicitly never flattened |
| tail self-call | wave chain + loop foot (§3) |
| `maxDepth` absent on tail edge | "lap N" + budget line, no ≤M. Pinned by `workflowRunMap.test.ts` "maxDepth absent on the tail edge \| \"lap N\" plus the budget line, no ≤M" |
| ceiling kind `tokens` | `12.3k of 50.0k tokens` beside the lap, in the app's token shapes. The dollar line cannot speak for it (`$1.25 of 400000` is a comparison that does not exist), so the ceiling gets its own statement rather than being dropped, which is what left a token-bounded campaign rendering "lap 3" and nothing else. Pinned by `workflowRunMap.test.ts` "a ceiling that is not in dollars leaves the summary comparing nothing" |
| ceiling kind `wall_clock` | `4m of 30m`, elapsed against the bound, in `workflowDuration`'s own shapes, so the strip and the node durations read alike. Pinned by `workflowRunMap.test.ts` "a wall-clock ceiling reads as elapsed against the bound, in duration shapes" |
| ceiling kind this build cannot read | nothing, rather than a number compared against a unit it is not in. Pinned by `workflowRunMap.test.ts` "a ceiling whose kind this build cannot read states nothing rather than guessing" |
| `maxDepth` present on tail edge | `lap N of ≤maxDepth+1`, never `≤maxDepth`. `max_depth` bounds EDGE TRAVERSALS (`engine/calls.go#checkCallDepth` refuses the call whose ancestry already holds that many), so a root plus `maxDepth` child waves is legal and the raw bound rendered a perfectly legal final wave as "lap 3 of ≤2". Pinned by `workflowRunMap.test.ts` "maxDepth 2 \| the third wave is legal and the ceiling says so" |
| retried tail call: two waves at one lap | both waves are kept, and the duplicated ordinal is disambiguated as `wave N ·M` (`lapSeq`, 0 when the lap has a single wave), the same `·N` an attempt carries, meaning the same kind of thing. Dropping either would hide a real run; leaving both labelled "wave 2" is two rows the reader cannot tell apart. Pinned by `workflowRunMap.test.ts` "a retried tail call keeps BOTH child runs as waves rather than dropping one" and "…keeps the wave ordinals monotonic" |
| human gate pending | amber "awaiting approval" on that node + cause; resolution stays in the action row |
| check phase (`isCheck`) | normal node rules; checks strip unchanged |
| step-mode checkpoint | it is a run REASON like any other, not a node annotation: `checkpoint` renders as "Stopped at checkpoint" through `workflowRunSignal`, in the frontier strip's blocker chip. There is no per-node checkpoint treatment, and a second vocabulary for one reason is how R1's two hues drift |
| run reasons (all 17) | frontier-strip label via `workflowRunSignal`; notable extras: `budget-exhausted` shows spend/budget, `wiring-error` parks at the offending node with cause |
| `auto_resume_at` set | frontier chip "resumes in X" (shared clock, `formatCountdownSpan` collapse rules) |
| soft-stop armed | loop-foot note "stops after this wave" |
| phase attempt reopened (resume / unit repair) | same node returns to running; attempt sequence renders in place |
| unit `taken-over` | person glyph + triage-thread link |
| unit `dropped` | struck styling inside the done-chip expansion |
| intervention recorded on an attempt | "touched by hand" marker glyph from `interventionKind` |
| run just created, zero attempts | all-ghost segment; follow target = segment top, the WAVE's own key, so no ghost is marked `now ▸` and the controller resolves it to the wave row. "No leaf" is not "no target": the follow default is decided once, at open, off this value, so a null here cost the whole visit its follow for a run opened in the half-second before its first attempt landed. A run parked before it ran anything (setup failure, pre-flight gate) carries its blocker on the same entry. Pinned by `workflowRunMap.test.ts` "run just created, zero attempts \| all-ghost segment, follow target = segment top", "parked before it ran anything \| the segment top carries the blocker", "a first attempt replaces the segment top — the target moves, so follow moves", and `WorkflowRunMap.test.ts` "renders an all-ghost segment for a run with zero attempts" |
| ABSENT snapshot | records-only mode (§5.8), rendered silently. A run that failed before it ever froze a definition is ordinary history, and annotating it would make normal history look like a defect |
| CORRUPT snapshot (`skeletonError`) | records-only mode PLUS a stated notice in the failure hue, outside the wave's fold, because a corrupt wave is usually a terminal one, so news behind a fold is news the reader does not get. The decode failure itself never reaches the surface (R2): what they can act on is that the definition is unreadable. Pinned by `workflowRunMap.test.ts` "projects a corrupt snapshot as its own wave-level signal, apart from mere absence" and `WorkflowRunMap.test.ts` "states a corrupt frozen definition as a failure, not as ordinary history" |
| map REFUSED (`not-found` / `too-large` / `corrupt-linkage`) | a state of its own, never the error state: the RPC SUCCEEDED and the answer is permanent (§4.2), so there is no retry to offer and "nothing to show yet" would promise a later yes. Per-code headline for what it means to this surface, the backend's already-user-shaped sentence beneath it for which run it happened to; an unrecognised code still gets the honest headline rather than a bare sentence. Pinned by `workflowRunMap.test.ts` "carries a refusal through as user-shaped state with no waves to draw" and `WorkflowRunMap.test.ts` "renders the %s refusal as permanent state, not as a failed fetch" |
| spend carrying unpriced ledger rows | the money line says `$4.12 priced · 3 rows unpriced`, never a bare `$4.12`: rows whose model resolves to no rate have their tokens counted and their dollars in nothing, so the total is a LOWER BOUND and the one place that is worded is the projection. With a dollar ceiling in force it reads `$4.12 of $10.00`; with neither, `$4.12 spent`. Pinned by `workflowRunMap.test.ts` "says \"priced\" and names the unpriced rows when the total is a lower bound", "says \"spent\" when nothing is missing…", "a ceiling that is not in dollars leaves the summary comparing nothing" |
| definition drift between waves | per-wave skeleton (§5.7); orphan records appended (§5.1) |
| child run failed mid-chain | chain ends; failed wave row red; no ghost next wave |
| terminal run (done / failed / cancelled) | no ghosts, no undecided loop stubs (§5.6) |
| done awaiting disposition | fully solid map, loop decided "done"; disposition UI unchanged below |
| stale nav entry / deep link to a child id | server root-resolution (§5.9) |
| fan-out at the width cap (32) | group nodes for the scalar bulk, folded headers for settled lanes with structure, and the lane row's wrap for whatever is left (§6). Nothing scrolls sideways. Pinned by `workflowRunMap.test.ts` "fan-out at the width cap (32) \| columns stay bounded, the bulk becomes arithmetic" and `WorkflowRunMap.test.ts` "wraps the lane row rather than scrolling it — nothing goes sideways" |
| parallel parked leaves | all amber; follow priority per §13 |
| view-only (remote) session | map renders fully; follow chip active; mutating affordances elsewhere already disabled |
| reduced motion / low power | instant placement, no glides, no fold animation |
| fold whose region is off-screen | applies instantly, animation gate off (§9.8). Decided at the moment `open` flips, from one rect read in an `$effect.pre`, before the DOM update, which is the only frame that can answer "is this region on screen" about the layout the fold is about to change. Pinned by `runMapGeometry.test.ts` "foldAnimates" and `WorkflowRunMap.test.ts` "animates a fold in view and applies an off-screen one instantly" |
| light theme | token-driven; no component branches |
| transport gap / reconnect | `invalidateAll` re-source; wholesale apply wrapped in anchor-hold when not following, so recovery never moves the viewport |
| window / overlay resize | rate-bound recomputation (§9.12): re-track the frontier while following, write nothing while not. The reflow itself is CSS's, and nothing measures a column to pick a layout |

## 8. Components

All under `frontend/src/lib/components/workflows/`. The ≤300-line rule is
the repo's `.svelte` rule (frontend/AGENTS.md, "Do NOT stretch a
`.svelte` file past roughly 300 lines when a clear component split
exists") and every component below holds to it; the two plain `.ts`
modules are sized by their own cohesion, and the split between them is
stated where it lives.

| File | Role |
|---|---|
| `WorkflowRunMap.svelte` | orchestration: attach store, shared clock, refusal state, wave list, follow chip |
| `WorkflowRunMapFrontierStrip.svelte` | the strip above the spine: breadcrumb, blocker chip, resume countdown, lap + money |
| `WorkflowRunMapWave.svelte` | one wave: the corrupt-definition notice, its summary row, and, when open, the card that frames its flow. The row lives INSIDE the card, because a card that wrapped the body but not the header would put the wave's name outside its own frame |
| `WorkflowRunMapFan.svelte` | both fan layouts (§6): `columns` (fork bar, wrapping `safe center` lane row, rejoin bar) and `stacked` for every fan inside a lane; inline done chips, the oversized-done fold (button in the row, chips below it), the queued group node |
| `WorkflowRunMapLaneHeader.svelte` | a fan lane's header line in both layouts: glyph/spinner, the model-composed lane `title` (folded: rigid, `RUN_MAP_FOLDED_LABEL_MAX`; open: wrapping, `RUN_MAP_LABEL_MAX`), duration, and the `N runs` disclosure. It is also the WHOLE render of a folded lane |
| `WorkflowRunMapNode.svelte` | single node: glyph, name, duration, meta, thread-open click. Composition chains and lane chains both recurse through it, passed DOWN as snippets so the node ↔ composition relationship stays one import instead of a cycle |
| `WorkflowRunMapUnitChip.svelte` | one fan unit drawn as a node: the join, and every entry in the done group's expansion. A lane HEADER is deliberately not this: a header is a borderless summary line on top of a column, a chip is a box that stands for the unit itself |
| `WorkflowRunMapComposition.svelte` | a called run inside its caller: one summary node when collapsed, the framed sub-card with its blocker line and its folded laps when open, and the HEADERLESS shape for a lane's sole child: no header row, no card frame (the lane is the frame), blocker and waves directly |
| `WorkflowRunMapLoopFoot.svelte` | the loop decision at a wave's foot: lap label, soft-stop note, and the dashed fork into the two outcome stubs (or the single decided one) |
| `WorkflowRunMapSummaryRow.svelte` | ONE folded row renderer, shared by top-level waves and a composition's laps, because a lap is a lap and two renderers for it drift |
| `WorkflowRunMapFold.svelte` | the `grid-template-rows` 0fr⇄1fr reveal both folds use, plus the §9.8 in-view decision that turns the transition off for a region the reader cannot see (§9.8, §10) |
| `runMapFollow.svelte.ts` | scroll/follow controller (§9): engagement, glide, escape, chip, resize cadence, the one write chokepoint |
| `runMapGeometry.ts` + `.test.ts` | the pure rect arithmetic the controller decides on: band, resting line, off-screen, and the anchor descent, which is the one non-obvious rule here and gets direct tests rather than being inferred from a compensation number |
| `overlayScroller.ts` | the §9.9 scroller handoff: the overlay frame provides, the map requires |
| `utils/workflowRunMap.ts` + `.test.ts` | pure projection |
| `stores/workflowRunMap.svelte.ts` | entity store + event patching |

`WorkflowRunDetail.svelte` swaps `WorkflowRunTree` for `WorkflowRunMap`
in place; header/digest/evidence/outputs/action-row order unchanged.
The frontier strip (breadcrumb + amber blocker chip + lap/budget) sits
at the top of the map, not in the header. The header keeps its
existing role. Test ids: `workflow-run-map`, `workflow-map-wave`,
`workflow-map-wave-card`, `workflow-map-node`, `workflow-map-branch`,
`workflow-map-lane-name`, `workflow-map-lane-toggle`,
`workflow-map-composition`, `workflow-map-summary`,
`workflow-map-decision`, `workflow-map-follow`, replacing the
`workflow-run-tree` family.

Interaction:

- Any **node with a thread** opens it via `openWorkflowThreadById`
  (closes the overlay, R3, unchanged).
- A **folded wave row** expands inline (model already built; pure
  render). Expansion state lives in the overlay nav store
  (`workflowsOverlay.svelte.ts`) keyed by run id so it survives
  detail remounts. The current tree loses expansion on remount;
  don't replicate that. There are THREE sets per run (waves,
  compositions, lanes), because the three things a reader can open are
  keyed differently (`waveItemId`, the called run's `itemId`, the
  branch key) and one merged set would let a lane's key open a wave.
  They go in and out of `buildRunMap` as `expandedWaveIds` /
  `expandedCompositionIds` / `expandedLaneIds`; every toggle is wrapped
  in the follow controller's `holdAnchor`, because opening a fold above
  the reader changes the document height under them (§9.7).
- A **folded lane** and a **collapsed composition** expand the same way
  and cost the same nothing while shut: the projection does not build
  what nobody opened, so a click is the first time a subtree exists.
- Every mutating affordance asks for the capability it needs —
  `hasScope('threads:autonomy')` for the workflow controls
  (`frontend/src/lib/transport/scopes.ts`, §10 remote posture). The map
  itself is read-only, so this touches only the follow chip (allowed)
  and nothing else.

## 9. Scroll and follow: the intentionality contract

Hard rules, in priority order. These are the product requirement, not
implementation detail:

1. **No scroll write without a cause the user can name.** The complete
   set of writers: (a) placement on open, (b) user-clicked jump,
   (c) follow mode tracking the frontier, (d) anchor-preserving
   compensation (net visual delta zero). Anything else is a bug.
   All writes go through one chokepoint function in
   `runMapFollow.svelte.ts` with a caller tag (mirrors
   `utils/scroll/chokepoint.ts` discipline, without importing the
   timeline machinery, which is virtualizer/spring-shaped and
   wrong-sized for this surface).
2. **Escape is event-sourced, never geometry-inferred** (verbatim from
   `utils/scroll/intent.ts:9-11`). Wheel-up, PageUp/ArrowUp/Home,
   touch-drag down, scrollbar-gutter pointerdown, middle-click: any
   of these disengages follow. A programmatic write can never
   false-escape because escape only listens to input events, and it can
   never be mistaken for input because inputs are the only escape
   triggers. Text-selection inside the map holds writes (same rule as
   the timeline).

   The corollary is that follow may not run with no listeners installed,
   and that is enforced by state rather than by a report. `attach()`
   waits a few frames for a late-binding scroller and then LATCHES the
   controller shut (`writeScrollTop` no-ops, follow disengages, the
   chip hides) before it throws. The throw alone changed nothing: the
   chokepoint reaches the element through the live getter rather than
   through anything the failed install owned, so a scroller that turned
   up moments later left follow gliding with nothing able to hear the
   reader ask it to stop, which is precisely the state the message
   names. A later successful `attach()` clears the latch completely.
   Pinned by `runMapFollow.svelte.test.ts` "exhausting the attach frames
   latches the controller shut, not just loud" and "a later successful
   attach clears the latch completely".
3. **Re-engage is explicit only**: clicking the follow chip. No
   "scrolled near the frontier so we grabbed you back". (Deliberately
   stricter than the thread timeline's bottom-restick: the map's
   "bottom" is mid-content and moves, so implicit restick would be
   exactly the force-grab the requirement forbids.)
4. **Follow default** (decided): ON when opening a `running` run, OFF
   when opening a parked/terminal run (placement rule below). Any user
   scroll input disengages for the visit; only the chip re-engages.
5. **Placement on open** is placement, not scrolling: running run ⇒
   position the follow target in view (frontier visible, header
   context above it); parked/terminal ⇒ top (digest + cause are the
   payload). Placement happens pre-paint; the user never sees a jump.
6. **Follow tracking**: when the follow target moves (frontier
   advances / wave folds), scroll the target into a stable viewport
   band. Frontier moves are minutes apart; use one short programmatic
   glide (`motionReduced()` ⇒ instant). A second move during a glide
   retargets, never queues.
7. **Compensation**: any map-initiated height change **above** the
   current viewport anchor while not following (fold/unfold, growth)
   is wrapped in an anchor-hold: measure the anchor element's viewport
   offset before the state flip, restore it after flush, pre-paint:
   the `preserveScrollAnchor` pattern, map-local. User-initiated
   expands keep the clicked row pinned. Because folds happen at the
   frontier and escaped readers are above it, this path is rare, but
   it is the difference between "solid" and "fights you", so it gets
   tests, not hope.
8. **Fold animation only when visible**: a fold whose region is
   outside the viewport applies instantly (no animation, one
   compensation). On-screen folds animate via `grid-template-rows`
   0fr/1fr (the mockup mechanism), gated by `motionReduced()`.

   This is a scroll rule, not a taste one, and it is the other half of
   clause 7. The height transition runs 200ms while the anchor
   compensation that cancels it is measured ONCE, at the flush, so an
   auto-fold above a disengaged reader drifts their viewport for every
   frame after the first. Instant makes the whole delta land inside the
   hold, which is the only moment compensation can see it.

   The decision is made where the state flips, in an `$effect.pre` keyed
   on `open`: pre-effects run before the DOM update, so the rect they
   read is the layout the fold is about to change. One rect read per
   toggle, no per-frame work, and no write, so clause 1's writer set is
   untouched. The arithmetic is `runMapGeometry.foldAnimates`, pure and
   directly tested; a scroller or region it cannot measure answers
   "animate", which is the harmless side (a visible fold that should
   have been instant is cosmetic; the reverse is the drift).
9. `overflow-anchor: none` on the overlay body scroller (doctrine:
   native anchoring fights owned compensation), and the overlay's
   scroll position **resets per level/run navigation**, which fixes the
   existing sweep-leaves-stale-scrollTop defect as an adjacent fix.
10. **Jump chip**: `now ▸` chip; click = engage + glide. Sits outside the
    scroll container (the ScrollToBottomButton lesson,
    `MessageTimeline.svelte:710-717`).

    It renders only when the click would DO something, which is a
    different question on each side of engagement. While FOLLOWING it is
    the recovery from a target that drifted out of sight anyway, so the
    condition is "the target is off-screen". While DISENGAGED it is an
    OFFER TO TRAVEL, and the condition is that engaging would move the
    viewport: `|restingScrollTop(target) − scrollTop|` at or above the
    same floor the glide refuses to write below. A reader who scrolled
    back down to the marker is already looking at where a click would
    put them, and re-engagement on its own has nothing for them to see.
    An affordance that does nothing must not render.

    The predicate is deliberately neither of the other two rect
    questions this surface asks. `inBand` is narrower than "the click
    would do nothing" (a target sitting visibly below the band still
    has a rest line to be glided to), and `isOffscreen` is wider: a rest
    line clamped against the end of the document leaves an off-screen
    target with nowhere to travel. Sharing the glide's own floor is what
    keeps the offer and the write it promises the same number.

    Hiding it is NOT a re-engagement (§9.3 stands: only a click is): the
    reader stays disengaged, and the moment the run carries the target
    away from them the same offer comes back. That means chip
    visibility is re-measured on plain scroll input as well (the
    reader coming back is what hides it), on the same rAF-coalesced
    frame the escape path already spends, and on every anchor release,
    because growth below the fold raises `maxScrollTop` and unclamps a
    rest line with no scroll event, frontier move or resize to report
    it. Pinned by `runMapFollow.svelte.test.ts` "chipVisible — the
    disengaged offer is only rendered when it travels" and by the
    overlay e2e's return-to-the-marker leg.
11. The glide does not author `will-change` or content transforms.
    Promotion transitions caused visible flicker, and a permanent content
    layer later produced stale WebView2 pixels while the DOM stayed live.
12. **Container resize is a recomputation event, and rate-bound**: a
    `ResizeObserver` on the scroller re-runs the follow decision and the
    chip's geometry, rate-bound (≥100ms end-to-start) so live resizing
    never thrashes layout or scroll. While FOLLOWING, that means the
    frontier is re-tracked into the band, since a reflow can leave it
    anywhere. What reflowed is not this clause's business: the fan
    columns wrap or scroll by CSS alone, and there is no "fan tier" for
    anything here to decide or observe.

    While NOT following it writes nothing, which is a deliberate
    amendment to this clause as first written ("wrap in the same
    anchor-hold as folds"). An anchor-hold compensates a layout change
    the map CAUSED; a container resize is one the reader caused, by
    dragging a window edge or the app changing size around them. The
    content does not grow. It reflows, and the browser has already
    chosen where the reader lands. Re-anchoring on top of that fights
    the drag: the viewport would shift with every intermediate size,
    each one a scroll write with no cause the reader could name (§9.1).
    The rule stands unchanged for fold, unfold and growth, which are the
    map's own mutations, and for the wholesale view swap of a refetch
    (§7, "transport gap"), which is the map's own too.

## 10. Transitions

- State changes = class swaps on nodes already in the DOM (CSS
  transitions on border/color, ~250ms). Never DOM insertion for a
  status change: ghosts are real elements from first render.
- Structural motion is exactly three cases: a branch column enters
  from the queued group chip (width slide, local to the fan), a
  finished branch folds into the done group chip, and a wave folds to
  its summary row while the next segment reveals. Each is one owned animation;
  nothing else moves. All gated by `motionReduced()`.
- Running glyph = `SteppedSpinner`, whose stepped rotation is a
  compositable CSS animation (`stepped-spin`), legal here because the
  overlay is not the timeline scroller. Any other standing indicator uses
  the ambient classes, which are stepped, never Tailwind's smooth
  `animate-spin`/`ping`/`bounce` loops, which pin GPU frame production to
  panel refresh for as long as they are on screen (2026-07-04).
- The svelte flush caps and quiet-work rules stay untouched: nothing
  here adds per-frame reactive work; the 1Hz clock is the only timer.

## 11. Deletions, migrations, adjacent fixes

Deleted (no aliases, no compatibility shims), **done**:

- `WorkflowRunTree.svelte`, `WorkflowRunTree.test.ts`: deleted.
- `utils/workflowRunTree.ts`: deleted OUTRIGHT, not hollowed out. A
  module named after a tree that no longer exists is a worse artefact
  than the relocation, so each survivor went where it belongs:
  `WorkflowNodeSignal` + `workflowNodeTone` → `utils/workflowRunSignal.ts`
  (the R1 module that already owned the run-state half of the same
  rule), `workflowDuration` → `utils/format.ts` (it is a pure formatter,
  and landing it in `stores/` first gave every pure map module a
  utils→stores import edge for a string), and `failedWorkflowUnitInDetail`
  → `utils/workflowActionRows.ts` (its only consumers are §4.3 evidence
  and the §8 unit actions). It no longer builds a whole tree to find one
  unit, and it returns a narrow `WorkflowFailedUnit` rather than a
  timeline node. The glyph map died with the tree, and the map's glyphs
  live in `utils/workflowRunMapStyle.ts`.
- `loadWorkflowDetail`'s child-recursion role and
  `retainWorkflowDetails`' child-walk: done. The detail cache is
  root-only and the re-entrancy note in frontend/CLAUDE.md restates the
  new guard ("is the cache already exactly the root").

Adjacent fixes surfaced by the survey (small, shipped with this work,
called out for review as riders):

1. **Done**: `types/workflow.ts` `WorkflowItemStateEvent` gains
   `phaseId`/`attempt` (wire already carries them).
2. **Done**: `eventsTransportGap.ts` gets an explicit `workflow:` case
   (was: unknown-channel default, a real correctness hole for any
   workflow UI, not just the map).
3. **Done**: `overflow-anchor: none` on the overlay body scroller plus
   a scroll reset on level/run navigation (§9.9). The reset runs in an
   `$effect.pre` keyed on the level, so it lands before the new level
   paints and strictly before the map's own `placeOnOpen`, which must
   have the last word.
4. **Done**: the stale `phase N/M` on `WorkflowRunHeader`: the header
   derives its position from the map's frontier (wave part + deepest
   part) whenever the map view is loaded, and falls back to the SQL
   counter only when it is not. It ATTACHES the run map itself rather
   than peeking a key the sibling map component happens to hold. Attach
   is refcounted, so the two share one RPC, and a header that read a key
   it never acquired would revert silently to the stale ordinal the
   moment the map beside it was reordered or lazily loaded. That attach
   effect is keyed on a `$derived` of the id, never on the `item` prop:
   the list cache mints a NEW row object per transition
   (`patchWorkflowItems`), and an effect tracking the object released and
   re-attached on every one, which resets the retry curve, so a failing
   fetch never backed off for as long as the run kept moving. Pinned by
   `WorkflowRunHeader.test.ts` "a new row object for the same run does
   not release and re-attach the map". Home rows keep the existing
   summary+`livePhases` pairing (default per §13). The SQL's
   alphabetical tiebreak defect is documented in
   `internal/store/work_items.go` above `workItemSummaryProgressJoin`
   rather than rewritten in passing. It stops mattering for the detail
   surface entirely.

Rider found during integration, outside this spec's surface: the
markdown renderer's Mermaid component shipped
`:global([data-expanded='true'])`, an UNSCOPED app-wide rule that pinned
any element carrying that ordinary attribute `position: fixed` at
`z-index: 2147483647`. The map's expanded wave row hit it and covered
the whole app, swallowing every click on the run detail's action row.
Scoped to `[data-streamdown-mermaid] [data-expanded='true']`, then
deleted outright along with the mermaid panzoom that set the attribute.
The map's wave row keeps its own attribute name, `data-wave-expanded`:
the rule is that a stylesheet from the markdown renderer must not be
able to reach a component that has never rendered markdown
(`frontend/src/lib/markdown/AGENTS.md` § Landmines), and staying out of
its namespace is what keeps the next component that reaches for
`data-expanded` from re-finding a rule like it.

Docs/tests bookkeeping:

- **Done**: UI-SPEC §4.2 rewritten for the map; §12 integration rows
  updated; this file is the map's spec of record.
- **Done**: e2e `workflows-overlay.spec.ts` migrated off the tree ids
  and onto map ids and map semantics, with new coverage for: a live run
  tracked from held to parked on ONE mounted detail (the event-patch
  path, no navigation, no reload), a thread opened from a map node
  (R3), and a soft stop landing on the loop foot of a wave chain.
- **Done**: §9 has e2e coverage as well as the controller's unit suite,
  and the split is not the one first planned here. The unit suite
  (`runMapFollow.svelte.test.ts`) owns the DECISIONS (escape sources,
  re-engage, glide retarget, band arithmetic) against a stated layout,
  because happy-dom has none. What only a browser can answer is what the
  anchor search finds in the REAL DOM: escape-then-hold-then-chip on a
  live run, and compensation across engine-driven growth above a reader
  parked at the tail. That second case earned its keep immediately.
  It caught `pickAnchor` stopping at a container that SPANNED the
  growth, which measures a delta of zero and compensates nothing. Every
  container between the scroller and the row does that, so the flat
  fixture the unit suite used could not see it and the whole
  compensation clause was silently inert in production.
- **Done**: projection unit tests are the bulk: wave flattening (incl.
  broken chains, outer-call + inner-chain, definition drift), ghost
  synthesis, branch states, decision node, attempts, frontier priority.
  Follow controller: escape matrix × re-engage × placement ×
  compensation (transition coverage, not just state coverage).

## 12. Workstreams and sequencing

Each lands with `make go-build`, `make go-test`,
`pnpm run check`, `pnpm run build` green; review per repo practice
(implementation via Opus subagents, codex review lenses after).

1. **Backend**: `WorkflowGetRunMap` + skeleton projection +
   `tailSelfCall` + `PhaseEvent.occurredAt` + `autoResumeAt` on the
   view. Store-level tests over a fixture campaign tree.
2. **Frontend data plane**: `workflowRunMap` entity store, event
   patcher (+ its invalidate fallback), gap case, reconnect posture,
   shared-clock wiring. Unit tests over recorded event sequences,
   incl. patch-vs-refetch equivalence (apply events then compare
   against a fresh fetch fixture).
3. **Projection** (pure) + exhaustive tables.
4. **Components** + R1-compliant styles; static states first
   (storybook-less: table-driven component tests + harness).
5. **Follow controller** + placement + compensation + jump chip.
6. **Integration**: swap into `WorkflowRunDetail`, deletions,
   adjacent fixes, e2e migration, spec updates.

1–3 are parallelizable after the RPC shape is fixed; 4–5 depend on 3;
6 last. The mockup (`frontier-stack-mockup.html`, session scratchpad /
published artifact) is the visual reference for 4–5, **except** its
cyan/pulse vocabulary, which §2 overrides.

## 13. Decisions

Resolved 2026-08-12 with the user:

- **No split UI.** The tree is deleted outright; inline wave/branch
  expansion and thread links are the only inspection surfaces. No
  "raw tree" toggle exists or will be added.
- **Now marker uses `--accent`**: one deliberate positional accent;
  R1's amber/red meanings untouched.
- **Follow defaults confirmed**: ON for running runs, OFF for parked;
  any scroll input disengages for the visit; only the chip re-engages.
- **Follow priority confirmed**: needs-human first, else the deepest
  leaf with the most recent transition; all running branches render as
  running regardless of which one is followed.

Resolved 2026-08-12, second pass (visual-language rework, against the
approved mockup):

- **Collapse policy inverted.** Every composition off the frontier path
  is collapsed by default, at every depth. The first implementation gave
  depth ≤2 away free, which read as a wall for the campaign shape the
  map exists for. `RUN_MAP_COMPOSITION_DEPTH` is deleted rather than
  demoted to a runaway bound, because a constant nobody reads is a
  constant that grows a reader.
- **"Settled" means handed off.** `waveIsSettled` = not live OR has tail
  children, and `waveSignalOf` renders a handed-off lap `done` rather
  than spinning. Attention (parked/failed) still wins over both.
- **A single-lap composition never folds its lap.** There is nothing to
  fold, and folding it hid the only content the sub-card exists to show.
- **Settled fan lanes stay lanes, folded to their header.** Only SCALAR
  settled units go to the `done ·N` group; a unit with a child run keeps
  a lane, because the group renders a node and nothing else.
- **`RunMapFan.totals` removed.** The wave's summary row already states
  the unit tally; the fan's second one was a number to reconcile. What
  the projection guarantees is the partition, and that is what the tests
  assert.
- **Connective tissue is CSS pseudo-elements only.** No SVG, no
  measurement JS, nothing positioned out of flow, because §9.7's anchor
  descent has to keep finding every node as an ordinary row.
- **No legend.** The mockup carried one; frontend/CLAUDE.md forbids
  in-app explanatory text for internal mechanics, and the mockup does not
  override the app's rules.

Resolved 2026-08-14, third pass (legibility rework, against a real
campaign: "collapsed items where it shouldn't collapse, basically all
text is cut off, multiple clicks to see completed items, the width isn't
used, horizontal scrolling everywhere"):

- **Labels wrap; CSS ellipsis is banned on the map.** A phase name is
  the node's whole meaning. `RUN_MAP_LABEL_MAX` went 56 → 96 and is a
  runaway guard, not a line budget. The one deliberate nowrap is a
  folded lane's one-line header; the one deliberate clamp is a cause's
  two-line preview with an expander. Audited by "nothing on the map
  carries a CSS ellipsis".
- **The map owns the full card width.** The 34rem `RUN_MAP_COLUMN` cap
  is gone. It squeezed four lanes into half the card while the other
  half sat empty.
- **Lanes are readable or they are not lanes.** 15rem floor / 26rem cap
  (was 120px/200px), and the lane row WRAPS instead of scrolling.
  A scrollbar hid whole lanes; a second rank hides nothing.
- **Nested fans stack.** `RunMapFan.layout` is the model's call by
  lane containment: `columns` anywhere the card's full width is
  available, `stacked` once inside a lane. Columns inside a column can
  only subdivide a width that was already minimal.
- **A sole child merges into its lane.** Headerless composition (no
  header row, no card frame), name composed onto the lane's `title` in
  the model, opened by the lane click. The one-call lane is the
  campaign's dominant shape and it was paying a full sub-card of chrome
  per lane for a repeated name and duration.
- **Small done groups render inline.** ≤ `RUN_MAP_INLINE_DONE_MAX` (8)
  done units are chips in the flow, no click; past that the labelled
  fold returns. Queued never inlines, because it has no entries by
  construction.
- **An opened composition answers with content.** A settled multi-lap
  chain defaults its FINAL lap open; earlier laps stay one click away.

Riders from the third pass's own review round (2026-08-15), the fix
wave to the seven decisions above, reviewed as new code:

- **The merge earned guards.** `merged` requires the lane to own the
  fold (`toggleable`) or the child to be LIVE, and never fires for a
  failed child. An actionable lane merging a settled child
  force-painted a subtree with no collapse anywhere on it, and a failed
  child's own row is where its red glyph lives (§6, merge guards).
- **"Final lap" is the tail leaf, and its default inverts on click.**
  Chain position broke on retried tails; a one-way default could not be
  re-closed. `expandedWaveIds` now INVERTS a settled lap's default
  (§6, vertical scale).
- **The lane row centers with `safe center`.** Plain centering made an
  overflowing rank bleed out of BOTH edges with the head unreachable;
  and the folded lane title took a hard 40-char budget
  (`RUN_MAP_FOLDED_LABEL_MAX`) because a rigid line was the one place
  sheer length still overflowed (§6, width/text).
- **The oversized done fold split.** Button in the lane row, chips
  below as a block, because the chip-row's intrinsic width inside a
  `flex-none` lane dragged the whole row past the card edge; and a
  group outgrowing the bound mid-run seeds its fold open (§7 rows).
- **Lane geometry moved to `workflowRunMapStyle.ts`.**
  `RUN_MAP_LANE_MIN`/`MAX` exported beside the label bounds; `app.css`
  re-declares them only as the `var()` resolution floor, pinned
  together by test (§2).

Resolved 2026-08-15, fourth pass (polish, after "doesn't feel
production grade" against the same real-shaped campaign; brief: "hints
of color to bring clarity", "clean apple type of UI vibes"):

- **Hierarchy moved from borders to SURFACE.** Every signal that
  happened is a FILLED box (`FILLS` in `workflowRunMapStyle.ts`);
  settled work drops its border ink entirely (`border-transparent`
  over `bg-surface-2/50`); live and attention signals keep a real
  border on top of their fill. A wall of equal-weight bordered boxes
  read as a form, not a flow: the border taxonomy carried all the
  meaning and none of the weight.
- **Ghosts are bare lines, not boxes.** `RUN_MAP_GHOST_ROW` replaces
  the dashed box: the future rendered at full box geometry weighed
  what the past weighs, and a screen of it was half the wall. The
  dashed border vocabulary survives only on the loop-foot's fork
  connector and the `unknown` hairline.
- **R1 amended with two clarity hints** (user-approved): the done
  glyph is `text-success` (the GLYPH, never the label, pinned as an
  exactly-one rule in `workflowRunMapStyle.test.ts`), and the `now ▸`
  row carries a `bg-accent/10` tint. Amber and red keep their monopoly
  on label hues. The evidence checks strip reads `.tone`, not
  `.glyphTone`, so it stays neutral by construction.
- **A row is ONE button in text flow.** `RUN_MAP_NODE_BOX` became
  `inline-block` text flow, and glyph + label + meta moved INSIDE the
  single label button as inline content (glyph/meta `inline-block` so
  the hover underline stays on the words; the strike for a dropped
  unit sits on the label span alone). Root cause of the orphan-glyph
  bug seen in the field: any atomic sibling beside a label
  (a flex item, a separate button) wraps as a UNIT when space runs
  out, stranding a lone `·` or spinner on the line above. The whole
  row is also a bigger click target.
- **The loop stubs de-boxed.** Undecided outcome stubs are centered
  hint text over the dashed fork, not two full-width bordered bars.
  They are futures, and the fork connector already draws the shape.

Standing defaults (not blocking; surface before changing):

- Home/list stays as-is apart from the §11.4 staleness rider.
- Queued-vs-waiting: v1 renders `pending`+provider as "queued";
  wait-state events from the engine semaphores are a v2 addition if
  the distinction proves to matter.
