// Escape on a popover is CLAIMED, not merely handled.
//
// `stopPropagation` keeps the press off the surfaces behind this one, but
// it says nothing to the code that asks "did anybody take it". The phone
// shell's hardware back button asks exactly that: it synthesises an Escape
// and reads `defaultPrevented` (`native/lifecycle.ts`). Without the
// `preventDefault` one back press dismissed the sheet AND went back a
// screen, or left the app from the list.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { createRawSnippet, flushSync, mount, unmount } from 'svelte';
import Popover from './Popover.svelte';

let dispose: (() => void) | null = null;

afterEach(() => {
  dispose?.();
  dispose = null;
  document.body.innerHTML = '';
});

function openPopover(): { onClose: ReturnType<typeof vi.fn> } {
  const anchor = document.createElement('button');
  document.body.appendChild(anchor);
  const host = document.createElement('div');
  document.body.appendChild(host);
  const onClose = vi.fn();
  const app = mount(Popover, {
    target: host,
    props: {
      anchor,
      open: true,
      onClose,
      children: createRawSnippet(() => ({ render: () => '<span>body</span>' })),
    },
  });
  // The document-level keydown listener is installed by an effect, so the
  // press below has nothing to hit until the effect has run.
  flushSync();
  dispose = () => void unmount(app);
  return { onClose };
}

function pressEscape(): KeyboardEvent {
  const event = new KeyboardEvent('keydown', {
    key: 'Escape',
    code: 'Escape',
    bubbles: true,
    cancelable: true,
  });
  document.dispatchEvent(event);
  return event;
}

describe('Popover, Escape', () => {
  it('closes and marks the press taken, so the back button sees it', () => {
    const { onClose } = openPopover();

    const event = pressEscape();

    expect(onClose).toHaveBeenCalledWith('escape');
    expect(event.defaultPrevented).toBe(true);
  });

  it('leaves an unrelated key alone', () => {
    const { onClose } = openPopover();

    const event = new KeyboardEvent('keydown', {
      key: 'a',
      bubbles: true,
      cancelable: true,
    });
    document.dispatchEvent(event);

    expect(onClose).not.toHaveBeenCalled();
    expect(event.defaultPrevented).toBe(false);
  });
});
