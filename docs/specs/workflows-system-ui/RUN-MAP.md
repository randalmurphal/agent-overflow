# Run Map — spec and implementation plan

Replaces the recursive run-detail tree (`WorkflowRunTree.svelte`) with a
vertical **run map**: time flows down, solid is done, one marked "now",
dashed is not-yet. Ground-up on the data plane where the current shape
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
hasn't yet is pre-rendered as dashed ghosts below the action, ending in
the loop decision ("issues → wave N+1" / "clean → done") when the
definition tail-self-calls. The reading rule is the whole UI: **position
= progress, solid = happened, marked node = now, dashed = not yet.**
A workflow with no self-call is simply a map with one segment and no
loop affordance — that is the base case, not a special case.

Non-goals: the map is not a chat surface, not an envelope inspector
(R2), and not a replacement for the home list. Digest, evidence,
outputs, disposition, and the action row are untouched.

## 2. Visual vocabulary (R1-compliant)

The interactive mockup used cyan glow + pulse for "running". The app's
R1 rule (frontend/CLAUDE.md, `utils/workflowRunSignal.ts`) reserves
amber for human-blocked, red for failed, pulse/glow for amber only.
The map adopts R1 rather than extending it:

| Map state | Treatment |
|---|---|
| done (phase/unit/wave) | solid border, glyph `✓`, tone `text-fg-muted` — neutral, per `workflowNodeTone`. Green is not one of this surface's hues |
| running | solid border `border-border-strong`, `text-fg`, glyph = `SteppedSpinner` (the app's standing-spinner primitive). **No pulse, no new hue.** |
| the **now marker** | position + weight, plus a left-gutter `now ▸` tag in `--accent` — the one deliberate accent use on the surface (decided: it marks position, not status; R1's hue meanings stay untouched) |
| pending / queued unit | solid border, `text-fg-subtle`, glyph `◌`, meta `queued` |
| ghost (not yet reached) | dashed `border` at `--fg-hint` weight, `text-fg-hint` |
| parked / needs-human cause | amber chip/border via `workflowRunSignal` tones + existing `status-glow-warning` ring — the only glow on the surface |
| failed node / failed wave row | `text-error` / `border-error` per `workflowNodeTone` |
| retry attempt | `·N` suffix on the label. That is the whole convention: the row keeps its own status's border, because a retried attempt is not a ghost and a second border vocabulary for "again" would collide with the dashed one that means "not yet" |
| soft-stop armed | note on the loop decision node: "stops after this wave" |

Colour decisions stay in `workflowRunSignal.ts` / `workflowNodeTone` —
extend those, never inline hues in components. `animate-pulse` is a
disarmed marker class (ambientTicker); do not use it for running.

## 3. Wave semantics — what flattens, what nests

**Flatten only tail self-calls.** A run's definition tail-self-calls iff
its snapshot's last phase `IsCall()` and `CallTarget() == workflow.ID`
(`def/calls.go:21,26`). The chain root → child → grandchild along that
edge is the **wave chain**; wave ordinal = position in the chain
(equals `callDepth` relative to the chain root). Everything else —
non-tail self-calls, calls to other workflows, call-bound units —
renders as **composition**: a chain inside its parent's node/branch,
recursively.

Per wave segment:

- Phases render in **frozen-snapshot declared order** (not
  `started_at` order — that ordering is what makes the current SQL
  counter lie). Attempts of one phase render as sequential nodes
  (`audit`, `fix`, `audit ·2`). `superseded` is a dead status — ignore.
- The terminal tail-self-call phase does **not** render as a phase
  node; it renders as the **loop decision affordance** at the segment
  foot: two outcome stubs (loop → next wave / done), ghost until the
  gate resolves. Lap counter reads `lap N of ≤M` where M is the call
  edge's `maxDepth`; when `maxDepth < 1`, show `lap N` plus the budget
  line (the ceiling in force is then the only bound — say so, don't
  imply unbounded). The strip states a ceiling whenever one exists,
  which is a superset of that rule, so no flag decides it.
- The strip's loop foot describes the **deepest LIVE wave**, falling
  back to the chain's last wave when nothing is live. The chain is
  level order, so its tail is the deepest — but a lap can hold two
  waves (a retried tail call), and which of those the walk reaches last
  is an accident of the parents' ordering. Taking the tail outright had
  the strip describing a dead-end sibling while the live wave beside it
  was the one the run is in. Pinned by `workflowRunMap.test.ts` "two
  waves at the deepest lap | the foot describes the LIVE one, not the
  tail".
- A fan-out phase's full unit list is persisted `pending` at expansion
  (`engine/units.go:443-457`), so branch columns are **known, real
  records** from the moment the phase starts — pre-rendering queued
  branches requires no guessing. Before expansion (phase not yet
  reached) the fan-out renders as a single ghost node named from the
  skeleton ("ports — declared by plan").
- Completed waves fold to one summary row: `✓ wave N · duration ·
  <unit count summary> · <audit/gate outcome incl. retry count>`.
  Failed/cancelled waves keep their state colour on the row.
- Chain edge cases: chain root restarted fresh has `callDepth 0` but
  authored wave numbering may continue — the map shows **chain-local
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

Server side: TWO round trips, whatever the tree's size — the upward root
resolution, then `store.ReadWorkItemTree`, which runs SIX statements (runs,
attempts, units, auto-resumes, ledger sum, ledger split by model/cost source)
inside ONE read-pool transaction. The single transaction is load-bearing, not
tidiness: under WAL it pins one snapshot, so a run created between two reads
cannot contribute attempt rows belonging to no run the answer carries.
Membership is a recursive CTE over `work_items.parent_item_id`
(`internal/store/work_item_tree.go`) — upward to resolve the root from
any member, downward for the run listing — and the phase/unit/auto-resume/
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
Payload is metadata-only — no envelopes, no narrative bodies, no diffs —
so a 40-wave campaign is a few hundred small rows: bounded, in line with
core principle 4.

**The refusals are DATA, and they are permanent.** `not-found` (a stale nav
entry or a discarded run), `too-large`, and `corrupt-linkage` come back as
`view.refusal` with the RPC succeeding, because the transport strips a method
error's text for every non-loopback caller — an error return cannot carry a
sentence to a remote client — and because the entity store answers a thrown
error with a retry ladder that would re-ask an unanswerable question forever.
Anything a retry could fix (a store read, a ledger group with an unknown cost
source) stays an error.

`WorkflowGetItem` stays for the root's evidence/digest/actions and
loses its map duties; child-expand fetching (`loadWorkflowDetail`
recursion) and `retainWorkflowDetails` child-walking are deleted, and so
is the detail view's `children` list itself — call linkage is a tree
fact, and the map answers for the whole tree instead of one level of it
(the per-child summary join parsed every child's frozen snapshot to find
a phase ordinal, on every detail fetch).

### 4.3 Event changes (small, backend)

1. `engine.PhaseEvent` gains `occurredAt int64` (event-time ms). A
   `running` transition patches `startedAt`; a terminal one patches
   `endedAt`. Without this the frontend must stamp client time, which
   drifts across reconnects. Emit sites already funnel through
   `emitUnitState` / the phase emit helpers — one field, all sites.
2. Frontend `WorkflowItemStateEvent` type adds `phaseId`/`attempt` —
   the Go payload already carries them (`engine/types.go:571-573`);
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

Keyed by **the item id the UI asked for** — the nav-stack run id — not by
the tree root it resolves to. The root is resolved server-side (§5.9), so
the frontend cannot know it before the first answer, and keying on it
would mean an attach that has to wait for a fetch to learn its own key.
The ANSWER covers the whole tree whichever member was named, so one entry
serves the wave, the root and every run below either. Two keys naming one
tree (a deep link to a child plus the root) are two entries holding two
copies — the shape `gitStatusStore` already carries for two spellings of
one directory, and it costs one extra RPC only when both surfaces are open
at once.

- `source()` = fetch map view, `apply()` it. Consumers attach from the
  run-detail component keyed on the entity id alone (getter-ctx rule,
  `ChatHeaderActions.svelte:66` shape).
- `eventsWorkflow.ts` routes `workflow:phase-state` /
  `workflow:item-state` / `workflow:soft-stop` into the store module,
  which resolves the event's `itemId` to a watched root (parent-index
  maintained on apply) and **patches the node in place** through
  `store.apply(key, next, { preserveError: true })` — the single write
  chokepoint, so any derived revision logic lives at the write site
  (the WebView2 lesson: bumps are a property of writing).
- **Patches are an optimization, never load-bearing for correctness.**
  Any event the patcher can't place precisely (unknown child, unknown
  phase id, attempt reopen it can't model) ⇒ `invalidate(rootKey)`,
  debounced 200ms. `source()` keeps the last value while refetching, so
  reconciliation never flickers.
- **A refusal ends the event-driven refetch, on every path.** A patch
  that lands while a fetch is in the air is marked, and the apply that
  may have buried it re-asks — unless the answer that landed was a
  refusal, in which case the mark is CONSUMED and nothing is re-asked.
  Every refusal code is permanent (§4.2), so the refetch could only
  produce the same refusal; this was the one path back into the loop the
  item-state path is explicitly guarded against.
- Transition cadence is phase/unit-level (not token-level), and a patch
  is O(tree-clone) at worst — well inside the 100ms occupancy contract
  without needing the quiet-work scheduler. If profiling ever says
  otherwise, the fix is keyed sub-maps, not skipping events.
- **Gap recovery**: add an explicit `workflow:` case to
  `eventsTransportGap.ts` (currently falls to the unknown-channel
  default): `workflowRunMapStore.invalidateAll()` +
  `refreshWorkflowRunsSoon()`. Edge-triggered channel ⇒ a dropped frame
  is terminal; blanket invalidate is the established recovery
  (`eventsTransportGap.ts:121-136` rationale).
- Reconnect: entityStore's `resetAll`/`suspend` handles re-source; the
  current workflows store has no reconnect story at all — the map store
  must not inherit that.

### 4.5 Time

Durations tick client-side from `startedAt` via the existing shared 1Hz
clock: thread `createSharedNowClock(hasRunningNode).now` into the pure
projection as `nowMs` (its `workflowDuration(started, ended, nowMs)`
signature already accepts it; every current caller defaults it — the
bug). Keep `workflowDuration` formatting as-is (deliberately drops
seconds above a minute). Nothing time-related crosses the wire besides
timestamps.

## 5. Pure projection: `utils/workflowRunMap.ts`

`buildRunMap(view, nowMs, options?): RunMapModel` — pure, no Svelte
imports, exhaustively table-tested (mirrors the `buildWorkflowRunTree`
posture, which this replaces).

**The model's shape is stated in
[`utils/workflowRunMapTypes.ts`](../../../frontend/src/lib/utils/workflowRunMapTypes.ts),
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
  flag anywhere — model field or component prop — because the surface only
  stays honest while two answers agree, and open-with-no-segments renders
  "Nothing recorded in this wave yet." over a wave full of records.
  `runMapWaveIsOpen` is the one spelling of the question.
- Segment nodes are a discriminated union on `kind`
  (`phase` | `fan` | `call` | `decision`), so a component switches and the
  compiler catches the case it forgot; statuses are unions too, never raw
  strings.
- Every display string — durations, labels, metas, the money line, the lap
  label — is precomputed. Rendering is a read, never a derivation.

Projection rules (each a table-test group):

1. **Skeleton ∪ records.** Every skeleton phase yields a node; records
   overlay status/timing; skeleton-only ⇒ ghost. Records whose phase id
   is missing from the skeleton (definition drift on rerun) render
   appended with a neutral "not in current definition" meta — never
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
5. **Ghost synthesis after the frontier only** — phases before the
   current one that never ran (loop-back re-entry) render as their
   recorded reality, not ghosts; the ordinal question ("how far") is
   answered by position, so no `phase N/M` arithmetic exists anywhere
   in the map.
6. **Ghosts exist only for live runs.** A terminal run (done / failed /
   cancelled) renders no future — ghosts and undecided loop stubs are
   exclusive to `running` / `needs-human` states; nothing further will
   happen, so nothing further is drawn.
7. **Each wave renders against its own run's frozen skeleton**, never
   the root's — a definition refresh between waves is reachable and the
   waves legitimately differ.
8. **Records-only degradation**: an empty or undecodable snapshot
   (pre-migration rows, oversized snapshot) yields an empty skeleton —
   that wave renders its recorded phases in recorded order with no
   ghosts and no loop affordance. Never a crash, never a blank map.
9. **Root resolution**: `WorkflowGetRunMap` accepts any item id and
   resolves to the tree root server-side, returning `rootItemId`; a
   stale nav-stack entry or deep link pointing at a child normalizes
   instead of erroring.

## 6. Layout resilience — sizes, text, scale

The map must be correct at every reachable size and content shape. These
rules are deterministic — **there is no measurement-driven JS layout on
this surface at all**, and the "fan-tier decision" this section first
imagined does not exist: the fan is `overflow-x: auto` and CSS decides,
per frame, whether the columns need to scroll. Nothing measures a column
to pick a tier. Each rule below is testable without a layout engine.

**Width.**

- The spine — wave summary rows, phase nodes, the loop foot — is a
  single centered column that owns 100% of the map width and **never
  scrolls horizontally**. The overlay card yields roughly 520–950px
  depending on window size; the spine stays legible across that whole
  range because nodes size to content with truncation, never fixed
  widths.
- The **fan region is the only horizontally elastic element**. Branch
  columns have a 120px floor / 200px preference, declared once as
  `--run-map-lane-min` / `--run-map-lane-max` (the resting width, the
  enter keyframes and the leaving transition are three renderings of the
  same two numbers). When active columns exceed the available width, the
  fan region alone gets `overflow-x: auto` (the app-wide wide-content
  rule) — the page and spine never scroll sideways. **Nothing writes the
  fan's `scrollLeft`**: §9's chokepoint owns exactly one number, the
  overlay body's `scrollTop`, and a second automatic scroll axis would be
  a write with no cause the reader could name (§9.1).

**Fan scale.** Fan width is engine-capped at `EffectiveMaxFanOutWidth`
(default 32); 32 uniform columns communicate nothing. Columns are
reserved for branches with structure or actionability — `running`,
`failed`, `parked`, `taken-over`, **plus any unit that CALLED a run**,
each rendering its chain. Structure is a fact about the unit and not
about its status: a group chip renders a chip and nothing else, so
routing a finished call-bound unit into `done` deleted the child run and
its whole composition subtree from the map the moment the branch stopped
running (§7, "unit-bound call"). Scalar statuses — the ones with nothing
under them — collapse into two **group chips** flanking the columns:
`queued ·N` and `done ·N` (`dropped` lives in done's expansion with
struck styling). Either chip expands inline into a wrapping unit-chip
grid (glyph + middle-truncated unitId + duration; wraps vertically, no
scroll). A finishing branch folds into the done chip; a starting queued
unit slides out into a column (§10 motion). This is information design,
not just space: the interesting subset gets geometry, the bulk gets
arithmetic.

**Text.** No text length reachable from the engine may break layout:

- Node labels: phase `name` (fallback `id`), one line, middle-truncated
  (`truncateMiddle`) with full text in `title`.
- Unit ids are engine-stamped slugs but unbounded in principle — same
  truncation rule.
- Park causes / cause chips clamp to two lines with inline expand.
- Wave summary rows are one flex line; parts yield by priority
  (outcome > unit counts > duration) via CSS `min-width: 0` truncation
  order — no JS measurement.
- Numerics use `tabular-nums`; durations keep `workflowDuration`'s
  shapes.

**Vertical scale.**

- Waves are one summary row each; a 40-wave campaign is 40 rows plus
  one expanded segment — no virtualization in v1. The projection builds
  `segments` lazily per expanded wave, so folded history costs O(1) per
  wave and the adversarial ceiling (`MaxCallDepth` 256 chain) stays
  linear and small.
- **The frontier path is always fully expanded regardless of depth** —
  it is a path, so it costs O(depth) single nodes. Non-frontier
  composition deeper than two levels below its wave collapses to a
  summary node with inline expand: "no clicks to see what's running"
  holds, while a pathological definition can't paint unbounded detail.

**Rebuild cost — reviewed and ACCEPTED, recorded so it is not
re-litigated.** Every store write rebuilds the whole model in one bounded
walk: `buildRunMap` is one `buildIndex` plus one frontier collection, over
a tree the RPC refuses past `maxWorkflowRunMapMembers` (4096, §4.2). Three
things make that the right shape rather than a thing to memoise:

- The inputs are DISCRETE and LOW-RATE. A phase or unit transition is the
  event that moves the map, and those arrive at human-visible cadence, not
  per token. There is nothing to coalesce that the store's 200ms
  invalidate debounce does not already coalesce.
- The 1Hz clock, which is the one genuinely periodic input, gates on
  `runMapViewIsLive(view)` — an OPEN SPAN or a live `auto_resume_at`
  anywhere in the tree, not on run state. A tree parked on a human has no
  open span, so a person reading a stationary page rebuilds nothing at
  all. (Gating on `needs-human` rebuilt the whole model once a second for
  hours; that is the bug this clause exists to keep fixed.)
- The alternative is incremental invalidation keyed by run, which means a
  second source of truth about what changed — and the map's whole
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
| fan-out pre-expansion (static `fanOut` **or** dynamic `over`/`as`) | ONE rule, not two: a count-less ghost named from the skeleton phase — "units — declared by ports". The skeleton carries SHAPE and never a width, so a static list is no more countable here than a dynamic one, and a count "known from the skeleton" was never available to know. Pinned by `workflowRunMap.test.ts` "fan-out pre-expansion \| count-less ghost named from the skeleton phase" and "names a pre-expansion fan by where its units come from, never by a count" |
| fan-out expanded | columns + group chips per §6; join unit = merge node |
| unit-bound call | branch chain (composition), recursing per §5 — **at every unit status, including terminal ones**. Structure earns the column (§6); a unit that called a run keeps its branch after it finishes, because the group chip it would otherwise fold into renders a chip and nothing else. Pinned by `workflowRunMap.test.ts` "unit-bound call \| a COMPLETED call-bound unit keeps its branch and its subtree" |
| call phase, other workflow | composition chain (CallNode) |
| self-call, **not** tail | composition — explicitly never flattened |
| tail self-call | wave chain + loop foot (§3) |
| `maxDepth` absent on tail edge | "lap N" + budget line, no ≤M. Pinned by `workflowRunMap.test.ts` "maxDepth absent on the tail edge \| \"lap N\" plus the budget line, no ≤M" |
| ceiling kind `tokens` | `12.3k of 50.0k tokens` beside the lap, in the app's token shapes. The dollar line cannot speak for it — `$1.25 of 400000` is a comparison that does not exist — so the ceiling gets its own statement rather than being dropped, which is what left a token-bounded campaign rendering "lap 3" and nothing else. Pinned by `workflowRunMap.test.ts` "a ceiling that is not in dollars leaves the summary comparing nothing" |
| ceiling kind `wall_clock` | `4m of 30m` — elapsed against the bound, in `workflowDuration`'s own shapes, so the strip and the node durations read alike. Pinned by `workflowRunMap.test.ts` "a wall-clock ceiling reads as elapsed against the bound, in duration shapes" |
| ceiling kind this build cannot read | nothing, rather than a number compared against a unit it is not in. Pinned by `workflowRunMap.test.ts` "a ceiling whose kind this build cannot read states nothing rather than guessing" |
| `maxDepth` present on tail edge | `lap N of ≤maxDepth+1`, never `≤maxDepth`. `max_depth` bounds EDGE TRAVERSALS (`engine/calls.go#checkCallDepth` refuses the call whose ancestry already holds that many), so a root plus `maxDepth` child waves is legal and the raw bound rendered a perfectly legal final wave as "lap 3 of ≤2". Pinned by `workflowRunMap.test.ts` "maxDepth 2 \| the third wave is legal and the ceiling says so" |
| retried tail call: two waves at one lap | both waves are kept, and the duplicated ordinal is disambiguated as `wave N ·M` (`lapSeq`, 0 when the lap has a single wave) — the same `·N` an attempt carries, meaning the same kind of thing. Dropping either would hide a real run; leaving both labelled "wave 2" is two rows the reader cannot tell apart. Pinned by `workflowRunMap.test.ts` "a retried tail call keeps BOTH child runs as waves rather than dropping one" and "…keeps the wave ordinals monotonic" |
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
| run just created, zero attempts | all-ghost segment; follow target = segment top — the WAVE's own key, so no ghost is marked `now ▸` and the controller resolves it to the wave row. "No leaf" is not "no target": the follow default is decided once, at open, off this value, so a null here cost the whole visit its follow for a run opened in the half-second before its first attempt landed. A run parked before it ran anything (setup failure, pre-flight gate) carries its blocker on the same entry. Pinned by `workflowRunMap.test.ts` "run just created, zero attempts \| all-ghost segment, follow target = segment top", "parked before it ran anything \| the segment top carries the blocker", "a first attempt replaces the segment top — the target moves, so follow moves", and `WorkflowRunMap.test.ts` "renders an all-ghost segment for a run with zero attempts" |
| ABSENT snapshot | records-only mode (§5.8), rendered silently. A run that failed before it ever froze a definition is ordinary history, and annotating it would make normal history look like a defect |
| CORRUPT snapshot (`skeletonError`) | records-only mode PLUS a stated notice in the failure hue, outside the wave's fold — a corrupt wave is usually a terminal one, so news behind a fold is news the reader does not get. The decode failure itself never reaches the surface (R2): what they can act on is that the definition is unreadable. Pinned by `workflowRunMap.test.ts` "projects a corrupt snapshot as its own wave-level signal, apart from mere absence" and `WorkflowRunMap.test.ts` "states a corrupt frozen definition as a failure, not as ordinary history" |
| map REFUSED (`not-found` / `too-large` / `corrupt-linkage`) | a state of its own, never the error state: the RPC SUCCEEDED and the answer is permanent (§4.2), so there is no retry to offer and "nothing to show yet" would promise a later yes. Per-code headline for what it means to this surface, the backend's already-user-shaped sentence beneath it for which run it happened to; an unrecognised code still gets the honest headline rather than a bare sentence. Pinned by `workflowRunMap.test.ts` "carries a refusal through as user-shaped state with no waves to draw" and `WorkflowRunMap.test.ts` "renders the %s refusal as permanent state, not as a failed fetch" |
| spend carrying unpriced ledger rows | the money line says `$4.12 priced · 3 rows unpriced`, never a bare `$4.12`: rows whose model resolves to no rate have their tokens counted and their dollars in nothing, so the total is a LOWER BOUND and the one place that is worded is the projection. With a dollar ceiling in force it reads `$4.12 of $10.00`; with neither, `$4.12 spent`. Pinned by `workflowRunMap.test.ts` "says \"priced\" and names the unpriced rows when the total is a lower bound", "says \"spent\" when nothing is missing…", "a ceiling that is not in dollars leaves the summary comparing nothing" |
| definition drift between waves | per-wave skeleton (§5.7); orphan records appended (§5.1) |
| child run failed mid-chain | chain ends; failed wave row red; no ghost next wave |
| terminal run (done / failed / cancelled) | no ghosts, no undecided loop stubs (§5.6) |
| done awaiting disposition | fully solid map, loop decided "done"; disposition UI unchanged below |
| stale nav entry / deep link to a child id | server root-resolution (§5.9) |
| fan-out at the width cap (32) | group chips + scrolling fan (§6) |
| parallel parked leaves | all amber; follow priority per §13 |
| view-only (remote) session | map renders fully; follow chip active; mutating affordances elsewhere already disabled |
| reduced motion / low power | instant placement, no glides, no fold animation |
| fold whose region is off-screen | applies instantly, animation gate off (§9.8). Decided at the moment `open` flips, from one rect read in an `$effect.pre` — before the DOM update, which is the only frame that can answer "is this region on screen" about the layout the fold is about to change. Pinned by `runMapGeometry.test.ts` "foldAnimates" and `WorkflowRunMap.test.ts` "animates a fold in view and applies an off-screen one instantly" |
| light theme | token-driven; no component branches |
| transport gap / reconnect | `invalidateAll` re-source; wholesale apply wrapped in anchor-hold when not following — recovery never moves the viewport |
| window / overlay resize | rate-bound recomputation (§9.12): re-track the frontier while following, write nothing while not. The reflow itself is CSS's — nothing measures a column to pick a layout |

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
| `WorkflowRunMapWave.svelte` | one wave's body: corrupt-definition notice, fold wrapper, rail, segment nodes |
| `WorkflowRunMapFan.svelte` | split bar / branch columns / queued+done group chips / join bar / the fan's own `overflow-x` |
| `WorkflowRunMapNode.svelte` | single node: glyph, name, duration, meta, thread-open click; composition chains recurse through it |
| `WorkflowRunMapSummaryRow.svelte` | folded wave row (click = inline expand) |
| `WorkflowRunMapFold.svelte` | the `grid-template-rows` 0fr⇄1fr reveal both folds use, plus the §9.8 in-view decision that turns the transition off for a region the reader cannot see (§9.8, §10) |
| `runMapFollow.svelte.ts` | scroll/follow controller (§9): engagement, glide, escape, chip, resize cadence, the one write chokepoint |
| `runMapGeometry.ts` + `.test.ts` | the pure rect arithmetic the controller decides on — band, resting line, off-screen, and the anchor descent, which is the one non-obvious rule here and gets direct tests rather than being inferred from a compensation number |
| `overlayScroller.ts` | the §9.9 scroller handoff: the overlay frame provides, the map requires |
| `utils/workflowRunMap.ts` + `.test.ts` | pure projection |
| `stores/workflowRunMap.svelte.ts` | entity store + event patching |

`WorkflowRunDetail.svelte` swaps `WorkflowRunTree` for `WorkflowRunMap`
in place; header/digest/evidence/outputs/action-row order unchanged.
The frontier strip (breadcrumb + amber blocker chip + lap/budget) sits
at the top of the map, not in the header — the header keeps its
existing role. Test ids: `workflow-run-map`, `workflow-map-wave`,
`workflow-map-node`, `workflow-map-branch`, `workflow-map-summary`,
`workflow-map-follow`, replacing the `workflow-run-tree` family.

Interaction:

- Any **node with a thread** opens it via `openWorkflowThreadById`
  (closes the overlay, R3 — unchanged).
- A **folded wave row** expands inline (model already built; pure
  render). Expansion state lives in the overlay nav store
  (`workflowsOverlay.svelte.ts`) keyed by run id so it survives
  detail remounts — the current tree loses expansion on remount;
  don't replicate that.
- Every mutating affordance respects `isViewOnlySession()` (§10 remote
  posture) — the map itself is read-only, so this touches only the
  follow chip (allowed) and nothing else.

## 9. Scroll and follow — the intentionality contract

Hard rules, in priority order. These are the product requirement, not
implementation detail:

1. **No scroll write without a cause the user can name.** The complete
   set of writers: (a) placement on open, (b) user-clicked jump,
   (c) follow mode tracking the frontier, (d) anchor-preserving
   compensation (net visual delta zero). Anything else is a bug.
   All writes go through one chokepoint function in
   `runMapFollow.svelte.ts` with a caller tag (mirrors
   `utils/scroll/chokepoint.ts` discipline, without importing the
   timeline machinery — that package is virtualizer/spring-shaped and
   wrong-sized for this surface).
2. **Escape is event-sourced, never geometry-inferred** (verbatim from
   `utils/scroll/intent.ts:9-11`). Wheel-up, PageUp/ArrowUp/Home,
   touch-drag down, scrollbar-gutter pointerdown, middle-click — any
   of these disengages follow. A programmatic write can never
   false-escape because escape only listens to input events, and it can
   never be mistaken for input because inputs are the only escape
   triggers. Text-selection inside the map holds writes (same rule as
   the timeline).

   The corollary is that follow may not run with no listeners installed,
   and that is enforced by state rather than by a report. `attach()`
   waits a few frames for a late-binding scroller and then LATCHES the
   controller shut — `writeScrollTop` no-ops, follow disengages, the
   chip hides — before it throws. The throw alone changed nothing: the
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
   offset before the state flip, restore it after flush, pre-paint —
   the `preserveScrollAnchor` pattern, map-local. User-initiated
   expands keep the clicked row pinned. Because folds happen at the
   frontier and escaped readers are above it, this path is rare — but
   it is the difference between "solid" and "fights you", so it gets
   tests, not hope.
8. **Fold animation only when visible**: a fold whose region is
   outside the viewport applies instantly (no animation, one
   compensation). On-screen folds animate via `grid-template-rows`
   0fr/1fr (the mockup mechanism), gated by `motionReduced()`.

   This is a scroll rule, not a taste one, and it is the other half of
   clause 7. The height transition runs 200ms while the anchor
   compensation that cancels it is measured ONCE, at the flush — so an
   auto-fold above a disengaged reader drifts their viewport for every
   frame after the first. Instant makes the whole delta land inside the
   hold, which is the only moment compensation can see it.

   The decision is made where the state flips, in an `$effect.pre` keyed
   on `open`: pre-effects run before the DOM update, so the rect they
   read is the layout the fold is about to change. One rect read per
   toggle — no per-frame work, and no write, so clause 1's writer set is
   untouched. The arithmetic is `runMapGeometry.foldAnimates`, pure and
   directly tested; a scroller or region it cannot measure answers
   "animate", which is the harmless side (a visible fold that should
   have been instant is cosmetic; the reverse is the drift).
9. `overflow-anchor: none` on the overlay body scroller (doctrine:
   native anchoring fights owned compensation), and the overlay's
   scroll position **resets per level/run navigation** — fixing the
   existing sweep-leaves-stale-scrollTop defect as an adjacent fix.
10. **Jump chip**: `now ▸` chip appears when the follow target is
    off-screen or follow is disengaged; click = engage + glide. Sits
    outside the scroll container (the ScrollToBottomButton lesson,
    `MessageTimeline.svelte:710-717`).
11. `will-change` is never toggled; if the glide needs compositing it
    uses a static class or nothing (post-incident doctrine,
    `chokepoint.ts:179-199`).
12. **Container resize is a recomputation event, and rate-bound**: a
    `ResizeObserver` on the scroller re-runs the follow decision and the
    chip's geometry, rate-bound (≥100ms end-to-start) so live resizing
    never thrashes layout or scroll. While FOLLOWING, that means the
    frontier is re-tracked into the band — a reflow can leave it
    anywhere. What reflowed is not this clause's business: the fan
    columns wrap or scroll by CSS alone, and there is no "fan tier" for
    anything here to decide or observe.

    While NOT following it writes nothing, which is a deliberate
    amendment to this clause as first written ("wrap in the same
    anchor-hold as folds"). An anchor-hold compensates a layout change
    the map CAUSED; a container resize is one the reader caused, by
    dragging a window edge or the app changing size around them. The
    content does not grow — it reflows, and the browser has already
    chosen where the reader lands. Re-anchoring on top of that fights
    the drag: the viewport would shift with every intermediate size,
    each one a scroll write with no cause the reader could name (§9.1).
    The rule stands unchanged for fold, unfold and growth, which are the
    map's own mutations, and for the wholesale view swap of a refetch
    (§7, "transport gap"), which is the map's own too.

## 10. Transitions

- State changes = class swaps on nodes already in the DOM (CSS
  transitions on border/color, ~250ms). Never DOM insertion for a
  status change — ghosts are real elements from first render.
- Structural motion is exactly three cases: a branch column enters
  from the queued group chip (width slide, local to the fan), a
  finished branch folds into the done group chip, and a wave folds to
  its summary row while the next segment reveals. Each is one owned animation;
  nothing else moves. All gated by `motionReduced()`.
- Running glyph = `SteppedSpinner`; standing indicators otherwise use
  the ambientTicker marker classes if ever needed — no CSS `animate-*`
  loops (disarmed/incident history).
- The svelte flush caps and quiet-work rules stay untouched — nothing
  here adds per-frame reactive work; the 1Hz clock is the only timer.

## 11. Deletions, migrations, adjacent fixes

Deleted (no aliases, no compatibility shims) — **done**:

- `WorkflowRunTree.svelte`, `WorkflowRunTree.test.ts` — deleted.
- `utils/workflowRunTree.ts` — deleted OUTRIGHT, not hollowed out. A
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
  timeline node. The glyph map died with the tree — the map's glyphs
  live in `utils/workflowRunMapStyle.ts`.
- `loadWorkflowDetail`'s child-recursion role and
  `retainWorkflowDetails`' child-walk — done; the detail cache is
  root-only and the re-entrancy note in frontend/CLAUDE.md restates the
  new guard ("is the cache already exactly the root").

Adjacent fixes surfaced by the survey (small, shipped with this work,
called out for review as riders):

1. **Done** — `types/workflow.ts` `WorkflowItemStateEvent` gains
   `phaseId`/`attempt` (wire already carries them).
2. **Done** — `eventsTransportGap.ts` gets an explicit `workflow:` case
   (was: unknown-channel default — a real correctness hole for any
   workflow UI, not just the map).
3. **Done** — `overflow-anchor: none` on the overlay body scroller plus
   a scroll reset on level/run navigation (§9.9). The reset runs in an
   `$effect.pre` keyed on the level, so it lands before the new level
   paints and strictly before the map's own `placeOnOpen`, which must
   have the last word.
4. **Done** — the stale `phase N/M` on `WorkflowRunHeader`: the header
   derives its position from the map's frontier (wave part + deepest
   part) whenever the map view is loaded, and falls back to the SQL
   counter only when it is not. It ATTACHES the run map itself rather
   than peeking a key the sibling map component happens to hold — attach
   is refcounted, so the two share one RPC, and a header that read a key
   it never acquired would revert silently to the stale ordinal the
   moment the map beside it was reordered or lazily loaded. That attach
   effect is keyed on a `$derived` of the id, never on the `item` prop:
   the list cache mints a NEW row object per transition
   (`patchWorkflowItems`), and an effect tracking the object released and
   re-attached on every one — which resets the retry curve, so a failing
   fetch never backed off for as long as the run kept moving. Pinned by
   `WorkflowRunHeader.test.ts` "a new row object for the same run does
   not release and re-attach the map". Home rows keep the existing
   summary+`livePhases` pairing (default per §13). The SQL's
   alphabetical tiebreak defect is documented in
   `internal/store/work_items.go` above `workItemSummaryProgressJoin`
   rather than rewritten in passing — it stops mattering for the detail
   surface entirely.

Rider found during integration, outside this spec's surface: the
vendored `svelte-streamdown` Mermaid component shipped
`:global([data-expanded='true'])`, an UNSCOPED app-wide rule that pinned
any element carrying that ordinary attribute `position: fixed` at
`z-index: 2147483647`. The map's expanded wave row hit it and covered
the whole app, swallowing every click on the run detail's action row.
Scoped to `[data-streamdown-mermaid] [data-expanded='true']` and
recorded as entry 16 in `vendor/svelte-streamdown/DIVERGENCE.md`. The
map's wave row also moved off the shared attribute name to
`data-wave-expanded`: the scoping is the fix, and staying out of a
vendored stylesheet's namespace is what keeps the next component that
reaches for `data-expanded` from re-finding it.

Docs/tests bookkeeping:

- **Done** — UI-SPEC §4.2 rewritten for the map; §12 integration rows
  updated; this file is the map's spec of record.
- **Done** — e2e `workflows-overlay.spec.ts` migrated off the tree ids
  and onto map ids and map semantics, with new coverage for: a live run
  tracked from held to parked on ONE mounted detail (the event-patch
  path, no navigation, no reload), a thread opened from a map node
  (R3), and a soft stop landing on the loop foot of a wave chain.
- **Done** — §9 has e2e coverage as well as the controller's unit suite,
  and the split is not the one first planned here. The unit suite
  (`runMapFollow.svelte.test.ts`) owns the DECISIONS — escape sources,
  re-engage, glide retarget, band arithmetic — against a stated layout,
  because happy-dom has none. What only a browser can answer is what the
  anchor search finds in the REAL DOM: escape-then-hold-then-chip on a
  live run, and compensation across engine-driven growth above a reader
  parked at the tail. That second case earned its keep immediately —
  it caught `pickAnchor` stopping at a container that SPANNED the
  growth, which measures a delta of zero and compensates nothing. Every
  container between the scroller and the row does that, so the flat
  fixture the unit suite used could not see it and the whole
  compensation clause was silently inert in production.
- **Done** — projection unit tests are the bulk: wave flattening (incl.
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
- **Now marker uses `--accent`** — one deliberate positional accent;
  R1's amber/red meanings untouched.
- **Follow defaults confirmed** — ON for running runs, OFF for parked;
  any scroll input disengages for the visit; only the chip re-engages.
- **Follow priority confirmed** — needs-human first, else the deepest
  leaf with the most recent transition; all running branches render as
  running regardless of which one is followed.

Standing defaults (not blocking; surface before changing):

- Home/list stays as-is apart from the §11.4 staleness rider.
- Queued-vs-waiting: v1 renders `pending`+provider as "queued";
  wait-state events from the engine semaphores are a v2 addition if
  the distinction proves to matter.
