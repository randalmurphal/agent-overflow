// Activity-run membership over a flat list of ROWS, which is the form the
// server classifies in (docs/architecture/timeline-window-pages.md §1).
//
// `activityRunGrouping.ts` groups projected NODES — a read group or a
// subagent card is one row standing for several items — because that is
// what renders. The data layer needs the same rule over physical rows:
// matching a server stub to the members a pane holds, deciding whether a
// pushed row falls inside a held run, and shedding a run's older members
// are all statements about items, not about nodes. Keeping both readings
// of one rule here rather than re-deriving membership from a projection
// is also what lets the shared fixture
// (`src/test/fixtures/activityRunVectors.json`) run without one.

import type { Item } from '../types/models';
import { isWindowedTimelineRow } from '../stores/threadWindowDigest';
import { RAIL_EXEMPT_PAYLOAD_KINDS, RAIL_LEAF_KINDS } from './timelineRail';
import { isAgentMessageItem } from './agentMessage';

/**
 * A rail row: the kinds that sit on the activity rail, minus the payload
 * kinds that break out of it into a full-width card. Mirrors
 * `timelineNodeHasRail` for a leaf, which is the predicate
 * `activityRunGrouping.ts` applies to every node it wraps.
 */
export function isActivityRailRow(item: Item): boolean {
  return RAIL_LEAF_KINDS.has(item.kind)
    && !RAIL_EXEMPT_PAYLOAD_KINDS.has(item.payloadKind ?? '');
}

/**
 * A row an open run absorbs: it joins the run when a member sits right
 * before it, and stands alone otherwise. A `notification` bell, and an
 * agent message (`isAgentMessageItem`: a message the app wrote into the
 * thread for an agent, a wake, a reminder, a loop or a watch). Both land at
 * the write head, inside the run of the work around them; the person's own
 * messages are not one and end the run. The store's twin is
 * `activityScanRow.isAbsorbed`.
 */
export function isActivityRunAbsorbedRow(item: Item): boolean {
  return item.kind === 'notification' || isAgentMessageItem(item);
}

/**
 * One maximal run over a contiguous stretch of rows, in timeline order.
 * `items` is every physical member the caller's list holds; `firstItemId`
 * and `lastItemId` are that stretch's edges, which are the RUN's edges
 * only when the caller holds the whole run.
 */
export interface ActivityRunSpan {
  firstItemId: string;
  lastItemId: string;
  items: Item[];
}

/**
 * Group visible top-level rows into maximal activity runs.
 *
 * A run is a maximal consecutive stretch of rail rows, plus absorbed rows
 * (`isActivityRunAbsorbedRow`) with a run member immediately before them.
 * `knownMember` admits an absorbed row a history page says is a member
 * though the row before it is not loaded. Anything else
 * ends the run. Rows a history page would not return (subagent children,
 * `plan_update` notifications) are skipped without breaking a run: they
 * are not part of any page's range, so they cannot split one.
 *
 * Pure, and allocation-proportional to the runs found rather than to the
 * input: a prose-only window allocates one empty array.
 */
export function groupActivityRunSpans(
  items: readonly Item[],
  knownMember: (item: Item) => boolean = () => false,
  includes: (item: Item) => boolean = isWindowedTimelineRow,
): ActivityRunSpan[] {
  const spans: ActivityRunSpan[] = [];
  let open: ActivityRunSpan | null = null;
  for (const item of items) {
    if (!includes(item)) continue;
    if (isActivityRailRow(item) || (knownMember(item) && isActivityRunAbsorbedRow(item))) {
      if (open === null) {
        open = { firstItemId: item.id, lastItemId: item.id, items: [item] };
        spans.push(open);
      } else {
        open.items.push(item);
        open.lastItemId = item.id;
      }
      continue;
    }
    if (open !== null && isActivityRunAbsorbedRow(item)) {
      open.items.push(item);
      open.lastItemId = item.id;
      continue;
    }
    open = null;
  }
  return spans;
}

/** The span holding `itemId`, or null. Linear; callers index when it matters. */
export function spanHolding(
  spans: readonly ActivityRunSpan[],
  itemId: string,
): ActivityRunSpan | null {
  if (itemId === '') return null;
  for (const span of spans) {
    for (const item of span.items) {
      if (item.id === itemId) return span;
    }
  }
  return null;
}
