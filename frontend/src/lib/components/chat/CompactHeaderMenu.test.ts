// CompactHeaderMenu — the generic "too much chrome" dropdown the
// chat header falls back to at narrow widths. The harness is a thin
// Svelte wrapper because snippets can't be constructed from a .ts
// file; it exposes the `onClose` callback on a button so we can
// exercise the callback-close path directly.

import { describe, expect, it, beforeAll } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import CompactHeaderMenuTestHarness from './CompactHeaderMenuTestHarness.svelte';

// happy-dom lacks Element.animate; the fade/fly transitions call it
// when the popover mounts/unmounts. Install the same shim other tests
// in this directory use so the element actually lands in the DOM.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

describe('<CompactHeaderMenu>', () => {
  it('starts closed and renders only the trigger', () => {
    const { getByTestId, queryByTestId } = render(CompactHeaderMenuTestHarness, {
      props: { bodyText: 'hello' },
    });

    const trigger = getByTestId('compact-header-menu-trigger');
    expect(trigger).toBeInTheDocument();
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(queryByTestId('compact-header-menu')).toBeNull();
    expect(queryByTestId('menu-body')).toBeNull();
  });

  it('opens on trigger click and renders the snippet children', async () => {
    const { getByTestId } = render(CompactHeaderMenuTestHarness, {
      props: { bodyText: 'payload' },
    });
    const trigger = getByTestId('compact-header-menu-trigger');
    await fireEvent.click(trigger);
    await tick();

    expect(getByTestId('compact-header-menu')).toBeInTheDocument();
    expect(trigger.getAttribute('aria-expanded')).toBe('true');
    const body = getByTestId('menu-body');
    expect(body.textContent ?? '').toContain('payload');
  });

  it('uses the label prop for the trigger and aria-label', async () => {
    const { getByTestId } = render(CompactHeaderMenuTestHarness, {
      props: { label: 'Header actions' },
    });
    const trigger = getByTestId('compact-header-menu-trigger');
    expect(trigger.textContent?.trim()).toBe('Header actions');
    expect(trigger.getAttribute('aria-label')).toBe('Header actions');

    await fireEvent.click(trigger);
    await tick();
    expect(getByTestId('compact-header-menu').getAttribute('aria-label')).toBe(
      'Header actions',
    );
  });

  it('closes on backdrop click', async () => {
    const { getByTestId, queryByTestId } = render(CompactHeaderMenuTestHarness);
    await fireEvent.click(getByTestId('compact-header-menu-trigger'));
    await tick();

    const backdrop = getByTestId('compact-header-menu-backdrop');
    await fireEvent.click(backdrop);
    await tick();

    expect(queryByTestId('compact-header-menu')).toBeNull();
    expect(getByTestId('compact-header-menu-trigger').getAttribute('aria-expanded')).toBe(
      'false',
    );
  });

  it('closes on Escape from inside the menu', async () => {
    const { getByTestId, queryByTestId } = render(CompactHeaderMenuTestHarness);
    await fireEvent.click(getByTestId('compact-header-menu-trigger'));
    await tick();

    const menu = getByTestId('compact-header-menu');
    await fireEvent.keyDown(menu, { key: 'Escape' });
    await tick();

    expect(queryByTestId('compact-header-menu')).toBeNull();
  });

  it('closes when the snippet invokes onClose', async () => {
    const { getByTestId, queryByTestId } = render(CompactHeaderMenuTestHarness);
    await fireEvent.click(getByTestId('compact-header-menu-trigger'));
    await tick();

    // The harness surfaces the passed-in `onClose` on this button so
    // we can exercise the exact path a real child would take.
    await fireEvent.click(getByTestId('menu-close-from-child'));
    await tick();

    expect(queryByTestId('compact-header-menu')).toBeNull();
  });

  it('traps Tab focus inside the open menu', async () => {
    const { getByTestId } = render(CompactHeaderMenuTestHarness);
    await fireEvent.click(getByTestId('compact-header-menu-trigger'));
    await tick();

    const closeBtn = getByTestId('menu-close-from-child');
    const lastBtn = getByTestId('menu-focus-last');

    // The open-effect focuses the first interactive child.
    expect(document.activeElement).toBe(closeBtn);

    // Shift+Tab from the first item should wrap to the last.
    await fireEvent.keyDown(closeBtn, { key: 'Tab', shiftKey: true });
    await tick();
    expect(document.activeElement).toBe(lastBtn);

    // Tab from the last item should wrap to the first.
    await fireEvent.keyDown(lastBtn, { key: 'Tab' });
    await tick();
    expect(document.activeElement).toBe(closeBtn);
  });
});
