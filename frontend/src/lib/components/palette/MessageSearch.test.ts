import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import MessageSearch from './MessageSearch.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { refreshThreads } from '../../stores/threads.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Thread } from '../../types/models';

beforeAll(installAnimateShim);

beforeEach(async () => {
  setBindingMock('ListThreads', async () => []);
  await refreshThreads();
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
});

function makePane() {
  return createThreadPane();
}

function hit(overrides: Partial<{
  threadId: string;
  threadTitle: string;
  provider: string;
  itemId: string;
  turnIndex: number;
  itemKind: string;
  itemRole: string;
  summary: string;
  matchType: 'title' | 'item';
}> = {}) {
  return {
    threadId: 't1',
    threadTitle: 'Some Thread',
    provider: 'codex',
    itemId: 'i1',
    turnIndex: 3,
    itemKind: 'text',
    itemRole: 'assistant',
    summary: 'matched preview',
    matchType: 'item' as const,
    ...overrides,
  };
}

// ---- Visibility / idle state ----

describe('<MessageSearch> — visibility & idle state', () => {
  it('renders nothing when closed', () => {
    const pane = makePane();
    const { queryByTestId } = render(MessageSearch, { open: false, pane, onClose: vi.fn() });
    expect(queryByTestId('message-search')).toBeNull();
  });

  it('shows the idle hint when open with an empty query', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    expect(getByTestId('message-search-idle')).toBeInTheDocument();
  });

  it('resets the query on each reopen', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId, rerender } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'stale' } });
    await rerender({ open: false, pane, onClose: vi.fn() });
    await rerender({ open: true, pane, onClose: vi.fn() });
    expect((getByTestId('message-search-input') as HTMLInputElement).value).toBe('');
  });
});

// ---- Live search ----

describe('<MessageSearch> — search behavior', () => {
  it('calls the backend when the user types', async () => {
    const search = setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'bug' } });
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(search.mock.calls[0]).toEqual(['bug', 50]);
  });

  it('renders a hit row per result with thread title and match-type badge', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', threadTitle: 'First', matchType: 'title', itemId: '' }),
      hit({ threadId: 't2', threadTitle: 'Second', matchType: 'item', itemId: 'i9', turnIndex: 7, summary: 'hit body' }),
    ]);
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'q' } });
    await findByTestId('message-search-hit-t1-title');
    expect(getByTestId('message-search-hit-t1-title').textContent).toContain('First');
    expect(getByTestId('message-search-hit-t1-title').textContent).toMatch(/title/);
    expect(getByTestId('message-search-hit-t2-i9').textContent).toContain('Second');
    expect(getByTestId('message-search-hit-t2-i9').textContent).toMatch(/turn 7/);
    expect(getByTestId('message-search-hit-t2-i9').textContent).toContain('hit body');
  });

  it('shows the empty-state message when no hits match', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'xyzzy' } });
    const empty = await findByTestId('message-search-empty');
    expect(empty.textContent).toContain('xyzzy');
  });

  it('surfaces backend errors', async () => {
    setBindingMock('SearchThreadMessages', async () => {
      throw new Error('db is down');
    });
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'q' } });
    const err = await findByTestId('message-search-error');
    expect(err.textContent).toMatch(/db is down/);
  });

  it('ignores stale responses when the user types again mid-flight', async () => {
    // First search is slow; second search resolves first. The stale first
    // result must not overwrite the second.
    let releaseFirst: (value: unknown[]) => void = () => {};
    const firstPending = new Promise<unknown[]>((resolve) => { releaseFirst = resolve; });
    let counter = 0;
    setBindingMock('SearchThreadMessages', async () => {
      counter += 1;
      if (counter === 1) return firstPending;
      return [hit({ threadId: 't-new', threadTitle: 'Newer', matchType: 'title', itemId: '' })];
    });

    const pane = makePane();
    const { getByTestId, findByTestId, queryByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'one' } });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'two' } });
    await findByTestId('message-search-hit-t-new-title');
    // Now release the slow first response. It should NOT replace what we see.
    releaseFirst([hit({ threadId: 't-old', threadTitle: 'Older', matchType: 'title', itemId: '' })]);
    await new Promise((r) => setTimeout(r, 20));
    expect(queryByTestId('message-search-hit-t-old-title')).toBeNull();
    expect(getByTestId('message-search-hit-t-new-title')).toBeInTheDocument();
  });
});

