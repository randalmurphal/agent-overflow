import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DiffPreview from './DiffPreview.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { createPayloadExpansion } from './payloadExpansion.svelte';
import type { DiffMeta, Item } from '../../types/models';

/** Minimal pane fake that satisfies the expansion-registry surface
 * `<DiffPreview>` reads from. The registry methods are local-only —
 * each test starts with a fresh map, so cache survival across remount
 * is not exercised here (covered by `scroll.test.ts` and the
 * `thread.svelte.test.ts` registry tests). */
function makeFakePane(extra: Partial<import('../../stores/thread.svelte').ThreadPane> = {}): import('../../stores/thread.svelte').ThreadPane {
  const cache = new Map<string, ReturnType<typeof createPayloadExpansion>>();
  return {
    expansionStateFor(item: Item) {
      const key = 'i:' + item.id;
      let h = cache.get(key);
      if (!h) {
        h = createPayloadExpansion(() => item.payloadId, () => item.threadId);
        cache.set(key, h);
      }
      return h;
    },
    expansionStateForPayload(payloadId: string, threadId: string) {
      const key = 'p:' + payloadId;
      let h = cache.get(key);
      if (!h) {
        h = createPayloadExpansion(() => payloadId, () => threadId);
        cache.set(key, h);
      }
      return h;
    },
    ...extra,
  } as unknown as import('../../stores/thread.svelte').ThreadPane;
}

const META: DiffMeta = {
  filePath: 'src/lib/foo.ts',
  changeKind: 'modified',
  insertions: 3,
  deletions: 1,
  preview: '',
};

const ITEM: Item = {
  id: 'item-1',
  threadId: 't1',
  turnIndex: 0,
  itemIndex: 0,
  kind: 'tool_call',
  status: 'completed',
  summary: '',
  createdAt: 0,
  updatedAt: 0,
} as unknown as Item;

describe('<DiffPreview> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Avoid the payload-expansion path firing real RPCs in this test —
    // we only exercise the header.
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
    setBindingMock('GetPayloadChunk', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders the toggle button + the editor-link icon as siblings', () => {
    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    const link = getByTestId('editor-link-icon');
    expect(toggle.tagName).toBe('BUTTON');
    expect(link.tagName).toBe('BUTTON');
    expect(toggle.contains(link)).toBe(false);
  });

  it('clicking the toggle toggles the diff (parent toggle still fires)', async () => {
    const { getByTestId, container } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(toggle);
    // expansion is async (preview loads), so wait for the aria-expanded
    // flip rather than asserting synchronously.
    await waitFor(() => {
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });
    void container; // narrow tooling silence: container intentionally unused
  });

  it('clicking the editor-link does NOT toggle the diff', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    const link = getByTestId('editor-link-icon');

    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(link);

    // OpenInEditor was invoked with the file path.
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 0, 0, '']);

    // The parent's toggle did NOT fire — diff stays collapsed.
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  // Regression for the original click-to-open bug: diff cards emit
  // repo-relative file paths (`src/lib/foo.ts`), which the backend used
  // to reject because `editor.Open` required absolute paths. The fix
  // threads `pane.thread.workspacePath` so the backend can join. Pin
  // that the prop chain actually carries the value through to the
  // binding — without this test, removing the workspacePath wiring
  // anywhere along the path would silently re-introduce the bug.
  it('forwards pane.thread.workspacePath to the OpenInEditor binding', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const pane = makeFakePane({
      thread: { workspacePath: '/home/user/repo' },
    } as Partial<import('../../stores/thread.svelte').ThreadPane>);
    const { getByTestId } = render(DiffPreview, {
      props: { pane, meta: META, payloadId: 'p-1', item: ITEM },
    });
    await fireEvent.click(getByTestId('editor-link-icon'));
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 0, 0, '/home/user/repo']);
  });

  it('does not render the open-in-sidebar button when no pane prop is supplied', () => {
    const { queryByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    expect(queryByTestId('diff-preview-open-sidebar')).toBeNull();
  });

  it('renders the open-in-sidebar button when pane is supplied and routes clicks to pane.openDiffSidebar', async () => {
    const captures: Array<{ payloadId: string; filePath?: string }> = [];
    const fakePane = makeFakePane({
      openDiffSidebar(p: { payloadId: string; filePath?: string }) {
        captures.push(p);
      },
    } as unknown as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM, pane: fakePane },
    });
    const trigger = getByTestId('diff-preview-open-sidebar');
    await fireEvent.click(trigger);
    expect(captures).toEqual([{ payloadId: 'p-1', filePath: 'src/lib/foo.ts' }]);
  });

  it('Cmd-click on the header opens the sidebar instead of toggling inline', async () => {
    const captures: Array<{ payloadId: string; filePath?: string }> = [];
    const fakePane = makeFakePane({
      openDiffSidebar(p: { payloadId: string; filePath?: string }) {
        captures.push(p);
      },
    } as unknown as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM, pane: fakePane },
    });
    const toggle = getByTestId('diff-preview-toggle');
    await fireEvent.click(toggle, { metaKey: true });
    expect(captures).toEqual([{ payloadId: 'p-1', filePath: 'src/lib/foo.ts' }]);
    // The chevron stays collapsed because Cmd-click routed to the sidebar.
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('does not render the open-in-sidebar button when filePathFilter is set', () => {
    const fakePane = makeFakePane({ openDiffSidebar() {} } as unknown as Partial<import('../../stores/thread.svelte').ThreadPane>);
    const { queryByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM, pane: fakePane, filePathFilter: 'src/lib/foo.ts' },
    });
    // Sidebar promotion isn't meaningful for a slice of a cumulative
    // turn diff.
    expect(queryByTestId('diff-preview-open-sidebar')).toBeNull();
  });

  it('caps the expanded body height with internal scroll', async () => {
    // Long preview content (300 lines) so the cap actually engages.
    const longPreview = Array.from({ length: 300 }, (_, i) => ` line ${i}`).join('\n');
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: longPreview, size: longPreview.length, isComplete: true })));
    const { getByTestId, container } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    await fireEvent.click(toggle);
    await waitFor(() => {
      const pre = container.querySelector('pre');
      // cap class is "max-h-[32em] overflow-auto"
      expect(pre?.className).toMatch(/max-h-\[32em\]/);
      expect(pre?.className).toMatch(/overflow-auto/);
    });
  });
});
