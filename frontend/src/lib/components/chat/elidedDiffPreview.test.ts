// The client half of the wire projection (internal/itemwire): a diff
// file whose `previewPatch` the projection removed carries a
// `previewElided` marker, renders from the chrome that survived, and
// recovers its patch on expand through GetThreadItemProjectionSource.
//
// The two properties that make the projection invisible are asserted
// here rather than described: a collapsed elided file is
// pixel-identical to a collapsed complete one, and nothing is fetched
// until a reader expands it.

import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { Item, ToolResultMeta } from '../../types/models';
import { resetDiffSpanCacheForTest } from '../../utils/diffSpanCache.svelte';
import { __resetItemProjectionSourceCacheForTest } from '../../utils/itemProjectionSource.svelte';
import DiffFileStack from './DiffFileStack.svelte';

const PATCH_A = [
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1,1 +1,2 @@',
  ' const a = 1;',
  '+const b = 2;',
].join('\n');

const PATCH_B = [
  'diff --git a/src/b.ts b/src/b.ts',
  '--- a/src/b.ts',
  '+++ b/src/b.ts',
  '@@ -1,1 +1,2 @@',
  ' const c = 3;',
  '+const d = 4;',
].join('\n');

/** The row as the backend stores it: every preview intact. */
function storedMeta(): ToolResultMeta {
  return {
    itemType: 'file_change',
    title: 'Edited 2 files',
    inlineDiff: {
      availability: 'exact_patch',
      files: [
        { path: 'src/a.ts', kind: 'modified', insertions: 1, deletions: 0, previewPatch: PATCH_A },
        { path: 'src/b.ts', kind: 'modified', insertions: 1, deletions: 0, previewPatch: PATCH_B },
      ],
      totalFiles: 2,
      insertions: 2,
      deletions: 0,
    },
  };
}

/** The same row as the projection puts it on the wire. */
function projectedMeta(): ToolResultMeta {
  const meta = storedMeta();
  for (const file of meta.inlineDiff!.files) {
    delete file.previewPatch;
    file.previewElided = true;
  }
  return meta;
}

function rowItem(): Item {
  return makeItem({
    id: 'item-diff',
    kind: 'tool_completion',
    payloadId: 'payload-diff',
    payloadKind: 'diff',
    updatedAt: 7,
  });
}

function mockRecoveryRoute(): ReturnType<typeof setBindingMock> {
  return setBindingMock('GetThreadItemProjectionSource', async () => ({
    itemId: 'item-diff',
    payloadMeta: JSON.stringify(storedMeta()),
    payloadPreviewSpans: '',
  }));
}

beforeEach(async () => {
  resetDiffSpanCacheForTest();
  __resetItemProjectionSourceCacheForTest();
  resetSettingsForTest();
  // The shipped default. It is also what makes the projection ask for no
  // previews in the first place, so it is the case that matters.
  setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: true }));
  await loadSettings();
});

describe('a diff file whose preview the wire projection removed', () => {
  it('renders identically to a complete file while collapsed, and fetches nothing', async () => {
    const recover = mockRecoveryRoute();

    const complete = render(DiffFileStack, {
      props: { item: rowItem(), meta: storedMeta(), payloadId: 'payload-diff' },
    });
    const completeHeader = complete.getAllByTestId('diff-file-header')[0].innerHTML;
    complete.unmount();

    const elided = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });
    const elidedHeader = elided.getAllByTestId('diff-file-header')[0].innerHTML;

    expect(elidedHeader).toBe(completeHeader);
    // No body, no loading affordance, and above all no fetch: a
    // collapsed card renders nothing the projection took away.
    expect(elided.queryByTestId('diff-file-body')).toBeNull();
    expect(elided.queryByTestId('diff-file-recovering')).toBeNull();
    expect(recover).not.toHaveBeenCalled();
  });

  it('recovers the patch when the reader expands it', async () => {
    const recover = mockRecoveryRoute();
    const { getAllByTestId, getByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });

    getAllByTestId('diff-file-toggle')[0].click();

    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[0].textContent).toContain('const b = 2;');
    });
    expect(recover).toHaveBeenCalledTimes(1);
    expect(recover).toHaveBeenCalledWith('thread-1', 'item-diff');
    // The affordance retires with the marker it stood in for.
    expect(getByTestId('diff-file-body')).toBeTruthy();
  });

  it('recovers every elided file in the row from one fetch', async () => {
    const recover = mockRecoveryRoute();
    const { getAllByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });

    // Expanding the FIRST file recovers the second one's patch too: the
    // recovery route is per item, not per file.
    getAllByTestId('diff-file-toggle')[0].click();
    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[0].textContent).toContain('const b = 2;');
    });

    getAllByTestId('diff-file-toggle')[1].click();
    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[1].textContent).toContain('const d = 4;');
    });
    expect(recover).toHaveBeenCalledTimes(1);
  });

  it('shows the shared loading affordance until the patch lands', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    setBindingMock('GetThreadItemProjectionSource', async () => {
      await gate;
      return { itemId: 'item-diff', payloadMeta: JSON.stringify(storedMeta()), payloadPreviewSpans: '' };
    });

    const { getAllByTestId, getByTestId, queryByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });

    getAllByTestId('diff-file-toggle')[0].click();
    await waitFor(() => {
      expect(getByTestId('diff-file-recovering').textContent).toContain('Loading…');
    });

    release?.();
    await waitFor(() => {
      expect(queryByTestId('diff-file-recovering')).toBeNull();
    });
  });

  it('offers a retry when the recovery route fails', async () => {
    let attempts = 0;
    setBindingMock('GetThreadItemProjectionSource', async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('transport closed');
      return { itemId: 'item-diff', payloadMeta: JSON.stringify(storedMeta()), payloadPreviewSpans: '' };
    });

    const { getAllByTestId, getByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });

    getAllByTestId('diff-file-toggle')[0].click();
    await waitFor(() => {
      expect(getByTestId('diff-file-recovering').textContent).toContain('transport closed');
    });

    getByTestId('diff-file-recover-retry').click();
    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[0].textContent).toContain('const b = 2;');
    });
    expect(attempts).toBe(2);
  });

  it('fetches on mount only for a card that is already expanded', async () => {
    const recover = mockRecoveryRoute();
    // `collapseDiffPreviews` off means the client asked for inline
    // previews; a file elided anyway is over the byte budget, and its
    // card paints expanded, so the recovery is due immediately.
    resetSettingsForTest();
    setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: false }));
    await loadSettings();

    const { getAllByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });

    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[0].textContent).toContain('const b = 2;');
    });
    expect(recover).toHaveBeenCalledTimes(1);
  });

  it('does not treat an elided file as a legacy row needing a payload fetch', async () => {
    mockRecoveryRoute();
    const payload = setBindingMock('GetPayloadPreview', async () => {
      throw new Error('an elided row must not fetch its payload');
    });

    const { getAllByTestId } = render(DiffFileStack, {
      props: { item: rowItem(), meta: projectedMeta(), payloadId: 'payload-diff' },
    });
    getAllByTestId('diff-file-toggle')[0].click();

    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')[0].textContent).toContain('const b = 2;');
    });
    expect(payload).not.toHaveBeenCalled();
  });
});
