# Activity Runs

An **activity run** is one maximal stretch of consecutive activity rows:
tool calls, completions, terminal interactions, thinking, and the group cards
that sit on the same rail. It renders as a single timeline row that scrolls in
place and collapses to one line.

Designed 2026-07-20, shipped 2026-07-25/26. This document describes what is
in the tree; where the original design and the implementation disagree, the
implementation is right and the difference is called out under
[Deltas from the original design](#deltas-from-the-original-design).

## Why

- An upstream Anthropic bug frequently emits Fable prose as `thinking`
  items, so threads degenerate into screens of activity with sparse real
  prose. This is a presentation-side mitigation, not a fix for that bug.
- During activity spam the pane churns continuously: the last prose block
  flies off screen while nothing readable is arriving. A capped run holds
  the pane still. Prose stays put, activity streams inside the cap.
- Collapsed, a tool-heavy thread reads as prose plus one-line count headers.

## Shape

```
items → filterRedundantNotifications → groupItemsBySubagent
      → groupConsecutiveReads → patchStructuralTimelineNodeItemRefs
      → sliceRevealedNodes → groupActivityRuns   ← last
```

`groupActivityRuns` (`utils/activityRunGrouping.ts`) is the final pass, and
it must stay there. `patchStructuralTimelineNodeItemRefs` scans **top-level**
indexes for `group` / `wait_group`; once runs wrap those they are no longer
top-level, so the patch runs on the pre-run array and the run pass consumes
its result. Placing the pass after the reveal gate also leaves the streaming
smoother untouched: items reveal one at a time and the run is rebuilt from
whatever is revealed.

`revealedNodes` is the run-wrapped array. It stays the single index basis for
size priors, paging, window anchoring, restore, diagnostics, row-UI pruning,
`findTimelineNodeIndex`, and the template's index-keyed decorations. There is
no second array and no index skew.

Membership is `timelineNodeHasRail` verbatim, `RAIL_EXEMPT_PAYLOAD_KINDS`
(`proposed_plan`) included. Rail continuity and run continuity are the same
property, which is what makes the rail coherent as the run's collapse
control. Because the predicate reads live item state, a late-arriving
`payloadKind` can **split** a run mid-stream; the registry's membership rule
below is what keeps state attached across that.

One absorption rides on top of the predicate: a `notification` leaf with a
rail member before it joins the open run (`isAbsorbedNotification` in
`activityRunGrouping.ts`). Bells land at the current write head
(`backgroundCompletionTurnIndex`), which during a turn is inside the live
run. Before absorption every Monitor ping cut the run there, minting a
fresh tail id and remounting its rows from a one-row unfaded clip
(2026-08-22). A trailing bell stays absorbed so settling never pops it out;
a bell with no rail member before it renders standalone. The header line
counts absorbed bells as `N notification(s)`, ranked between tools and
thinking.

That is also why rail participation is part of `itemTimelineStructureKey`
(`utils/timelineStructure.ts`): a row leaving the rail changes which top-level
rows exist, so it has to bump `timelineRevision` like an insertion does.
Carrying it there rather than gating the projection on a membership scan is
what lets the whole pass stay `untrack`ed with no per-delta walk, and it is
the exempt-or-not bit only, never the payload kind, so the many payload
attachments a turn makes stay non-structural. No provider path flips a row's
rail membership today (triage attaches `proposed_plan` before the row's first
emit, on both providers); the key keeps the rule honest for the card-style
kinds the set invites, which arrive through the same late-payload-upgrade
pattern.

`nodeRole('activity_run')` is `'tool'`, so the `mt-4` tool↔text gap lands at
both prose↔run boundaries uniformly. It previously depended on which rail
kind happened to sit at the edge: `thinking` has role `'other'`, and
`isToolTextBoundary` suppresses the gap when either side is `'other'`, so a
block that opened or closed with thinking swallowed it.

### The node

```ts
interface ActivityRunNode {
  kind: 'activity_run'
  runId: string                      // registry-assigned, stable at both edges
  threadId: string                   // no anchor item of its own
  children: TimelineNode[]           // the wrapped rows, >= 1
  collapsed: boolean                 // resolved: answer, else default, else live
  live: boolean                      // holds the tail; new activity lands here
  atTail: boolean                    // last REVEALED node; wider than live
  mountedFrom: number                // the mounted row window
  mountedRows: number
  memberItemIds: readonly string[]   // positional identity membership
  summaryItemIds: readonly string[]  // current rows the header summarizes
}
```

Two facts about that shape are load-bearing:

- **Resolved state is ON the node**, not read from the registry at render
  time, because the row signature has to price a closed run differently from
  an open one and a moved window differently from a still one. A signature
  blind to either replays a measured height that no longer applies. `collapsed`
  arrives with liveness already resolved into it, so neither of them has to know
  what `live` means. `atTail` is on the node for the same reason: the
  controller's lifetime and the auto-collapse gate's skip both key on it, and
  stamping it once stops those consumers re-deriving "is this the tail" from
  a list they would each have to hold onto. `live` stays beside it as the
  strict withheld-aware fact, surfaced as the row's `data-live` attribute.
- **Liveness counts withheld nodes**, so it is a fact about the items rather
  than about what the reveal gate has let through. The pass runs after
  `sliceRevealedNodes`, so without `GroupActivityRunsOptions.withheld` a run
  whose closing prose is still behind the gate looks like the tail. That
  flapped (finished, live, finished) every time the gate opened, rebuilding
  the scroll controller each time (and, when this shipped with a settle
  animation, aborting it mid-flight). Withheld ACTIVITY
  does not count against it: those rows join this very run when the gate
  opens.
- **Counts are NOT on the node.** Per-tool counts, the failure marker, and
  the running label all move on ordinary streaming deltas, so baking them in
  would rebuild the virtualizer's data array on every chunk. The header
  resolves current items behind `summaryItemIds` and calls
  `components/chat/activityRunSummary.ts`, the same rule leaf rows already
  follow. Summary dependencies are separate from identity membership: a
  detached completion settles both its immutable launch header and its later
  completion-card header, but only the completion card owns that completion
  as an identity member.

A fifth `kind` had to be threaded through every node dispatcher.
Compile- or runtime-enforced: `timelineNodeKey`, `timelineNodeItemId`,
`timelineNodeRootItem`, `nodeContainsItem`, `descendantItems`,
`countDescendants`, `nodeSignature` (which *throws* on an unknown kind).
Silent if missed: `nodeRole`, `retainNode` / `retainActiveGroupKeys`,
`messageTimelineTrace`, the row-estimate table.

## Identity: `stores/threadActivityRuns.svelte.ts`

Runs are not stored entities; they are recomputed from scratch every
projection pass. The registry is the only thing that carries a run across
passes, and identity is the load-bearing part.

No member id is stable: lazy older-paging extends a run backward (new first
member) and the live-window prune trims its head, the second happening
mid-stream on exactly the long runs this feature exists to bound. Keying on
any member would remount the row and recreate its scroll controller mid-turn.
So the registry mints a `runId` once and migrates it by membership: an entry
sharing any member with the run being built lends that run its id. On a split,
the entry lands on the sub-run holding its previous first member; on a merge,
the entry whose member sits earliest wins.

A run can also vanish and come back. The prune can take every item a head
run had, and a thread switch drops the lot. Entries carrying explicit state
are **archived** on their way out (keyed on first and last member, LRU-capped
at 128 keys ≈ 64 runs) and revived by either of those edge members. Without
that, collapsing a noisy run and then paging, or switching threads and
returning, hands back a default run, the run-level form of the position loss
`utils/threadScrollSnapshots.ts` exists to prevent. A live entry always beats
an archived one.

Two properties of the archive are worth stating exactly, because both are
easy to assume wrong:

- **Keys are thread-qualified.** Item ids are unique only *within* a thread
  (the store's primary key is `(thread_id, item_id)`, and synthesized ids like
  `think:0:0` recur in every thread), so a bare id would let the incoming
  thread's first run revive the outgoing one's collapse state and scroll
  offset on a switch.
- **Only the two edge members can find a run again.** A reload landing on
  neither (a jump into the middle of a long run, whose window loads only
  interior rows) gets a default run. Accepted: keying every member would
  scale the archive with run length, and the loss is one collapse override.

Per `runId` the registry holds: the collapse override (absent → the setting),
the inner scroll offset plus the controller's escape flag, the mount window,
and a pending jump focus request. Session-only, matching item-expansion
leases; the setting is the durable layer.

`revision` bumps whenever a value `resolve` returns could differ. The
projection walk runs `untrack`ed (it would otherwise re-run on every
streaming delta), so it reads `revision` to know when a rebuild is owed.
Scroll snapshots deliberately do **not** bump it. They change every inner
scroll frame and nothing on the node depends on them.

The two readers that sit *outside* that graph are `scrollSnapshot` and
`windowAnchor`: their one consumer is the row's controller effect, which would
tear down and rebuild the spring every time the position or pin it writes
moved.

## Render

A header that is always there, and under it a clip that comes and goes. Closed
is the header alone; open is both, and `collapsed` on the node is exactly
which. Liveness is already folded into it upstream, see [A working run shows
what it is doing](#a-working-run-shows-what-it-is-doing).

**Header** (`ActivityRunHeader.svelte`) is one line whose chevron sits centred
on the rail's x, OUTSIDE the rail itself (the border spans only the clip below,
so the header is not walled behind a second edge and a collapse reads as the
rail folding up into its control): a chevron, per-tool counts (count-descending, thinking last: `14 Bash, 6 Read, 3 Edit, 9
thinking`), a failure marker when a member failed, and the display label of
anything still running. Claude keeps its human-readable native tool names;
Codex protocol categories use the same aliases as their rows, capitalized for
the header (`command_execution` → `Bash`, `file_change` → `Edit`,
`collab_agent` → `Agent`), and nameless terminal interactions read `Wait`.
Collapsing must hide neither that something went wrong nor that something is
still going; ticking counts alone do not answer "what is it doing". All of it
stays while the run is OPEN, because an open run mounts
only `activityRunWindowRows` of its rows. A failure or a running tool can sit
outside that window exactly the way a closed run hides one, and the header is
the only place the run's full size can be stated at all.

It renders in both states, and that is a fix rather than a decoration. It used
to render only while collapsed, so expanding a run removed the element the
reader had just clicked and left the rail, invisible by construction (see
below), as the only way back. Three other things now depend on the header
never moving: the row estimate is `header + clip-when-open` rather than three
separate shapes, the structure signature has two letters rather than three, and
a run collapsing off-screen changes nothing about where its header sits.

**The clip** holds the rows inside a height-capped box:

- `max-height: min(50vh, 18rem)`, inflated to `calc(… + Npx)` by expanded
  child bodies (below). `max-height`, so a run under the cap renders exactly
  as it did before this component existed, which is why no row-count
  threshold is needed.

  The rem half is derived, not chosen: `ACTIVITY_RUN_CAP_ROWS` (8) ×
  `ACTIVITY_RUN_ROW_REM` (2.25). The cap is a height only because CSS has no
  "eight rows" unit, and editing it as a bare rem value is how it would stop
  meaning a row count. The row figure is a typical activity row, NOT the
  tightest one. `ROW_KIND_ESTIMATE_PX` prices a bare one-line `tool_call`
  near 25px because it is a placement floor, and sizing the cap off a floor
  shows a third fewer rows than it promises. In rem so the cap tracks the
  font-size setting. The `50vh` half is now purely the short-viewport guard:
  it only wins below a ~576px window.
- A **top fade**, the conversation's own effect at run scale: a 24px gradient
  overlay (`var(--surface-0)` → transparent), so rows dissolve as they rise
  out of the clip instead of meeting a hard edge. Same overlay-not-mask
  technique and for the same reason as `MessageTimeline`'s `TOP_FADE_PX`: a
  mask on the clip rasterizes a full clip-sized texture on every streaming
  repaint. It paints only while `scrollTop` is off the top edge: a run resting
  at its first row has nothing above to dissolve, and tinting that row would
  just make it look dimmer than the rows below it. Paint-only, so it never
  touches `scrollHeight`/`clientHeight`/`scrollTop` and stays clear of the
  controller. It needs no scrollbar-safe inset (the conversation's does): the
  overlay bar hangs outside the clip's right edge. All three scroll-surface
  copies use `.scroll-top-fade`, whose extra opaque pixel starts
  above the clip so independently snapped WebKit compositor edges cannot expose
  an unfaded row; that overdraw does not shorten the declared 24px fade.
- `overflow-y: auto`, `overflow-x: hidden`: a wide preview inside a tool row
  must not raise a horizontal bar at run level, which would consume *height*
  and shift every row below.
- `overscroll-behavior` stays **auto**. Chaining out at the inner edge is
  wanted; gesture correctness comes from attribution, not from blocking the
  chain.
- `overflow-anchor: none`. WebKitGTK 6.0 does not implement it at all
  (`CSS.supports('overflow-anchor', 'none')` → false, measured against
  2.52.3), so the manual prepend compensation is what does the work in the
  desktop webview. The declaration still matters: a remote browser client on
  Blink honors it, and there the browser's own anchoring would fight the
  compensation. Same reason the outer scroller carries it.

The rail belongs to the run: one continuous border for the block of rows,
doubling as a second collapse target (an absolutely positioned hit strip in the
gutter, consuming no width). The per-row `isRail` styling in
`MessageTimeline`'s wrapper retired. It spans the CLIP only. The header sits
outside it with its chevron marking the rail's x, and the strip folds with the
clip, since a collapsed run has no edge left to click and the header is the
whole run then.

Consuming no width is what makes it invisible, so it cannot be the only
control. The run's own header is the discoverable one, in both directions, and
the rail is the larger target for readers who find it. Both call the same
`toggle`.

Which is also why the rail is **pointer-only**: `aria-hidden="true"` with
`tabindex="-1"`, both, because hiding a focusable element is its own defect. The
header is always present and already carries the disclosure contract, so ARIA on
the rail would buy a keyboard user a focus ring on a transparent 16px strip and
a screen-reader user the run's state announced twice from two buttons naming one
region. An invisible duplicate is worth having for the mouse (the whole block
reads as one thing, so clicking its edge should fold it) and worth hiding from
everything else. It was a labelled button while the header was conditional,
because then it really was the only control left when a run expanded; that
premise is gone.

The chat header's **collapse-all toggle** (`activity-runs-toggle` in
`ChatHeaderActions.svelte`) is the THREAD-level bulk action, one button whose
direction comes from `activityRuns.bulkCollapsed`.

### The header speaks in tool hues

Each term of the count line wears its own tool's colour (`14 Bash` in the
terminal hue, `9 thinking` in reasoning's) from the same `--ico-*` tokens the
run's own icons carry. The line describes a block the reader has already seen
in colour; a grey tally throws that recognition away for no gain. The count
stays muted: the colour identifies WHICH tool, and tinting the number too would
only dilute it.

`TOOL_KIND_COLOR_CLASS` lives in `toolCardHeader.ts` for this. The hue belongs
to the tool KIND, not to the icon (the header paints text where no icon
renders), and a second copy would let the two disagree about what colour Bash
is. `activityRunSummary.ts` therefore lives beside the header in
`components/chat/`: it composes provider-aware display labels with the
canonical classifier and carries the resolved icon kind on each count entry.
Thinking and tool buckets have distinct presentation identities, so a tool
named `thinking` cannot inherit reasoning's hue or sort position.

### A working run shows what it is doing

Collapsing a thread should compress its history, not blind the reader to the
work in front of them. So a run whose collapse comes from a DEFAULT renders open
while it is the timeline's newest revealed run: the default (the
`activityRunDefault` setting, or the thread's collapse-all state) says how a
run should sit once the reader has moved past it, and the newest run is the one
in front of them.

Four facts, ranked once, in the registry
(`ActivityRunIdentity.collapsedFor(runId, atTail)`):

1. **The reader's answer for this run** wins outright. Including against
   tail-ness: somebody who collapses the run they are watching means now, and
   the clip going away is the only evidence the click landed at all.
2. **Tail-ness**: the timeline's newest revealed run renders open when nobody
   has answered, and rendering open is RECORDED (`openedLive` on the entry),
   because it is a commitment that outlives the moment. Tail-ness rather than
   the node's `live`, deliberately (2026-08-18): `live` goes false as soon as
   the run's closing prose EXISTS, revealed or not, so a fast run whose next
   section arrived behind the reveal gate before the run's first projection
   pass was never once seen live. No hold was ever recorded, and the run was
   born collapsed in front of the reader who watched it stream (the
   sampled-liveness race). The revealed tail run is what the reader is
   watching whether or not the wire has raced ahead, and the live run is
   always the tail run, so the widening only ADDS holds. `node.live` itself
   keeps the strict withheld-aware definition. Widening IT is what flapped.
   Since 2026-08-19 the scroll controller and the auto-collapse gate key on
   tail-ness too (`node.atTail`, stamped beside `live`); what remains on the
   strict field is the row's `data-live` attribute, the forensic seam that
   proves the withheld window exists.
3. **The recorded hold.** A run that opened as the newest keeps rendering
   open after prose displaces it, until the timeline's auto-collapse gate
   releases it (below) or the reader answers directly.
4. **The thread's collapse-all state**, then the setting behind it. Both are
   defaults (`setAllCollapsed` sets the thread's and DROPS per-run overrides,
   which is what lets a later flip reach every run), so both are answers about
   how runs sit, not about the one that is working or the one nobody has
   moved past yet.

Everything downstream then reads one field. `collapsed` on the node means
"renders without its clip", and the row's template, the structure signature and
the row estimate all take it at face value; none of them looks at `live`. That
ranking is the fix for a real defect: while liveness was an OVERRIDE applied at
each consumer, collapsing the live run moved the chevron and nothing else, and
the header claimed collapsed over a clip full of streaming rows.

### Auto-collapse happens off-screen, or not at all

The transition nobody asked for (the run settles and its collapse would fall
back to a `collapsed` default) is not allowed to happen in front of the
reader. Snapping shut on the settle frame removes up to a viewport of content
under the eyes of whoever watched the run stream. Animating the removal was
built first and rejected in use: the clip is capped near half the viewport,
and no duration or easing makes losing that much height pleasant while you
are looking at it. The change every physics tweak was softening was one the
reader never wanted to see. (Survey result, for the record: no shipping
desktop agent UI solves this either; the closest issue upstream was closed
"not planned".)

So the collapse is not softened, it is **scheduled**. The
`openedLive` hold keeps the settled run open, and a gate on the timeline
(`components/chat/timelineActivityRunAutoCollapse.ts`) releases it only when
applying the default is provably invisible and provably unwanted by nobody:

- **Fully outside the viewport**, by a 48px margin
  (`utils/activityRunAutoCollapse.ts`). A run a scrollbar-nudge from view
  must not be found mysteriously collapsed.
- **More than a viewport behind the thread's tail**, measured in content
  space from the run's bottom to the end of the timeline. Distance from the
  TAIL, deliberately not "has left the viewport": a reader who scrolls up to
  check something and returns to the bottom must find the latest runs exactly
  as they left them. Their viewport went somewhere; the conversation did not
  move on.
- **Nothing inside is engaged.** The reader has not pinned the run's mount
  window or scrolled up inside it (`windowAnchor`, the snapshot's `escaped`
  flag, registry facts precisely because the rows unmount), and has not
  explicitly expanded anything within it (`hasUserExpansionWithin`, below).

A failed member deliberately does NOT hold the run open (an earlier exemption,
removed 2026-08-18): the collapsed chip's failure marker (`activityRunSummary`)
keeps the failure visible either way, so pinning a whole run of history open
because one command errored restated the chip at the cost of the compression
the collapse exists for.

Released, the run takes the defaults instantly (no animation, because by
construction nobody can see it happen), and its inner position is forgotten
the same way a clicked collapse forgets it. The gate runs on the row-UI
prune's cadence (structural changes + scroll end, one-tick debounced, never
per scroll frame), reads only the engine's cached geometry, and pays nothing
when no holds exist. A hidden document invalidates that cache as proof of
invisibility: provider events and Svelte flushes can continue while rAF and
ResizeObserver delivery pause. `timelineVisibilityGeometry.ts` therefore
closes only this pass from the hidden edge through resume, and reopens it only
when MessageTimeline's existing virtualizer subscription publishes a new
visible, post-flush content-geometry sample. The edge adds no timer, layout
read, observer, or normal-streaming schedule; the sample schedules one pass
only when it clears the barrier. No sample means deliberate over-retention —
an old run stays open until later geometry rather than folding against an
unproven viewport.

