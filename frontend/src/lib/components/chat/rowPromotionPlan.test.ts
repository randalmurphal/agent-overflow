import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import { makeItem } from '../../../test/helpers/chat';
import {
  itemTurnIndexKey,
  type ActivityRunNode,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { planRowPromotion } from './rowPromotionPlan';
import { timelineRowDecorations } from './timelineRows';

function leaf(overrides: Partial<Item>): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

function run(children: TimelineNode[], overrides: Partial<ActivityRunNode> = {}): ActivityRunNode {
  return {
    kind: 'activity_run',
    runId: 'run-1',
    threadId: 'thread-1',
    children,
    collapsed: false,
    live: true,
    atTail: true,
    mountedFrom: 0,
    mountedRows: children.length,
    membershipEpoch: 0,
    memberItemIds: [],
    summaryItemIds: [],
    memberCount: children.length,
    unshippedBefore: 0,
    unshippedAfter: 0,
    ...overrides,
  } as ActivityRunNode;
}

const user = leaf({ id: 'user', kind: 'user_text', role: 'user', turnIndex: 1, itemIndex: 0 });
const tool = leaf({ id: 'tool', kind: 'tool_call', turnIndex: 1, itemIndex: 1 });
const thinking = leaf({ id: 'think', kind: 'thinking', status: 'streaming', turnIndex: 1, itemIndex: 2 });
const prose = makeItem({ id: 'think', kind: 'assistant_text', turnIndex: 1, itemIndex: 2, meta: '{"narration":true}' });
const queued = leaf({ id: 'queued', kind: 'tool_call', turnIndex: 1, itemIndex: 3 });

function plan(revealed: TimelineNode[], structure: TimelineNode[], activeTurnKey: number | null = 1) {
  return planRowPromotion({
    itemId: 'think',
    row: prose,
    revealed,
    structure,
    decorations: timelineRowDecorations(revealed, activeTurnKey, itemTurnIndexKey, structure),
    activeTurnKey,
    turnKeyOf: itemTurnIndexKey,
  });
}

describe('row promotion plan', () => {
  it('plans prose after a run that keeps its rows: gap and plain divider', () => {
    const result = plan([user, run([tool, thinking])], [user, tool, thinking, queued]);
    expect(result).toMatchObject({
      itemId: 'think',
      runId: 'run-1',
      removesRun: false,
      boundary: true,
      divider: true,
      pill: false,
      runBoundary: true,
    });
    expect(result!.node).toEqual({ kind: 'leaf', item: prose });
  });

  it('gives the pill to prose that closes a settled turn', () => {
    const result = plan([user, run([tool, thinking])], [user, tool, thinking], null);
    expect(result).toMatchObject({ divider: true, pill: true });
  });

  it('plans prose that replaces a run of one: the decorations of its new place', () => {
    // The run sits at a text/tool boundary after the user message. Prose in
    // its place follows text, so it carries no gap and opens no response.
    const afterUser = plan([user, run([thinking])], [user, thinking]);
    expect(afterUser).toMatchObject({
      removesRun: true, runBoundary: true, boundary: false, divider: false, pill: false,
    });

    // Only a run directly above opens a response; a row between the earlier
    // activity and the prose closes it.
    const earlier = run([tool], { runId: 'run-0' });
    const between = leaf({ id: 'note', kind: 'notification', turnIndex: 1, itemIndex: 1 });
    const afterActivity = plan(
      [user, earlier, between, run([thinking])],
      [user, tool, between, thinking],
    );
    expect(afterActivity).toMatchObject({ removesRun: true, divider: false, pill: false });
  });

  it('declines anything it cannot present on screen', () => {
    // Not the last revealed row.
    expect(plan([user, run([tool, thinking]), queued], [user, tool, thinking, queued])).toBeNull();
    // Not the run's last row.
    expect(plan([user, run([thinking, tool])], [user, thinking, tool])).toBeNull();
    // A collapsed run shows no rows.
    expect(plan([user, run([tool, thinking], { collapsed: true })], [user, tool, thinking])).toBeNull();
    // The last row is outside the mounted window.
    expect(plan([user, run([tool, thinking], { mountedRows: 1 })], [user, tool, thinking])).toBeNull();
    // Members the pane does not hold follow the row.
    expect(plan([user, run([tool, thinking], { unshippedAfter: 2 })], [user, tool, thinking])).toBeNull();
    // A run of one whose other members are not loaded keeps its row.
    expect(plan([user, run([thinking], { unshippedBefore: 3 })], [user, thinking])).toBeNull();
    // No run at the tail.
    expect(plan([user, thinking], [user, thinking])).toBeNull();
    expect(plan([], [])).toBeNull();
  });
});
