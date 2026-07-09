import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  createPane,
  destroyPane,
  resetPanesForTest,
} from './panes.svelte';
import {
  getPaneLayoutItems,
  movePaneLayoutItem,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import {
  closeTakeControl,
  getTakeControlPane,
  installTakeControl,
  isTakeControlOpen,
  openTakeControl,
  resetTakeControlForTest,
  takeControlForSource,
  toggleTakeControl,
} from './takeControl.svelte';

function threadItem(paneId: string): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx: 1 };
}

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

beforeEach(() => {
  resetPanesForTest();
  resetTakeControlForTest();
  installTakeControl();
});

afterEach(() => {
  resetTakeControlForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('takeControl store', () => {
  it('opens a take-control pane immediately to the right of its source', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    const state = openTakeControl('main');

    expect(state).not.toBeNull();
    expect(state?.paneId).toBe('take-control-main');
    expect(state?.sourcePaneId).toBe('main');
    expect(paneIds()).toEqual(['main', 'take-control-main']);

    const item = getPaneLayoutItems()[1];
    expect(item.kind).toBe('take-control');
    expect(item.sourcePaneId).toBe('main');
    expect(isTakeControlOpen('main')).toBe(true);
    expect(getTakeControlPane('take-control-main')).toEqual(state);
    expect(takeControlForSource('main')).toEqual(state);
  });

  it('inserts the take-control pane between its source and the next pane', () => {
    setPaneLayoutItemsForTest([threadItem('a'), threadItem('b')]);

    openTakeControl('a');

    expect(paneIds()).toEqual(['a', 'take-control-a', 'b']);
  });

  it('does not open a second take-control pane for the same source', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    const first = openTakeControl('main');
    const second = openTakeControl('main');

    expect(second).toBe(first);
    expect(getPaneLayoutItems().filter((i) => i.kind === 'take-control')).toHaveLength(1);
  });

  it('refuses to open when the source pane is absent from the layout', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(openTakeControl('ghost')).toBeNull();
    expect(getPaneLayoutItems().some((i) => i.kind === 'take-control')).toBe(false);
  });

  it('closes the take-control pane while leaving the source pane in place', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    openTakeControl('main');

    closeTakeControl('take-control-main');

    expect(paneIds()).toEqual(['main']);
    expect(isTakeControlOpen('main')).toBe(false);
    expect(getTakeControlPane('take-control-main')).toBeNull();
  });

  it('closeTakeControl on an unknown pane id is a no-op', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    openTakeControl('main');

    expect(() => closeTakeControl('take-control-nope')).not.toThrow();
    expect(isTakeControlOpen('main')).toBe(true);
  });

  it('toggles the take-control pane open then closed', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(toggleTakeControl('main')).toBe(true);
    expect(isTakeControlOpen('main')).toBe(true);

    expect(toggleTakeControl('main')).toBe(false);
    expect(isTakeControlOpen('main')).toBe(false);
    expect(paneIds()).toEqual(['main']);
  });

  it('cascade-closes the take-control pane when its source pane is destroyed', () => {
    setPaneLayoutItemsForTest([threadItem('p1')]);
    createPane('p1');
    openTakeControl('p1');
    expect(paneIds()).toEqual(['p1', 'take-control-p1']);

    destroyPane('p1');

    expect(isTakeControlOpen('p1')).toBe(false);
    expect(getPaneLayoutItems().some((i) => i.paneId === 'take-control-p1')).toBe(false);
  });

  it('keeps the take-control pane glued to its source across a reorder (no dangling pane)', () => {
    setPaneLayoutItemsForTest([threadItem('a'), threadItem('b'), threadItem('c')]);
    openTakeControl('b');
    expect(paneIds()).toEqual(['a', 'b', 'take-control-b', 'c']);

    // Move 'a' one slot to the right. Naively this would land it between 'b'
    // and its take-control pane; the resnap invariant re-pins the pair so the
    // take-control pane stays immediately right of 'b'.
    movePaneLayoutItem('a', 1);

    const ids = paneIds();
    const bIndex = ids.indexOf('b');
    expect(ids[bIndex + 1]).toBe('take-control-b');
    expect(ids).toEqual(['b', 'take-control-b', 'a', 'c']);
    // Exactly one take-control item, and its source ('b') sits immediately to
    // its left.
    const takeControlItems = getPaneLayoutItems().filter((i) => i.kind === 'take-control');
    expect(takeControlItems).toHaveLength(1);
    const tcIndex = ids.indexOf('take-control-b');
    expect(ids[tcIndex - 1]).toBe('b');
  });
});
