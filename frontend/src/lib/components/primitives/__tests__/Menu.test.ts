// Exercises Menu's keyboard navigation and ARIA contract:
//   - role=menu + aria-orientation=vertical on the container.
//   - first enabled menuitem is focused on mount.
//   - ArrowDown / ArrowUp move focus; wrap at edges.
//   - Home / End jump to first/last.
//   - Typeahead jumps to the first item starting with the typed letter.
//   - Escape calls onClose.
//   - Disabled items are skipped by arrow navigation.
//   - Stage 1 redesign: container uses rounded-[var(--radius-control)] + shadow-menu + border-border-subtle.

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
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
  it('exposes role=menu with vertical orientation', async () => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    expect(menu.getAttribute('aria-orientation')).toBe('vertical');
  });

  it('focuses the first enabled menuitem on mount', async () => {
    render(Harness);
    await flushMicrotasks();
    expect(activeLabel()).toBe('Apple');
  });

  it('ArrowDown moves focus forward and wraps', async () => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    await pressKey(menu, 'ArrowDown');
    expect(activeLabel()).toBe('Banana');
    await pressKey(menu, 'ArrowDown');
    expect(activeLabel()).toBe('Cherry');
    await pressKey(menu, 'ArrowDown');
    expect(activeLabel()).toBe('Date');
    await pressKey(menu, 'ArrowDown');
    // Wrap.
    expect(activeLabel()).toBe('Apple');
  });

  it('ArrowUp moves focus backwards and wraps', async () => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    await pressKey(menu, 'ArrowUp');
    expect(activeLabel()).toBe('Date');
  });

  it('Home / End jump to first and last items', async () => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    await pressKey(menu, 'End');
    expect(activeLabel()).toBe('Date');
    await pressKey(menu, 'Home');
    expect(activeLabel()).toBe('Apple');
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

  it('typeahead jumps focus to the item starting with the typed letter', async () => {
    const { getByRole } = render(Harness);
    await flushMicrotasks();
    const menu = getByRole('menu');
    await pressKey(menu, 'c');
    expect(activeLabel()).toBe('Cherry');
  });

  it('applies roving tabindex — only one menuitem has tabindex=0 at a time', async () => {
    const { getAllByRole } = render(Harness);
    await flushMicrotasks();
    const items = getAllByRole('menuitem');
    const zeroTabIndex = items.filter((i) => i.tabIndex === 0);
    expect(zeroTabIndex).toHaveLength(1);
    // First enabled item gets the zero.
    expect(zeroTabIndex[0].textContent?.trim()).toBe('Apple');
  });

  // Stage 1 redesign: menu container now uses the new token scale so
  // every menu surface reads at the same radius + shadow as its
  // consumers.
  it('container uses the redesigned radius + shadow + subtle border tokens', () => {
    const { getByRole } = render(Harness);
    const menu = getByRole('menu');
    expect(menu.className).toContain('rounded-[var(--radius-control)]');
    expect(menu.className).toContain('shadow-menu');
    expect(menu.className).toContain('border-border-subtle');
    // Legacy classes should be gone so the old look can't sneak back in.
    expect(menu.className).not.toContain('rounded-lg');
    expect(menu.className).not.toContain('shadow-xl');
  });

  // Regression: DiscussionsSubmenu and cold-cache ProviderModelsSubmenu
  // render a loading placeholder first, then swap in the real
  // MenuItems after an async binding round-trip. The old one-shot
  // queueMicrotask inside Menu's focus effect fired before items
  // existed, bailed, and never re-attempted — so no row ever got
  // tabindex=0 and keyboard nav was broken until the user pressed an
  // arrow key. Fix: a MutationObserver watches the container for
  // added children and lands focus on the first real item when it
  // appears.
  describe('async-hydrated items', () => {
    it('focuses the first item once it mounts (MutationObserver path)', async () => {
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
    });

    it('does not re-steal focus on subsequent mutations', async () => {
      const { rerender, getByRole } = render(AsyncHarness, {
        props: { hydrated: false },
      });
      await flushMicrotasks();
      await rerender({ hydrated: true });
      await flushMicrotasks();
      await flushMicrotasks();

      // Move focus with ArrowDown.
      const menu = getByRole('menu');
      await pressKey(menu, 'ArrowDown');
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
