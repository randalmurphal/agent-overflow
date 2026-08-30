// DiffFileStack covers the multi-file Codex apply_patch + Claude
// Edit/Write/MultiEdit/NotebookEdit dispatch path. Modern rows render
// per-file previewPatch metadata directly; legacy rows lazy-fetch the
// payload preview through a local expansion handle, parse with
// parsePatchFiles, then render one DiffFileBlock per file in
// meta.inlineDiff.files.

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { tick } from 'svelte';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import DiffFileStack from './DiffFileStack.svelte';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { loadSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';
import type { ToolResultMeta } from '../../types/models';
import {
  INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
  INLINE_DIFF_PREVIEW_FILE_COUNT,
} from '../../utils/inlineThreshold';

const claudeEditPatch =
  'diff --git a//tmp/diff-test.txt b//tmp/diff-test.txt\n' +
  '--- a//tmp/diff-test.txt\n' +
  '+++ b//tmp/diff-test.txt\n' +
  '@@ -1,1 +1,1 @@\n' +
  '-old line\n' +
  '+new line';

const meta: ToolResultMeta = {
  itemType: 'file_change',
  title: 'Edit applied',
  inlineDiff: {
    availability: 'exact_patch',
    insertions: 1,
    deletions: 1,
    files: [
      {
        path: '/tmp/diff-test.txt',
        kind: 'modified',
        insertions: 1,
        deletions: 1,
      },
    ],
  },
};

const twoFilePreviewMeta: ToolResultMeta = {
  itemType: 'file_change',
  title: 'Edited 2 files (+202 -0)',
  inlineDiff: {
    availability: 'exact_patch',
    insertions: 202,
    deletions: 0,
    files: [
      {
        path: 'src/first.py',
        kind: 'added',
        insertions: 1,
        deletions: 0,
        previewPatch: [
          'diff --git a/src/first.py b/src/first.py',
          'new file mode 100644',
          '--- /dev/null',
          '+++ b/src/first.py',
          '@@ -0,0 +1 @@',
          '+first file line',
        ].join('\n'),
      },
      {
        path: 'src/second.py',
        kind: 'added',
        insertions: 201,
        deletions: 0,
        previewTruncated: true,
        previewPatch: [
          'diff --git a/src/second.py b/src/second.py',
          'new file mode 100644',
          '--- /dev/null',
          '+++ b/src/second.py',
          '@@ -0,0 +1,201 @@',
          '+second file line 1',
          '+second file line 2',
        ].join('\n'),
      },
    ],
  },
};

describe('<DiffFileStack>', () => {
  // These cases are about what the EXPANDED stack renders, so they seed
  // collapseDiffPreviews off rather than riding the shipped default (which
  // collapses); the collapsed case below seeds it on for itself.
  beforeEach(async () => {
    resetBindingMocks();
    resetSettingsForTest();
    setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: false }));
    await loadSettings();
  });

  afterEach(() => {
    resetSettingsForTest();
  });

  it('renders separate file rows from per-file preview metadata without fetching a byte prefix', async () => {
    const previewFetch = setBindingMock('GetPayloadPreview', async () => {
      throw new Error(
        'preview fetch should not be needed for per-file previews',
      );
    });

    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const item = makeItem({
      id: 'tu_multifile_preview',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'fileChange',
      payloadId: 'tool-result:tu_multifile_preview',
      payloadKind: 'tool_result',
      createdAt,
    });
    const pane = await buildPane(undefined, [item]);

    const { findAllByTestId } = render(DiffFileStack, {
      props: {
        pane,
        item,
        meta: twoFilePreviewMeta,
        payloadId: 'tool-result:tu_multifile_preview',
      },
    });

    const blocks = await findAllByTestId('diff-file-block');
    expect(blocks).toHaveLength(2);
    expect(blocks[0]).toHaveAttribute('data-file-path', 'src/first.py');
    expect(blocks[1]).toHaveAttribute('data-file-path', 'src/second.py');

    const bodies = await findAllByTestId('diff-file-body');
    expect(bodies).toHaveLength(2);
    expect(bodies[0].textContent).toContain('first file line');
    expect(bodies[1].textContent).toContain('second file line 1');
    expect(
      blocks[1].querySelector('[data-testid="diff-file-counts"]')?.textContent,
    ).toContain('+201');
    // Every stacked file row carries the owning item's clock time, same
    // as single-file diff rows and every other tool row.
    for (const block of blocks) {
      expect(
        block
          .querySelector('[data-testid="diff-file-time"]')
          ?.getAttribute('datetime'),
      ).toBe(new Date(createdAt).toISOString());
    }
    expect(previewFetch).not.toHaveBeenCalled();
  });

  it('caps rendered file rows and shows an overflow side-panel action', async () => {
    const files = Array.from({ length: INLINE_DIFF_PREVIEW_FILE_COUNT + 2 }, (_, index) => ({
      path: `src/file-${index}.ts`,
      kind: 'added' as const,
      insertions: 1,
      deletions: 0,
      previewPatch: [
        `diff --git a/src/file-${index}.ts b/src/file-${index}.ts`,
        'new file mode 100644',
        '--- /dev/null',
        `+++ b/src/file-${index}.ts`,
        '@@ -0,0 +1 @@',
        `+line ${index}`,
      ].join('\n'),
    }));
    const cappedMeta: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edited 27 files (+27 -0)',
      inlineDiff: {
        availability: 'exact_patch',
        totalFiles: files.length,
        omittedFiles: 2,
        filesTruncated: true,
        insertions: files.length,
        deletions: 0,
        files,
      },
    };
    const item = makeItem({
      id: 'tu_many_files',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'fileChange',
      payloadId: 'tool-result:tu_many_files',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { findAllByTestId, findByTestId } = render(DiffFileStack, {
      props: {
        pane,
        item,
        meta: cappedMeta,
        payloadId: 'tool-result:tu_many_files',
      },
    });

    const blocks = await findAllByTestId('diff-file-block');
    expect(blocks).toHaveLength(INLINE_DIFF_PREVIEW_FILE_COUNT);
    expect(blocks.at(-1)).toHaveAttribute('data-file-path', 'src/file-24.ts');
    const overflow = await findByTestId('diff-file-overflow');
    expect(overflow.textContent).toContain('2 more files changed');
    expect(overflow.textContent).toContain('27 total');
    expect(await findByTestId('diff-file-overflow-open-sidebar')).toBeInTheDocument();
  });

  it('lazy-fetches payload data and renders the diff body for each file', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: claudeEditPatch,
      nextOffset: claudeEditPatch.length,
      totalSize: claudeEditPatch.length,
      isComplete: true,
    }));

    const item = makeItem({
      id: 'tu_edit_e2e',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'tool-result:tu_edit_e2e',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { findByTestId, findAllByTestId } = render(DiffFileStack, {
      props: { pane, item, meta, payloadId: 'tool-result:tu_edit_e2e' },
    });

    // Wait for the lazy fetch + parse to complete.
    await tick();
    await tick();

    const blocks = await findAllByTestId('diff-file-block');
    expect(blocks).toHaveLength(1);
    expect(blocks[0]).toHaveAttribute('data-file-path', '/tmp/diff-test.txt');

    // Body should render the actual diff lines (not just header).
    const body = await findByTestId('diff-file-body');
    expect(body.textContent).toContain('old line');
    expect(body.textContent).toContain('new line');
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledWith(
      'thread-1',
      'tool-result:tu_edit_e2e',
      INLINE_DIFF_PAYLOAD_PREVIEW_BYTES,
    );
  });

  it('loads more legacy payload data when a later metadata file is past the initial preview', async () => {
    const firstFilePatch = [
      'diff --git a/src/first.py b/src/first.py',
      '--- a/src/first.py',
      '+++ b/src/first.py',
      '@@ -1 +1 @@',
      '-first old',
      '+first new',
    ].join('\n');
    const secondFilePatch = [
      '\n',
      'diff --git a/src/second.py b/src/second.py',
      '--- a/src/second.py',
      '+++ b/src/second.py',
      '@@ -1 +1 @@',
      '-second old',
      '+second new',
    ].join('\n');
    setBindingMock('GetPayloadPreview', async () => ({
      data: firstFilePatch,
      nextOffset: firstFilePatch.length,
      totalSize: firstFilePatch.length + secondFilePatch.length,
      isComplete: false,
    }));
    const chunkFetch = setBindingMock('GetPayloadChunk', async () => ({
      data: secondFilePatch,
      nextOffset: firstFilePatch.length + secondFilePatch.length,
      totalSize: firstFilePatch.length + secondFilePatch.length,
      isComplete: true,
    }));

    const legacyMeta: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edited 2 files (+2 -2)',
      inlineDiff: {
        availability: 'exact_patch',
        insertions: 2,
        deletions: 2,
        files: [
          {
            path: 'src/first.py',
            kind: 'modified',
            insertions: 1,
            deletions: 1,
          },
          {
            path: 'src/second.py',
            kind: 'modified',
            insertions: 1,
            deletions: 1,
          },
        ],
      },
    };
    const item = makeItem({
      id: 'tu_legacy_multifile',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'fileChange',
      payloadId: 'tool-result:tu_legacy_multifile',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { getAllByTestId } = render(DiffFileStack, {
      props: {
        pane,
        item,
        meta: legacyMeta,
        payloadId: 'tool-result:tu_legacy_multifile',
      },
    });

    await waitFor(() => {
      expect(getAllByTestId('diff-file-body')).toHaveLength(2);
    });
    const bodies = getAllByTestId('diff-file-body');
    expect(bodies[0].textContent).toContain('first new');
    expect(bodies[1].textContent).toContain('second new');
    expect(chunkFetch).toHaveBeenCalled();
  });

  it('renders all blocks collapsed when collapseDiffPreviews is on; expanding one reveals only that body', async () => {
    setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: true }));
    await loadSettings();
    const item = makeItem({
      id: 'tu_collapsed_stack',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'fileChange',
      payloadId: 'tool-result:tu_collapsed_stack',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { findAllByTestId, getAllByTestId, queryAllByTestId } = render(DiffFileStack, {
      props: {
        pane,
        item,
        meta: twoFilePreviewMeta,
        payloadId: 'tool-result:tu_collapsed_stack',
      },
    });

    const blocks = await findAllByTestId('diff-file-block');
    expect(blocks).toHaveLength(2);
    expect(queryAllByTestId('diff-file-body')).toHaveLength(0);

    // Expanding the first file must not expand its sibling — the
    // override is keyed per (itemId, file.path) even though both
    // blocks share the same owning item.
    const toggles = getAllByTestId('diff-file-toggle');
    await fireEvent.click(toggles[0]);

    const bodies = getAllByTestId('diff-file-body');
    expect(bodies).toHaveLength(1);
    expect(bodies[0].textContent).toContain('first file line');
  });

  it('renders a header-only placeholder when the payload data has not landed yet', async () => {
    // No GetPayloadPreview mock — the binding will hang / never resolve.
    setBindingMock('GetPayloadPreview', () => new Promise(() => {}));

    const item = makeItem({
      id: 'tu_pending',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'tool-result:tu_pending',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { findAllByTestId, queryByTestId } = render(DiffFileStack, {
      props: { pane, item, meta, payloadId: 'tool-result:tu_pending' },
    });

    const blocks = await findAllByTestId('diff-file-block');
    expect(blocks).toHaveLength(1);
    // Body absent until data lands. The outer shell (with header) is
    // stable; only the indented body region is missing.
    expect(queryByTestId('diff-file-body')).toBeNull();
  });

  it('shows the sidebar CTA when the fetched preview is incomplete before the file crosses the line cap', async () => {
    const shortPreview =
      'diff --git a//tmp/diff-test.txt b//tmp/diff-test.txt\n' +
      '--- a//tmp/diff-test.txt\n' +
      '+++ b//tmp/diff-test.txt\n' +
      '@@ -1,2 +1,2 @@\n' +
      ' context line\n' +
      '+partial line';
    setBindingMock('GetPayloadPreview', async () => ({
      data: shortPreview,
      nextOffset: shortPreview.length,
      totalSize: shortPreview.length + 1024,
      isComplete: false,
    }));

    const item = makeItem({
      id: 'tu_incomplete_preview',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'tool-result:tu_incomplete_preview',
      payloadKind: 'tool_result',
    });
    const pane = await buildPane(undefined, [item]);

    const { findByTestId } = render(DiffFileStack, {
      props: {
        pane,
        item,
        meta,
        payloadId: 'tool-result:tu_incomplete_preview',
      },
    });

    await tick();
    await tick();

    expect(await findByTestId('diff-file-show-full')).toBeInTheDocument();
  });
});
