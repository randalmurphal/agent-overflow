// The thread link's actions as every surface calls them: open the ticket
// in the Issues pane beside the thread, say why it cannot open, copy its
// key, and remove the link.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  copyLinkedTicketKey,
  linkedTicketUnopenable,
  openLinkedTicket,
  removeThreadTicketLink,
} from './threadTicketLinks';
import { takeTicketOpenRequest } from './issuesPane.svelte';
import { getPaneLayoutItems, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { getThreadById, prependThread, removeThread } from './threads.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { makeThread } from '../../test/helpers/chat';
import { pairWithScopes, resetToLocalPage } from '../../test/helpers/scopes';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';
import type { ThreadTicketLink } from '../../../bindings/agent-overflow/internal/store/models';
import type { CapturedError } from './errorReports.svelte';

function link(overrides: Partial<ThreadTicketLink> = {}): ThreadTicketLink {
  return {
    kind: 'jira', site: 'https://example.atlassian.net', issueId: '10001', key: 'WEB-1', title: 'Retry cursor returns no rows',
    statusId: '3', statusName: 'In Progress', statusGroup: 'in_progress', origin: 'manual', linkedAt: 1,
    state: 'current', connectionId: 'conn-1',
    ...overrides,
  };
}

function linkedThread(overrides: Partial<ThreadTicketLink> = {}): Thread {
  return makeThread({ ticketLink: link(overrides) });
}

const toastTexts = (): string[] => getToasts().map((t) => t.message);

beforeEach(() => {
  for (const toast of getToasts()) removeToast(toast.id);
  resetPaneLayoutForTest();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
});

afterEach(() => {
  takeTicketOpenRequest();
  removeThread('thread-1');
  resetToLocalPage();
});

describe('openLinkedTicket', () => {
  it('opens the Issues pane beside the thread and asks it for the ticket, over the thread\'s project\'s boards', async () => {
    await openLinkedTicket(linkedThread());
    expect(getPaneLayoutItems().map((item) => item.kind)).toEqual(['thread', 'issues']);
    const request = takeTicketOpenRequest();
    expect(request?.ref).toEqual({ backend: expect.any(String), connection: 'conn-1', issue: '10001' });
    expect(request?.seed).toEqual({ key: 'WEB-1', title: 'Retry cursor returns no rows', card: null });
    expect(request?.project).toBe('project-1');
    expect(toastTexts()).toEqual([]);
  });

  it.each([
    ['turned_off', 'This computer has its Jira connection to example.atlassian.net turned off, so WEB-1 cannot open. Turn it on in Settings > Issues > Connections.'],
    ['no_connection', 'This computer has no Jira connection to example.atlassian.net, so WEB-1 cannot open. Add one in Settings > Issues > Connections.'],
  ] as const)('says why a %s link cannot open, and opens nothing', async (state, sentence) => {
    const thread = linkedThread({ state, connectionId: '' });
    expect(linkedTicketUnopenable(thread, thread.ticketLink!)).toBe(sentence);
    await openLinkedTicket(thread);
    expect(toastTexts()).toEqual([sentence]);
    expect(getPaneLayoutItems().map((item) => item.kind)).toEqual(['thread']);
    expect(takeTicketOpenRequest()).toBeNull();
  });

  it('leaves no request behind when this page may not open Issues', async () => {
    await pairWithScopes(['threads:read']);
    await openLinkedTicket(linkedThread());
    expect(toastTexts()).toHaveLength(1);
    expect(toastTexts()[0]).toMatch(/^WEB-1 cannot open: .*tracker:read/);
    expect(takeTicketOpenRequest()).toBeNull();
  });

  it('says so when no board of its site is listed, and when the boards could not be read', async () => {
    await openLinkedTicket(linkedThread());
    const request = takeTicketOpenRequest()!;
    request.unplaced(null);
    request.unplaced({ report: { reason: 'the computer answered 500' } } as CapturedError);
    expect(toastTexts()).toEqual([
      'This computer lists no board of example.atlassian.net in any project, so WEB-1 cannot open. Add one in Settings > Issues > Boards.',
      'This computer could not read its boards, so WEB-1 cannot open: the computer answered 500',
    ]);
  });
});

describe('copyLinkedTicketKey', () => {
  it('copies the key, and says so when the clipboard refuses', async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    await copyLinkedTicketKey(linkedThread());
    expect(writeText).toHaveBeenCalledWith('WEB-1');
    writeText.mockRejectedValueOnce(new Error('denied'));
    vi.spyOn(console, 'error').mockImplementation(() => {});
    await copyLinkedTicketKey(linkedThread());
    expect(toastTexts()).toEqual(['Could not copy WEB-1.']);
    vi.unstubAllGlobals();
  });
});

describe('removeThreadTicketLink', () => {
  it('applies the row the computer answers', async () => {
    const thread = linkedThread();
    prependThread(thread);
    const clear = setBindingMock('ClearThreadTicket', async () => ({ ...thread, ticketLink: null }));
    await removeThreadTicketLink(thread);
    expect(clear).toHaveBeenCalledWith('thread-1');
    expect(getThreadById('thread-1')?.ticketLink).toBeNull();
    expect(toastTexts()).toEqual([]);
  });

  it('keeps the link and says why when the computer refuses', async () => {
    const thread = linkedThread();
    prependThread(thread);
    setBindingMock('ClearThreadTicket', async () => {
      throw new Error('thread is archived');
    });
    await removeThreadTicketLink(thread);
    expect(getThreadById('thread-1')?.ticketLink?.key).toBe('WEB-1');
    expect(toastTexts()).toEqual(['The link to WEB-1 was not removed: Thread is archived.']);
  });
});
