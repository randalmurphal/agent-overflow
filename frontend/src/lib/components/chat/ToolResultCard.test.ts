// ToolResultCard renders the file-change tool_result payload (Edit /
// Write / MultiEdit / Codex apply_patch). Pre-unification it
// hard-coded a green "done" span regardless of `item.status` or the
// `is_error` flag in payload meta — meaning a failed Edit rendered as
// success. These tests pin the unified-badge wiring and the bug fix.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ToolResultCard from './ToolResultCard.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { createPayloadExpansion } from './payloadExpansion.svelte';
import type { Item, ToolResultMeta } from '../../types/models';

/** Minimal pane fake satisfying the expansion-registry surface
 * `<ToolResultCard>` reads from. */
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

function meta(overrides: Partial<ToolResultMeta> = {}): ToolResultMeta {
  return {
    itemType: 'file_change',
    title: 'Edit applied to src/foo.ts',
    ...overrides,
  };
}

describe('<ToolResultCard>', () => {
  it('renders the success badge when the underlying tool result completed cleanly', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit applied' }),
    });

    const { getByTestId } = render(ToolResultCard, {
      props: { item, meta: meta({ title: 'Edit applied' }) },
    });

    const badge = getByTestId('completion-badge');
    expect(badge.getAttribute('data-status')).toBe('success');
    expect(badge.className).toContain('text-success');
  });

  it('renders the failure badge when payload meta carries is_error=true (regression for the hard-coded "done" bug)', () => {
    // Pre-fix this row showed `<span class="text-success">done</span>`
    // regardless of status / is_error — failed Edit/apply_patch
    // looked successful. With the unified badge driven by
    // deriveCompletionStatus, an inline tool_result with
    // is_error: true must paint the failure badge.
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        itemType: 'file_change',
        title: 'Edit failed: file not found',
        is_error: true,
      }),
    });

    const { getByTestId, container } = render(ToolResultCard, {
      props: { item, meta: meta({ title: 'Edit failed: file not found' }) },
    });

    const badge = getByTestId('completion-badge');
    expect(badge.getAttribute('data-status')).toBe('failure');
    expect(badge.className).toContain('text-error');
    // And nothing in the row still claims success — pin the bug.
    expect(container.querySelector('.text-success')).toBeNull();
  });

  it('renders the failure badge for an errored item even when payload meta is empty', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'errored',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit declined' }),
    });

    const { getByTestId } = render(ToolResultCard, {
      props: { item, meta: meta({ title: 'Edit declined' }) },
    });

    expect(getByTestId('completion-badge').getAttribute('data-status')).toBe('failure');
  });

  it('renders the failure badge for a declined item (approval was rejected)', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'declined',
      decision: 'declined',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit declined' }),
    });

    const { getByTestId } = render(ToolResultCard, {
      props: { item, meta: meta({ title: 'Edit declined' }) },
    });

    expect(getByTestId('completion-badge').getAttribute('data-status')).toBe('failure');
  });
});

describe('<ToolResultCard> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders an editor-link icon next to each inline-diff chip', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit applied' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'summary_only',
        files: [{ path: 'src/lib/foo.ts', previousPath: 'src/lib/old.ts', insertions: 4, deletions: 1, kind: 'renamed' }],
      },
    };
    const { getByTestId } = render(ToolResultCard, { props: { item, meta: m } });
    const region = getByTestId('tool-result-inline-diffs');
    expect(region.textContent).toContain('src/lib/old.ts -> src/lib/foo.ts');
    const links = region.querySelectorAll('[data-testid="editor-link-icon"]');
    expect(links.length).toBe(1);
    expect(links[0].getAttribute('data-path')).toBe('src/lib/foo.ts');
  });

  it('clicking the editor-link invokes OpenInEditor', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit applied' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'summary_only',
        files: [{ path: 'src/lib/foo.ts', insertions: 4, deletions: 1, kind: 'modified' }],
      },
    };
    const { getByTestId } = render(ToolResultCard, { props: { item, meta: m } });
    const region = getByTestId('tool-result-inline-diffs');
    const link = region.querySelector('[data-testid="editor-link-icon"]') as HTMLElement;

    await fireEvent.click(link);
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 0, 0, '']);
  });

  // Regression for the original click-to-open bug: inline-diff file
  // paths are repo-relative. The card threads `pane.thread.workspacePath`
  // through to EditorLink so the backend can join. Pin that the prop
  // wiring carries the value end-to-end.
  it('forwards pane.thread.workspacePath to the OpenInEditor binding', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit applied' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'summary_only',
        files: [{ path: 'src/lib/foo.ts', insertions: 4, deletions: 1, kind: 'modified' }],
      },
    };
    // Minimal fake pane with the expansion-registry surface ToolResultCard
    // reads from, plus the workspace anchor we want to verify.
    const expansionCache = new Map<string, ReturnType<typeof createPayloadExpansion>>();
    const pane = {
      thread: { workspacePath: '/home/user/repo' },
      expansionStateFor(it: Item) {
        let h = expansionCache.get(it.id);
        if (!h) {
          h = createPayloadExpansion(() => it.payloadId, () => it.threadId);
          expansionCache.set(it.id, h);
        }
        return h;
      },
    } as unknown as import('../../stores/thread.svelte').ThreadPane;
    const { getByTestId } = render(ToolResultCard, { props: { pane, item, meta: m } });
    const region = getByTestId('tool-result-inline-diffs');
    const link = region.querySelector('[data-testid="editor-link-icon"]') as HTMLElement;

    await fireEvent.click(link);
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 0, 0, '/home/user/repo']);
  });
});

