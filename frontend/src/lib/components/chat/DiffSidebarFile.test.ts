import { describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import DiffSidebarFile from './DiffSidebarFile.svelte';
import { parsePatchFiles } from '../../utils/patchFiles';
import type { FileVirtualizerHandle } from '../../utils/diffSidebarVirtualizer.svelte';

function fakeVirtualizer(rowId: string): FileVirtualizerHandle {
  return {
    visiblePaths: new Set([rowId]),
    init: vi.fn(),
    register: vi.fn(),
    unregister: vi.fn(),
    isVisible: (path: string) => path === rowId,
    height: vi.fn(),
    destroy: vi.fn(),
  };
}

describe('<DiffSidebarFile>', () => {
  it('scopes whitespace preservation to diff rows so template newlines do not render as blank lines', () => {
    const rowId = '0:src/example.ts';
    const [file] = parsePatchFiles(`diff --git a/src/example.ts b/src/example.ts
--- a/src/example.ts
+++ b/src/example.ts
@@ -1,1 +1,3 @@
 existing();
+addedOne();
+addedTwo();
`);

    const { getByTestId } = render(DiffSidebarFile, {
      props: {
        file,
        rowId,
        expanded: true,
        threadId: 'thread-1',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        theme: 'github-dark',
        virtualizer: fakeVirtualizer(rowId),
        onToggle: vi.fn(),
      },
    });

    const stackedBody = getByTestId('diff-sidebar-stacked-body');
    expect(stackedBody.className).not.toContain('whitespace-pre');
    const rows = Array.from(stackedBody.children).filter(
      (child) => child instanceof HTMLElement && !child.dataset.testid,
    );
    expect(rows).toHaveLength(3);

    const contents = rows.map((row) =>
      row.querySelector('[data-testid="diff-sidebar-line-content"]'),
    );
    expect(contents.every((c) => c instanceof HTMLElement)).toBe(true);
    expect(contents.every((c) => c!.className.includes('whitespace-pre'))).toBe(true);
    expect(contents.map((c) => c!.textContent)).toEqual([
      ' existing();',
      '+addedOne();',
      '+addedTwo();',
    ]);
  });

  it('scopes wrapped whitespace preservation to split cells', () => {
    const rowId = '0:src/example.ts';
    const [file] = parsePatchFiles(`diff --git a/src/example.ts b/src/example.ts
--- a/src/example.ts
+++ b/src/example.ts
@@ -1,2 +1,2 @@
-oldValue();
+newValue();
 unchanged();
`);

    const { getByTestId } = render(DiffSidebarFile, {
      props: {
        file,
        rowId,
        expanded: true,
        threadId: 'thread-1',
        workspacePath: '/tmp/project',
        viewMode: 'split',
        wordWrap: true,
        theme: 'github-dark',
        virtualizer: fakeVirtualizer(rowId),
        onToggle: vi.fn(),
      },
    });

    const splitBody = getByTestId('diff-sidebar-split-body');
    expect(splitBody.className).not.toContain('whitespace-pre');

    const cells = Array.from(splitBody.children);
    expect(cells).toHaveLength(4);

    const contents = cells.map((cell) =>
      cell.querySelector('[data-testid="diff-sidebar-line-content"]'),
    );
    expect(contents.every((c) => c instanceof HTMLElement)).toBe(true);
    expect(contents.every((c) => c!.className.includes('whitespace-pre-wrap'))).toBe(true);
    expect(contents.every((c) => c!.className.includes('break-all'))).toBe(true);
    expect(contents.map((c) => c!.textContent)).toEqual([
      '-oldValue();',
      '+newValue();',
      ' unchanged();',
      ' unchanged();',
    ]);
  });

  it('renders new-side line numbers in the stacked-mode gutter', () => {
    const rowId = '0:src/example.ts';
    const [file] = parsePatchFiles(`diff --git a/src/example.ts b/src/example.ts
--- a/src/example.ts
+++ b/src/example.ts
@@ -10,1 +10,3 @@
 existing();
+addedOne();
+addedTwo();
`);

    const { getByTestId } = render(DiffSidebarFile, {
      props: {
        file,
        rowId,
        expanded: true,
        threadId: 'thread-1',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        theme: 'github-dark',
        virtualizer: fakeVirtualizer(rowId),
        onToggle: vi.fn(),
      },
    });

    const stackedBody = getByTestId('diff-sidebar-stacked-body');
    const rows = Array.from(stackedBody.children).filter(
      (child) => child instanceof HTMLElement && !child.dataset.testid,
    );
    const gutters = rows.map((row) =>
      row.querySelector('[data-testid="diff-sidebar-line-gutter"]')?.textContent?.trim(),
    );
    expect(gutters).toEqual(['10', '11', '12']);
  });

  it('aligns hunk separators between rows so line numbers jump across the divider', () => {
    const rowId = '0:src/example.ts';
    const [file] = parsePatchFiles(`diff --git a/src/example.ts b/src/example.ts
--- a/src/example.ts
+++ b/src/example.ts
@@ -1,2 +1,2 @@
-removedTop();
+addedTop();
@@ -50,2 +50,2 @@
-removedBottom();
+addedBottom();
`);

    const { getByTestId, getAllByTestId } = render(DiffSidebarFile, {
      props: {
        file,
        rowId,
        expanded: true,
        threadId: 'thread-1',
        workspacePath: '/tmp/project',
        viewMode: 'stacked',
        wordWrap: false,
        theme: 'github-dark',
        virtualizer: fakeVirtualizer(rowId),
        onToggle: vi.fn(),
      },
    });

    expect(getAllByTestId('diff-sidebar-hunk-separator')).toHaveLength(1);

    const stackedBody = getByTestId('diff-sidebar-stacked-body');
    const children = Array.from(stackedBody.children) as HTMLElement[];
    const separatorIdx = children.findIndex((c) => c.dataset.testid === 'diff-sidebar-hunk-separator');
    expect(separatorIdx).toBeGreaterThan(0);

    const beforeRow = children[separatorIdx - 1];
    const afterRow = children[separatorIdx + 1];
    const gutterText = (row: HTMLElement) =>
      row.querySelector('[data-testid="diff-sidebar-line-gutter"]')?.textContent?.trim();

    // Last row before the separator should be the first hunk's add line
    // (newLine=1); first row after should be the second hunk's del line
    // (oldLine=50). Locks the comment-asserted invariant that
    // `buildPatchDisplayRows` and `separatorAfter` walk meta lines in
    // identical order.
    expect(gutterText(beforeRow)).toBe('1');
    expect(gutterText(afterRow)).toBe('50');
  });

  it('renders old-on-left, new-on-right line numbers in split mode', () => {
    const rowId = '0:src/example.ts';
    const [file] = parsePatchFiles(`diff --git a/src/example.ts b/src/example.ts
--- a/src/example.ts
+++ b/src/example.ts
@@ -10,1 +10,3 @@
 existing();
+addedOne();
+addedTwo();
`);

    const { getByTestId } = render(DiffSidebarFile, {
      props: {
        file,
        rowId,
        expanded: true,
        threadId: 'thread-1',
        workspacePath: '/tmp/project',
        viewMode: 'split',
        wordWrap: false,
        theme: 'github-dark',
        virtualizer: fakeVirtualizer(rowId),
        onToggle: vi.fn(),
      },
    });

    const splitBody = getByTestId('diff-sidebar-split-body');
    const cells = Array.from(splitBody.children);
    expect(cells).toHaveLength(6); // 3 rows × 2 sides

    const gutters = cells.map((cell) =>
      cell.querySelector('[data-testid="diff-sidebar-line-gutter"]')?.textContent ?? '',
    );
    // Row 0: context line on both sides; Row 1+2: add lines (left blank).
    expect(gutters).toEqual([
      '10', // left, oldLine=10
      '10', // right, newLine=10
      '',   // left, no oldLine for add
      '11', // right, newLine=11
      '',   // left, no oldLine for add
      '12', // right, newLine=12
    ]);
  });
});
