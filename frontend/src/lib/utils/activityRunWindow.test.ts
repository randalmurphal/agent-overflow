import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import {
  activityRunFocusWindow,
  activityRunRowIndexOfItem,
  activityRunWindowGrownNewer,
  activityRunWindowGrownOlder,
  revealActivityRunItem,
  type ActivityRunReveal,
} from './activityRunWindow';
import type { ActivityRunNode, TimelineNode } from './subagentGrouping';

function leaf(id: string): TimelineNode {
  return { kind: 'leaf', item: makeItem({ id, kind: 'tool_call', toolName: 'Bash' }) };
}

/** A subagent card: one run row, several items, none of them the row's own. */
function card(parentId: string, childId: string): TimelineNode {
  return {
    kind: 'group',
    parent: makeItem({ id: parentId, kind: 'tool_call', toolName: 'Task' }),
    groupKey: parentId,
    children: [leaf(childId)],
    descendantCount: 1,
    loadedDescendantCount: 1,
    latestChildSummary: '',
  };
}

function run(
  children: TimelineNode[],
  window: { from: number; rows: number },
): ActivityRunNode {
  return {
    kind: 'activity_run',
    runId: 'r1',
    threadId: 'thread-1',
    children,
    collapsed: false,
    live: false,
    atTail: false,
    mountedFrom: window.from,
    mountedRows: window.rows,
    membershipEpoch: 1,
    memberItemIds: [],
  };
}

function rows(n: number): TimelineNode[] {
  return Array.from({ length: n }, (_, i) => leaf(`i${i}`));
}

describe('activityRunRowIndexOfItem', () => {
  it('finds the row a leaf item is', () => {
    expect(activityRunRowIndexOfItem(run(rows(5), { from: 0, rows: 5 }), 'i3')).toBe(3);
  });

  it('finds the card a nested item is inside', () => {
    const node = run([leaf('i0'), card('agent', 'agent-child')], { from: 0, rows: 2 });

    expect(activityRunRowIndexOfItem(node, 'agent-child')).toBe(1);
  });

  it('reports -1 for an item the run does not carry', () => {
    expect(activityRunRowIndexOfItem(run(rows(5), { from: 0, rows: 5 }), 'zz')).toBe(-1);
  });
});

describe('activityRunFocusWindow', () => {
  it('anchors half a window above the target, keeping the window size', () => {
    const node = run(rows(100), { from: 90, rows: 10 });

    expect(activityRunFocusWindow(node, 'i40'))
      .toEqual({ rows: 10, startItemId: 'i35', from: 35 });
  });

  it('stops at the run start rather than anchoring past it', () => {
    const node = run(rows(100), { from: 90, rows: 10 });

    expect(activityRunFocusWindow(node, 'i2'))
      .toEqual({ rows: 10, startItemId: 'i0', from: 0 });
  });

  it('returns tail mode for a target near the end — nothing would be hidden below', () => {
    const node = run(rows(100), { from: 90, rows: 10 });

    // `from` reports where tail mode lands, not the requested 92 — the run's
    // last row pins the window's end.
    expect(activityRunFocusWindow(node, 'i97'))
      .toEqual({ rows: 10, startItemId: null, from: 90 });
  });

  it('anchors on the card, not the item, for a nested target', () => {
    const node = run(
      [...rows(20), card('agent', 'agent-child'), ...rows(20)],
      { from: 31, rows: 10 },
    );

    // Row 20 is the card; half a window above lands on row 15.
    expect(activityRunFocusWindow(node, 'agent-child')).toEqual({
      rows: 10,
      startItemId: 'i15',
      from: 15,
    });
  });

  it('reports null for an item the run does not carry', () => {
    expect(activityRunFocusWindow(run(rows(10), { from: 0, rows: 10 }), 'zz')).toBeNull();
  });
});

