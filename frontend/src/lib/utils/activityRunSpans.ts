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
 * How agent messages group into activity runs. It is the rule of the
 * backend that owns the thread's history, because that backend's stubs and
 * members answers describe runs by it (`threadActivityRunRule`); the
 * store's twin is `store.ActivityRunRule`.
 */
export type ActivityRunRule = typeof AGENT_MESSAGE_MEMBERS_RULE | typeof AGENT_MESSAGES_ABSORBED_RULE;

/**
 * An agent message is a run member on sight, wherever it lands: alone it
 * is a run of one, and a member on either side shares its run.
 */
export const AGENT_MESSAGE_MEMBERS_RULE = 'agent-message-members' as const;

/**
 * The v0.0.19 rule: an agent message joins a run only when a member sits
 * right before it, as a notification does, and stands alone otherwise. It
 * exists for backends without the `activity-runs.agent-message-members.v1`
 * capability, which classify their history this way.
 */
export const AGENT_MESSAGES_ABSORBED_RULE = 'agent-messages-absorbed' as const;

/**
 * A row that is a run member on sight, wherever it lands: a rail row, or
 * under `AGENT_MESSAGE_MEMBERS_RULE` an agent message
 * (`isAgentMessageItem`: a message the app wrote into the thread for an
 * agent, such as another thread's reply or request, a wake, a reminder, a
 * loop or a watch). An agent message is the thread tools' traffic, so it
 * groups exactly as a tool row does. It does not sit on the rail
 * (`timelineRail.ts`). The person's own messages end a run. The store's
 * twin is `activityScanRow.isMember`.
 */
export function isActivityRunMemberRow(item: Item, rule: ActivityRunRule): boolean {
  return isActivityRailRow(item) || (rule === AGENT_MESSAGE_MEMBERS_RULE && isAgentMessageItem(item));
}

/**
 * A row an open run absorbs: it joins the run when a member sits right
 * before it, and stands alone otherwise. A `notification` bell, and under
 * `AGENT_MESSAGES_ABSORBED_RULE` an agent message. The store's twin is
 * `activityScanRow.isAbsorbed`.
 */
export function isActivityRunAbsorbedRow(item: Item, rule: ActivityRunRule): boolean {
  return item.kind === 'notification'
    || (rule === AGENT_MESSAGES_ABSORBED_RULE && isAgentMessageItem(item));
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
 * A run is a maximal consecutive stretch of members
 * (`isActivityRunMemberRow`), plus absorbed rows (`isActivityRunAbsorbedRow`)
 * with a run member immediately before them, both under `rule`.
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
  rule: ActivityRunRule,
  knownMember: (item: Item) => boolean = () => false,
  includes: (item: Item) => boolean = isWindowedTimelineRow,
): ActivityRunSpan[] {
  const spans: ActivityRunSpan[] = [];
  let open: ActivityRunSpan | null = null;
  for (const item of items) {
    if (!includes(item)) continue;
    if (isActivityRunMemberRow(item, rule) || (knownMember(item) && isActivityRunAbsorbedRow(item, rule))) {
      if (open === null) {
        open = { firstItemId: item.id, lastItemId: item.id, items: [item] };
        spans.push(open);
      } else {
        open.items.push(item);
        open.lastItemId = item.id;
      }
      continue;
    }
    if (open !== null && isActivityRunAbsorbedRow(item, rule)) {
      open.items.push(item);
      open.lastItemId = item.id;
      continue;
    }
    open = null;
  }
  return spans;
}
