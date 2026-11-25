// The ticket's Work section: the threads linked to the ticket and no
// other, the pull requests and branches its computer found, each row's
// menu, and the Start work form filled from the project's saved defaults.

import { afterEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import TicketWork from './TicketWork.svelte';
import type { BoardChoice, DetailView, SetupView, TicketSubscription, TicketView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ThreadTicketLink } from '../../../../bindings/agent-overflow/internal/store/models';
import { addProjectLocal } from '../../stores/projects.svelte';
import { listThread } from '../../stores/threads.svelte';
import { attachTrackerTicket, type TrackerTicketAttachment } from '../../stores/trackerTicket.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';

const SITE = 'https://example.atlassian.net';

const DETAIL = {
  card: { id: '10101', key: 'WEB-1', title: 'Retry cursor', status: { id: 's1', name: 'To Do' }, created: 0, updated: 0 },
  url: `${SITE}/browse/WEB-1`, description: '', fields: [], comments: [], links: [], attachments: [], history: [], rows: [],
  moves: [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }, { id: '22', name: 'Finish', to: { id: 's3', name: 'Done' } }],
} as unknown as DetailView;

function link(issueId: string, origin = 'start_work'): ThreadTicketLink {
  return {
    kind: 'jira', site: SITE, issueId, key: issueId === '10101' ? 'WEB-1' : 'WEB-2', title: '', statusId: '', statusName: '', statusGroup: '',
    origin, linkedAt: 1, state: 'current', connectionId: 'c1',
  } as ThreadTicketLink;
}

let ticket: TrackerTicketAttachment | null = null;
afterEach(() => {
  ticket?.release();
  ticket = null;
});

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

async function mountWork() {
  setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, poll_every: 10_000, loaded: true, detail: DETAIL, read_at: 1 } as TicketSubscription));
  setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: DETAIL, read_at: 2 } as TicketView));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  setBindingMock('TrackerBoardChoices', async () => [{
    project: 'p1', connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
    verified_account: 'acc-1', board: 'b1', name: 'Board', tracker_project: 'WEB', opens_first: false,
  } as BoardChoice]);
  setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: SITE, label: 'Work', enabled: true, account: 'User 1' }]);
  setBindingMock('GetTicketWork', async () => ({
    key: 'WEB-1',
    pullRequests: [{ ref: { Forge: 'github', Host: 'github.com', Namespace: 'example', Repo: 'widgets', Number: 5 }, url: 'https://github.com/example/widgets/pull/5', title: 'WEB-1 Retry cursor', author: 'user1', state: 'open', ci: 'success', branch: 'WEB-1-retry', projectId: 'p1' }],
    branches: [{ projectId: 'p1', name: 'web-1-spike', isCurrent: false, isDefault: false, aheadCount: 2 }],
    problems: [{ projectId: 'p2', error: 'git for-each-ref failed: not a git repository' }],
  }));
  addProjectLocal({ id: 'p1', path: '/tmp/widgets', name: 'widgets', archived: false } as never);
  listThread(makeThread({ id: 't-linked', projectId: 'p1', title: 'WEB-1 Retry cursor', ticketLink: link('10101') }));
  listThread(makeThread({ id: 't-other', projectId: 'p1', title: 'WEB-2 Other', ticketLink: link('10102', 'ask') }));
  listThread(makeThread({ id: 't-plain', projectId: 'p1', title: 'No ticket' }));
  ticket = attachTrackerTicket({ backend: HOME_BACKEND, connection: 'c1', issue: '10101' }, true);
  await settle();
  const view = render(TicketWork, { attachment: ticket, board: 'b1' });
  await settle();
  return view;
}

describe('Work', () => {
  it('lists the threads linked to the ticket and what its computer found', async () => {
    const view = await mountWork();
    await waitFor(() => expect(view.getAllByTestId('ticket-work-thread').map((r) => r.dataset.thread)).toEqual(['t-linked']));
    const pull = view.getByTestId('ticket-work-pull');
    expect(pull.textContent).toContain('#5');
    expect(pull.textContent).toContain('WEB-1 Retry cursor');
    expect(view.getByTestId('ticket-work-pull-state').textContent).toBe('Open');
    const branch = view.getByTestId('ticket-work-branch');
    expect(branch.textContent).toContain('web-1-spike');
    expect(branch.textContent).toContain('2 ahead');
    expect(view.getByTestId('ticket-work-problem').textContent).toContain('git for-each-ref failed: not a git repository');
    view.unmount();
  });

  it('carries each row\'s actions in its menu, grouped', async () => {
    const view = await mountWork();
    await waitFor(() => expect(view.getAllByTestId('ticket-work-thread')).toHaveLength(1));
    await fireEvent.contextMenu(view.getByTestId('ticket-work-thread'));
    expect(document.querySelector('[data-testid="ticket-work-menu-open"]')).not.toBeNull();
    expect(document.querySelector('[data-testid="ticket-work-menu-unlink"]')).not.toBeNull();
    await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
    await fireEvent.contextMenu(view.getByTestId('ticket-work-branch'));
    expect(document.querySelector('[data-testid="ticket-work-menu-copy-branch"]')).not.toBeNull();
    expect(document.querySelector('[data-testid="ticket-work-menu-start-branch"]')).not.toBeNull();
    view.unmount();
  });

  it('fills Start work from the project\'s saved defaults and says a thread from Start work exists', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({
      revision: 'r1',
      setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', status: 's2', assign: false, branch: 'feature/{key}' }] },
    } as SetupView));
    const view = await mountWork();
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    await settle();
    const form = await waitFor(() => {
      const found = document.querySelector<HTMLElement>('[data-testid="start-work-form"]');
      expect(found).not.toBeNull();
      return found!;
    });
    await waitFor(() => expect((form.querySelector('[data-testid="start-work-branch"]') as HTMLInputElement).value).toBe('feature/WEB-1'));
    expect((form.querySelector('[data-testid="start-work-status"]') as HTMLSelectElement).value).toBe('s2');
    expect((form.querySelector('[data-testid="start-work-assign"]') as HTMLInputElement).checked).toBe(false);
    expect((form.querySelector('[data-testid="start-work-repository"]') as HTMLSelectElement).value).toBe('p1');
    expect(form.querySelector('[data-testid="start-work-started"]')?.textContent).toContain('WEB-1 already has a thread from Start work: WEB-1 Retry cursor');
    view.unmount();
  });
});