describe('growing the window', () => {
  it('older keeps the bottom edge', () => {
    const node = run(rows(200), { from: 100, rows: 20 });

    // 25 rows up, same last row: 100 - 25 = 75, and 120 - 75 = 45 rows.
    expect(activityRunWindowGrownOlder(node)).toEqual({ rows: 45, startItemId: 'i75' });
  });

  it('older stops at the run start and stays in tail mode there', () => {
    const node = run(rows(30), { from: 10, rows: 20 });

    expect(activityRunWindowGrownOlder(node)).toEqual({ rows: 30, startItemId: null });
  });

  it('newer keeps the top edge', () => {
    const node = run(rows(200), { from: 100, rows: 20 });

    expect(activityRunWindowGrownNewer(node)).toEqual({ rows: 45, startItemId: 'i100' });
  });

  it('newer returns to tail mode when it reaches the last row', () => {
    const node = run(rows(120), { from: 100, rows: 10 });

    // 10 rows left below, so the grown window covers the tail — and giving up
    // the anchor is what lets the run follow new activity again.
    expect(activityRunWindowGrownNewer(node)).toEqual({ rows: 20, startItemId: null });
  });
});

describe('revealActivityRunItem', () => {
  /** `members` defaults to "the registry still holds everything the node says". */
  function registry(runExists = true, members: (id: string) => boolean = () => true): ActivityRunReveal {
    return {
      expandForReveal: vi.fn(),
      setWindowAnchor: vi.fn(),
      containsMember: vi.fn((_runId: string, itemId: string) => members(itemId)),
      requestFocus: vi.fn(() => runExists),
    };
  }

  it('expands, relocates, and requests focus together', () => {
    const reveal = registry();
    const node = run(rows(100), { from: 90, rows: 10 });

    expect(revealActivityRunItem(reveal, node, 'i40')).toBe(true);
    // The hold-free expand verb: a jump retargets the viewport itself, so
    // going through `setCollapsed`'s viewport-bottom hold would fight it.
    expect(reveal.expandForReveal).toHaveBeenCalledWith('r1');
    // The anchor alone. A jump moves the window; asking for a size would
    // record the size the run already has as an explicit override.
    expect(reveal.setWindowAnchor).toHaveBeenCalledWith('r1', 'i35');
    // Relocated: the clip's offset belonged to rows 90-99, so the row consuming
    // this request has to place the target rather than reveal it in place.
    expect(reveal.requestFocus)
      .toHaveBeenCalledWith('r1', { itemId: 'i40', relocated: true });
  });

  it('reports an unmoved window, so a visible target is left where it is', () => {
    const reveal = registry();
    const node = run(rows(100), { from: 90, rows: 10 });

    // Row 95 already sits in the mounted window, and half a window above it is
    // past the tail clamp, so the window resolves to exactly where it is.
    expect(revealActivityRunItem(reveal, node, 'i95')).toBe(true);
    expect(reveal.requestFocus)
      .toHaveBeenCalledWith('r1', { itemId: 'i95', relocated: false });
  });

  it('reports the registry refusing a run it no longer holds', () => {
    // The node was resolved by an earlier pass and the entry has since been
    // swept (or a thread switch cleared it), so all three mutators no-op. The
    // jump has to hear that, or it goes on to tick and scroll for a run that
    // will never mount the target.
    const reveal = registry(false);
    const node = run(rows(100), { from: 90, rows: 10 });

    expect(revealActivityRunItem(reveal, node, 'i40')).toBe(false);
  });

  it('degrades a stale anchor to tail-follow instead of writing it', () => {
    // A jump is resolved when the reader clicks, against a node the projection
    // built earlier. If an older-side prune took `i35` in between, writing it
    // would be coerced to tail-follow by the registry AND reported as a broken
    // TimelineNode kind — a false positive on the one writer that cannot read
    // from a live node. Ask first; write the answer the coercion would give.
    const reveal = registry(true, (id) => id !== 'i35');
    const node = run(rows(100), { from: 90, rows: 10 });

    expect(revealActivityRunItem(reveal, node, 'i40')).toBe(true);
    expect(reveal.setWindowAnchor).toHaveBeenCalledWith('r1', null);
  });

  it('changes nothing for an item the run does not carry', () => {
    const reveal = registry();
    const node = run(rows(10), { from: 0, rows: 10 });

    expect(revealActivityRunItem(reveal, node, 'zz')).toBe(false);
    expect(reveal.expandForReveal).not.toHaveBeenCalled();
    expect(reveal.setWindowAnchor).not.toHaveBeenCalled();
    expect(reveal.requestFocus).not.toHaveBeenCalled();
  });
});