The gate also stands down while a reader-visible glide is in flight or armed
(`PaneScrollController.autoScrollInFlight`): a release
routes through the bottom-held transaction, whose pinned restore is a
direct write, and landing that mid-glide, or in the armed gap before the
spring's first frame, would turn the animation into a snap to the bottom.
Deferring loses nothing, because the settle that ends the glide synthesizes
the scrollend that re-runs the gate in quiet. The stand-down can only see
motion that exists when the pass runs (an append can still land in the
flushes between the release and its restore), so the transaction also
restores with takeover `'yield'` (`PreserveViewportBottomOptions`): the
restore's `requestBottom` stands down when the append has armed the
structural spring, which then glides the new row in
(regression: appendAfterQuiet.browser.test.ts). Runs that never opened live
(history loads, revived archive entries, thread switches) carry no hold and follow the defaults
outright, which is why a loaded thread does not arrive as a wall of clips.

The engagement peek (`ThreadPane.hasUserExpansionWithin`, on the row-UI
registry) answers "did the reader explicitly expand anything in these items":
diff cards overridden to expanded, subagent / wait / read groups, payload
bodies whose default is collapsed. It shares `expansionSignature`'s "user
deviations from default" contract, which is what makes it safe where raw
`expandedPx` was not: with `collapseDiffPreviews` off, diffs render expanded,
so a pixel-based guard would have pinned every run holding an edit forever,
while a deviation-based one counts only what the reader opened. The same
rule reaches expansion entries through `autoExpands`: an entry created with
`loadOnMount` (auto-loaded diff bodies, plan cards) flips expanded with no
reader involved, so those entries are skipped wholesale. An override back to
COLLAPSED is an answer about that card, not engagement.