// ---- Keyboard navigation ----

describe('<MessageSearch> — keyboard navigation', () => {
  it('arrow keys wrap the active index forward and backward', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'First' }),
      hit({ threadId: 't2', itemId: 'a', matchType: 'item', summary: 'second', threadTitle: 'Second' }),
      hit({ threadId: 't3', itemId: 'b', matchType: 'item', summary: 'third', threadTitle: 'Third' }),
    ]);
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    const backdrop = getByTestId('message-search-backdrop');

    // Start at 0 (First).
    expect(getByTestId('message-search-hit-t1-title').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(backdrop, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t2-a').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(backdrop, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t3-b').getAttribute('aria-current')).toBe('true');
    // Wrap forward.
    await fireEvent.keyDown(backdrop, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t1-title').getAttribute('aria-current')).toBe('true');
    // Wrap backward.
    await fireEvent.keyDown(backdrop, { key: 'ArrowUp' });
    expect(getByTestId('message-search-hit-t3-b').getAttribute('aria-current')).toBe('true');
  });

  it('Enter opens the currently-active hit', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'First' }),
      hit({ threadId: 't2', itemId: 'a', matchType: 'item', summary: 'second', threadTitle: 'Second' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    const backdrop = getByTestId('message-search-backdrop');
    // Default is first hit. Enter opens it.
    await fireEvent.keyDown(backdrop, { key: 'Enter' });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('t1');
  });

  it('Enter after ArrowDown opens the next hit', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'First' }),
      hit({ threadId: 't2', itemId: 'a', matchType: 'item', summary: 'second', threadTitle: 'Second' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    const backdrop = getByTestId('message-search-backdrop');
    await fireEvent.keyDown(backdrop, { key: 'ArrowDown' });
    await fireEvent.keyDown(backdrop, { key: 'Enter' });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('t2');
  });
});

// ---- Click / close interactions ----

describe('<MessageSearch> — interactions', () => {
  it('clicking a hit switches the pane and closes the dialog', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'First' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    const btn = await findByTestId('message-search-hit-t1-title');
    await fireEvent.click(btn);
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('t1');
  });

  it('falls back to a minimal thread shape when the sidebar does not know the id', async () => {
    // No threads in the sidebar view. The hit references one anyway — e.g.
    // archived thread not currently filtered in.
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't-missing', threadTitle: 'Archived', provider: 'claude', matchType: 'title', itemId: '' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await fireEvent.click(await findByTestId('message-search-hit-t-missing-title'));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    // The pane switched to a minimal shape with the right id.
    expect(pane.threadId).toBe('t-missing');
    expect(pane.thread?.title).toBe('Archived');
  });

  it('closes on Escape', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.keyDown(getByTestId('message-search-backdrop'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on backdrop click but not on dialog click', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId } = render(MessageSearch, { open: true, pane, onClose });
    await fireEvent.click(getByTestId('message-search'));
    expect(onClose).not.toHaveBeenCalled();
    await fireEvent.click(getByTestId('message-search-backdrop'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ---- Provider badge ----

describe('<MessageSearch> — provider rendering', () => {
  it('uses the hit\'s provider to pick the badge letter', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't-c', itemId: '', matchType: 'title', threadTitle: 'Claude one', provider: 'claude' }),
      hit({ threadId: 't-x', itemId: '', matchType: 'title', threadTitle: 'Codex one', provider: 'codex' }),
    ]);
    const pane = makePane();
    const { getByTestId, findByTestId } = render(MessageSearch, { open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'one' } });
    const claudeRow = await findByTestId('message-search-hit-t-c-title');
    const codexRow = await findByTestId('message-search-hit-t-x-title');
    // The badge's visible character is right next to the title.
    expect(claudeRow.textContent).toMatch(/\bC\b/);
    expect(codexRow.textContent).toMatch(/\bX\b/);
  });
});

// Silence unused-import warnings for tests that don't need a Thread type
// but import it for completeness.
void (null as unknown as Thread);
