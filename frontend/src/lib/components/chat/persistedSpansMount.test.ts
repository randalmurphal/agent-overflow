// Cold-mount integration for persisted highlight spans (phase D): a
// row whose item carries a version-stamped span blob must paint
// highlighted WITHOUT any highlight RPC — the architectural point of
// persisting spans with history. The ingest happens at component init
// (utils/persistedSpans.ts), before the code/diff hosts mount and take
// their synchronous cache reads. A version-mismatched blob must fall
// back to the RPC path instead of coloring by an old grammar's spans.

import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { resetSettingsForTest } from '../../stores/settings.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { ToolResultMeta } from '../../types/models';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { resetDiffSpanCacheForTest } from '../../utils/diffSpanCache.svelte';
import { contentKey } from '../../utils/fnv1a';
import {
  ensureHighlightSchemaVersion,
  ensureSyntaxClassNames,
  resetSyntaxClassNamesForTest,
} from '../../utils/syntaxSpans';
import AssistantMessage from './AssistantMessage.svelte';
import DiffFileStack from './DiffFileStack.svelte';

const HV = 'hv-test';

const FENCE_SOURCE = 'def f():\n    pass';
const SUMMARY = '```python\n' + FENCE_SOURCE + '\n```';

const PREVIEW_PATCH = [
  'diff --git a/src/a.py b/src/a.py',
  '--- a/src/a.py',
  '+++ b/src/a.py',
  '@@ -1,1 +1,2 @@',
  ' def f():',
  '+    pass',
].join('\n');

function assistantMetaWithCodeSpans(hv: string): string {
  return JSON.stringify({
    codeSpans: {
      hv,
      // "def f():" = 8 bytes: keyword run so the DOM assertion can see
      // a syntax class; second line plain.
      blocks: [{ lang: 'python', contentKey: contentKey(FENCE_SOURCE), lines: [{ r: [8, 1] }, {}] }],
    },
  });
}

function previewSpansBlob(hv: string): string {
  return JSON.stringify({
    hv,
    files: [
      {
        path: 'src/a.py',
        contentKey: contentKey(PREVIEW_PATCH),
        // 1:1 with the patch's six lines; runs on the two body lines
        // (their content INCLUDES the +/space prefix byte).
        lines: [{}, {}, {}, {}, { r: [9, 1] }, { r: [9, 1] }],
      },
    ],
  });
}

const toolResultMeta: ToolResultMeta = {
  itemType: 'file_change',
  title: 'Edit src/a.py',
  inlineDiff: {
    availability: 'exact_patch',
    files: [
      {
        path: 'src/a.py',
        kind: 'modified',
        insertions: 1,
        deletions: 0,
        previewPatch: PREVIEW_PATCH,
      },
    ],
    totalFiles: 1,
    omittedFiles: 0,
    filesTruncated: false,
    insertions: 1,
    deletions: 0,
  },
};

async function warmTables(): Promise<void> {
  await ensureHighlightSchemaVersion();
  await ensureSyntaxClassNames();
}

beforeEach(async () => {
  resetCodeSpanCacheForTest();
  resetDiffSpanCacheForTest();
  resetSyntaxClassNamesForTest();
  setBindingMock('HighlightSchemaVersion', async () => HV);
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  // The diff case asserts on painted body spans, so the stack has to be
  // expanded: collapseDiffPreviews defaults on and would render header-only.
  resetSettingsForTest();
  setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews: false }));
  await loadSettings();
});

describe('cold mount with persisted spans', () => {
  it('paints an assistant code fence from meta codeSpans with zero highlight RPCs', async () => {
    const rpc = setBindingMock('HighlightCode', async () => {
      throw new Error('cold mount must not RPC');
    });
    await warmTables();

    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ summary: SUMMARY, meta: assistantMetaWithCodeSpans(HV) }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      expect(body.querySelector('.syntax-keyword')?.textContent).toBe('def f():');
    });
    expect(rpc).not.toHaveBeenCalled();
  });

  it('falls back to the RPC path when the codeSpans blob has a stale schema version', async () => {
    const rpc = setBindingMock('HighlightCode', async () => ({
      lang: 'python',
      lines: [{ r: [3, 1] }, {}],
      truncated: false,
      incomplete: false,
    }));
    await warmTables();

    const { getByTestId } = render(AssistantMessage, {
      props: {
        item: makeItem({ summary: SUMMARY, meta: assistantMetaWithCodeSpans('stale-schema') }),
      },
    });

    const body = getByTestId('assistant-message-body');
    await waitFor(() => {
      // The RPC result (keyword over "def") renders — proving the stale
      // blob was dropped and the request path recomputed.
      expect(body.querySelector('.syntax-keyword')?.textContent).toBe('def');
    });
    expect(rpc).toHaveBeenCalled();
  });

  it('paints an inline diff stack from payloadPreviewSpans with zero highlight RPCs', async () => {
    const rpc = setBindingMock('HighlightPatch', async () => {
      throw new Error('cold mount must not RPC');
    });
    await warmTables();

    const { container } = render(DiffFileStack, {
      props: {
        item: makeItem({
          kind: 'tool_call',
          toolName: 'file_change',
          payloadPreviewSpans: previewSpansBlob(HV),
        }),
        meta: toolResultMeta,
      },
    });

    await waitFor(() => {
      const styled = container.querySelectorAll('.syntax-keyword');
      expect(styled.length).toBeGreaterThan(0);
    });
    expect(rpc).not.toHaveBeenCalled();
  });
});
