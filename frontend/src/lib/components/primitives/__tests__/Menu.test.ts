// Exercises Menu's keyboard navigation and ARIA contract:
//   - role=menu + aria-orientation=vertical on the container.
//   - first enabled menuitem is focused on mount.
//   - ArrowDown / ArrowUp move focus; wrap at edges.
//   - Home / End jump to first/last.
//   - Plain j / k alias ArrowDown / ArrowUp (no modifier).
//   - Escape calls onClose.
//   - Disabled items are skipped by arrow navigation.
//   - Stage 1 redesign: container uses rounded-[var(--radius-control)] + shadow-menu + border-border-subtle.

import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Harness from './MenuHarness.svelte';
import AsyncHarness from './MenuAsyncHarness.svelte';

function activeLabel(): string | null {
  return document.activeElement?.textContent?.trim() ?? null;
}

async function pressKey(target: EventTarget, key: string): Promise<KeyboardEvent> {
  const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  target.dispatchEvent(ev);
  await tick();
  return ev;
}

async function flushMicrotasks(): Promise<void> {
  // Menu focuses its first item in a queueMicrotask; a single await is
  // enough in happy-dom but we chain two for safety.
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

describe('<Menu>', () => {
  it('mounts as a vertical menu with focus and the roving tabindex on the first enabled item', async () => {
    const { getByRole, getAllByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    expect(menu.getAttribute('aria-orientation')).toBe('vertical');
    expect(activeLabel()).toBe('Apple');
    const zeroTabIndex = getAllByRole('menuitem').filter((i) => i.tabIndex === 0);
    expect(zeroTabIndex).toHaveLength(1);
    expect(zeroTabIndex[0].textContent?.trim()).toBe('Apple');
    // Stage 1 redesign: the container uses the shared token scale so every
    // menu surface reads at the same radius + shadow as its consumers.
    expect(menu.className).toContain('rounded-[var(--radius-control)]');
    expect(menu.className).toContain('shadow-menu');
    expect(menu.className).toContain('border-border-subtle');
    // Legacy classes should be gone so the old look can't sneak back in.
    expect(menu.className).not.toContain('rounded-lg');
    expect(menu.className).not.toContain('shadow-xl');
  });

  // Each step presses a key and names the item that holds focus after it.
  // Focus starts on Apple; the items are Apple, Banana, Cherry, Date.
  it.each([
    { name: 'ArrowDown moves focus forward and wraps', steps: [['ArrowDown', 'Banana'], ['ArrowDown', 'Cherry'], ['ArrowDown', 'Date'], ['ArrowDown', 'Apple']] },
    { name: 'ArrowUp moves focus backwards and wraps', steps: [['ArrowUp', 'Date']] },
    { name: 'Home / End jump to first and last items', steps: [['End', 'Date'], ['Home', 'Apple']] },
    { name: 'plain j moves focus forward like ArrowDown', steps: [['j', 'Banana'], ['j', 'Cherry']] },
    { name: 'plain k moves focus backwards like ArrowUp', steps: [['k', 'Date']] },
    { name: 'does not typeahead — single letters other than j/k are inert', steps: [['c', 'Apple']] },
  ])('$name', async ({ steps }) => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    for (const [key, label] of steps) {
      await pressKey(menu, key);
      expect(activeLabel()).toBe(label);
    }
  });

  it('skips disabled items during arrow navigation', async () => {
    const { getByRole } = render(Harness, { props: { disableSecond: true } });
    await flushMicrotasks();
    const menu = getByRole('menu');
    await pressKey(menu, 'ArrowDown');
    // Banana is disabled; focus should skip to Cherry.
    expect(activeLabel()).toBe('Cherry');
  });

  it('Escape calls onClose', async () => {
    const onClose = vi.fn();
    const { getByRole } = render(Harness, { props: { onClose } });
    await flushMicrotasks();
    await pressKey(getByRole('menu'), 'Escape');
    expect(onClose).toHaveBeenCalled();
  });

  // Regression: a cold-cache ProviderModelsSubmenu renders a loading
  // placeholder first, then swaps in the real MenuItems after an async
  // binding round-trip. The old one-shot
  // queueMicrotask inside Menu's focus effect fired before items
  // existed, bailed, and never re-attempted — so no row ever got
  // tabindex=0 and keyboard nav was broken until the user pressed an
  // arrow key. Fix: a MutationObserver watches the container for
  // added children and lands focus on the first real item when it
  // appears.
  describe('async-hydrated items', () => {
    it('focuses the first item once it mounts (MutationObserver path) and does not re-steal focus on subsequent mutations', async () => {
      const { rerender, getByRole, queryByRole } = render(AsyncHarness, {
        props: { hydrated: false },
      });
      await flushMicrotasks();
      // No menuitems yet — only the loading placeholder renders.
      expect(queryByRole('menuitem')).toBeNull();
      expect(document.activeElement?.textContent).not.toMatch(/Alpha/);

      // Hydrate: real MenuItems appear. The observer should see the
      // child-list mutation and call setFocus(0, items).
      await rerender({ hydrated: true });
      // Observer callbacks are queued as microtasks — flush.
      await flushMicrotasks();
      await flushMicrotasks();

      // First real item owns the roving tabindex AND has focus.
      const first = getByRole('menuitem', { name: 'Alpha' });
      expect(first.tabIndex).toBe(0);
      expect(document.activeElement).toBe(first);

      // Move focus with ArrowDown.
      await pressKey(getByRole('menu'), 'ArrowDown');
      expect(activeLabel()).toBe('Bravo');

      // A subsequent rerender (simulating an upstream prop change
      // that doesn't affect the item list) must not drag focus back
      // to item 0.
      await rerender({ hydrated: true });
      await flushMicrotasks();
      expect(activeLabel()).toBe('Bravo');
    });
  });
});
