import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import MessageSearch from './MessageSearch.svelte';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { refreshThreads } from '../../stores/threads.svelte';
import type { MessageSearchMode } from '../../stores/messageSearch.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { installAnimateShim } from '../../../test/integration/_helpers';
import type { Thread } from '../../types/models';

beforeAll(installAnimateShim);

beforeEach(async () => {
  resetPanesForTest();
  setBindingMock('ListThreads', async () => []);
  await refreshThreads();
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentTurns', async () => []);
});

function makePane() {
  return createThreadPane();
}

// Existing tests exercise the global cross-thread search; default the mode so
// each call site only states what it cares about. Thread-mode tests pass
// `mode: 'thread'` explicitly.
function renderSearch(props: {
  open: boolean;
  pane: ThreadPane | null;
  onClose?: () => void;
  mode?: MessageSearchMode;
}) {
  const { open, pane, onClose = vi.fn(), mode = 'global' } = props;
  return render(MessageSearch, { open, pane, onClose, mode });
}

// In-thread find reads only `pane.threadId` before calling SearchThreadItems.
// Driving a real pane to a loaded thread needs switchThread's full binding
// set; this stub provides the one field the search path touches. The shared
// openHit/navigation path is covered by the global-mode tests above.
function threadPaneStub(threadId: string): ThreadPane {
  return { threadId, requestScrollToItem: vi.fn() } as unknown as ThreadPane;
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
    const { queryByTestId } = renderSearch({ open: false, pane, onClose: vi.fn() });
    expect(queryByTestId('message-search')).toBeNull();
  });

  it('shows the idle hint when open with an empty query', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    expect(getByTestId('message-search-idle')).toBeInTheDocument();
  });

  it('resets the query on each reopen', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId, rerender } = renderSearch({ open: true, pane, onClose: vi.fn() });
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
    const { getByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'xyzzy' } });
    const empty = await findByTestId('message-search-empty');
    expect(empty.textContent).toContain('xyzzy');
  });

  it('surfaces backend errors', async () => {
    setBindingMock('SearchThreadMessages', async () => {
      throw new Error('db is down');
    });
    const pane = makePane();
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'q' } });
    const err = await findByTestId('message-search-error');
    expect(err.textContent).toMatch(/db is down/);
  });

  it('ignores stale responses when the user types again mid-flight', async () => {
    // First search is slow; second search resolves first. The stale first
    // result must not overwrite the second. Debounce coalesces same-tick
    // keystrokes, so we wait for the first query to actually fire before
    // typing again — both queries must reach the backend for the seq guard
    // (not the debounce) to be what discards the late response.
    let releaseFirst: (value: unknown[]) => void = () => {};
    const firstPending = new Promise<unknown[]>((resolve) => { releaseFirst = resolve; });
    let counter = 0;
    const search = setBindingMock('SearchThreadMessages', async () => {
      counter += 1;
      if (counter === 1) return firstPending;
      return [hit({ threadId: 't-new', threadTitle: 'Newer', matchType: 'title', itemId: '' })];
    });

    const pane = makePane();
    const { getByTestId, findByTestId, queryByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    const input = getByTestId('message-search-input');
    await fireEvent.input(input, { target: { value: 'one' } });
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    await fireEvent.input(input, { target: { value: 'two' } });
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    // Arrow keys are caught at the dialog body level so the input keeps
    // receiving letter keystrokes. Escape is the backdrop's contract.
    const body = getByTestId('message-search');

    // Start at 0 (First).
    expect(getByTestId('message-search-hit-t1-title').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t2-a').getAttribute('aria-current')).toBe('true');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t3-b').getAttribute('aria-current')).toBe('true');
    // Wrap forward.
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    expect(getByTestId('message-search-hit-t1-title').getAttribute('aria-current')).toBe('true');
    // Wrap backward.
    await fireEvent.keyDown(body, { key: 'ArrowUp' });
    expect(getByTestId('message-search-hit-t3-b').getAttribute('aria-current')).toBe('true');
  });

  it('Enter opens the currently-active hit', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'First' }),
      hit({ threadId: 't2', itemId: 'a', matchType: 'item', summary: 'second', threadTitle: 'Second' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    const body = getByTestId('message-search');
    // Default is first hit. Enter opens it.
    await fireEvent.keyDown(body, { key: 'Enter' });
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    await findByTestId('message-search-hit-t1-title');
    const body = getByTestId('message-search');
    await fireEvent.keyDown(body, { key: 'ArrowDown' });
    await fireEvent.keyDown(body, { key: 'Enter' });
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'x' } });
    const btn = await findByTestId('message-search-hit-t1-title');
    await fireEvent.click(btn);
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(pane.threadId).toBe('t1');
  });

  it('clicking an item hit publishes a scroll-to-item request for the match', async () => {
    // Post-windowing behavior: openHit must ask the pane to scroll to
    // the hit's itemId after switching threads. The pane mediates
    // out-of-window targets via loadUntilItem; this test pins the
    // wiring at the MessageSearch edge.
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: 'item-xyz', matchType: 'item', summary: 'match', threadTitle: 'Open' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const spy = vi.spyOn(pane, 'requestScrollToItem');

    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'q' } });
    await fireEvent.click(await findByTestId('message-search-hit-t1-item-xyz'));

    await waitFor(() => expect(spy).toHaveBeenCalledWith('item-xyz'));
  });

  it('does not publish a scroll request for a title-only hit', async () => {
    // Title matches carry no itemId — switching to the thread is
    // enough, the timeline should not receive a scroll request.
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'Title' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const spy = vi.spyOn(pane, 'requestScrollToItem');

    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'q' } });
    await fireEvent.click(await findByTestId('message-search-hit-t1-title'));
    await waitFor(() => expect(onClose).toHaveBeenCalled());

    expect(spy).not.toHaveBeenCalled();
  });

  it('falls back to a minimal thread shape when the sidebar does not know the id', async () => {
    // No threads in the sidebar view. The hit references one anyway — e.g.
    // archived thread not currently filtered in.
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't-missing', threadTitle: 'Archived', provider: 'claude', matchType: 'title', itemId: '' }),
    ]);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose });
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
    const { container } = renderSearch({ open: true, pane, onClose });
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on backdrop click but not on dialog click', async () => {
    setBindingMock('SearchThreadMessages', async () => []);
    const onClose = vi.fn();
    const pane = makePane();
    const { getByTestId, container } = renderSearch({ open: true, pane, onClose });
    // Clicking the dialog body should not dismiss; Modal only fires
    // onClose when the click originates on the backdrop itself.
    await fireEvent.click(getByTestId('message-search'));
    expect(onClose).not.toHaveBeenCalled();
    const backdrop = container.querySelector('[data-modal-backdrop]') as HTMLElement;
    await fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ---- Match highlighting ----

