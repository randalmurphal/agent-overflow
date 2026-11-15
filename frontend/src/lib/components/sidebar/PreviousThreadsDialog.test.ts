import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import PreviousThreadsDialog from './PreviousThreadsDialog.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  closePreviousThreadsDialog,
  getPreviousThreads,
  openPreviousThreadsDialog,
  PREVIOUS_THREADS_PAGE,
  resetPreviousThreadsForTest,
} from '../../stores/assistantPastThreads.svelte';
import { openThreadFromNavigation } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { takePinnedBackend } from '../../transport/backends';
import { noteThread, threadBackend } from '../../transport/entityIndex';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import type { PastThreadsCursor } from '../../stores/bindings';
import type { Thread } from '../../types/models';

vi.mock('../../stores/panes.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/panes.svelte')>()),
  openThreadFromNavigation: vi.fn(async () => {}),
}));

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', model: 'test', projectId: '', projectPath: '/tmp/ws',
    workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0, archived: false, ...overrides,
  };
}

// A page as the store reads it; the binding's row type is the generated one.
type PastThreadsPage = { threads: Thread[]; next?: PastThreadsCursor };

const agent = thread('agent', { title: 'Ops', agentId: 'ops' });
// Newest superseded first, as the computer answers.
const past = Array.from({ length: PREVIOUS_THREADS_PAGE + 5 }, (_, i) =>
  thread(`past-${i}`, { title: `Earlier ${i}`, archived: true, pastAgentId: 'ops', agentSupersededAt: Date.now() - (i + 1) * 3_600_000 }));

function pages() {
  return setBindingMock('ListAssistantPastThreads', async (_agentId: string, after: PastThreadsCursor, limit: number): Promise<PastThreadsPage> => {
    const from = after.threadId === '' ? 0 : past.findIndex((t) => t.id === after.threadId) + 1;
    const rows = past.slice(from, from + limit);
    const last = rows[rows.length - 1];
    return from + limit < past.length ? { threads: rows, next: { supersededAt: last.agentSupersededAt!, threadId: last.id } } : { threads: rows };
  });
}

const items = (el: HTMLElement) => Array.from(el.querySelectorAll<HTMLElement>('[data-testid="previous-threads-item"]'), (b) => b.dataset.threadId);

describe('<PreviousThreadsDialog>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    resetPreviousThreadsForTest();
    vi.mocked(openThreadFromNavigation).mockClear();
    replaceAllThreads([agent]);
    noteThread('agent', HOME_BACKEND);
  });
  afterEach(() => resetPreviousThreadsForTest());

  it('lists the newest first with when each was superseded, a page at a time', async () => {
    const list = pages();
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, null);
    await waitFor(() => expect(items(view.baseElement)).toHaveLength(PREVIOUS_THREADS_PAGE));
    expect(view.baseElement.textContent).toContain("Ops's previous threads");
    expect(list).toHaveBeenCalledWith('ops', { supersededAt: 0, threadId: '' }, PREVIOUS_THREADS_PAGE);
    expect(items(view.baseElement).slice(0, 2)).toEqual(['past-0', 'past-1']);
    expect(view.getAllByTestId('previous-threads-superseded')[0].textContent?.trim()).toBe('Superseded 1h ago');

    await fireEvent.click(view.getByTestId('previous-threads-show-more'));
    await waitFor(() => expect(items(view.baseElement)).toHaveLength(past.length));
    expect(list).toHaveBeenLastCalledWith('ops', { supersededAt: past[PREVIOUS_THREADS_PAGE - 1].agentSupersededAt, threadId: `past-${PREVIOUS_THREADS_PAGE - 1}` }, PREVIOUS_THREADS_PAGE);
    expect(view.queryByTestId('previous-threads-show-more')).toBeNull();
  });

  it('reads the assistant\'s own computer, and routes a listed thread there', async () => {
    stageBackend({ id: 'gpu' });
    const remote = thread('remote-agent', { agentId: 'remote-ops' });
    replaceAllThreads([agent, remote]);
    noteThread('remote-agent', 'gpu', 1);
    const asked: (string | null)[] = [];
    setBindingMock('ListAssistantPastThreads', async () => {
      asked.push(takePinnedBackend());
      return { threads: [thread('remote-past', { ownershipEpoch: 1 })] };
    });
    render(PreviousThreadsDialog);
    openPreviousThreadsDialog(remote, null);
    await waitFor(() => expect(getPreviousThreads()?.loaded).toBe(true));
    expect(asked).toEqual(['gpu']);
    expect(threadBackend('remote-past')).toBe('gpu');
  });

  it('opens a picked thread in the pane the menu came from and closes', async () => {
    pages();
    const pane = createThreadPane();
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, pane);
    await waitFor(() => expect(items(view.baseElement).length).toBeGreaterThan(0));
    await fireEvent.click(view.baseElement.querySelector('[data-thread-id="past-1"]')!);
    expect(openThreadFromNavigation).toHaveBeenCalledWith(expect.objectContaining({ id: 'past-1' }), pane);
    expect(getPreviousThreads()).toBeNull();
    await tick();
    expect(view.queryByTestId('previous-threads-dialog')).toBeNull();
  });

  it('says so when the assistant has none', async () => {
    setBindingMock('ListAssistantPastThreads', async () => ({ threads: [] }));
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, null);
    await waitFor(() => expect(view.getByTestId('previous-threads-empty').textContent).toBe('No previous threads.'));
    expect(view.queryByTestId('previous-threads-show-more')).toBeNull();
  });

  it('drops a page that answers after the dialog closed or opened again', async () => {
    const answers: ((page: PastThreadsPage) => void)[] = [];
    setBindingMock('ListAssistantPastThreads', () => new Promise<PastThreadsPage>((resolve) => answers.push(resolve)));
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, null);
    closePreviousThreadsDialog();
    answers[0]({ threads: [past[0]] });
    await tick();
    expect(getPreviousThreads()).toBeNull();

    openPreviousThreadsDialog(agent, null);
    openPreviousThreadsDialog(agent, null);
    answers[1]({ threads: [past[1]] });
    await tick();
    expect(getPreviousThreads()?.loaded).toBe(false);
    answers[2]({ threads: [past[2]] });
    await waitFor(() => expect(items(view.baseElement)).toEqual(['past-2']));
  });

  it('shows a failed read and reads again on Try again', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListAssistantPastThreads', async () => { throw new Error('database is locked'); });
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, null);
    await waitFor(() => expect(view.getByTestId('previous-threads-error').textContent?.trim()).toBe('Database is locked.'));
    expect(view.queryByTestId('previous-threads-empty')).toBeNull();
    pages();
    await fireEvent.click(view.getByTestId('previous-threads-show-more'));
    await waitFor(() => expect(items(view.baseElement)).toHaveLength(PREVIOUS_THREADS_PAGE));
    expect(view.queryByTestId('previous-threads-error')).toBeNull();
    expect(view.getByTestId('previous-threads-show-more').textContent?.trim()).toBe('Show more…');
  });

  it('closes when its assistant leaves the list', async () => {
    pages();
    const view = render(PreviousThreadsDialog);
    openPreviousThreadsDialog(agent, null);
    await waitFor(() => expect(items(view.baseElement).length).toBeGreaterThan(0));
    replaceAllThreads([]);
    await waitFor(() => expect(getPreviousThreads()).toBeNull());
  });
});
