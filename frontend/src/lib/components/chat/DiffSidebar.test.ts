// Smoke test for DiffSidebar. Mounts the component against a real
// pane (via buildPane) with `activeDiffPayload` armed, then drives
// the payload-fetch binding through its three branches: loading,
// empty patch, and fetch error.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, waitFor, fireEvent } from '@testing-library/svelte';
import DiffSidebar from './DiffSidebar.svelte';
import { buildPane } from '../../../test/helpers/chat';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../test/mocks/bindings-app';
import { resetSharedTokenCacheForTest } from '../../utils/tokenCacheReactive.svelte';

// Stub the Shiki worker pool — happy-dom's Web Worker support is
// incomplete and the real pool would never resolve. The spy lets us
// assert that the body's coordinator actually posts a tokenize batch.
const tokenizeSpy = vi.fn();
vi.mock('../../utils/diffHighlighterPool', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('../../utils/diffHighlighterPool')>();
  return {
    ...actual,
    getSharedDiffHighlighterPool: () => ({
      tokenize: tokenizeSpy,
      terminate: vi.fn(),
      get isActive() {
        return true;
      },
    }),
  };
});

describe('<DiffSidebar>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSharedTokenCacheForTest();
    tokenizeSpy.mockReset();
    tokenizeSpy.mockResolvedValue([]);
  });

  it('renders the loading state while the payload preview is in flight', async () => {
    // GetPayloadPreview returns a never-resolving promise so the
    // sidebar stays in the loading branch.
    setBindingMock('GetPayloadPreview', () => new Promise(() => {}));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-loading')).toBeTruthy();
    });
  });

  it('renders the empty state when the patch has no parseable content', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '',
      nextOffset: 0,
      totalSize: 0,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-empty')).toBeTruthy();
    });
  });

  it('renders the error state with a Retry button when the payload fetch rejects', async () => {
    setBindingMock(
      'GetPayloadPreview',
      vi.fn(async () => {
        throw new Error('payload missing');
      }),
    );
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId, getByText } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-error')).toBeTruthy();
    });
    expect(getByText('Retry')).toBeTruthy();
  });

  it('keeps horizontal diff overflow inside the expanded file body', async () => {
    const patch = [
      'diff --git a/src/foo.ts b/src/foo.ts',
      '--- a/src/foo.ts',
      '+++ b/src/foo.ts',
      '@@ -1 +1 @@',
      `-const value = "${'x'.repeat(180)}";`,
      `+const nextValue = "${'x'.repeat(180)}";`,
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: patch,
      nextOffset: patch.length,
      totalSize: patch.length,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p-horizontal' });

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-file-body')).toBeTruthy();
    });
    expect(getByTestId('diff-sidebar-body').className).toContain('overflow-y-auto');
    expect(getByTestId('diff-sidebar-file-body').className).toContain('overflow-x-auto');
  });

  it('dispatches tokenization on expand without waiting for IntersectionObserver', async () => {
    // happy-dom does not fire IntersectionObserver — same hole as
    // production for a fully-visible diff that never re-fires the
    // observer when the user scrolls within it. The dispatcher must
    // run on expand alone so lines past the inline preview cap still
    // tokenize. Pre-fix this test failed because the gate
    // `visiblePaths.size === 0` short-circuited the effect; the
    // tokenize batch was never posted to the worker.
    const patch = [
      'diff --git a/src/foo.md b/src/foo.md',
      '--- a/src/foo.md',
      '+++ b/src/foo.md',
      '@@ -1,1 +1,2 @@',
      ' existing line',
      '+brand new line past the inline preview',
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: patch,
      nextOffset: patch.length,
      totalSize: patch.length,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p-tokenize' });

    render(DiffSidebar, { props: { pane } });

    await waitFor(() => {
      expect(tokenizeSpy).toHaveBeenCalled();
    });
    const callArg = tokenizeSpy.mock.calls[0]?.[0] as
      | { lang: string; lines: string[]; theme: string }
      | undefined;
    expect(callArg?.lang).toBe('markdown');
    // Context lines keep their leading space (stripPatchLinePrefix
    // only strips +/- for add/del); add lines have the + stripped.
    expect(callArg?.lines).toContain(' existing line');
    expect(callArg?.lines).toContain('brand new line past the inline preview');
  });

  it('expands only the requested focus file when opening a small multi-file diff', async () => {
    const patch = [
      'diff --git a/src/one.ts b/src/one.ts',
      '--- a/src/one.ts',
      '+++ b/src/one.ts',
      '@@ -1 +1 @@',
      '-one',
      '+one updated',
      'diff --git a/src/two.ts b/src/two.ts',
      '--- a/src/two.ts',
      '+++ b/src/two.ts',
      '@@ -1 +1 @@',
      '-two',
      '+two updated',
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: patch,
      nextOffset: patch.length,
      totalSize: patch.length,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p-focus', filePath: 'src/two.ts' });

    const { getAllByTestId } = render(DiffSidebar, { props: { pane } });

    await waitFor(() => {
      expect(getAllByTestId('diff-sidebar-file')).toHaveLength(2);
      expect(getAllByTestId('diff-sidebar-stacked-body')).toHaveLength(1);
    });
    const files = getAllByTestId('diff-sidebar-file');
    expect(files[0]?.querySelector('button')).toHaveAttribute(
      'aria-expanded',
      'false',
    );
    expect(files[1]?.querySelector('button')).toHaveAttribute(
      'aria-expanded',
      'true',
    );
    expect(files[1]?.textContent).toContain('two updated');
  });

  it('refocuses when opening a different file from the same sidebar payload', async () => {
    const patch = [
      'diff --git a/src/one.ts b/src/one.ts',
      '--- a/src/one.ts',
      '+++ b/src/one.ts',
      '@@ -1 +1 @@',
      '-one',
      '+one updated',
      'diff --git a/src/two.ts b/src/two.ts',
      '--- a/src/two.ts',
      '+++ b/src/two.ts',
      '@@ -1 +1 @@',
      '-two',
      '+two updated',
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: patch,
      nextOffset: patch.length,
      totalSize: patch.length,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p-refocus', filePath: 'src/two.ts' });

    const { getAllByTestId } = render(DiffSidebar, { props: { pane } });

    await waitFor(() => {
      const files = getAllByTestId('diff-sidebar-file');
      expect(files[0]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'false',
      );
      expect(files[1]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'true',
      );
    });

    pane.openDiffSidebar({ payloadId: 'p-refocus', filePath: 'src/one.ts' });

    await waitFor(() => {
      const files = getAllByTestId('diff-sidebar-file');
      expect(files[0]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'true',
      );
      expect(files[1]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'false',
      );
    });
  });

  it('loads more payload data before falling back when the focused file is not in the preview', async () => {
    const firstPatch = [
      'diff --git a/src/one.ts b/src/one.ts',
      '--- a/src/one.ts',
      '+++ b/src/one.ts',
      '@@ -1 +1 @@',
      '-one',
      '+one updated',
    ].join('\n');
    const secondPatch = [
      '\n',
      'diff --git a/src/two.ts b/src/two.ts',
      '--- a/src/two.ts',
      '+++ b/src/two.ts',
      '@@ -1 +1 @@',
      '-two',
      '+two updated',
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: firstPatch,
      nextOffset: firstPatch.length,
      totalSize: firstPatch.length + secondPatch.length,
      isComplete: false,
    }));
    const chunkFetch = setBindingMock('GetPayloadChunk', async () => ({
      data: secondPatch,
      nextOffset: firstPatch.length + secondPatch.length,
      totalSize: firstPatch.length + secondPatch.length,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p-focus-late', filePath: 'src/two.ts' });

    const { getAllByTestId } = render(DiffSidebar, { props: { pane } });

    await waitFor(() => {
      expect(chunkFetch).toHaveBeenCalled();
      const files = getAllByTestId('diff-sidebar-file');
      expect(files).toHaveLength(2);
      expect(files[0]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'false',
      );
      expect(files[1]?.querySelector('button')).toHaveAttribute(
        'aria-expanded',
        'true',
      );
      expect(files[1]?.textContent).toContain('two updated');
    });
  });

  it('clicking close dismisses the sidebar via pane.closeRhsPanel', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '',
      nextOffset: 0,
      totalSize: 0,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });
    expect(pane.activeDiffPayload).not.toBeNull();

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await fireEvent.click(getByTestId('diff-sidebar-close'));
    expect(pane.activeDiffPayload).toBeNull();
  });
});