describe('<MessageSearch> — highlight', () => {
  it('wraps matched substrings in <mark> within thread titles', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: '', matchType: 'title', threadTitle: 'Release pipeline' }),
    ]);
    const pane = makePane();
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'release' } });
    const row = await findByTestId('message-search-hit-t1-title');
    const marks = row.querySelectorAll('mark');
    expect(marks.length).toBeGreaterThan(0);
    expect(marks[0].textContent).toBe('Release');
  });

  it('wraps matched substrings in <mark> within item summaries', async () => {
    setBindingMock('SearchThreadMessages', async () => [
      hit({ threadId: 't1', itemId: 'i1', matchType: 'item', summary: 'found the bug today' }),
    ]);
    const pane = makePane();
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'bug' } });
    const row = await findByTestId('message-search-hit-t1-i1');
    const marks = row.querySelectorAll('mark');
    expect(marks.length).toBeGreaterThan(0);
    expect(Array.from(marks).some((m) => m.textContent === 'bug')).toBe(true);
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
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, onClose: vi.fn() });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'one' } });
    const claudeRow = await findByTestId('message-search-hit-t-c-title');
    const codexRow = await findByTestId('message-search-hit-t-x-title');
    // The badge's visible character is right next to the title.
    expect(claudeRow.textContent).toMatch(/\bC\b/);
    expect(codexRow.textContent).toMatch(/\bX\b/);
  });
});

