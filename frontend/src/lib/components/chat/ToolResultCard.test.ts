// ToolResultCard renders the file-change tool_result payload (Edit /
// Write / MultiEdit / Codex apply_patch). Pre-unification it
// hard-coded a green "done" span regardless of `item.status` or the
// `is_error` flag in payload meta — meaning a failed Edit rendered as
// success. These tests pin the unified-badge wiring and the bug fix.

import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolResultCard from './ToolResultCard.svelte';
import { makeItem } from '../../../test/helpers/chat';
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
