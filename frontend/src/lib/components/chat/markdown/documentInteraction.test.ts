import { acquireDocumentInteraction } from 'svelte-streamdown';
import { afterEach, describe, expect, it, vi } from 'vitest';

const mounted: Node[] = [];

afterEach(() => {
  document.getSelection()?.removeAllRanges();
  document.dispatchEvent(new Event('selectionchange'));
  for (const node of mounted.splice(0)) node.parentNode?.removeChild(node);
});

describe('document interaction snapshots', () => {
  it('tracks selection start, range ownership, and clear transitions', () => {
    const root = document.createElement('div');
    const text = document.createTextNode('selected text');
    root.append(text);
    document.body.append(root);
    mounted.push(root);

    const changed = vi.fn();
    const interaction = acquireDocumentInteraction(document, changed);
    const selection = document.getSelection();
    if (!selection) throw new Error('test document has no Selection');

    document.dispatchEvent(new Event('selectstart'));
    expect(interaction.selectionPending).toBe(true);

    const range = document.createRange();
    range.setStart(text, 0);
    range.setEnd(text, 8);
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event('selectionchange'));
    expect(interaction.selectionPending).toBe(false);
    expect(interaction.ranges).toHaveLength(1);
    expect(interaction.ranges[0].endpointAncestors.has(root)).toBe(true);

    selection.removeAllRanges();
    document.dispatchEvent(new Event('selectionchange'));
    expect(interaction.ranges).toHaveLength(0);
    expect(changed.mock.calls.length).toBeGreaterThanOrEqual(3);
    const callsBeforeRelease = changed.mock.calls.length;
    interaction.release();
    document.dispatchEvent(new Event('selectionchange'));
    expect(changed).toHaveBeenCalledTimes(callsBeforeRelease);
  });

  it('shares one tracker while leases release independently and idempotently', () => {
    const firstChanged = vi.fn();
    const secondChanged = vi.fn();
    const first = acquireDocumentInteraction(document, firstChanged);
    const second = acquireDocumentInteraction(document, secondChanged);
    const button = document.createElement('button');
    document.body.append(button);
    mounted.push(button);

    button.focus();
    expect(first.focusedAncestors.has(button)).toBe(true);
    expect(second.focusedAncestors.has(button)).toBe(true);
    expect(firstChanged).toHaveBeenCalledTimes(1);
    expect(secondChanged).toHaveBeenCalledTimes(1);

    first.release();
    first.release();
    button.blur();
    expect(firstChanged).toHaveBeenCalledTimes(1);
    expect(secondChanged).toHaveBeenCalledTimes(2);

    second.release();
    button.focus();
    expect(secondChanged).toHaveBeenCalledTimes(2);
  });
});