The peek's scope is the run's RENDERED items
(`renderedItemIdsWithin(run.children)`), not its identity membership:
`memberItemIds` deliberately stops at group anchors, but a reader's
expansion can sit on any row a group renders: a wait group's children or
its folded completion, an opened subagent card's transcript. Failure keeps
the membership scope, matching what the collapsed chip summarizes.

The release itself is batched: the gate hands the whole eligible set to
`releaseOpenedLive(runIds)`, and the registry runs it inside ONE
`withViewportBottomHeld`, the same anchored transaction every
reader-clicked collapse uses, owned by the registry's mutators rather than
their callers. For a bottom-pinned
reader the browser's own `scrollTop` clamp already cancels a shrink above the
viewport; the transaction is what makes zero motion deterministic for
everyone else. A reader parked mid-list below a released run gets their
anchor row put back after the flush instead of trusting the engine to
compensate an estimate-driven height change above them. The zero-motion claim
is asserted per-frame in real Chromium, for a mounted run and for one whose
row has left the virtualizer's buffer entirely
(`activityRunAutoCollapse.browser.test.ts`).

The registry learns about clip presence from the same resolution.
`saveScrollSnapshot` refuses a save for a run with no clip. The refusal has
to be central, because the row that just lost its clip tears it down THROUGH
that method, and the `clipOpen` flag it checks is recorded by `collapsedFor`
itself: the resolved answer IS what the row renders, and only the resolution
sees all four facts. `forgetInnerPosition` still closes it eagerly on the
same flush as the collapse, so a save arriving between the collapse and the
next projection is refused too.

