import { mount, unmount } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import ReviewFileTree from './ReviewFileTree.svelte';
import type { PatchFile } from '../../utils/patchFiles';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

// The file tree mounts only the rows in view. In real Chromium: a diff of
// thousands of files mounts a window of rows, each as tall as an `h-7` row
// and laid edge to edge, the scroll range covers every row, the last file
// is reached by scrolling and opens, rows follow the root font size, and a
// focused row scrolled away keeps its place in the tab order.

const FILE_COUNT = 3000;
// One directory row above the files.
const ROW_COUNT = FILE_COUNT + 1;
const PANE_HEIGHT = 420;

const FILES: PatchFile[] = Array.from({ length: FILE_COUNT }, (_, index) => ({
  path: `src/mod${String(index).padStart(4, '0')}.ts`,
  kind: 'modified',
  additions: 1,
  deletions: 0,
  lines: [],
}));

let host: HTMLDivElement | undefined;
let app: Record<string, unknown> | undefined;

function mountTree(onSelectFile: (path: string) => void = () => {}): HTMLDivElement {
  host = document.createElement('div');
  host.style.cssText = `display: flex; flex-direction: column; width: 260px; height: ${PANE_HEIGHT}px;`;
  document.body.appendChild(host);
  app = mount(ReviewFileTree, { target: host, props: { files: FILES, onSelectFile } });
  return host;
}

afterEach(() => {
  if (app) void unmount(app);
  host?.remove();
  app = undefined;
  host = undefined;
  document.documentElement.style.removeProperty('font-size');
});

const viewport = (root: HTMLElement) => root.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
const rows = (root: HTMLElement) => [...root.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]')];
const filePaths = (root: HTMLElement) =>
  [...root.querySelectorAll<HTMLElement>('[data-testid="review-tree-file"]')].map((node) => node.dataset.filePath);

/** Every mounted row is `height` tall, holds a button of the same height, and meets the next. */
function expectTiled(root: HTMLElement, height: number): void {
  const mounted = rows(root);
  expect(mounted.length).toBeGreaterThan(0);
  let previousBottom: number | undefined;
  for (const row of mounted) {
    const rect = row.getBoundingClientRect();
    expect(rect.height).toBeCloseTo(height, 3);
    expect(row.querySelector('button')!.getBoundingClientRect().height).toBeCloseTo(height, 3);
    if (previousBottom !== undefined) expect(rect.top).toBeCloseTo(previousBottom, 3);
    previousBottom = rect.bottom;
  }
}

