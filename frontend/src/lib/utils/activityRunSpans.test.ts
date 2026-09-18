// Physical run membership over rows (docs/architecture/timeline-window-pages.md §1).
// The cross-language cases live in `activityRunVectors.test.ts`; what is
// pinned here is the reading this module adds on top of that rule — the
// rows a history page would never return, which must not split a run.
import { describe, expect, it } from 'vitest';
import { groupActivityRunSpans, isActivityRailRow, spanHolding } from './activityRunSpans';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';

let index = 0;
function row(id: string, overrides: Partial<Item> = {}): Item {
  index += 1;
  return makeItem({ id, turnIndex: 0, itemIndex: index, kind: 'tool_call', toolName: 'Bash', ...overrides });
}
function prose(id: string): Item {
  return row(id, { kind: 'assistant_text', toolName: '' });
}

describe('isActivityRailRow', () => {
  it('takes the rail kinds and drops a proposed plan', () => {
    expect(isActivityRailRow(row('a'))).toBe(true);
    expect(isActivityRailRow(row('b', { kind: 'thinking' }))).toBe(true);
    expect(isActivityRailRow(row('c', { kind: 'terminal_interaction' }))).toBe(true);
    expect(isActivityRailRow(row('d', { kind: 'tool_completion' }))).toBe(true);
    expect(isActivityRailRow(row('e', { payloadKind: 'proposed_plan' }))).toBe(false);
    expect(isActivityRailRow(prose('f'))).toBe(false);
    expect(isActivityRailRow(row('g', { kind: 'notification' }))).toBe(false);
  });
});

describe('groupActivityRunSpans', () => {
  it('finds maximal stretches and absorbs a trailing bell', () => {
    const items = [prose('p0'), row('t0'), row('t1'), row('n0', { kind: 'notification' }), prose('p1'), row('t2')];
    expect(groupActivityRunSpans(items).map((span) => span.items.map((item) => item.id)))
      .toEqual([['t0', 't1', 'n0'], ['t2']]);
  });

  it('does not absorb a bell with no member before it', () => {
    const items = [prose('p0'), row('n0', { kind: 'notification' }), row('t0')];
    expect(groupActivityRunSpans(items).map((span) => span.items.map((item) => item.id)))
      .toEqual([['t0']]);
  });

  it('skips rows a page would not return without breaking a run', () => {
    // A subagent child and a plan_update bell are not part of any page's
    // range, so they cannot split a run the page ships whole.
    const items = [
      row('t0'),
      row('c0', { parentId: 't0' }),
      row('pu', { kind: 'notification', toolName: 'plan_update' }),
      row('t1'),
    ];
    expect(groupActivityRunSpans(items).map((span) => span.items.map((item) => item.id)))
      .toEqual([['t0', 't1']]);
  });

  it('allocates nothing for a prose-only window', () => {
    expect(groupActivityRunSpans([prose('p0'), prose('p1')])).toEqual([]);
  });

  it('finds the span holding an id', () => {
    const spans = groupActivityRunSpans([row('t0'), prose('p0'), row('t1'), row('t2')]);
    expect(spanHolding(spans, 't2')?.firstItemId).toBe('t1');
    expect(spanHolding(spans, 'p0')).toBeNull();
    expect(spanHolding(spans, '')).toBeNull();
  });
});
