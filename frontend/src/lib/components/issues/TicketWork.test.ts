// The ticket's Work section: the threads linked to the ticket and no
// other, the pull requests and branches its computer found, each row's
// menu, and the Start work form filled from the project's saved defaults.

import { afterEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import TicketWork from './TicketWork.svelte';
import type { DetailView, SetupView, TicketSubscription, TicketView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { ThreadTicketLink } from '../../../../bindings/agent-overflow/internal/store/models';
import { addProjectLocal } from '../../stores/projects.svelte';
import { listThread } from '../../stores/threads.svelte';
import { attachTrackerTicket, type TrackerTicketAttachment } from '../../stores/trackerTicket.svelte';
import { trackerIssueUpdated } from '../../stores/trackerBoardEvents';
import { HOME_BACKEND } from '../../transport/backendKey';
import { noteProject } from '../../transport/entityIndex';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { TransportError } from '../../transport/wsClient';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { getToasts } from '../../stores/toast.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { drawTicketWrite, fieldTarget, moveTicket, ticketWrite } from '../../stores/trackerTicketWrites.svelte';

const SITE = 'https://example.atlassian.net';

const DETAIL = {
  card: { id: '10101', key: 'WEB-1', title: 'Retry cursor', status: { id: 's1', name: 'To Do' }, created: 0, updated: 0 },
  url: `${SITE}/browse/WEB-1`, description: '', fields: [], comments: [], links: [], attachments: [], history: [], rows: [], sprints: [],
  moves: [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }, { id: '22', name: 'Finish', to: { id: 's3', name: 'Done' } }],
} as unknown as DetailView;

function link(issueId: string, origin = 'start_work'): ThreadTicketLink {
  return {
    kind: 'jira', site: SITE, issueId, key: issueId === '10101' ? 'WEB-1' : 'WEB-2', title: '', statusId: '', statusName: '', statusGroup: '',
    origin, linkedAt: 1, state: 'current', connectionId: 'c1',
  } as ThreadTicketLink;
}

let ticket: TrackerTicketAttachment | null = null;
// A Status field's registration a test made, released after it.
let statusField: (() => void) | null = null;
afterEach(() => {
  statusField?.();
  statusField = null;
  ticket?.release();
  ticket = null;
});

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

const WORK = {
  key: 'WEB-1',
  pullRequests: [{ ref: { Forge: 'github', Host: 'github.com', Namespace: 'example', Repo: 'widgets', Number: 5 }, url: 'https://github.com/example/widgets/pull/5', title: 'WEB-1 Retry cursor', author: 'user1', state: 'open', ci: 'success', branch: 'WEB-1-retry', projectId: 'p1' }],
  branches: [{ projectId: 'p1', name: 'web-1-spike', isCurrent: false, isDefault: false, aheadCount: 2 }],
  problems: [{ projectId: 'p2', error: 'git for-each-ref failed: not a git repository' }],
};

async function mountWork(opts: { backend?: string; first?: DetailView | null; work?: () => Promise<unknown>; search?: () => Promise<unknown>; archived?: boolean; shown?: () => boolean } = {}) {
  const backend = opts.backend ?? HOME_BACKEND;
  const first = opts.first === undefined ? DETAIL : opts.first;
  setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, poll_every: 10_000, kind: 'jira', site: SITE, loaded: first !== null, detail: first ?? undefined, read_at: first ? 1 : 0 } as TicketSubscription));
  setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: DETAIL, read_at: 2 } as TicketView));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  const reads = setBindingMock('GetTicketWork', opts.work ?? (async () => WORK));
  if (opts.search) setBindingMock('SearchTicketWork', opts.search);
  noteProject('p1', backend);
  addProjectLocal({ id: 'p1', path: '/tmp/widgets', name: 'widgets', archived: opts.archived ?? false } as never);
  listThread(makeThread({ id: 't-linked', projectId: 'p1', title: 'WEB-1 Retry cursor', ticketLink: link('10101') }));
  listThread(makeThread({ id: 't-other', projectId: 'p1', title: 'WEB-2 Other', ticketLink: link('10102', 'ask') }));
  listThread(makeThread({ id: 't-plain', projectId: 'p1', title: 'No ticket' }));
  ticket = attachTrackerTicket({ backend, connection: 'c1', issue: '10101' }, true);
  await settle();
  // The ticket screen Work is under, shown unless the test says.
  const view = render(TicketWork, {
    props: { attachment: ticket, project: 'p1', onProject: () => {}, me: 'acc-1', startWorkAsked: false, onStartWorkTaken: () => {} },
    context: ticketWriteSurfaceContext(opts.shown ?? (() => true)),
  });
  await settle();
  return Object.assign(view, { reads });
}

