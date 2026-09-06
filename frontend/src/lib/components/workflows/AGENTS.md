# components/workflows/

Run-map behavior and its section numbers:
[`workflow-run-map.md`](../../../../../docs/architecture/workflow-run-map.md).
Engine and phase semantics:
[`workflows-system.md`](../../../../../docs/specs/workflows-system.md).

## The overlay

`WorkflowsOverlay.svelte` is a full-surface layer rendered as a SIBLING of
the pane host, so the pane tree stays mounted underneath and opening or
closing rebuilds nothing. Navigation is home, run detail and the terminal
all-clear: a workflow's runs expand in place on home rather than adding a
third depth. `stores/workflowsOverlay.svelte.ts` owns the state that
survives a close.

ONE scroller serves every level, and it is an ANCESTOR of the run map
rather than the map's own element. The overlay frame hands it down through
`overlayScroller.ts` context and the map requires it, so a map mounted
without a provider fails loudly at mount. Never walk the DOM for a
scroller: a walk picks up whatever `overflow-y` a future wrapper
introduces, and returns null in exactly the case that must be loud. The
context value is a GETTER, because `bind:` lands after the frame's first
render and a captured null would never heal.

## Follow and scroll

`runMapFollow.svelte.ts` is the map's state machine and it re-derives the
timeline's doctrine deliberately rather than importing it, because
`utils/scroll/` is virtualizer- and spring-shaped while the map is one
short document with one moving point of interest. What carries over is the
product requirement (§9):

- ONE write chokepoint. Every `scrollTop` write goes through
  `writeScrollTop` with a cause the user could name: `place`, `jump`,
  `follow`, `compensate`. Anything else is a bug.
- Escape is EVENT-SOURCED, never geometry-inferred. Only wheel, key, touch
  and pointer escape follow. A `scroll` event never does, since a
  programmatic write and a finger produce the same event, which is what
  makes a follow glide incapable of false-escaping itself.
- Re-engagement is EXPLICIT ONLY (§9.3), stricter than the timeline's
  bottom-restick, because the map's follow target sits mid-content and
  moves, so an implicit restick would be the force-grab the requirement
  forbids.
- The chip is an OFFER TO TRAVEL (§9.10): while disengaged it renders only
  when engaging would move the viewport, and hiding it is not a
  re-engagement.
- Nothing here authors `will-change` or a content transform (§9.11).

Rect arithmetic stays in `runMapGeometry.ts`, pure and directly tested, so
the anchor descent is observable as itself rather than as a compensation
number. New geometry goes there, not the controller.

## Trusting the data

Run-map patches are a latency optimization, never load-bearing for
correctness. Every `workflow:*` event carries strictly less than the view,
so anything a payload does not determine EXACTLY returns `invalidate` from
`stores/workflowRunMapPatch.ts`, which refetches while keeping the last
value. A plausible guess is a map that lies until something refetches.

## Connected computers

The overlay combines runs from attached computers. Run actions follow the run's
owner, including threads excluded from the ordinary sidebar. Pause-all requires
workflow control on every attached computer and reports partial failures by
computer. Engine state is per computer; one paused engine must never make a
second engine look paused. Creating a run uses the selected project's computer.
