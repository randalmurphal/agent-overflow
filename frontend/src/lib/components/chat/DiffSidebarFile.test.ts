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

    expect(getByTestId('diff-sidebar-stacked-body').className).not.toContain('whitespace-pre');
    const rows = Array.from(getByTestId('diff-sidebar-stacked-body').children).filter(
      (child) => child instanceof HTMLElement && !child.dataset.testid,
    );
    expect(rows).toHaveLength(3);
    expect(rows.every((row) => row.className.includes('whitespace-pre'))).toBe(true);
    expect(rows.map((row) => row.textContent)).toEqual([
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
    expect(cells.every((cell) => cell.className.includes('whitespace-pre-wrap'))).toBe(true);
    expect(cells.every((cell) => cell.className.includes('break-all'))).toBe(true);
    expect(cells.map((cell) => cell.textContent)).toEqual([
      '-oldValue();',
      '+newValue();',
      ' unchanged();',
      ' unchanged();',
    ]);
  });
});
