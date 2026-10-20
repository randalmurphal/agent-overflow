import { fireEvent, render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewFileTree from './ReviewFileTree.svelte';
import ReviewRail from './ReviewRail.svelte';
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

  it('shows per-file comment count badges', () => {
    const view = render(ReviewFileTree, {
      files: FILES,
      onSelectFile: () => {},
      commentCounts: new Map([['docs/readme.md', { total: 3, unresolved: 0 }]]),
    });
    const badge = view.getByTestId('review-tree-comment-count');
    expect(badge).toHaveTextContent('3');
    expect(badge.closest('[data-file-path]')?.getAttribute('data-file-path')).toBe('docs/readme.md');
  });

  it('tints the badge warning while a file has unresolved threads', () => {
    const view = render(ReviewFileTree, {
      files: FILES,
      onSelectFile: () => {},
      commentCounts: new Map([
        ['docs/readme.md', { total: 3, unresolved: 2 }],
        ['src/lib/app.ts', { total: 1, unresolved: 0 }],
      ]),
    });
    const badges = view.getAllByTestId('review-tree-comment-count');
    const open = badges.find((badge) => badge.closest('[data-file-path]')?.getAttribute('data-file-path') === 'docs/readme.md')!;
    const quiet = badges.find((badge) => badge.closest('[data-file-path]')?.getAttribute('data-file-path') === 'src/lib/app.ts')!;
    expect(open.classList.contains('text-warning')).toBe(true);
    expect(open.getAttribute('title')).toBe('3 comments, 2 unresolved');
    expect(quiet.classList.contains('text-warning')).toBe(false);
    expect(quiet.getAttribute('title')).toBe('1 comment');
  });
});

// Rail width/resize moved from the tree to the ReviewRail shell.
function renderRail(overrides: { open?: boolean; onToggleOpen?: () => void } = {}) {
  return render(ReviewRail, {
    files: FILES,
    onSelectFile: () => {},
    commentCounts: new Map(),
    open: overrides.open ?? true,
    onToggleOpen: overrides.onToggleOpen ?? (() => {}),
  });
}

describe('<ReviewRail>', () => {
  it('reads the persisted rail width, clamped to bounds', () => {
    appStorageSet('reviewTreeWidth', '320');
    const view = renderRail();
    expect(view.getByTestId('review-rail').style.getPropertyValue('--rail-w')).toBe('320px');
  });

  it('double-clicking the resize handle resets and persists the default width', async () => {
    appStorageSet('reviewTreeWidth', '9999');
    const view = renderRail();
    // Out-of-range stored values clamp on read.
    expect(view.getByTestId('review-rail').style.getPropertyValue('--rail-w')).toBe('480px');

    await fireEvent.dblClick(view.getByTestId('review-tree-resize'));
    expect(view.getByTestId('review-rail').style.getPropertyValue('--rail-w')).toBe('240px');
    expect(appStorageGet('reviewTreeWidth')).toBe('240');
  });
});

describe('<ReviewRail> open and closed', () => {
  it('closed, is a strip holding only the button that opens it', async () => {
    const onToggleOpen = vi.fn();
    const view = renderRail({ open: false, onToggleOpen });
    expect(view.getByTestId('review-rail').getAttribute('data-open')).toBe('false');
    expect(view.queryByTestId('review-file-tree')).toBeNull();
    const toggle = view.getByTestId('review-tree-toggle');
    expect(toggle.getAttribute('aria-pressed')).toBe('false');
    await fireEvent.click(toggle);
    expect(onToggleOpen).toHaveBeenCalledTimes(1);
  });

  it('open, carries the same toggle in the tree header, pressed', async () => {
    const onToggleOpen = vi.fn();
    const view = renderRail({ open: true, onToggleOpen });
    expect(view.getByTestId('review-rail').getAttribute('data-open')).toBe('true');
    const toggle = view.getByTestId('review-tree-toggle');
    expect(toggle.getAttribute('aria-pressed')).toBe('true');
    expect(view.getByTestId('review-file-tree')).toBeInTheDocument();
    await fireEvent.click(toggle);
    expect(onToggleOpen).toHaveBeenCalledTimes(1);
  });
});

describe('<ReviewFileTree> hide tests', () => {
  const WITH_TESTS = [
    file('src/app.ts'),
    file('src/app.test.ts'),
    file('internal/git/forge_test.go'),
    file('internal/git/forge.go'),
  ];

  it('offers "Hide tests" with the test-file count and drops them from the tree', async () => {
    const onHideTestsChange = vi.fn();
    const view = render(ReviewFileTree, {
      files: WITH_TESTS,
      onSelectFile: () => {},
      hideTests: false,
      onHideTestsChange,
    });
    expect(view.getAllByTestId('review-tree-file')).toHaveLength(4);
    await fireEvent.click(view.getByTestId('review-tree-ext-trigger'));
    const item = view.getByTestId('review-tree-hide-tests');
    expect(item.textContent).toContain('2');
    await fireEvent.click(item);
    expect(onHideTestsChange).toHaveBeenCalledWith(true);
    view.unmount();

    const hidden = render(ReviewFileTree, {
      files: WITH_TESTS,
      onSelectFile: () => {},
      hideTests: true,
      onHideTestsChange,
    });
    expect(hidden.getAllByTestId('review-tree-file').map((node) => node.getAttribute('data-file-path')))
      .toEqual(['internal/git/forge.go', 'src/app.ts']);
    // The trigger counts the hidden-tests choice as one active filter.
    expect(hidden.getByTestId('review-tree-ext-trigger').textContent).toContain('1');
  });

  it('shows the filter menu for tests alone, with one file type', () => {
    const view = render(ReviewFileTree, {
      files: [file('src/app.ts'), file('src/app.test.ts')],
      onSelectFile: () => {},
      onHideTestsChange: () => {},
    });
    expect(view.getByTestId('review-tree-ext-trigger')).toBeInTheDocument();
  });

  it('"Clear filters" also turns hide tests off', async () => {
    const onHideTestsChange = vi.fn();
    const view = render(ReviewFileTree, {
      files: WITH_TESTS,
      onSelectFile: () => {},
      hideTests: true,
      onHideTestsChange,
    });
    await fireEvent.click(view.getByTestId('review-tree-ext-trigger'));
    await fireEvent.click(view.getByText('Clear filters'));
    expect(onHideTestsChange).toHaveBeenCalledWith(false);
  });
});
