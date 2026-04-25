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
import type { ToolResultMeta } from '../../types/models';

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
        files: [{ path: 'src/lib/foo.ts', insertions: 4, deletions: 1, kind: 'modified' }],
      },
    };
    const { getByTestId } = render(ToolResultCard, { props: { item, meta: m } });
    const region = getByTestId('tool-result-inline-diffs');
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
    expect(openMock.mock.calls[0][0]).toBe('src/lib/foo.ts');
  });
});