describe('<ReviewFileTree> windowing in a real browser', () => {
  it('mounts a window of 28px rows over the full scroll range', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');

    // 420px pane less the search header: under 15 rows in view, 8 of overscan.
    expect(rows(root).length).toBeLessThan(40);
    expectTiled(root, 28);
    const list = viewport(root);
    expect(rows(root)[0].getBoundingClientRect().top).toBeCloseTo(list.getBoundingClientRect().top, 3);
    // Every row plus the list's bottom padding (pb-2).
    expect(list.scrollHeight).toBe(ROW_COUNT * 28 + 8);
  });

  it('reaches the last file by scrolling and opens it', async () => {
    const onSelectFile = vi.fn();
    const root = mountTree(onSelectFile);
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    const list = viewport(root);
    const lastPath = FILES[FILE_COUNT - 1].path;

    list.scrollTop = list.scrollHeight;
    await waitFor(() => filePaths(root).at(-1) === lastPath, 'the last file to mount');
    expect(filePaths(root)).not.toContain(FILES[0].path);
    expectTiled(root, 28);
    const last = root.querySelector<HTMLElement>(`[data-file-path="${lastPath}"]`)!;
    // Above the list's bottom padding, within the browser's whole-pixel scroll limit.
    const gap = list.getBoundingClientRect().bottom - last.getBoundingClientRect().bottom;
    expect(Math.abs(gap - 8)).toBeLessThan(1);

    last.click();
    expect(onSelectFile).toHaveBeenCalledWith(lastPath);
  });

  it('follows the root font size', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');

    document.documentElement.style.setProperty('font-size', '20px');
    await waitFor(() => rows(root)[0].getBoundingClientRect().height === 35, 'rows to take the new height');
    expectTiled(root, 35);
    expect(viewport(root).scrollHeight).toBe(ROW_COUNT * 35 + 10);
  });

  it('keeps the row height while the rail is hidden', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    document.documentElement.style.setProperty('font-size', '20px');
    await waitFor(() => rows(root)[0].getBoundingClientRect().height === 35, 'rows to take the new height');

    // Hidden, the row probe measures 0.
    root.style.display = 'none';
    await raf();
    await raf();
    expect(root.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]')!.style.height).toBe(`${ROW_COUNT * 35}px`);
    expect(rows(root)[0].style.height).toBe('35px');

    root.style.display = 'flex';
    await raf();
    expectTiled(root, 35);
  });

  it('shows a filter typed while scrolled to the end', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    const list = viewport(root);
    list.scrollTop = list.scrollHeight;
    await waitFor(() => filePaths(root).at(-1) === FILES[FILE_COUNT - 1].path, 'the last file to mount');

    const search = root.querySelector<HTMLInputElement>('[data-testid="review-tree-search"]')!;
    search.value = 'mod001';
    search.dispatchEvent(new Event('input', { bubbles: true }));
    await waitFor(() => filePaths(root).length === 10, 'the ten matches to mount');
    expect(filePaths(root)).toEqual(Array.from({ length: 10 }, (_, index) => `src/mod001${index}.ts`));
    expect(list.scrollTop).toBe(0);
    expectTiled(root, 28);
  });

  it('keeps keyboard focus on a row that scrolls out of the window', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    const list = viewport(root);
    const path = FILES[2].path;
    const button = root.querySelector<HTMLElement>(`[data-file-path="${path}"]`)!;
    button.focus();
    expect(document.activeElement).toBe(button);

    list.scrollTop = list.scrollHeight;
    await waitFor(() => filePaths(root).at(-1) === FILES[FILE_COUNT - 1].path, 'the last file to mount');
    expect(document.activeElement).toBe(button);
    expect(button.isConnected).toBe(true);
    // Out of flow at its own place: file 2 is the fourth row.
    const top = button.getBoundingClientRect().top - list.getBoundingClientRect().top + list.scrollTop;
    expect(top).toBeCloseTo(3 * 28, 3);
    expect(viewport(root).scrollHeight).toBe(ROW_COUNT * 28 + 8);

    list.scrollTop = 0;
    await waitFor(() => filePaths(root)[0] === FILES[0].path, 'the first file to mount');
    expect(document.activeElement).toBe(button);
    expectTiled(root, 28);

    list.scrollTop = list.scrollHeight;
    await waitFor(() => filePaths(root).at(-1) === FILES[FILE_COUNT - 1].path, 'the last file to mount');
    root.querySelector<HTMLInputElement>('[data-testid="review-tree-search"]')!.focus();
    await waitFor(() => !filePaths(root).includes(path), 'the unfocused row to unmount');
  });

  it('moves Tab and Shift+Tab to the neighbors of a focused row scrolled out of the window', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    const list = viewport(root);
    const scrollToEnd = async () => {
      list.scrollTop = list.scrollHeight;
      await waitFor(() => filePaths(root).at(-1) === FILES[FILE_COUNT - 1].path, 'the last file to mount');
    };
    const focusedPath = () => (document.activeElement as HTMLElement | null)?.dataset.filePath;
    root.querySelector<HTMLElement>(`[data-file-path="${FILES[2].path}"]`)!.focus();

    await scrollToEnd();
    await userEvent.keyboard('{Shift>}{Tab}{/Shift}');
    expect(focusedPath()).toBe(FILES[1].path);

    await scrollToEnd();
    await userEvent.keyboard('{Tab}');
    expect(focusedPath()).toBe(FILES[2].path);

    // The kept rows are the focused row and its two neighbors.
    await scrollToEnd();
    const kept = rows(root).filter((row) => row.style.position === 'absolute');
    expect(kept.map((row) => row.dataset.rowIndex)).toEqual(['2', '3', '4']);
  });

  it('positions rows without a transform and names the whole list to a screen reader', async () => {
    const root = mountTree();
    await waitFor(() => rows(root).length > 16, 'the viewport height to reach the list');
    const list = viewport(root);
    list.scrollTop = 1000 * 28;
    await waitFor(() => Number(rows(root)[0].dataset.rowIndex) > 900, 'the window to move');

    for (let node: HTMLElement | null = rows(root)[0]; node && node !== list.parentElement; node = node.parentElement) {
      expect(getComputedStyle(node).transform).toBe('none');
    }
    expect(list.getAttribute('role')).toBe('list');
    const row = rows(root)[0];
    expect(row.getAttribute('role')).toBe('listitem');
    expect(row.getAttribute('aria-setsize')).toBe(String(ROW_COUNT));
    expect(row.getAttribute('aria-posinset')).toBe(String(Number(row.dataset.rowIndex) + 1));
  });
});
