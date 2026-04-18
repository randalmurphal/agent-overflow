// Exercises Menu's keyboard navigation and ARIA contract:
//   - role=menu + aria-orientation=vertical on the container.
//   - first enabled menuitem is focused on mount.
//   - ArrowDown / ArrowUp move focus; wrap at edges.
//   - Home / End jump to first/last.
//   - Typeahead jumps to the first item starting with the typed letter.
//   - Escape calls onClose.
//   - Disabled items are skipped by arrow navigation.

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import Harness from './MenuHarness.svelte';

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
});