describe('<ToolResultCard> open-in-sidebar triggers', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('does not render chip / patch sidebar triggers when no pane prop is supplied', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'exact_patch',
        files: [{ path: 'a.ts', insertions: 1, deletions: 0, kind: 'modified' }],
      },
    };
    const { queryAllByTestId } = render(ToolResultCard, { props: { item, meta: m, payloadId: 'p-1' } });
    expect(queryAllByTestId('tool-result-chip-open-sidebar')).toHaveLength(0);
    expect(queryAllByTestId('tool-result-patch-open-sidebar')).toHaveLength(0);
  });

  it('renders a stable disabled exact-patch shell when the patch payload has not arrived yet', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'exact_patch',
        insertions: 2,
        deletions: 1,
        files: [{ path: 'src/a.ts', insertions: 2, deletions: 1, kind: 'modified' }],
      },
    };

    const { getByTestId, queryByTestId } = render(ToolResultCard, {
      props: { item, meta: m },
    });

    const toggle = getByTestId('tool-result-patch-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(queryByTestId('tool-result-patch-open-sidebar')).toBeNull();
  });

  it('routes a chip-button click to pane.openDiffSidebar with the chip\'s file path', async () => {
    const captures: Array<{ payloadId: string; filePath?: string }> = [];
    const fakePane = makeFakePane({
      openDiffSidebar(p: { payloadId: string; filePath?: string }) {
        captures.push(p);
      },
    } as unknown as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'summary_only',
        files: [
          { path: 'src/a.ts', insertions: 1, deletions: 0, kind: 'modified' },
          { path: 'src/b.ts', insertions: 0, deletions: 1, kind: 'modified' },
        ],
      },
    };
    const { container } = render(ToolResultCard, {
      props: { item, meta: m, payloadId: 'p-1', pane: fakePane },
    });

    const triggers = container.querySelectorAll('[data-testid="tool-result-chip-open-sidebar"]');
    expect(triggers.length).toBe(2);
    // Each chip's button carries `data-file-path` so we can pick the right one.
    const second = Array.from(triggers).find((t) => t.getAttribute('data-file-path') === 'src/b.ts');
    expect(second).toBeTruthy();
    await fireEvent.click(second as Element);

    expect(captures).toEqual([{ payloadId: 'p-1', filePath: 'src/b.ts' }]);
  });

  it('clicking the patch sidebar trigger does NOT toggle the exact-patch expander', async () => {
    const captures: Array<{ payloadId: string; filePath?: string }> = [];
    const fakePane = makeFakePane({
      openDiffSidebar(p: { payloadId: string; filePath?: string }) {
        captures.push(p);
      },
    } as unknown as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change' }),
    });
    const m: ToolResultMeta = {
      itemType: 'file_change',
      title: 'Edit applied',
      inlineDiff: {
        availability: 'exact_patch',
        insertions: 2,
        deletions: 1,
        files: [{ path: 'src/a.ts', insertions: 2, deletions: 1, kind: 'modified' }],
      },
    };
    const { getByTestId, queryByText } = render(ToolResultCard, {
      props: { item, meta: m, payloadId: 'p-1', pane: fakePane },
    });

    expect(queryByText('Exact patch')).toBeTruthy();
    const trigger = getByTestId('tool-result-patch-open-sidebar');
    await fireEvent.click(trigger);

    expect(captures).toEqual([{ payloadId: 'p-1' }]);
    expect(getByTestId('tool-result-patch-toggle')).toHaveAttribute('aria-expanded', 'false');
  });
});
