// The thread link's actions as every surface calls them: open the ticket
// in the Issues pane beside the thread, say why it cannot open, copy its
// key, and remove the link.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  copyLinkedTicketKey,
  findThreadTicket,
  linkedTicketUnopenable,
  linkThreadTicket,
  openLinkedTicket,
  removeThreadTicketLink,
  threadTicketFinding,
  ticketLinkPending,
} from './threadTicketLinks.svelte';
import { createThreadPane } from './thread.svelte';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { forgetProject, noteProject } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import type { Project } from '../types/models';
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

describe('linking a New thread', () => {
  const project: Project = { id: 'p-new', path: '/tmp/new', name: 'New', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
  const ticket = { kind: 'jira', site: 'https://example.atlassian.net', issueId: '10001', key: 'WEB-1' };

  beforeEach(() => {
    installThreadPaneTestEnv();
    noteProject(project.id, HOME_BACKEND);
  });
  afterEach(() => {
    forgetProject(project.id);
    resetStagedBackends();
  });

  function newThreadFinding() {
    const pane = createThreadPane();
    pane.startDraftPlaceholder(project, 'chat');
    findThreadTicket(pane.thread!, undefined, pane);
    const found = threadTicketFinding();
    if (!found) throw new Error('no finder opened');
    return { pane, found };
  }

  it('creates the thread through its pane and links that thread, holding the empty-draft cleanup until the row lands', async () => {
    const { pane, found } = newThreadFinding();
    const created = makeThread({ id: 'created', projectId: project.id, projectPath: project.path, workspacePath: project.path, isDraft: true });
    setBindingMock('CreateThread', async () => created);
    const held: boolean[] = [];
    const set = setBindingMock('SetThreadTicket', async () => {
      held.push(ticketLinkPending(pane.paneId));
      return { ...created, ticketLink: link() };
    });
    try {
      await linkThreadTicket(found, ticket);
      expect(set).toHaveBeenCalledWith('created', { kind: 'jira', site: 'https://example.atlassian.net', issueId: '10001' });
      expect(held).toEqual([true]);
      expect(ticketLinkPending(pane.paneId)).toBe(false);
      expect(pane.thread?.id).toBe('created');
      expect(toastTexts()).toEqual([]);
    } finally { pane.clear(); }
  });

  it('links nothing once the pane no longer shows that New thread', async () => {
    const { pane, found } = newThreadFinding();
    const create = setBindingMock('CreateThread', async () => makeThread({ id: 'created' }));
    const set = setBindingMock('SetThreadTicket', async () => makeThread({ id: 'created' }));
    pane.clear();
    await linkThreadTicket(found, ticket);
    expect(create).not.toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
    expect(toastTexts()).toEqual(['The thread was not linked to WEB-1: the New thread is no longer open.']);
    expect(ticketLinkPending(pane.paneId)).toBe(false);
  });

  it('opens no finder, and no ticket, for a thread no computer verifiably holds', async () => {
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    const thread = linkedThread();
    findThreadTicket(thread, undefined);
    expect(threadTicketFinding()).toBeNull();
    await openLinkedTicket(thread);
    expect(takeTicketOpenRequest()).toBeNull();
    expect(toastTexts()).toEqual([
      'Agent Overflow cannot tell which computer holds this thread, so it cannot be linked to a ticket.',
      'Agent Overflow cannot tell which computer holds this thread, so WEB-1 cannot open.',
    ]);
  });
});