// ---- In-thread find (mode='thread') ----

describe('<MessageSearch> — in-thread find', () => {
  it('titles the dialog "Find in thread" and queries SearchThreadItems scoped to the pane thread', async () => {
    const scoped = setBindingMock('SearchThreadItems', async () => [
      hit({ threadId: 'thr-1', itemId: 'm5', turnIndex: 5, summary: 'thirty seconds in', matchType: 'item' }),
    ]);
    const pane = threadPaneStub('thr-1');
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, mode: 'thread' });
    // Title reflects the scoped mode.
    expect(getByTestId('message-search-input').getAttribute('aria-label')).toBe('Find in thread');
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'seconds' } });
    await findByTestId('message-search-hit-thr-1-m5');
    // Scoped binding, called with the pane's own thread id + result limit.
    expect(scoped).toHaveBeenCalledTimes(1);
    expect(scoped.mock.calls[0]).toEqual(['thr-1', 'seconds', 50]);
  });

  it('renders a thread-scoped row with a turn marker and no provider/match-type badge', async () => {
    setBindingMock('SearchThreadItems', async () => [
      hit({
        threadId: 'thr-1', itemId: 'm5', turnIndex: 9,
        summary: 'preserve seconds after one hour', matchType: 'item', provider: 'codex',
      }),
    ]);
    const pane = threadPaneStub('thr-1');
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, mode: 'thread' });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'seconds' } });
    const row = await findByTestId('message-search-hit-thr-1-m5');
    expect(row.textContent).toMatch(/turn 9/);
    expect(row.textContent).toContain('preserve seconds after one hour');
    // The global-mode codex badge ('X') and the title/turn match-type badge
    // are both suppressed in thread mode — every hit is already in-thread.
    expect(row.textContent).not.toMatch(/\bX\b/);
    expect(row.textContent).not.toMatch(/\btitle\b/);
  });

  it('does not query the backend when the pane has no thread yet', async () => {
    const scoped = setBindingMock('SearchThreadItems', async () => []);
    const pane = makePane(); // fresh pane → threadId is null
    const { getByTestId, findByTestId } = renderSearch({ open: true, pane, mode: 'thread' });
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'seconds' } });
    await findByTestId('message-search-empty');
    expect(scoped).not.toHaveBeenCalled();
  });

  it('uses the global title + binding when mode is "global"', async () => {
    const global = setBindingMock('SearchThreadMessages', async () => []);
    const scoped = setBindingMock('SearchThreadItems', async () => []);
    const pane = threadPaneStub('thr-1');
    const { getByTestId } = renderSearch({ open: true, pane, mode: 'global' });
    expect(getByTestId('message-search-input').getAttribute('aria-label')).toBe('Search messages');
    await fireEvent.input(getByTestId('message-search-input'), { target: { value: 'seconds' } });
    await waitFor(() => expect(global).toHaveBeenCalled());
    // Even though the pane has a thread, global mode never hits the scoped path.
    expect(scoped).not.toHaveBeenCalled();
  });
});

// ---- Debounce ----

describe('<MessageSearch> — debounce', () => {
  it('collapses rapid keystrokes into a single backend query for the final value', async () => {
    const search = setBindingMock('SearchThreadMessages', async () => []);
    const pane = makePane();
    const { getByTestId } = renderSearch({ open: true, pane });
    const input = getByTestId('message-search-input');
    await fireEvent.input(input, { target: { value: 's' } });
    await fireEvent.input(input, { target: { value: 'se' } });
    await fireEvent.input(input, { target: { value: 'sec' } });
    await fireEvent.input(input, { target: { value: 'seconds' } });
    await waitFor(() => expect(search).toHaveBeenCalled());
    // One scan for the settled value, not one per keystroke.
    expect(search).toHaveBeenCalledTimes(1);
    expect(search.mock.calls[0]).toEqual(['seconds', 50]);
  });
});

// Silence unused-import warnings for tests that don't need a Thread type
// but import it for completeness.
void (null as unknown as Thread);
