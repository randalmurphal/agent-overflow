import { cleanup, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../chat/ChatView.svelte', () => ({ default: () => ({}) }));

import PaneHost from './PaneHost.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { getPaneWidth, resetLayoutMetricsForTest } from '../../stores/layoutMetrics.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  observed: Element | null = null;
  private readonly callback: ResizeObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed = target;
  }

  disconnect(): void {
    this.observed = null;
  }

  unobserve(): void {
    this.observed = null;
  }

  trigger(width: number): void {
    if (!this.observed) return;
    this.callback([
      {
        target: this.observed,
        contentRect: { width, height: 400 } as DOMRectReadOnly,
      } as ResizeObserverEntry,
    ], this as unknown as ResizeObserver);
  }
}

describe('PaneHost', () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof FireableResizeObserver }).ResizeObserver =
      FireableResizeObserver;
    FireableResizeObserver.instances = [];
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
  });

  afterEach(() => {
    cleanup();
    if (originalResizeObserver) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
        originalResizeObserver;
    }
    FireableResizeObserver.instances = [];
    resetLayoutMetricsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
  });

  it('publishes and clears measured widths by pane id', () => {
    registerPaneForTest('left', createThreadPane({ paneId: 'left' }));
    registerPaneForTest('right', createThreadPane({ paneId: 'right' }));
    setPaneLayoutItemsForTest([
      { id: 'left-item', paneId: 'left', kind: 'thread', minWidth: 560 },
      { id: 'right-item', paneId: 'right', kind: 'thread', minWidth: 560 },
    ]);

    const rendered = render(PaneHost);
    const host = rendered.getByTestId('pane-host');
    const left = rendered.container.querySelector('[data-pane-id="left"]');
    const right = rendered.container.querySelector('[data-pane-id="right"]');
    if (!left || !right) throw new Error('expected pane sections');

    FireableResizeObserver.instances.find((ro) => ro.observed === host)?.trigger(1200);
    FireableResizeObserver.instances.find((ro) => ro.observed === left)?.trigger(500);
    FireableResizeObserver.instances.find((ro) => ro.observed === right)?.trigger(700);

    expect(getPaneWidth('left')).toBe(500);
    expect(getPaneWidth('right')).toBe(700);

    rendered.unmount();

    expect(getPaneWidth('left')).toBe(1200);
    expect(getPaneWidth('right')).toBe(1200);
  });
});
