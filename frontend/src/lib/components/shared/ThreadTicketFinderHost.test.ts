// Link ticket… and Change ticket…: the host loads the finder on its first
// open, over the thread's computer's Jira and Linear connections that are
// on, and links the thread to the ticket picked; a computer with none, or
// a release with no trackers, says so and closes.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { CardView, LookupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import ThreadTicketFinderHost from './ThreadTicketFinderHost.svelte';
import { closeThreadTicketFinder, findThreadTicket, threadTicketFinding } from '../../stores/threadTicketLinks.svelte';
import { getThreadById, prependThread, removeThread } from '../../stores/threads.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { makeThread } from '../../../test/helpers/chat';
import { trackerConnection } from '../../../test/helpers/trackerConnections';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { Thread } from '../../types/models';

const body = () => within(document.body);
const toastTexts = (): string[] => getToasts().map((t) => t.message);

let anchor: HTMLButtonElement;
beforeEach(() => {
  anchor = document.createElement('button');
  document.body.append(anchor);
  for (const toast of getToasts()) removeToast(toast.id);
});
afterEach(() => {
  closeThreadTicketFinder();
  removeThread('thread-1');
  anchor.remove();
});

const CONNECTIONS = [
  trackerConnection({ id: 'jira-on', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work Jira' }),
  trackerConnection({ id: 'linear-on', kind: 'linear', site: 'example', account: '', label: 'Side Linear' }),
  trackerConnection({ id: 'jira-off', kind: 'jira', site: 'https://other.atlassian.net', enabled: false }),
  trackerConnection({ id: 'tempo', kind: 'tempo', site: '', parentId: 'jira-on' }),
];

function open(thread: Thread): void {
  render(ThreadTicketFinderHost);
  findThreadTicket(thread, anchor);
}

describe('<ThreadTicketFinderHost>', () => {
  it('asks the Jira and Linear connections that are on, and links the thread to the ticket picked', async () => {
    const thread = makeThread();
    prependThread(thread);
    setBindingMock('TrackerConnections', async () => CONNECTIONS);
    const lookup = setBindingMock('TrackerLookup', async (connection: string): Promise<LookupView> => (
      connection === 'linear-on'
        ? { found: [{ key: 'APP-2', card: { id: 'lin-2', key: 'APP-2', title: 'Login page' } as CardView }] } as LookupView
        : { found: [] } as unknown as LookupView
    ));
    const set = setBindingMock('SetThreadTicket', async () => ({ ...thread, ticketLink: { key: 'APP-2' } }));
    open(thread);
    await fireEvent.input(await body().findByTestId('ticket-picker-find'), { target: { value: 'app-2' } });
    await waitFor(() => expect(body().getAllByTestId('ticket-picker-item').map((i) => i.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['APP-2 Login page · Side Linear']));
    expect(lookup.mock.calls.map((c) => c[0]).sort()).toEqual(['jira-on', 'linear-on']);
    await fireEvent.click(body().getByTestId('ticket-picker-item'));
    await waitFor(() => expect(set).toHaveBeenCalledWith('thread-1', { kind: 'linear', site: 'example', issueId: 'lin-2' }));
    expect(threadTicketFinding()).toBeNull();
    await waitFor(() => expect(getThreadById('thread-1')?.ticketLink?.key).toBe('APP-2'));
    expect(body().queryByTestId('ticket-picker-find')).toBeNull();
  });

  it('links a ticket one connection found while another refused the search, naming the one that refused', async () => {
    const thread = makeThread();
    prependThread(thread);
    setBindingMock('TrackerConnections', async () => CONNECTIONS);
    setBindingMock('TrackerLookup', async (connection: string): Promise<LookupView> => {
      if (connection === 'jira-on') throw new Error('the token was refused');
      return { found: [{ key: 'APP-2', card: { id: 'lin-2', key: 'APP-2', title: 'Login page' } as CardView }] } as LookupView;
    });
    const set = setBindingMock('SetThreadTicket', async () => ({ ...thread, ticketLink: { key: 'APP-2' } }));
    open(thread);
    await fireEvent.input(await body().findByTestId('ticket-picker-find'), { target: { value: 'app-2' } });
    await waitFor(() => expect(body().getAllByTestId('ticket-picker-item')).toHaveLength(1));
    expect(body().getByTestId('ticket-picker-partial')).toHaveTextContent('Work Jira');
    await fireEvent.click(body().getByTestId('ticket-picker-item'));
    await waitFor(() => expect(set).toHaveBeenCalledWith('thread-1', { kind: 'linear', site: 'example', issueId: 'lin-2' }));
    await waitFor(() => expect(getThreadById('thread-1')?.ticketLink?.key).toBe('APP-2'));
    expect(toastTexts()).toEqual([]);
  });

  it('says the computer has no connection to link through, and closes', async () => {
    setBindingMock('TrackerConnections', async () => [CONNECTIONS[2], CONNECTIONS[3]]);
    open(makeThread());
    await waitFor(() => expect(toastTexts()).toEqual(['This computer has no Jira or Linear connection turned on. Add one in Settings > Issues > Connections.']));
    expect(threadTicketFinding()).toBeNull();
    expect(body().queryByTestId('ticket-picker-find')).toBeNull();
  });

  it('says a release with no trackers cannot link, and closes', async () => {
    setBindingMock('TrackerConnections', async () => null);
    open(makeThread());
    await waitFor(() => expect(toastTexts()).toEqual(['This computer runs a release without Issues, so its threads cannot be linked to tickets.']));
    expect(threadTicketFinding()).toBeNull();
  });

  it('says why a link was refused, and keeps the thread as it was', async () => {
    const thread = makeThread();
    prependThread(thread);
    setBindingMock('TrackerConnections', async () => CONNECTIONS);
    setBindingMock('TrackerLookup', async (connection: string): Promise<LookupView> => (
      connection === 'jira-on'
        ? { found: [{ key: 'WEB-1', card: { id: '10001', key: 'WEB-1', title: 'First story' } as CardView }] } as LookupView
        : { found: [] } as unknown as LookupView
    ));
    setBindingMock('SetThreadTicket', async () => {
      throw new Error('the ticket is not there any more');
    });
    open(thread);
    await fireEvent.input(await body().findByTestId('ticket-picker-find'), { target: { value: 'WEB-1' } });
    await fireEvent.click(await body().findByTestId('ticket-picker-item'));
    await waitFor(() => expect(toastTexts()).toEqual(['The thread was not linked to WEB-1: The ticket is not there any more.']));
    expect(getThreadById('thread-1')?.ticketLink ?? null).toBeNull();
  });
});
