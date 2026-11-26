// ContextMenu in real Chromium on a short phone screen: a menu taller than
// the visible viewport stays inside it and scrolls its rows, so its last
// row can be reached.

import { afterEach, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { page } from 'vitest/browser';
import '../../../../app.css';
import Harness from './ContextMenuHarness.svelte';
import { setCompactLayoutForTest } from '../../../stores/layoutMode.svelte';

const mounted: object[] = [];

afterEach(() => {
  for (const app of mounted.splice(0)) unmount(app);
  setCompactLayoutForTest(false);
});

it.each([
  { layout: 'a short landscape phone', compact: true },
  { layout: 'a short desktop window', compact: false },
])('keeps a menu taller than $layout inside it, its rows scrolling to the last', async ({ compact }) => {
  await page.viewport(640, 320);
  setCompactLayoutForTest(compact);
  mounted.push(mount(Harness, { target: document.body, props: { x: 30, y: 200, more: 30 } }));
  await tick();
  await new Promise((resolve) => requestAnimationFrame(resolve));
  const surface = document.querySelector<HTMLElement>('[data-context-menu]')!;
  const box = surface.getBoundingClientRect();
  const height = window.visualViewport?.height ?? window.innerHeight;
  expect(box.top).toBeGreaterThanOrEqual(0);
  expect(box.bottom).toBeLessThanOrEqual(height);
  const last = [...surface.querySelectorAll<HTMLElement>('[role="menuitem"]')].at(-1)!;
  expect(last.textContent).toContain('Row 30');
  last.scrollIntoView({ block: 'nearest' });
  await new Promise((resolve) => requestAnimationFrame(resolve));
  const row = last.getBoundingClientRect();
  expect(row.top).toBeGreaterThanOrEqual(box.top);
  expect(row.bottom).toBeLessThanOrEqual(box.bottom + 0.5);
  // Its own rows scroll, not the page.
  expect(document.scrollingElement!.scrollTop).toBe(0);
});
