// The keyboard-vs-mouse contract of the popover active-row reveal: keyboard
// changes scroll the active option into view, mouse-declared changes never
// scroll, and a skip is consumed by exactly one sync.

import { describe, expect, it, vi } from 'vitest';
import { createActiveOptionReveal } from './popoverOptionReveal';

function buildList(count: number, activeIndex: number) {
  const container = document.createElement('div');
  const scrolls: ReturnType<typeof vi.fn>[] = [];
  for (let i = 0; i < count; i++) {
    const row = document.createElement('button');
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', String(i === activeIndex));
    const spy = vi.fn();
    row.scrollIntoView = spy;
    container.appendChild(row);
    scrolls.push(spy);
  }
  return { container, scrolls };
}

describe('createActiveOptionReveal', () => {
  it('scrolls the aria-selected row into view on a keyboard change', () => {
    const { container, scrolls } = buildList(3, 2);
    createActiveOptionReveal().sync(2, container);
    expect(scrolls[2]).toHaveBeenCalledWith({ block: 'nearest' });
    expect(scrolls[0]).not.toHaveBeenCalled();
    expect(scrolls[1]).not.toHaveBeenCalled();
  });

  it('never scrolls for the change the mouse announced', () => {
    const { container, scrolls } = buildList(3, 1);
    const reveal = createActiveOptionReveal();
    reveal.hovered(1);
    reveal.sync(1, container);
    expect(scrolls[1]).not.toHaveBeenCalled();
  });

  it('consumes the hover skip in one sync, so the next keyboard move scrolls', () => {
    const reveal = createActiveOptionReveal();
    const first = buildList(3, 1);
    reveal.hovered(1);
    reveal.sync(1, first.container);
    const second = buildList(3, 1);
    reveal.sync(1, second.container);
    expect(second.scrolls[1]).toHaveBeenCalledWith({ block: 'nearest' });
  });

  it('scrolls when the keyboard overtakes a pending hover', () => {
    // Hover announced row 1, but by the time the effect flushed the keyboard
    // had moved the selection to row 2 — the final state is keyboard-made.
    const { container, scrolls } = buildList(3, 2);
    const reveal = createActiveOptionReveal();
    reveal.hovered(1);
    reveal.sync(2, container);
    expect(scrolls[2]).toHaveBeenCalledWith({ block: 'nearest' });
  });

  it('tolerates a missing container and an empty list', () => {
    const reveal = createActiveOptionReveal();
    reveal.sync(0, undefined);
    reveal.sync(0, document.createElement('div'));
  });
});
