import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import ForkDivider from './ForkDivider.svelte';
import { makeItem, makeThread } from '../../../test/helpers/chat';
import { prependThread, removeThread } from '../../stores/threads.svelte';

const openThreadFromNavigation = vi.fn(async () => undefined);
vi.mock('../../stores/panes.svelte', () => ({
  getAllPanes: () => [],
  openThreadFromNavigation: (...args: unknown[]) => openThreadFromNavigation(...(args as [])),
}));

function dividerItem(meta: Record<string, unknown>) {
  return makeItem({
    id: 'fork-origin-fork-1',
    threadId: 'fork-1',
    kind: 'notification',
    role: 'system',
    toolName: 'fork_origin',
    summary: `Forked from ${typeof meta.sourceTitle === 'string' ? meta.sourceTitle : ''}`,
    meta: JSON.stringify({ kind: 'fork_origin', ...meta }),
  });
}

describe('<ForkDivider>', () => {
  beforeEach(() => {
    openThreadFromNavigation.mockClear();
  });

  afterEach(() => {
    cleanup();
    removeThread('source-1');
  });

  it('names the source and opens it in the pane when the sidebar tracks it', async () => {
    const source = makeThread({ id: 'source-1', title: 'Release notes' });
    prependThread(source);
    const pane = { id: 'pane-1' } as never;
    const { getByTestId, queryByTestId } = render(ForkDivider, {
      props: { pane, item: dividerItem({ sourceThreadId: 'source-1', sourceTitle: 'Release notes' }) },
    });

    const button = getByTestId('fork-divider-source');
    expect(button.textContent).toContain('Forked from Release notes');
    expect(queryByTestId('fork-divider-deleted')).toBeNull();
    await fireEvent.click(button);
    expect(openThreadFromNavigation).toHaveBeenCalledWith(source, pane);
  });

  it('stays a plain label when the source is not in the sidebar', () => {
    const { getByTestId, queryByTestId } = render(ForkDivider, {
      props: { item: dividerItem({ sourceThreadId: 'source-archived', sourceTitle: 'Release notes' }) },
    });

    expect(queryByTestId('fork-divider-source')).toBeNull();
    expect(queryByTestId('fork-divider-deleted')).toBeNull();
    expect(getByTestId('fork-divider').textContent).toContain('Forked from Release notes');
  });

  it('records a deleted source and offers no way to open it', () => {
    // The sidebar may still hold the row for a moment after the deletion
    // event; the divider's own meta decides.
    prependThread(makeThread({ id: 'source-1', title: 'Release notes' }));
    const { getByTestId, queryByTestId } = render(ForkDivider, {
      props: { item: dividerItem({ sourceThreadId: 'source-1', sourceTitle: 'Release notes', sourceDeleted: true }) },
    });

    expect(queryByTestId('fork-divider-source')).toBeNull();
    expect(getByTestId('fork-divider').textContent).toContain('Forked from Release notes');
    expect(getByTestId('fork-divider-deleted').textContent).toContain('source deleted');
  });

  it('falls back to Untitled for a source without a title', () => {
    const { getByTestId } = render(ForkDivider, {
      props: { item: dividerItem({ sourceThreadId: 'source-1', sourceTitle: '' }) },
    });

    expect(getByTestId('fork-divider').textContent).toContain('Forked from Untitled');
  });
});
