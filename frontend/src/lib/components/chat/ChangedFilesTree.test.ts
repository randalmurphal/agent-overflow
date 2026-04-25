import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ChangedFilesTree from './ChangedFilesTree.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import type { ChangedFile } from '../../types/models';

const FILES: ChangedFile[] = [
  { path: 'src/lib/foo.ts', insertions: 3, deletions: 1, kind: 'modified', payloadId: 'p1' },
  { path: 'src/lib/bar.ts', insertions: 5, deletions: 0, kind: 'added', payloadId: 'p2' },
];

describe('<ChangedFilesTree> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders dir + file rows with editor-link icon siblings', () => {
    const { getByTestId, getAllByTestId } = render(ChangedFilesTree, {
      props: { files: FILES },
    });
    expect(getByTestId('changed-files-dir-row')).toBeTruthy();
    // File rows show only after the dir is expanded; until then only
    // the dir row is in the DOM.
    expect(getAllByTestId('changed-files-dir-row').length).toBe(1);
  });

  it('clicking the dir editor-link does NOT toggle the dir', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(ChangedFilesTree, { props: { files: FILES } });
    const dirToggle = getByTestId('changed-files-dir-toggle');
    expect(dirToggle.getAttribute('aria-expanded')).toBe('false');

    // The dir-row contains exactly one editor-link icon — the one we
    // wired alongside the toggle. queryAll lets us pick it without
    // brittle index slicing.
    const link = getByTestId('changed-files-dir-row').querySelector(
      '[data-testid="editor-link-icon"]',
    ) as HTMLElement;
    expect(link).not.toBeNull();
    await fireEvent.click(link);

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0][0]).toBe('src/lib');
    // dir stays collapsed because stopPropagation prevented the
    // toggle's onclick from running.
    expect(dirToggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('clicking a file editor-link does NOT toggle the file diff', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId, getAllByTestId } = render(ChangedFilesTree, {
      props: { files: FILES },
    });
    // First expand the dir so the file rows render.
    await fireEvent.click(getByTestId('changed-files-dir-toggle'));

    const fileRows = getAllByTestId('changed-files-file-row');
    expect(fileRows.length).toBe(2);
    const fooRow = fileRows.find((row) => row.getAttribute('data-path') === 'src/lib/foo.ts');
    expect(fooRow).toBeTruthy();
    const fileToggle = fooRow!.querySelector('[data-testid="changed-files-file-toggle"]') as HTMLElement;
    expect(fileToggle.getAttribute('aria-expanded')).toBe('false');
    const fileLink = fooRow!.querySelector('[data-testid="editor-link-icon"]') as HTMLElement;

    await fireEvent.click(fileLink);
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0][0]).toBe('src/lib/foo.ts');
    expect(fileToggle.getAttribute('aria-expanded')).toBe('false');
  });
});
