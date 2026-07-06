import { fireEvent, render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewFileTree from './ReviewFileTree.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from '../../stores/appStorage';
import type { PatchFile } from '../../utils/patchFiles';

function file(path: string, kind = 'modified', additions = 1, deletions = 0): PatchFile {
  return { path, kind, additions, deletions, lines: [] };
}

const FILES = [
  file('src/lib/app.ts'),
  file('src/lib/App.svelte', 'added'),
  file('src/lib/gone.ts', 'deleted'),
  file('docs/readme.md'),
  file('Makefile'),
];

beforeEach(() => {
  resetAppStorageForTest();
});

describe('<ReviewFileTree>', () => {
  it('renders all files and jumps on click', async () => {
    const onSelectFile = vi.fn();
    const view = render(ReviewFileTree, { files: FILES, onSelectFile });

    expect(view.getAllByTestId('review-tree-file')).toHaveLength(5);
    await fireEvent.click(view.getByText('readme.md'));
    expect(onSelectFile).toHaveBeenCalledWith('docs/readme.md');
  });

  it('search narrows the tree and overrides manual collapse', async () => {
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });

    // Collapse src/lib, then search: the match must be visible anyway.
    await fireEvent.click(view.getByText('src/lib'));
    expect(view.queryByText('app.ts')).toBeNull();

    await fireEvent.input(view.getByTestId('review-tree-search'), {
      target: { value: 'app.ts' },
    });
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(1);
    expect(view.getByText('app.ts')).toBeInTheDocument();

    // Clearing the search restores the manual collapse.
    await fireEvent.input(view.getByTestId('review-tree-search'), { target: { value: '' } });
    expect(view.queryByText('app.ts')).toBeNull();

    await fireEvent.input(view.getByTestId('review-tree-search'), {
      target: { value: 'no-such-file' },
    });
    expect(view.getByTestId('review-tree-empty')).toBeInTheDocument();
  });

  it('filters by file type through the dropdown with full-set counts', async () => {
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });

    await fireEvent.click(view.getByTestId('review-tree-ext-trigger'));

    // Each option is "<ext><count>"; strip the trailing count (and check
    // glyph) to recover the label. .ts ×2 sorts first, then count-1
    // alphabetical.
    const labelOf = (el: Element) => el.textContent!.replace(/[\s\d✓]+$/, '').trim();
    expect(view.getAllByRole('menuitem').map(labelOf)).toEqual([
      '.ts',
      '.md',
      '.svelte',
      'Makefile',
    ]);

    await fireEvent.click(view.getByRole('menuitem', { name: /^\.ts/ }));
    expect(
      view.getAllByTestId('review-tree-file').map((node) => node.getAttribute('data-file-path')),
    ).toEqual(['src/lib/app.ts', 'src/lib/gone.ts']);
    // The trigger surfaces the active-filter count; the menu stays open so
    // several types can be toggled in one visit.
    expect(view.getByTestId('review-tree-ext-trigger').textContent).toContain('1');

    await fireEvent.click(view.getByRole('menuitem', { name: /^\.ts/ }));
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(5);
  });

  it('clears all active type filters from the dropdown', async () => {
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });

    await fireEvent.click(view.getByTestId('review-tree-ext-trigger'));
    await fireEvent.click(view.getByRole('menuitem', { name: /^\.ts/ }));
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(2);

    await fireEvent.click(view.getByRole('menuitem', { name: /clear filters/i }));
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(5);
    expect(view.getByTestId('review-tree-ext-trigger').textContent).not.toContain('1');
  });

  it('tints file names by patch kind', () => {
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });

    expect(view.getByText('App.svelte').classList.contains('text-success')).toBe(true);
    expect(view.getByText('gone.ts').classList.contains('text-error')).toBe(true);
    expect(view.getByText('app.ts').classList.contains('text-success')).toBe(false);
  });

  it('reads the persisted rail width, clamped to bounds', () => {
    appStorageSet('reviewTreeWidth', '320');
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });
    expect(view.getByTestId('review-file-tree').style.width).toBe('320px');
  });

  it('double-clicking the resize handle resets and persists the default width', async () => {
    appStorageSet('reviewTreeWidth', '9999');
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });
    // Out-of-range stored values clamp on read.
    expect(view.getByTestId('review-file-tree').style.width).toBe('480px');

    await fireEvent.dblClick(view.getByTestId('review-tree-resize'));
    expect(view.getByTestId('review-file-tree').style.width).toBe('240px');
    expect(appStorageGet('reviewTreeWidth')).toBe('240');
  });
});