function formOf(): HTMLElement {
  const found = document.querySelector<HTMLElement>('[data-testid="start-work-form"]');
  expect(found).not.toBeNull();
  return found!;
}

describe('Work', () => {
  it('says the forge is being searched until its search answers, then lists what it found', async () => {
    let answer: (v: unknown) => void = () => {};
    const found = { ...WORK, problems: [], forgeProblems: [], searchFull: [], pullRequests: [...WORK.pullRequests,
      { ref: { Forge: 'github', Host: 'github.com', Namespace: 'example', Repo: 'widgets', Number: 9 }, url: 'https://github.com/example/widgets/pull/9', title: 'WEB-1 Docs', author: 'User 1', state: 'merged', branch: 'docs', projectId: 'p1' }] };
    const view = await mountWork({ search: () => new Promise((resolve) => { answer = resolve; }) });
    await waitFor(() => expect(view.getByTestId('ticket-work-searching').textContent).toBe('Searching the forge…'));
    expect(view.getAllByTestId('ticket-work-pull')).toHaveLength(1);
    answer(found);
    await waitFor(() => expect(view.queryByTestId('ticket-work-searching')).toBeNull());
    expect(view.getAllByTestId('ticket-work-pull-state').map((s) => s.textContent)).toEqual(['Open', 'Merged']);
    expect(view.getAllByTestId('ticket-work-pull')[1].textContent).toContain('User 1');
    view.unmount();
  });

  it('says each failed search in the Pull requests page\'s words beside the rows, and Retry reads again', async () => {
    const resumeAt = '2026-11-26T18:30:00Z';
    const view = await mountWork({
      search: async () => ({ ...WORK, problems: [], searchFull: [{ Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'grp', Repo: 'docs' }], forgeProblems: [
        { projectId: 'p1', repo: { Forge: 'github', Host: 'github.com', Namespace: 'example', Repo: 'widgets' }, error: 'failed to refresh from the forge (id: r1)', errorKind: 'rate_limited', resumeAt, errorDetails: { ref: 'r1', method: 'repository read', at: 1 } },
        { projectId: 'p1', repo: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'grp', Repo: 'app' }, error: 'The GitLab login cannot see grp/app.', errorKind: 'forbidden' },
        { projectId: 'p1', repo: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'grp', Repo: 'api' }, error: 'Run glab auth login --hostname gitlab.com.', errorKind: 'setup' },
        { projectId: 'p1', repo: { Forge: 'github', Host: 'github.com', Namespace: 'example', Repo: 'down' }, error: 'failed to refresh from the forge (id: r2)', errorKind: 'transient' },
      ] }),
    });
    const problems = await waitFor(() => {
      const shown = view.getAllByTestId('pulls-project-error');
      expect(shown).toHaveLength(4);
      return shown;
    });
    expect(problems.map((p) => p.dataset.errorKind)).toEqual(['rate_limited', 'forbidden', 'setup', 'transient']);
    expect(problems[0].textContent?.trim()).toMatch(/^GitHub rate limit reached\. Updates resume at /);
    expect(problems[1].textContent).toContain('Could not search grp/app: The GitLab login cannot see grp/app.');
    expect(problems[2].textContent).toContain('Could not search grp/api: Run glab auth login --hostname gitlab.com.');
    expect(problems[3].textContent).toContain('Could not search example/down: failed to refresh from the forge (id: r2)');
    expect(view.getByTestId('ticket-work-search-full').textContent).toBe('The search of grp/docs found more than 100 pull requests, so these may not be all of them.');
    // The rows from the stored sources still show.
    expect(view.getAllByTestId('ticket-work-pull')).toHaveLength(1);
    expect(view.getByTestId('ticket-work-branch').textContent).toContain('web-1-spike');
    const before = view.reads.mock.calls.length;
    await fireEvent.click(problems[3].querySelector('[data-testid="pulls-project-retry"]')!);
    await waitFor(() => expect(view.reads.mock.calls.length).toBe(before + 1));
    view.unmount();
  });

  it('lists the threads linked to the ticket by the tracker its subscription named, and what its computer found, reading no connections', async () => {
    const connections = setBindingMock('TrackerConnections', async () => []);
    const view = await mountWork();
    await waitFor(() => expect(view.getAllByTestId('ticket-work-thread').map((r) => r.dataset.thread)).toEqual(['t-linked']));
    expect(connections).not.toHaveBeenCalled();
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

  it('cannot be confirmed until the project\'s saved defaults have answered', async () => {
    let answer: (view: SetupView) => void = () => {};
    setBindingMock('ProjectTrackerSetup', () => new Promise<SetupView>((resolve) => { answer = resolve; }));
    const start = setBindingMock('StartTicketThread', async () => { throw new Error('stop here'); });
    const view = await mountWork();
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    const form = await waitFor(formOf);
    const go = form.querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
    await fireEvent.click(go);
    await settle();
    expect(start).not.toHaveBeenCalled();
    answer({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: false, branch: 'feature/{key}' }] } } as SetupView);
    await waitFor(() => expect(go.disabled).toBe(false));
    expect((form.querySelector('[data-testid="start-work-branch"]') as HTMLInputElement).value).toBe('feature/WEB-1');
    view.unmount();
  });

  it('reads Work once its computer has read the ticket, not before', async () => {
    // The subscription answers before the computer has the ticket's body;
    // a read of Work then is refused ("open it first").
    let readable = false;
    const view = await mountWork({
      first: null,
      work: async () => {
        if (!readable) throw new TransportError('ticket_not_read', 'this computer has not read the ticket yet: open it first');
        return { key: 'WEB-1', pullRequests: [], branches: [{ projectId: 'p1', name: 'WEB-1-late' }], problems: [] };
      },
    });
    readable = true;
    trackerIssueUpdated({ connection: 'c1', issue: '10101', seq: 2, changed: true }, HOME_BACKEND);
    await waitFor(() => expect(view.getByTestId('ticket-work-branch').textContent).toContain('WEB-1-late'));
    expect(view.queryByTestId('ticket-work-error')).toBeNull();
    view.unmount();
  });

  it('opens a branch with no thread in the Start work form on that branch, and a branch\'s thread only of its own project', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView));
    // A thread of another project whose worktree path is the same.
    listThread(makeThread({ id: 't-elsewhere', projectId: 'p9', title: 'Elsewhere', worktreePath: '/tmp/widgets-wt' }));
    const view = await mountWork({
      work: async () => ({ key: 'WEB-1', pullRequests: [], problems: [], branches: [{ projectId: 'p1', name: 'WEB-1-spike', worktreePath: '/tmp/widgets-wt' }] }),
    });
    const row = await waitFor(() => view.getByTestId('ticket-work-branch'));
    expect(row.textContent).not.toContain('Elsewhere');
    await fireEvent.click(row);
    const form = await waitFor(formOf);
    await waitFor(() => expect((form.querySelector('[data-testid="start-work-branch"]') as HTMLInputElement).value).toBe('WEB-1-spike'));
    view.unmount();
  });

  it('lists the archived project of the ticket in the form\'s picker and submits only a project the picker lists', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView));
    const start = setBindingMock('StartTicketThread', async () => { throw new Error('stop here'); });
    // A branch of an archived project that is not the ticket's.
    noteProject('p8', HOME_BACKEND);
    addProjectLocal({ id: 'p8', path: '/tmp/old', name: 'old', archived: true } as never);
    const view = await mountWork({
      archived: true,
      work: async () => ({ key: 'WEB-1', pullRequests: [], problems: [], branches: [{ projectId: 'p8', name: 'WEB-1-old' }] }),
    });
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    const form = await waitFor(formOf);
    const picker = form.querySelector('[data-testid="start-work-repository"]') as HTMLSelectElement;
    const go = form.querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
    await waitFor(() => expect(go.disabled).toBe(false));
    expect([...picker.options].map((o) => o.value)).toEqual(['p1']);
    expect(picker.value).toBe('p1');
    await fireEvent.click(go);
    await waitFor(() => expect(start).toHaveBeenCalledWith('p1', expect.objectContaining({ origin: 'start_work' })));
    view.unmount();
    ticket?.release();

    // The archived branch's project is not listed, so its form does not start.
    start.mockClear();
    const again = await mountWork({
      archived: true,
      work: async () => ({ key: 'WEB-1', pullRequests: [], problems: [], branches: [{ projectId: 'p8', name: 'WEB-1-old' }] }),
    });
    await fireEvent.click(await waitFor(() => again.getByTestId('ticket-work-branch')));
    const branchForm = await waitFor(formOf);
    const branchGo = branchForm.querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
    await settle();
    expect(branchGo.disabled).toBe(true);
    expect(branchGo.title).toBe('Pick the repository the thread runs in');
    await fireEvent.click(branchGo);
    expect(start).not.toHaveBeenCalled();
    again.unmount();
  });

  it('names the project a failed thread step makes its thread in beside its Try again, whatever project Work shows now', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({
      revision: 'r1',
      setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: false, branch: 'WEB-1-retry' }] },
    } as SetupView));
    setBindingMock('StartTicketThread', async () => { throw new TransportError('refused', 'The worktree could not be made.'); });
    const view = await mountWork();
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    const go = (await waitFor(formOf)).querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
    await waitFor(() => expect(go.disabled).toBe(false));
    await fireEvent.click(go);
    await waitFor(() => expect(view.getByTestId('start-work-retry')).toBeInTheDocument());
    const thread = view.getAllByTestId('start-work-step').find((s) => s.dataset.step === 'thread')!;
    expect(thread.children[0].textContent).toBe('Make the thread in widgets on WEB-1-retry');
    // Another surface of the ticket with no project shows the same run.
    await view.rerender({ project: null });
    expect(view.getAllByTestId('start-work-step').find((s) => s.dataset.step === 'thread')!.children[0].textContent).toBe('Make the thread in widgets on WEB-1-retry');
    view.unmount();
  });

  it('says each tracker step reads only while its read is out, saves while its write is, and the assignment waits while the move runs', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({
      revision: 'r1',
      setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', status: 's2', assign: true, branch: 'WEB-1-retry' }] },
    } as SetupView));
    setBindingMock('StartTicketThread', async () => makeThread({ id: 't-made', projectId: 'p1', title: 'WEB-1 Retry cursor', ticketLink: link('10101') }));
    const held = <T,>() => {
      let answer: (value: T) => void = () => {};
      const promise = new Promise<T>((resolve) => { answer = resolve; });
      return { promise, answer: (value: T) => answer(value) };
    };
    const refresh = held<void>();
    setBindingMock('RefreshTrackerSubscription', () => refresh.promise.then(() => {
      trackerIssueUpdated({ connection: 'c1', issue: '10101', seq: 5, changed: true }, HOME_BACKEND);
    }));
    const moved = held<unknown>();
    setBindingMock('MoveTrackerIssue', () => moved.promise);
    const assigned = held<unknown>();
    setBindingMock('EditTrackerIssue', () => assigned.promise);
    const view = await mountWork();
    statusField = drawTicketWrite(ticket!.key, 'status', () => true);
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    const go = (await waitFor(formOf)).querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
    await waitFor(() => expect(go.disabled).toBe(false));
    await fireEvent.click(go);
    const said = (step: string) => view.getAllByTestId('start-work-step').find((s) => s.dataset.step === step)!.children[1].textContent;

    await waitFor(() => expect(said('move')).toBe('Reading the ticket…'));
    expect(said('assign')).toBe('Waiting.');
    refresh.answer();
    await waitFor(() => expect(said('move')).toBe('Saving…'));
    expect(said('assign')).toBe('Waiting.');
    moved.answer({ parts: [{ step: 'move', state: 'applied' }] });
    await waitFor(() => expect(said('assign')).toBe('Saving…'));
    expect(said('move')).toBe('Done.');
    // Every step applied: the run ends and opens its thread.
    const opened = setBindingMock('SwitchThread', async () => makeThread({ id: 't-made', projectId: 'p1' }));
    assigned.answer({ parts: [{ step: 'edit', state: 'applied' }] });
    await waitFor(() => expect(opened).toHaveBeenCalledWith('t-made'));
    view.unmount();
  });

  it('says an assignment with no move reads, then saves, and the defaults save', async () => {
    setBindingMock('ProjectTrackerSetup', async () => ({
      revision: 'r1',
      setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: true, branch: 'WEB-1-retry' }] },
    } as SetupView));
    setBindingMock('StartTicketThread', async () => makeThread({ id: 't-made', projectId: 'p1', title: 'WEB-1 Retry cursor', ticketLink: link('10101') }));
    const held = <T,>() => {
      let answer: (value: T) => void = () => {};
      const promise = new Promise<T>((resolve) => { answer = resolve; });
      return { promise, answer: (value: T) => answer(value) };
    };
    const refresh = held<void>();
    setBindingMock('RefreshTrackerSubscription', () => refresh.promise.then(() => {
      trackerIssueUpdated({ connection: 'c1', issue: '10101', seq: 5, changed: true }, HOME_BACKEND);
    }));
    const assigned = held<unknown>();
    setBindingMock('EditTrackerIssue', () => assigned.promise);
    const saved = held<unknown>();
    setBindingMock('SetProjectTrackerSetup', () => saved.promise);
    const view = await mountWork();
    statusField = drawTicketWrite(ticket!.key, fieldTarget('assignee'), () => true);
    await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('ticket-work-start'));
    const form = await waitFor(formOf);
    const save = form.querySelector('[data-testid="start-work-save"]') as HTMLInputElement;
    await waitFor(() => expect(save.disabled).toBe(false));
    await fireEvent.click(save);
    await fireEvent.click(form.querySelector('[data-testid="start-work-go"]') as HTMLButtonElement);
    const said = (step: string) => view.getAllByTestId('start-work-step').find((s) => s.dataset.step === step)!.children[1].textContent;

    await waitFor(() => expect(said('assign')).toBe('Reading the ticket…'));
    expect(said('defaults')).toBe('Waiting.');
    refresh.answer();
    await waitFor(() => expect(said('assign')).toBe('Saving…'));
    assigned.answer({ parts: [{ step: 'edit', state: 'applied' }] });
    await waitFor(() => expect(said('defaults')).toBe('Saving…'));
    expect(said('assign')).toBe('Done.');
    const opened = setBindingMock('SwitchThread', async () => makeThread({ id: 't-made', projectId: 'p1' }));
    saved.answer({ saved: true, stored: { revision: 'r2', setup: { status_groups: [], ticket_pins: [], start_work: [] } } });
    await waitFor(() => expect(opened).toHaveBeenCalledWith('t-made'));
    view.unmount();
  });

  describe('what became of Start work\'s move', () => {
    /** Starts work with a move to In Progress that the tracker refuses,
     * under a Status field on a surface shown while `shown` answers true,
     * as the ticket screen mounts one beside Work. */
    async function refusedMove(shown: () => boolean) {
      setBindingMock('ProjectTrackerSetup', async () => ({
        revision: 'r1',
        setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', status: 's2', assign: false, branch: 'WEB-1-retry' }] },
      } as SetupView));
      setBindingMock('StartTicketThread', async () => makeThread({ id: 't-made', projectId: 'p1', title: 'WEB-1 Retry cursor', ticketLink: link('10101') }));
      setBindingMock('RefreshTrackerSubscription', async () => {
        trackerIssueUpdated({ connection: 'c1', issue: '10101', seq: 5, changed: true }, HOME_BACKEND);
      });
      const move = setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'A resolution is required.' } }] }));
      const view = await mountWork({ shown });
      statusField = drawTicketWrite(ticket!.key, 'status', shown);
      await waitFor(() => expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(false));
      await fireEvent.click(view.getByTestId('ticket-work-start'));
      const form = await waitFor(formOf);
      const go = form.querySelector('[data-testid="start-work-go"]') as HTMLButtonElement;
      await waitFor(() => expect(go.disabled).toBe(false));
      await fireEvent.click(go);
      await waitFor(() => expect(move).toHaveBeenCalledTimes(1));
      const row = await waitFor(() => {
        const found = view.getAllByTestId('start-work-step').find((s) => s.dataset.step === 'move');
        expect(found?.dataset.outcome).toBe('refused');
        return found!;
      });
      return { view, row };
    }

    const refusalToasts = () => getToasts().filter((t) => t.message.includes('A resolution is required.'));

    it('is kept under its Status field while the ticket is shown, shown under its row too, and not said in a toast', async () => {
      const { view, row } = await refusedMove(() => true);
      expect(ticketWrite(ticket!.key, 'status')?.outcome?.kind).toBe('refused');
      expect(row.querySelector('[data-testid="ticket-write-outcome"]')?.textContent).toContain('A resolution is required.');
      expect(refusalToasts()).toEqual([]);
      view.unmount();
    });

    it('is said once, in a toast, when it settles while the ticket is not shown', async () => {
      const { view, row } = await refusedMove(() => false);
      await waitFor(() => expect(refusalToasts().map((t) => t.message)).toEqual(['The tracker refused the change to WEB-1: A resolution is required.']));
      expect(row.textContent).not.toContain('A resolution is required.');
      expect(row.textContent).toContain('Refused.');
      view.unmount();
    });

    it('is no control of the status: a later move with no Status field shown is said in a toast and kept nowhere', async () => {
      const { view, row } = await refusedMove(() => true);
      statusField?.();
      statusField = null;
      setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'Done needs a resolution.' } }] }));
      await moveTicket({ key: ticket!.key, ref: ticket!.ref, label: 'WEB-1' }, '22');
      expect(getToasts().map((t) => t.message)).toContain('The tracker refused the change to WEB-1: Done needs a resolution.');
      expect(ticketWrite(ticket!.key, 'status')).toBeNull();
      expect(row.querySelector('[data-testid="ticket-write-outcome"]')).toBeNull();
      view.unmount();
    });
  });

  it('says why Ask and Start work cannot run when this device may not start threads or use git there', async () => {
    stageBackend({ id: 'laptop' } as never);
    setCarriedSessionScopes('laptop', ['tracker:read', 'threads:read']);
    try {
      const view = await mountWork({ backend: 'laptop' });
      await waitFor(() => expect(view.getByTestId('ticket-work-grants').textContent).toContain('may not start threads'));
      expect((view.getByTestId('ticket-work-ask') as HTMLButtonElement).disabled).toBe(true);
      expect((view.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(true);
      view.unmount();
      setCarriedSessionScopes('laptop', ['tracker:read', 'threads:read', 'threads:operate']);
      const again = await mountWork({ backend: 'laptop' });
      await waitFor(() => expect(again.getByTestId('ticket-work-grants').textContent).toContain('may not use git'));
      expect((again.getByTestId('ticket-work-ask') as HTMLButtonElement).disabled).toBe(false);
      expect((again.getByTestId('ticket-work-start') as HTMLButtonElement).disabled).toBe(true);
      again.unmount();
    } finally {
      resetStagedBackends();
    }
  });
});