### Collapsing all

`setAllCollapsed` sets a per-thread default and drops the per-run overrides
that contradict it, in the live entries **and in the archive**. An archived
override would revive against the bulk state as the reader pages, which would
make "all" untrue a scroll later. Runs projected afterwards follow it too, so
loading older history cannot reintroduce expanded runs.

The control renders from that default rather than from a survey of the
rendered runs. A survey would have to answer "all of *which* runs" (only the
loaded window holds any) and would relabel itself as older history paged in.
Reset by `clear()`: it is a view action taken on one thread, and carrying it
into an unrelated one would surprise. A run the reader toggles afterwards
takes its own override again, on top.

Because it is a default rather than a per-run answer, it does not close the run
that is still working. That one keeps showing its work (it re-records its
open-because-live hold on the next pass) and is auto-collapsed off-screen once
it settles, like any other held-open run. The bulk action does retire every
hold it can reach: "all" includes the settled runs those holds were keeping
open. This follows from what the action IS: it drops overrides
instead of writing them, precisely so a later flip still reaches every run, and
a fact that governs runs it has never seen governs how they sit rather than what
the reader wants from one of them. Collapsing the live run is one more click on
its own header, and that click is instant.

### A toggle opens upward

Every path that flips collapse state (the header, the rail, the chat header's
bulk toggle, and the auto-collapse gate's batch release) runs inside
`withViewportBottomHeld`, which holds the viewport's BOTTOM edge across the
change. The hold lives inside the registry's mutators, not at the call
sites; the one hold-free write is `expandForReveal`, for jumps that retarget
the viewport themselves (see
[frontend-scroll.md §Run Height Changes](frontend-scroll.md#run-height-changes)).

Two things follow, and both are the reason it exists. A run expands over the
rows above it instead of shoving the rows the reader is looking at down the
page (the change lands on the side of the viewport they are not reading), and
collapsing gives that height back the same way. And the spring is paused for
the duration: a toggle while bottom-pinned would otherwise reach the scroll
controller as content growth and animate across the whole delta, which for a
collapse-all is most of the conversation.

The one thing it cannot do is open upward past the top of the thread. Opening
upward spends `scrollTop`, so a run near the start of a short thread clamps at
0 and the remainder falls downward.

### Collapsing forgets the reading position

Collapsing drops the run's scroll snapshot **and** its window anchor
(`forgetInnerPosition`). Both describe where the reader was *inside* the run,
and collapsing removed the inside: restoring the offset reopens the run
mid-list (behind everything that arrived while it was closed, on a live run),
and a surviving pin would hold the mounted window away from the tail the clip
now lands on, so the newest rows would not even be mounted. An expanded run
therefore opens exactly where a never-scrolled one does.

The row-count override is deliberately kept: that is how many rows the reader
asked to mount, which closing the clip does not contradict.

### A resting clip is held on its last row while content settles

The position write happens once, at the instant the clip mounts, and the rows
inside are not finished then. Payload bodies resolve, highlight spans land, a
row remounts already expanded from its lease and lifts the cap. Each grows the
run AFTER the write, and `scrollTop` does not follow on its own, so a run the
reader just opened drifts out from under them. Barely visible expanding one
already-measured run; unmissable expanding many at once from the header, where
none of them is measured.

`ActivityRun.svelte` therefore observes the clip AND its content (the two ways
the gap opens are unrelated: content growing under a fixed clip, and the clip
growing when cap inflation gives it more room than its content needs) and
re-writes the bottom while `followingBottom`.

That flag is **stored, not measured**, and the difference is the whole
mechanism. "Is the clip at the bottom right now" is unanswerable during a
settle: the clip is written to the bottom, a row inside then resolves and grows
it, and the `scroll` event from the write is delivered after that growth,
correctly reporting a position that is no longer the bottom. Re-deriving from
it dropped the follow on the very first row to resolve, which is what left a
run reopened by the header's collapse-all sitting near its top and staying
there. So growth never clears the flag; only a reader gesture does
(`readerScrolling`, armed by wheel / touch / key / bar drag), and every write
the component makes states the flag rather than measuring it, via
`positionWritten(clip, following)`. The one thing still read back from live
geometry is the top fade, which is a pure function of it.

The same fact is what the scroll snapshot's `escaped` field carries, from
whichever half of the run owns it: the tail run's controller as
`escapedFromLock`, a run without one as `!followingBottom`. A run changes which
half owns it when a newer node displaces it, so the snapshot must not care which
wrote it.

Only for a run with no controller: the tail run's spring owns bottom-following,
with intent handling this cannot see, and a second pinner would contend for the
same pixels.

Both halves are owner-driven for the overlay scrollbar. Its
`ownerDrivenPosition` answers `!escapedFromLock` while the controller exists
and `followingBottom` once it does not, so neither the spring's writes nor
the settle observer's bottom-hold (nor the browser's clamp when content
shrinks under a resting clip) show the thumb. Only positions the reader
produced do. Answering for the controller half alone made every
settle-observer write flash the bar as if the reader had scrolled.

This **narrows accepted tradeoff 4** ("a historical run does not chase when it
grows") rather than reversing it. Growth is followed only while the reader is
resting on the newest row, the one position where following is what they are
looking at. A run they scrolled inside still never moves under them, which the
browser suite asserts alongside the following case.

`saveScrollSnapshot` refuses a save for a run with no clip, and that refusal is
load-bearing rather than defensive. A run losing its clip tears that clip
down *through* the method, and a detached element reports `scrollTop`
0, so without it every collapse-then-expand would reopen the run at its first
row (observed).

### Expanded bodies lift the cap

An expanded payload is not activity. It is content the reader explicitly
asked for, so the cap grows by exactly what expansion added and reading a
diff inside a run never means scroll-within-scroll.

`observeActivityRunExpansion` (`utils/activityRunClip.ts`) pairs a
MutationObserver on `aria-expanded` (which bodies count) with a
ResizeObserver on those bodies (what each contributes). Bodies are found
through the disclosure contract (`aria-expanded` + `aria-controls` on a
`TranscriptDisclosureHeader`) rather than a marker attribute rows have to
remember: a body that skipped the query would be an accessibility defect
first, so a new expandable body cannot silently opt out of the cap while
staying correct for a screen reader. Ids resolve **within the clip**, so one
run's open diff cannot lift another's cap, and a nested body inside an
expanded ancestor is dropped because the ancestor's height already includes
it.

"What expansion added" is the body's height **minus its collapsed
baseline**, because two body kinds exist. Mount-on-expand bodies (tool
payloads, subagent cards) are absent while collapsed: baseline zero, whole
height counts. Always-mounted bodies (the tail-clamped reasoning rows, a
command result's output pane) show a clamped preview while collapsed; that
preview is activity already inside the base cap's budget, and counting the
whole expanded height grew the run by the preview's height on top of what
the reader's expansion actually revealed. Visibly, expanding a short think
block added no height to the row while the run grew by three lines. The
baseline comes from `data-collapsed-lines` when the body declares a fixed
line clamp (exact at any moment: `min(height, N lines)` is what collapsing
would show, so it cannot go stale while an expanded body keeps streaming),
else from the height last measured while the body's disclosure was
collapsed (the content-dependent previews, recorded per body *element* in
a module-level WeakMap, because the run recreates its observers on every
mounted-set change and the baseline must survive that for a body that can
no longer be re-measured). Collapsed bodies are resize-observed too: their
current size is tomorrow's baseline, and for a body whose trigger remounts
on toggle (a command result swapping its load control) the content swap's
resize is the only signal the toggle emits.

### Scrollbar width: hard constraint

The clip's scrollbar consumes **zero layout width in every state**. It
appears and disappears constantly here (content crossing the cap, the cap
inflating, a chunk mounting, a run toggling), and any width it took would
re-wrap the run's text on every one of those transitions.

`scrollbar-gutter`, the outer scroller's fix, does not transfer. `app.css`
styles `::-webkit-scrollbar` globally, so this is a classic space-consuming
bar; measured in WebKitGTK 6.0 (2.52.3), a single-edge `stable` gutter is
byte-identical to no gutter at all (content is 45px overflowing and 50px
idle either way), so a non-shifting native bar needs `both-edges`, 20px,
whose left 10px pushes the run's rows off the rail the run itself draws. The
outer scroller could afford a gutter as the centered column's *parent* (and
used one until 2026-08-25, when it adopted the clip's overlay-bar approach);
the clip sits inside `mx-auto max-w-[62rem]`, so a gutter there insets the
run's rows relative to the prose above and below. That reads as a card, and
the run is a window onto activity, not a card.

The shift is self-inflicted, and worth knowing when this comes up again: with
no `::-webkit-scrollbar` rule at all, WebKitGTK's own bar is already an
overlay and reserves 0px in every configuration. The global 10px rule in
`app.css` is what converts every scroller in the app into a classic bar.
Scoping that rule per platform is the general fix and is out of scope here.

So the bar is suppressed on the clip (`scrollbar-width: none` for the
standards path, `::-webkit-scrollbar { width: 0; height: 0 }` for WebKit,
both of them, because the two engines honor different ones; via the shared
`.pane-scroll-surface` class since 2026-08-25, when the pane scrollers
adopted the same suppression plus `will-change: scroll-position`, per
`app.css`) and the affordance is rendered out of flow:
`components/shared/OverlayScrollbar.svelte` over
`utils/scroll/overlayScrollbar.ts`, absolutely positioned in the column's
existing `px-6` padding. Drag uses pointer capture so it survives the pointer
leaving the strip, and one pointer owns a drag until it ends; a track click
pages toward it; the strip is `touch-action: none` so a touch drag is the
control's gesture rather than a native pan of the surface behind it; opacity
follows scroll activity, suppressed while the position is the owner's
(`ownerDrivenPosition`) so a run that auto-follows for a whole turn does not
hold a permanent bar.

A wheel over the strip is the bar's to apply. It sits BESIDE the clip, not
inside it, so the notch would otherwise bubble to the conversation, which
scrolls *and* reads the gesture as the reader leaving the bottom. The bar
applies the delta to the clip, states the same intent a drag does, and takes
the event out of the tree; at the clip's own edge it does none of that, so the
gesture chains outward exactly as a nested box's does (same `canConsumeDelta`
as `wheelAttribution.ts`, so the two cannot disagree about where an edge is).

A zero-width bar makes `offsetWidth - clientWidth === 0`, so `intent.ts`'s
geometric scrollbar-gutter hit test can never fire for the clip. That is the
correct outcome (no false positives from a bar that is not there), but it
means a drag has to state its intent rather than have it inferred:
`pointerdown` → `setEscapedFromLock(true)`, and a release at the bottom
re-sticks via `markAtBottom()`. That matches the package's own rule that
intent is event-sourced, never geometry-inferred.

The geometric form of this guard is unobservable in the browser project as
configured: measured, Chromium reserves 10px for a classic bar when headed and
0px when headless, and `vitest.config.ts` runs the whole project headless. So
it is a headless artifact rather than a Chromium one. Running that one file
headed would close the gap, and needs a display (WSLg has one, CI may not).
See [Verification](#verification).

## The inner controller (tail run only)

Only the run holding the REVEALED tail (the run that IS the last node of
`revealedNodes`, not merely the last run in it) gets a
`createUseStickToBottomController`. The lifetime keys on the node's `atTail`
stamp, deliberately NOT on `live`: `live` additionally requires that nothing
foreign waits behind the reveal gate, so it ends the moment closing prose
arrives on the wire, mid-stream from where the reader sits. Keying the
controller there tore the spring down under a still-streaming thinking tail,
cancelled its glide in place, and the settle observer then snapped the whole
remaining distance in one frame on the next delta (the 2026-08-19 in-run
jump; the overlay scrollbar flashing on those unclaimed writes was the
witness). Tail-ness ends when the reader can SEE the displacing node, by
which point the run is quiet and the spring idle. The same widening the
collapse resolution took for the same race, one day earlier.

Prose REVEALED after a run still closes it: the next activity row starts a
new run, so a run with anything visible below it can never grow again. Since
a settled turn usually ends `[…, activity_run, assistant_text]`, scanning
backward past revealed prose would hand nearly every thread's last run a
controller it can never use. Same factory, spring constants, motion floor,
and scrollTop-only glide as the main pane, so a streaming run feels identical
to a streaming thread. Historical runs are plain `overflow-y: auto` with a
restored `scrollTop`: they never chase, so a controller each would be a
spring, an observer set, and intent listeners per run in the buffer for
physics only one of them can use.

A detaching controller hands off both halves of its follow state, after
`detach()` so no intent machine mistakes the write for a gesture. A
controller that was still following writes the clip to its bottom
(`clip.scrollTop = clip.scrollHeight` through `positionWritten`); in the
ordinary handoff that write is a no-op (the run is quiet by reveal time),
and the residual case it exists for, a glide genuinely in flight at reveal,
lands (instantly) at the bottom the reader was already gliding toward
instead of parking short and snapping later. A controller that was ESCAPED
states that too (`positionWritten(clip, false)`): the controller sees
escapes the component never gets a gesture for (selection auto-scroll,
middle-click autoscroll, a scrollbar drag), and a stale `followingBottom`
would let the settle observer pin the escaped reader back to the bottom.
The saved snapshot takes the same fact (`escaped` from the controller when
one existed, `!followingBottom` otherwise). Skipped when the clip is dying
(collapse, destroy). The clip carries
`data-scroll-owner="controller" | "settle"` so a trace or screenshot names
which half owns the position.

- It leaves `externalContentGeometry` unset. There is no virtualizer inside a
  run, so the controller's own contentEl ResizeObserver is the right geometry
  source, following the `ChannelView` precedent.
- That source is the CONTENT box only, so a clip-VIEWPORT change — the cap
  lift retracting when a body inside collapses, the `50vh` half on a window
  resize — must reach the controller separately: the component's clip
  ResizeObserver calls `observe('composer-geometry')` (escape-aware,
  live-capable), the same seam the conversation uses for its composer.
  Without it, a collapse's two layout passes (content shrink, then the
  cap retraction one RO round later) race: the shrink delivery re-pins
  against whichever cap is current at delivery time, and when the
  retraction resolves last the grown scrollable range is a delta no
  content delivery ever reports — the pinned run strands the retracted
  height above its newest row (2026-08-31, "the collapse doesn't collapse
  in the right direction").
- `liveContentActive` wires to `pane.lastLiveContentAt` through
  `isLiveContentActive`.
- It **never** calls `pane.attachScrollController`. That slot is
  single-occupancy and belongs to the timeline.

The load-bearing property: a capped run's *outer* height changes only on
explicit events (growth toward the cap, item expansion, a collapse toggle),
never from inner streaming. That is what keeps the outer engine quiet and the
prose stationary, and it also keeps `rowDelta === 0` for the straddling row
during pure inner scrolling, so the reading-anchor measurer from `7f4b626d`
is never called against inner movement.

Inner position survives the virtualizer evicting the row: controller lifetime
and position persistence are ONE effect, because the saved snapshot carries
the controller's escape flag and splitting them would make the saved value
depend on which teardown Svelte happened to run first. The snapshot is also
written on every inner scroll, not only at teardown. A thread switch clears
the registry synchronously with the data change, well before Svelte tears the
row down.

## The mount window

A run is one virtualizer row, so the DOM bound the virtualizer provides at
top level has to be re-established inside it: without a window, a 500-row run
would mount 500 rows the moment its single row entered the buffer.

The window is `(rows, startItemId)`, a size and an **item id**, never an
index, because both of a run's edges move and an index would silently slide
the window across its content. A null start means "the run's tail", which is
the default.

`utils/activityRunWindow.ts` owns the math; the registry resolves the pair to
`(mountedFrom, mountedRows)` per pass, dropping an anchor whose row has left
the run and clamping one too late to fit a full window.

### Following the tail is a fact about the reader

A tail-following window drops one head row for every row appended, an
implicit head trim. Under a reader who has scrolled up inside the clip that is
exactly wrong: the rows they are reading slide up by a row height per append,
and the one they were reading eventually unmounts from under them.

So the row, not the registry, decides. While the inner controller is escaped,
`ActivityRun.svelte` pins the window to its own head row
(`setWindowAnchor`); new activity collects behind the `N later` boundary
instead. Returning to the clip's bottom re-sticks the controller and releases
the pin, and the run resumes following. A jump escapes deliberately, so it
freezes the same way and releases by the same gesture.

Both directions are load-bearing, and the release is the one that bites: a pin
left behind after the reader returns would strand a still-streaming run behind its
boundary. The window is deliberately **not** released
by geometry. An anchor means "the reader is up here", which is not a question
the tail's position can answer.

Historical runs have no controller, so they never pin themselves, but a jump
can pin one, and that pin is the only record of the reader's position a
controller-less run can keep. So it is carried the way the escape flag is
carried across a remount: a controller built for a run whose window is
already pinned starts escaped. Without that, a historical run becoming the
live one (the prose after it reverts, a queued item is withdrawn) would hand a
fresh controller a clean flag, and this rule would read it as "the reader is
at the bottom" and drop them at the run's tail.

- Default size is the `activityRunWindowRows` setting (30, clamped
  `[10, 200]`), sized to overfill the cap.
- `· · · N earlier` / `· · · N later` boundaries mount one more chunk (25
  rows). The earlier edge compensates the prepend manually: two reads and a
  write after the DOM has the new rows and before the frame is visible. The
  later edge needs no compensation, because rows below the reading position
  move nothing above it. The head's OTHER direction is compensated too, for a
  different reason: see "An appended row glides" below.
- **The earlier edge also pages in on scroll**, so browsing back through a
  long run is one continuous gesture rather than scroll-click-scroll. The
  boundary stays a button. That is how a reader jumps a chunk without
  scrolling for it, and it is the only affordance the later edge has (that
  edge resolves by returning to the clip's bottom, which releases the window
  pin). `activityRunShouldMountEarlier` requires the clip to be scrollable by
  more than the 96px runway, which is what keeps the trigger from overriding
  `activityRunWindowRows`: a window whose rows all fit under the cap rests at
  a `scrollTop` already inside the trigger zone, and without the guard it
  would page chunk after chunk in at mount time until the content overflowed.
  Not scrollable means there was no gesture to act on. The two paths share one
  in-flight guard. Overlapping mounts would each measure a `scrollHeight` the
  other is about to change and compensate by the wrong amount.
- **The trigger arms on the gesture, not on the geometry it produced.** A
  wheel, a touch drag, a key, or an overlay-bar drag arms it; every position
  the run writes itself (`positionWritten`) disarms it. Without that the mount
  write is indistinguishable from the reader arriving at the top: it aims at
  `scrollHeight`, but the rows inside are not measured at that instant, so it
  lands inside the runway, pages a chunk in, and the compensation that follows
  strands the reader up there with the settle observer switched off. Same rule
  the scroll package states for the conversation: intent is event-sourced,
  never inferred from where the surface ended up.
- **No head trim.** Growth cannot accumulate DOM on its own: the window is a
  window, not a high-water mark, so a run that streams to 500 rows still
  mounts `mountedRows` of them, and a jump relocates the window without
  enlarging it. The only way past the window is the reader asking for another
  chunk, and trimming that back would revert an explicit action. A short run
  whose boundary is visible without scrolling would flash rows in and drop
  them in the same frame. What they asked for stays until the run unmounts.

Item state is untouched by all of this: the store's timeline window, item
expansion leases, and the payload LRU behave exactly as before. The mount
window governs DOM only.

### An appended row glides, even where the window slid under it

The implicit head trim above has a second victim: the reader who **is**
following. A tail-following window starts at `children.length - rows`, so an
appended row drops one off the head in the same flush: the clip's content
shrinks by the dropped row and grows by the new one.

That is worse than a stationary-content problem. With the two rows roughly the
same height the content's TOTAL barely changes, so the controller's geometry
observer reports a delta near zero, no spring has anything to chase, and the
rows on screen are instead displaced upward by a row height in one frame. A run
glides through its first `mountedRows` rows and starts teleporting from the next
one on: streaming tool calls jumped while thinking text growing 1→2→3 lines
(growth with no slide) glided throughout, which is exactly how it was reported.

`ActivityRun.svelte` compensates the head advance in two halves of one flush:

- `$effect.pre` sees the new window against the DOM that still holds the old
  one (the only moment the departing rows can be priced) and records where the
  incoming head row sits in the clip's viewport.
- The post-flush effect puts that row back where it was
  (`activityRunScrollTopHoldingRow`), which leaves the appended row exactly one
  row below the clip's edge, and then states the growth the observer could not
  see: `markStructuralContentPending()` + `observe('live-content')`. The spring
  glides that row into view.

Both writes are the controller's, never the element's:
`applyEngineCompensation({ kind: 'head-splice' })` is the engine's own name for
this change (content above the viewport spliced out, anchor holds) and the
resolver applies it verbatim. A bare `scrollTop =` on a run with a controller would read as
a reader gesture and escape bottom-follow. A run with no controller writes
directly, because there is no intent machine to mislead.

**Advances only.** A head that retreats is a chunk the reader paged in, which
compensates its own prepend, or a jump relocating the window, which places its
own target. Compensating those here would be a second write for one change. The
effect is declared before the focus effect for the same reason: it holds a
position the reader did not ask to leave, and a jump is one they did ask for.
Non-overlapping windows produce no shared row to hold, and that answer is
`null`, not zero. A zero would drag the reader to the top of the run.

### Jumps resolve inside the run

A search hit, review jump, target flash, or restore anchor whose item lives
in a run resolves through `findTimelineNodeIndex` to the RUN's row, so
`timelineRestore.svelte.ts` points the run at the item before scrolling the
timeline, then re-resolves the index (expanding a run re-measures every row
after it).

`revealActivityRunItem` does the three things a jump needs as one call
(expand the run, relocate the window around the target, leave a focus
request), because a partial application is a silent bug: a relocated window
on a collapsed run shows nothing, and a focus request the window does not
cover scrolls nowhere. It reports false both for an item the run does not
carry and for a run the registry no longer holds, and the second answer comes
from `requestFocus`'s own report rather than a pre-check: every mutator is a
no-op for a swept id, so a jump that returned true there would announce
success for a run that will never scroll.

The focus request lives on the registry entry rather than a prop because the
jump is usually what scrolls the run into the virtualizer's buffer in the
first place, and the row may not exist yet. `revision` bumps so an
already-mounted row notices a request that changes nothing on its node;
`takeFocus` is deliberately silent, so consuming a request schedules no
rebuild.

The request carries whether the window **moved**, and the two cases get
different treatment:

- **Relocated** → the clip's offset pointed at rows that are no longer
  mounted, so where the target sits under it is an accident. Place it:
  centered, clamped by the run's edges.
- **Unmoved** → the reader is already looking at these rows. Leave a target
  they can see exactly where it is; nudging it would be the jump fighting
  them.

Either way the write escapes bottom-follow first, or the next streamed chunk
would yank the reader off the item they jumped to.

Restore anchors deliberately do **not** focus: a restore anchor's item id is
`children[0]`, a re-find handle rather than a "show me this" intent.

## Retention, signatures, priors

`retainNode` must not walk a run's full child list. The ±96-node buffer is in
*node* space; 48 runs of 200 items inside that band would retain ~9600 items
where it retains 96 without runs. A run can only ever mount `mountedRows`
rows, so only those children can hold live expansion handles: retention is
the run's own mounted window (read off the node, so the bound is correct by
construction rather than by agreeing with a duplicated constant) plus any
child with an active expansion.

The prune's cadence has to include `pane.activityRuns.revision`, on both
halves: the pass's dedupe signature reads it, and the effect that schedules
the pass reads it too. A window relocation touches neither structure nor the
item list (same node count, same range, different mounted children), so
without it the pass either bails as a no-op or is never scheduled, and the
window the reader left stays retained until an unrelated outer scroll. Each
bump is one deliberate action (a toggle, a chunk, a jump), never a delta.

`nodeSignature` is
`A:{runId}:{c|o}:{childCount}:{mountedFrom}:{mountedRows}`, shape and window
included, because both change a sub-cap run's height. Two shape letters, read
straight off `collapsed`: `c` is a closed run (its header alone), `o` an open one
(header over clip).

Deliberately not three, and liveness is not a letter. It was resolved into
`collapsed` once per pass, so a run rendering open while it works signs `o` like
any other open run. Signing on `live` as well would throw a good prior away
every time a later run took the tail, at a moment when the run's height did not
change at all.

The row estimate is **state-aware**, not a kind-table entry: a run is ~24px
closed and up to the cap plus that header open, and a single
`ROW_KIND_ESTIMATE_PX` value would be wrong by ~20× in one shape, landing
fast-scroll placement badly through unmeasured runs.
`timelineRowStructuralSize` reads the same `collapsed` the signature does: one
header line when closed, that line plus `min(capFloor, mountedRows ×
rowFloor)` when open, where `capFloor` is the cap evaluated against the current
viewport. Taking the rem half unconditionally would overestimate every long
run on a short one, and an estimate above the real ceiling shrinks total
geometry when the measurement lands. The rem half comes from
`ACTIVITY_RUN_CAP_REM_PX`, exported beside the cap it mirrors so the two
cannot drift; its 16px-root assumption is safe in one direction only, which is
the direction it errs (a larger root makes it an underestimate, and
underestimates only grow on measure). Still a floor, per the existing
convention. Once measured, a
run is *better* for priors than what it replaced: one stable height instead
of N estimated ones.

## Settings

- `activityRunDefault`: `expanded` | `collapsed`, default `collapsed`
  (flipped 2026-08-30). Applies to the tail run too, which is where the
  collapsed-but-open state comes from: with `collapsed`, a streaming run shows
  its header AND its work, keeps showing it after it settles, and collapses
  only once the reader is provably past it (the auto-collapse gate). This is
  the durable
  layer under the chat header's per-thread bulk toggle, which overrides it for
  one thread and dies with the pane.
- `activityRunWindowRows`: default 30, clamped `[10, 200]`, validated
  strictly on update and clamped leniently on load.

Three hand-written places move together: the Go struct, Go `DefaultSettings`,
and `validate.go` (allow-list + strict update + lenient load), plus
`types/settings.ts` for the TS shape. The frontend's DEFAULT VALUES are not a
fourth mirror — `lib/generated/settingsDefaults.ts` is generated from
`DefaultSettings` (`go generate ./internal/settings`) and is what the store,
`activityRunPrefs` and `test/helpers/settings.ts` all read. The frontend reads
them through `stores/activityRunPrefs.svelte.ts`; the UI is
`components/settings/ActivityRunSection.svelte`.

## Verification

Unit (`pnpm test`): `activityRunGrouping.test.ts` (boundary rules, the
`proposed_plan` exemption splitting a run, composition with
read/subagent/wait groups, identity migration across backfill / prune /
split / merge, window resolution in row space),
`activityRunSummary.test.ts` (count aggregation, failure, running label),
`activityRunWindow.test.ts` (row lookup, focus window, grow/clamp, reveal),
`activityRunClip.test.ts` (which bodies lift the cap),
`threadActivityRuns.svelte.test.ts` (overrides, snapshots, the mount window
surviving a head prune, focus requests, state across a sweep and a
revival, the open-because-live hold's full transition matrix),
`ActivityRun.test.ts` (both shapes and the header that toggles
between them, its counts/failure/running, rail click, boundaries, jumps, the
collapsed-but-open state and the release that closes it),
`activityRunAutoCollapse.test.ts` (the out-of-sight predicate over its
geometry matrix), `timelineActivityRunAutoCollapse.test.ts` (the gate over a
real registry: release, and every refusal (live, visible, near-tail,
pinned/escaped/expanded/failed, unmeasured)), `wheelAttribution.test.ts` +
`intent.test.ts`, `overlayScrollbar.test.ts` + `OverlayScrollbar.test.ts`.
Go: enum rejection, defaults, round-trip, out-of-range clamps on load.

Browser (`pnpm test:browser`, real Chromium):
`activityRunClip.browser.test.ts` (the cap engaging and not bounding a short
run, growth by exactly an expanded body, margin containment inside the
clip's BFC) and `activityRunScroll.browser.test.ts` (prepend compensation
holding the reading position while nothing outside the run moves; the tail
window sliding under an appended row without displacing the rows on screen,
and the appended row then GLIDING in over several frames rather than
arriving, on the first append and on every one after it; a jump
centering an off-window target and the tail run's spring not dragging it
back) and `activityRunAutoCollapse.browser.test.ts` (the zero-motion proof:
per-frame anchor sampling across a release, for a mounted run and for one
whose row left the virtualizer's buffer; the settled run staying open in the
real trigger cascade; a visible run refused and released only after the
reader leaves; the end state measuring the same as a run that was never
open). The motion claims are the ones happy-dom cannot see at all. It lays
nothing out, so the unit suites assert state, never displacement.

**The scrollbar-width invariant cannot be asserted geometrically.** Headless
Chromium reserves no width for a scrollbar. Measured: a 300px box holding
1000px of content reports `clientWidth` 300 with the suppression removed,
with or without
`--disable-features=OverlayScrollbar,FluentOverlayScrollbar`, so "the
content width does not change when the run starts overflowing" passes with
the fix deleted. What the suite asserts instead is the declaration (the
computed `scrollbar-width` plus a CSSOM walk for the `::-webkit-scrollbar`
rule, both paths) and the consequence that IS observable: the clip takes no
gutter and its rows land exactly on the rail. The geometric consequence on
WebKitGTK is a manual check.

Manual, on a real stream: the outer `scrollTop` must not move while a capped
run streams: the last prose block stays put. Cap, chunk size, and the
default window are feel-tuned on the 165Hz setup as with prior scroll work.

## Deltas from the original design

1. **No `WINDOW_THRESHOLD_ROWS`, no `flat` state.** `max-height` self-gates:
   a run under the cap renders identically clipped or not, so the threshold
   only bought a mid-stream DOM-shape swap.
2. **One inner controller, not one per windowed run** (above).
3. **Registry-assigned `runId`.** The design keyed runs on the current first
   member id, which changes on backfill *and* on live-window prune.
4. **Jumps resolve inside the run**, with the relocated/unmoved distinction.
5. **A run's state survives the run disappearing**, via the archive tier. The
   design's "restore snapshots carry the run's inner offset" is not
   achievable through the snapshot store (one anchor and one offset per
   thread, and runIds are minted per pass); the archive gets the behavior for
   every run instead, and also fixes an in-session loss the design did not
   notice: a head prune sweeping a collapsed run.
6. **Counts and failure live off the node** (above).
7. **`RAIL_EXEMPT_PAYLOAD_KINDS` is part of membership**, which the design
   omitted, so a late `payloadKind` can split a run.
8. **A custom overlay scrollbar** rather than a native bar, because native
   costs width *and* rail alignment.
9. **The plan's head-trim exemptions became one rule.** The plan said to
   trim the head back to K, never while the user is scrolled up inside and
   never a row with an active expansion lease. The window IS the trim, so the
   exemption is where the behavior lives: it pins while the reader is escaped
   (above). The expansion-lease exemption is then unnecessary. Reaching a
   body above the tail means scrolling up, which pins the window, and an
   expansion made at the tail inflates the cap and stays mounted. Retention
   keeps the state either way.
10. **The chip became a permanent header, not the collapsed representation.**
   The design had two mutually exclusive renders, "chip" or "expanded", which
   shipped that way and was wrong in use: expanding a run removed the element
   the reader had just clicked, and the only control left was the rail, which
   is invisible by construction. The summary line is now always present and
   toggles both ways, so collapse is a clip that comes and goes under a fixed
   header. That also removed a shape: the estimate and the signature had three
   each (chip / chip-over-clip / clip) and now have two. The rail became
   pointer-only in the same change: it had carried the disclosure ARIA because it
   really was the last control standing when a run expanded, and once that
   stopped being true the same attributes bought a phantom tab stop on a
   transparent strip and a doubled announcement.
11. **Liveness became an input to collapse, not an override on top of it.** Both
   the design and the first implementation had every consumer ask
   `!collapsed || live`, which made "collapsed" and "has no clip" different
   states, and made the reader's collapse of the live run do nothing at all
   except rotate the chevron, because the clip they were trying to close was held
   open by the same rule that had drawn a collapsed chevron over it. The facts
   are now ranked once, in the registry (`collapsedFor`), with the
   reader's answer for this run outranking liveness. The full current order,
   which delta 12 later extended with the recorded hold, is under "Four facts"
   above. That makes `collapsed` mean "renders without its clip" everywhere
   downstream, so
   the predicate that used to reconcile the two is gone, the signature and the
   estimate each read one field, and the header cannot disagree with what is on
   screen. What the old rule was protecting is intact (a run nobody has answered
   for still shows its work while it works), because that was always a statement
   about DEFAULTS.

12. **The settle transition stopped being an animation.** The design (and the
   first shipped version) closed a settled run in place with a height fold.
   Rejected in use: the clip is capped near half the viewport, and no easing
   makes losing that much height acceptable while the reader is watching the
   exact spot it happens. Replaced by the `openedLive` hold plus the
   off-screen auto-collapse gate: the same end state, scheduled for when it
   is invisible instead of softened while it is not. The fold machinery
   (`activityRunClipPresence.svelte.ts`, `utils/activityRunFold.ts`) was
   deleted with it, and the row's clip renders from `collapsed` alone.

## Accepted tradeoffs

1. **Native find-in-page cannot reach a closed run or unmounted rows.** A new class
   next to the existing virtualization tradeoff: virtualization hides
   *offscreen* content, a collapsed run hides content that is on screen.
   In-app search routes through `findTimelineNodeIndex` and so resolves into
   runs; only the browser's own find is degraded, and it is not
   interceptable.
2. **Expansion state for run children drops sooner than before**, because
   retention is bounded per run. Memory-vs-state, resolved toward memory per
   the pane's stated budget priority.
3. **The tool↔text gap now appears at junctions that suppressed it**:
   blocks that open or close with a `thinking` row. Margin only, no
   separator; one-line revert if it reads wrong in situ.
4. **A historical run does not chase when it grows**, except while the
   reader is resting on its newest row. A late `tool_completion` pairing into
   an older run, or a subagent group hydrating on expand, grows it without
   following. Correct, since nobody is watching it, but an asymmetry, stated
   rather than assumed.

   Narrowed once, deliberately: "nobody is watching it" is false for the
   frames right after an expand, where the same growth left the reader partway
   up a run they had just opened. A clip following its last row is now held
   there as its content settles ("A resting clip is held on its last row"
   above). A run the reader has scrolled inside still never moves under them,
   which is the part of this tradeoff that was load-bearing.

   The visible edge of it: load-older paging that extends a short run at its
   HEAD gives that run more children than its window, so the clip (which was
   showing all of them, unscrolled) starts showing its oldest rows with the
   ones the reader was looking at below the fold. Holding position across that
   would mean measuring the clip before and after every mounted-set change,
   and the mounted set changes on every appended row, so the cheap fix costs a
   forced layout per streaming chunk on the live run. Left alone deliberately;
   the frozen-window rule above covers the frequent case (the reader is inside
   a run that is still streaming), and this one needs the reader to page older
   while a short run is on screen.
5. **Per-run state is session-only.** Matches item-expansion leases;
   promoting overrides into per-client `ui_state` later is additive.

**Unverified assumption:** the native-scrollbar path is dismissed partly on
the "WebKitGTK reserves a single-edge `stable` gutter only while the bar is
present" claim in `MessageTimeline.svelte`, taken from that comment rather
than re-verified. It does not change the conclusion (rail misalignment rules
the native path out independently), but it is a spike-policy item if it ever
becomes load-bearing.

## Tunables

| Constant | Value | Where |
|---|---|---|
| Clip cap | `min(50vh, 18rem)` | `ACTIVITY_RUN_CAP_CSS` |
| Rows the cap shows | 8 × 2.25rem | `ACTIVITY_RUN_CAP_ROWS`, `ACTIVITY_RUN_ROW_REM` |
| Top fade | 24px | `ActivityRun.svelte` |
| Auto-collapse viewport margin | 48px | `AUTO_COLLAPSE_VIEWPORT_MARGIN_PX` |
| Auto-collapse tail distance | 1 viewport | `AUTO_COLLAPSE_TAIL_DISTANCE_VIEWPORTS` |
| Default window rows | 30, clamped `[10, 200]` | setting + `activityRunWindow.ts` |
| Boundary chunk | 25 rows | `ACTIVITY_RUN_CHUNK_ROWS` |
| Scroll-to-mount runway | 96px | `MOUNT_EARLIER_PREFETCH_PX` |
| Archive capacity | 128 keys (≈64 runs) | `threadActivityRuns.svelte.ts` |

## Implementation map

| Concern | File |
|---|---|
| Projection pass, identity contract | `utils/activityRunGrouping.ts` |
| Header counts, failure, running label | `components/chat/activityRunSummary.ts` |
| Mount window, jump math, reveal | `utils/activityRunWindow.ts` |
| Cap geometry, disclosure observer, centering | `utils/activityRunClip.ts` |
| Registry + archive | `stores/threadActivityRuns.svelte.ts` |
| Settings accessor | `stores/activityRunPrefs.svelte.ts` |
| Row, clip, inner controller | `components/chat/ActivityRun.svelte` |
| Auto-collapse gate (reader half) | `components/chat/timelineActivityRunAutoCollapse.ts` |
| Auto-collapse predicate (geometry half) | `utils/activityRunAutoCollapse.ts` |
| Engagement peek | `stores/threadRowUiState.svelte.ts` `hasUserExpansionWithin` |
| Engagement scope (rendered items) | `utils/subagentGrouping.ts` `renderedItemIdsWithin` |
| Header, boundaries | `components/chat/ActivityRun{Header,Boundary}.svelte` |
| Overlay scrollbar | `utils/scroll/overlayScrollbar.ts`, `components/shared/OverlayScrollbar.svelte` |
| Wheel attribution | `utils/scroll/wheelAttribution.ts` |
| Settings UI | `components/settings/ActivityRunSection.svelte` |
| Go settings | `internal/settings/{settings,validate}.go` |

See also [`frontend-scroll.md`](frontend-scroll.md) (nested scrollers,
gesture attribution, the outer engine's contracts) and
[`scroll-contracts.md`](scroll-contracts.md) C7.
