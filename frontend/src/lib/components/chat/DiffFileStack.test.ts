// DiffFileStack covers the multi-file Codex apply_patch + Claude
// Edit/Write/MultiEdit/NotebookEdit dispatch path. Lazy-fetches the
// payload via pane.expansionStateFor(item), parses with
// parsePatchFiles, then renders one DiffFileBlock per file in
// meta.inlineDiff.files.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { tick } from 'svelte';
import { render } from '@testing-library/svelte';
import DiffFileStack from './DiffFileStack.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import type { ToolResultMeta } from '../../types/models';

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
    // The expansion handle keyed by item.id reads payloadId / threadId
    // out of the pane's items registry — pass the item to buildPane
    // so the lookup resolves.
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
});
