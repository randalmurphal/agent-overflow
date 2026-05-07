// DiffFileStack covers the multi-file Codex apply_patch + Claude
// Edit/Write/MultiEdit/NotebookEdit dispatch path. Lazy-fetches the
// payload preview through a local expansion handle, parses with
// parsePatchFiles, then renders one DiffFileBlock per file in
// meta.inlineDiff.files.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { tick } from 'svelte';
import { render } from '@testing-library/svelte';
import DiffFileStack from './DiffFileStack.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import type { ToolResultMeta } from '../../types/models';
import { INLINE_DIFF_PAYLOAD_PREVIEW_BYTES } from '../../utils/inlineThreshold';

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

describe('<DiffFileStack>', () => {
  beforeEach(() => {
    resetBindingMocks();
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
      props: { pane, item, meta, payloadId: 'tool-result:tu_incomplete_preview' },
    });

    await tick();
    await tick();

    expect(await findByTestId('diff-file-show-full')).toBeInTheDocument();
  });
});
