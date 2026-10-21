// The on-screen rows action: it reports the rows inside the container's
// box, and tells a consumer that goes (destroyed, or replaced) that its
// rows left the screen, so the focus set it fed does not stay behind.

import { waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { visibleRows } from './visibleRows';

function rect(top: number, bottom: number): DOMRect {
  return { top, bottom, left: 0, right: 100, width: 100, height: bottom - top, x: 0, y: top, toJSON: () => ({}) } as DOMRect;
}

function list(keys: string[]): HTMLElement {
  const node = document.createElement('div');
  node.getBoundingClientRect = () => rect(0, 100);
  keys.forEach((key, index) => {
    const row = document.createElement('div');
    row.dataset.prKey = key;
    row.getBoundingClientRect = () => rect(index * 40, index * 40 + 40);
    node.append(row);
  });
  document.body.append(node);
  return node;
}

afterEach(() => {
  document.body.replaceChildren();
});

describe('visibleRows', () => {
  it('reports the rows on screen, then none when destroyed', async () => {
    const node = list(['a', 'b', 'c']);
    const onVisible = vi.fn();
    const action = visibleRows(node, { attr: 'pr-key', onVisible });
    await waitFor(() => expect(onVisible).toHaveBeenLastCalledWith(['a', 'b', 'c']));

    action.destroy();
    expect(onVisible).toHaveBeenLastCalledWith([]);
    expect(onVisible).toHaveBeenCalledTimes(2);
  });

  it('says nothing on destroy when it reported no rows', async () => {
    const node = list([]);
    const onVisible = vi.fn();
    const action = visibleRows(node, { attr: 'pr-key', onVisible });
    await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
    action.destroy();
    expect(onVisible).not.toHaveBeenCalled();
  });

  it('tells a replaced consumer its rows left, and the new one what is on screen', async () => {
    const node = list(['a', 'b']);
    const first = vi.fn();
    const second = vi.fn();
    const action = visibleRows(node, { attr: 'pr-key', onVisible: first });
    await waitFor(() => expect(first).toHaveBeenLastCalledWith(['a', 'b']));

    action.update({ attr: 'pr-key', onVisible: second });
    expect(first).toHaveBeenLastCalledWith([]);
    await waitFor(() => expect(second).toHaveBeenLastCalledWith(['a', 'b']));

    // Attach, detach, attach: each consumer ends on what it was last told.
    action.destroy();
    expect(second).toHaveBeenLastCalledWith([]);
    const third = vi.fn();
    const again = visibleRows(node, { attr: 'pr-key', onVisible: third });
    await waitFor(() => expect(third).toHaveBeenLastCalledWith(['a', 'b']));
    again.destroy();
    expect(third).toHaveBeenLastCalledWith([]);
    expect(first).toHaveBeenCalledTimes(2);
  });
});
