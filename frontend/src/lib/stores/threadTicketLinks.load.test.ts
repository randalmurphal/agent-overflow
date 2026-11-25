// Opening a linked ticket loads the Issues pane's stores on the first
// open. A load that fails says so, and the next open loads them again.

import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { getToasts, removeToast } from './toast.svelte';
import { makeThread } from '../../test/helpers/chat';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';

let failLoad = true;
vi.mock('./issuesPane.svelte', async (importOriginal) => {
  if (failLoad) throw new Error('the chunk did not load');
  return importOriginal();
});

beforeEach(() => {
  for (const toast of getToasts()) removeToast(toast.id);
  resetPaneLayoutForTest();
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
});
afterEach(() => {
  failLoad = true;
});

it('says a failed load of the Issues pane, and loads it again on the next open', async () => {
  const { openLinkedTicket } = await import('./threadTicketLinks.svelte');
  const thread = makeThread({
    ticketLink: {
      kind: 'jira', site: 'https://example.atlassian.net', issueId: '10001', key: 'WEB-1', title: 'First story',
      statusId: '3', statusName: 'In Progress', statusGroup: 'in_progress', origin: 'manual', linkedAt: 1,
      state: 'current', connectionId: 'conn-1',
    },
  });
  vi.spyOn(console, 'error').mockImplementation(() => {});
  await openLinkedTicket(thread);
  expect(getToasts().map((t) => t.message)).toEqual([expect.stringMatching(/^WEB-1 cannot open: the Issues pane did not load: /)]);

  failLoad = false;
  vi.resetModules();
  for (const toast of getToasts()) removeToast(toast.id);
  await openLinkedTicket(thread);
  const { takeTicketOpenRequest } = await import('./issuesPane.svelte');
  expect(takeTicketOpenRequest()?.ref.issue).toBe('10001');
  expect(getToasts()).toEqual([]);
});
