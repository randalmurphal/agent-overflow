import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  paneHeaderLineFor,
  resolvePaneHeaderLine,
  type PaneHeaderLineInput,
} from './paneHeaderLine';
import { focusPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { setThreadStatus, type ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { makeThread } from '../../../test/helpers/chat';

const READ = { lastReadAt: 2, latestTurnCompletedAt: 1 };
const UNREAD = { lastReadAt: 1, latestTurnCompletedAt: 2 };

function unfocused(overrides: Partial<PaneHeaderLineInput> = {}): PaneHeaderLineInput {
  return { focused: false, paneCount: 2, compact: false, thread: READ, status: 'idle', ...overrides };
}

describe('resolvePaneHeaderLine', () => {
  it('draws nothing in compact layout, whatever the state', () => {
    expect(resolvePaneHeaderLine(unfocused({ compact: true, focused: true }))).toBeNull();
    expect(resolvePaneHeaderLine(unfocused({ compact: true, status: 'pending-approval' }))).toBeNull();
  });

  it('draws nothing on a lone pane, whatever the state', () => {
    expect(resolvePaneHeaderLine(unfocused({ paneCount: 1, focused: true }))).toBeNull();
    expect(resolvePaneHeaderLine(unfocused({ paneCount: 1, status: 'error' }))).toBeNull();
    expect(resolvePaneHeaderLine(unfocused({ paneCount: 1, thread: UNREAD }))).toBeNull();
  });

  it('marks the focused pane accent regardless of its thread', () => {
    for (const status of ['idle', 'running', 'pending-approval', 'error'] as const) {
      expect(resolvePaneHeaderLine(unfocused({ focused: true, status }))).toEqual({
        color: 'accent',
        label: 'Focused pane',
      });
    }
    expect(resolvePaneHeaderLine(unfocused({ focused: true, thread: null, status: null }))).toEqual({
      color: 'accent',
      label: 'Focused pane',
    });
  });

  it.each<[ThreadLiveStatus, string | null, string | null]>([
    ['error', 'error', 'Failed'],
    ['pending-approval', 'warning', 'Pending Approval'],
    ['awaiting-input', 'info', 'Awaiting Input'],
    ['running', null, null],
    ['setup-failed', 'warning', 'Setup Failed'],
    ['plan-ready', 'success', 'Plan Ready'],
    ['interrupted', 'warning', 'Interrupted'],
    ['idle', null, null],
  ])('colors an unfocused pane for %s', (status, color, label) => {
    const line = resolvePaneHeaderLine(unfocused({ status }));
    if (color === null) expect(line).toBeNull();
    else expect(line).toEqual({ color, label });
  });

  it('marks an unfocused idle pane with unread completion as Completed', () => {
    expect(resolvePaneHeaderLine(unfocused({ thread: UNREAD }))).toEqual({
      color: 'success',
      label: 'Completed',
    });
    expect(resolvePaneHeaderLine(unfocused({ thread: READ }))).toBeNull();
  });

  it('draws nothing for an unfocused pane without a thread', () => {
    expect(resolvePaneHeaderLine(unfocused({ thread: null, status: null }))).toBeNull();
  });
});

describe('paneHeaderLineFor', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    setCompactLayoutForTest(false);
  });

  afterEach(() => {
    setCompactLayoutForTest(false);
  });

  function layoutWithPanes(...paneIds: string[]): void {
    for (const paneId of paneIds) registerPaneForTest(paneId, createThreadPane({ paneId }));
    setPaneLayoutItemsForTest(
      paneIds.map((paneId) => ({ id: paneId, paneId, kind: 'thread' as const, widthPx: 1 })),
    );
  }

  it('reads focus, pane count and layout mode from the stores', () => {
    layoutWithPanes('a', 'b');
    focusPane('a');
    expect(paneHeaderLineFor('a', null)?.color).toBe('accent');
    expect(paneHeaderLineFor('b', null)).toBeNull();
    focusPane('b');
    expect(paneHeaderLineFor('a', null)).toBeNull();
    expect(paneHeaderLineFor('b', null)?.color).toBe('accent');
    setCompactLayoutForTest(true);
    expect(paneHeaderLineFor('b', null)).toBeNull();
  });

  it('counts companion panes as panes to tell apart', () => {
    registerPaneForTest('a', createThreadPane({ paneId: 'a' }));
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 1 },
      { id: 'agent-a', paneId: 'agent-a', kind: 'agent', widthPx: 1, sourcePaneId: 'a' },
    ]);
    focusPane('agent-a');
    expect(paneHeaderLineFor('a', null)).toBeNull();
    expect(paneHeaderLineFor('agent-a', null)?.color).toBe('accent');
  });

  it('resolves the unfocused pane color from the live thread status', () => {
    layoutWithPanes('a', 'b');
    focusPane('a');
    const thread = makeThread({ id: 'pane-header-line-test-thread' });
    expect(paneHeaderLineFor('b', thread)).toBeNull();
    setThreadStatus(thread.id, 'awaiting-input');
    expect(paneHeaderLineFor('b', thread)).toEqual({ color: 'info', label: 'Awaiting Input' });
    setThreadStatus(thread.id, 'idle');
    expect(paneHeaderLineFor('b', thread)).toBeNull();
  });
});
