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

  it('extension chips toggle a type filter with full-set counts', async () => {
    const view = render(ReviewFileTree, { files: FILES, onSelectFile: () => {} });

    const chips = view.getAllByTestId('review-tree-ext-filter');
    // .ts ×2 sorts first; the rest are count-1 alphabetical.
    expect(chips.map((chip) => chip.getAttribute('data-ext'))).toEqual([
      '.ts',
      '.md',
      '.svelte',
      'Makefile',
    ]);

    const tsChip = chips.find((chip) => chip.getAttribute('data-ext') === '.ts')!;
    await fireEvent.click(tsChip);
    expect(
      view.getAllByTestId('review-tree-file').map((node) => node.getAttribute('data-file-path')),
    ).toEqual(['src/lib/app.ts', 'src/lib/gone.ts']);
    expect(tsChip.getAttribute('aria-pressed')).toBe('true');

    await fireEvent.click(tsChip);
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(5);
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
