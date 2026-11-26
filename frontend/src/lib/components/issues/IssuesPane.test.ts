// The Issues pane over mocked bindings: the board it opens on, its columns
// and cards, Display, Only mine and Search over the held cards (reading a
// column whole when filtered), the notices of failure, refresh and end,
// the empty pane, the compact strip, the picker and the hidden vote; and
// the ticket a card opens over the board: the board's vote under it, a
// write that moves a card under it, the trail Back steps through, an edit
// in place and the comments it mounts.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type {
  BoardChoice,
  BoardPatch,
  BoardView,
  CommentView,
  DetailView,
  PokerView,
  RowView,
  Section,
  SectionPage,
  TicketSubscription,
  WriteView,
} from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
import IssuesPane from './IssuesPane.svelte';
import { trackerBoardUpdated as applyTrackerBoardEvent, trackerIssueUpdated } from '../../stores/trackerBoardEvents';
import { registerIssuesCommands } from '../../stores/issuesCommands';
import { clearCommandRegistry, runCommand, type CommandContext } from '../../stores/commandRegistry.svelte';
import { hiddenCardFields, requestTicketOpen, takeTicketOpenRequest } from '../../stores/issuesPane.svelte';
import { applyTrackerConnectionsChanged } from '../../stores/trackerConnections.svelte';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { detachBackend } from '../../transport/backends';
import { takePinnedBackend } from '../../transport/backends';
import { resetSidebarCatalogsForTest } from '../../stores/sidebarCatalogs';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { DisconnectedError } from '../../transport/wsClient';
import { openPaneFind, setPaneFindQuery } from '../../stores/paneFind.svelte';
import { noteCompactPaneShown, setCompactLayoutForTest, showCompactList, showCompactThread } from '../../stores/layoutMode.svelte';
import {
  getSettingsComputer,
  getSettingsSection,
  isSettingsOpen,
  requestedTrackerBoardsProject,
} from '../../stores/settingsOverlay.svelte';
import { addProjectLocal } from '../../stores/projects.svelte';
import { demotePane, getPaneLayoutItems, getPromotedPaneId, promotePane, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { focusPane, getFocusedPaneId } from '../../stores/panes.svelte';
import TicketPane from '../panes/TicketPane.svelte';
import { closeTicketPane, openTicketPane, restoreTicketPane, ticketPaneCarrier, ticketPaneRecord } from '../../stores/ticketPanes.svelte';
import { makeCommandContext } from '../../stores/builtinCommands.svelte';
import { SCOPES_AT_V0_0_19 } from '../../../test/helpers/olderComputer';
import { noteProject } from '../../transport/entityIndex';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { HOME_BACKEND } from '../../transport/backendKey';
import { TransportError } from '../../transport/wsClient';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { getMinuteNow } from '../../stores/minuteClock.svelte';
import { getToasts } from '../../stores/toast.svelte';
import { trackerTicketKey } from '../../stores/trackerTicket.svelte';
import { ticketDraft } from '../../stores/trackerTicketWrites.svelte';
import { localDay, mondayOf } from '../../stores/trackerTimeModel';
import { openNamedTicket } from '../../stores/ticketOpen';
import { openLinkedTicket } from '../../stores/threadTicketLinks.svelte';
import { makeThread } from '../../../test/helpers/chat';

// The project of the thread the pane was opened from (toolPanes owns it).
const openedFrom = vi.hoisted(() => ({ project: null as string | null }));
vi.mock('../../stores/toolPanes.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/toolPanes.svelte')>()),
  toolOpenedFromProject: () => openedFrom.project,
}));

const PANE = 'tool-issues';
const DAY = 86_400_000;
const BOARD_KEY = 'agent-overflow:frontend:issues-board';

/** The board this client's person picked last, as storage holds it. */
function picked(): TrackerBoardRef | null {
  const raw = localStorage.getItem(BOARD_KEY);
  return raw === null ? null : JSON.parse(raw);
}

/** Another window of this client saves `value` under `key`. */
function writeFromAnotherWindow(key: string, value: unknown): void {
  localStorage.setItem(key, JSON.stringify(value));
  window.dispatchEvent(new StorageEvent('storage', { key, storageArea: localStorage }));
}

function boardRef(board: string, projectId = 'p1', backend: string = HOME_BACKEND): TrackerBoardRef {
  return { backend, projectId, connection: 'c1', board };
}

function choice(board: string, extra: Partial<BoardChoice> = {}): BoardChoice {
  return {
    project: 'p1', connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
    verified_account: 'acc-user1', board, name: `Board ${board}`, tracker_project: 'WEB', opens_first: false, ...extra,
  } as BoardChoice;
}

function row(id: string, order: string, status: string, extra: Partial<RowView['card']> = {}): RowView {
  return {
    order, estimate: 2,
    card: {
      id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: status, name: status === 's2' ? 'In Progress' : 'To Do' },
      created: 0, updated: 0, ...extra,
    },
  } as RowView;
}

interface ColumnSeed { rows: RowView[]; total?: number; next?: boolean }

/** A board of two columns, To Do and In Progress. */
function kanban(todo: ColumnSeed, doing: ColumnSeed, extra: Partial<BoardView> = {}): BoardView {
  const seeds = [todo, doing];
  const sections = seeds.map((seed, i) => ({
    section: { kind: 'column', column: i },
    rows: seed.rows,
    next: seed.next ? { order: seed.rows.at(-1)!.order, issue: seed.rows.at(-1)!.card.id, lifetime: 1, revision: 1 } : null,
  }));
  const total = (seed: ColumnSeed) => ({ rows: seed.total ?? seed.rows.length, estimate: 2 * (seed.total ?? seed.rows.length) });
  return {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'Board 1', plans_sprints: false, estimate: { unit: 'points' },
    columns: [
      { name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: total(todo) },
      { name: 'In Progress', statuses: [{ id: 's2', group: 'in_progress' }], total: total(doing) },
    ],
    sprints: [], all: total({ rows: [...todo.rows, ...doing.rows] }), backlog: { rows: 0, estimate: 0 },
    sync: { read_at: Date.now() }, screen: sections.map((s) => s.section), sections, ...extra,
  } as unknown as BoardView;
}

/** A sprint board whose one column, To Do, holds statuses s1 and s2,
 * with its active sprint 8 holding `rows`, whole unless `more`; `left_out`
 * names the statuses the project leaves out of counts. */
function sprintBoard(rows: RowView[], left_out?: string[], more = false): BoardView {
  const section = { kind: 'sprint', sprint: '8' };
  const next = more ? { order: rows.at(-1)!.order, issue: rows.at(-1)!.card.id, lifetime: 1, revision: 1 } : null;
  return {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'Board 1', plans_sprints: true, estimate: { unit: 'points' },
    columns: [{ name: 'To Do', statuses: [{ id: 's1', group: 'todo' }, { id: 's2', group: 'in_progress' }], total: { rows: 0, estimate: 0 } }],
    sprints: [{ id: '8', name: 'Sprint 8', state: 'active', total: { rows: 0, estimate: 0 } }],
    all: { rows: 0, estimate: 0 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: Date.now() },
    screen: [section], sections: [{ section, rows, next }],
    ...(left_out ? { groups: 'g1', left_out } : {}),
  } as unknown as BoardView;
}

/** The worked example: four tickets of 3 points in the sprint, the last
 * in s2. */
function fourOfThree(): RowView[] {
  return ['1', '2', '3', '4'].map((id) => ({ ...row(id, `a${id}`, id === '4' ? 's2' : 's1'), estimate: 3 }));
}

function install(opts: { choices?: BoardChoice[]; view?: BoardView | null; failure?: { kind: string; message: string; at: number } } = {}) {
  const choices = setBindingMock('TrackerBoardChoices', async () => opts.choices ?? [choice('1', { opens_first: true }), choice('2')]);
  let n = 0;
  const subscribe = setBindingMock('SubscribeTrackerBoard', async (_p: string, _c: string, board: string) => {
    n += 1;
    const view = opts.view === undefined ? kanban({ rows: [row('1', 'a', 's1')] }, { rows: [row('2', 'b', 's2')] }) : opts.view;
    return { id: `sub-${board}-${n}`, seq: 10, loaded: view !== null, view: view ?? undefined, failure: opts.failure, poll_every: 10_000 };
  });
  const release = setBindingMock('ReleaseTrackerSubscription', async () => undefined);
  const visible = setBindingMock('SetTrackerSubscriptionVisible', async () => undefined);
  return { choices, subscribe, release, visible };
}

/** A project of this computer, as its catalog read lists it. */
function project(id: string, name: string, sortPosition: number, updatedAt = 1): void {
  noteProject(id, HOME_BACKEND);
  addProjectLocal({ id, name, path: `/repos/${name}`, createdAt: 1, updatedAt, sortPosition, archived: false });
}

function mount() {
  return render(IssuesPane, { paneId: PANE });
}

// Escape acts on the focused pane: the Issues pane is placed and focused,
// and the command runs as its chord does.
function focusIssuesPane(): () => void {
  setPaneLayoutItemsForTest([{ id: PANE, paneId: PANE, kind: 'issues', widthPx: 600 }]);
  focusPane(PANE);
  return () => runCommand('issues.escape', { pane: null, paneId: null, flags: { issuesPaneFocused: true, toolDetailOpen: true } } as unknown as CommandContext);
}

function columns(view: ReturnType<typeof mount>): Array<{ name: string; count: string; cards: string[] }> {
  return view.getAllByTestId('issues-column').map((col) => ({
    name: col.dataset.column!,
    count: within(col).queryByTestId('issues-column-count')?.textContent?.trim() ?? '',
    cards: within(col).queryAllByTestId('issues-card').map((c) => c.dataset.issue!),
  }));
}

function menu(): HTMLElement {
  return document.body.querySelector<HTMLElement>('[data-popover]')!;
}

describe('the Issues pane', () => {
  it('opens the board its project opens first and draws its columns and cards', async () => {
    const mocks = install();
    const view = mount();
    await waitFor(() => expect(view.getAllByTestId('issues-card')).toHaveLength(2));
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', [], true);
    expect(columns(view)).toEqual([
      { name: 'To Do', count: '1 · 2 pts', cards: ['1'] },
      { name: 'In Progress', count: '1 · 2 pts', cards: ['2'] },
    ]);
    expect(view.getByTestId('issues-board-picker')).toHaveTextContent('Board 1');
    // A board the pane chose is not a pick: nothing is saved.
    expect(picked()).toBeNull();
    view.unmount();
    await waitFor(() => expect(mocks.release).toHaveBeenCalledWith('sub-1-1'));
  });

  it('opens the board picked last before the boards are listed, and lets it go when its computer refuses it', async () => {
    const mocks = install();
    writeFromAnotherWindow(BOARD_KEY, boardRef('2'));
    let list: (rows: BoardChoice[]) => void = () => {};
    setBindingMock('TrackerBoardChoices', () => new Promise<BoardChoice[]>((resolve) => { list = resolve; }));
    mocks.subscribe.mockImplementationOnce(async () => {
      throw new TransportError('tracker_board_not_listed', 'no project lists this board any more');
    });
    const view = mount();
    await waitFor(() => expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '2', [], true));
    list([choice('1', { opens_first: true })]);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenLastCalledWith('p1', 'c1', '1', [], true));
    await view.findAllByTestId('issues-card');
    // The refusal is the computer's last word: no retry of board 2.
    expect(mocks.subscribe.mock.calls.filter((c) => c[2] === '2')).toHaveLength(1);
    expect(view.queryByTestId('issues-board-ended')).toBeNull();
    expect(picked()?.board).toBe('2');
    view.unmount();
  });

  it('shows a card its type, priority, key, parent, labels, estimate, assignee and days in status', async () => {
    install({
      view: kanban({
        rows: [row('1', 'a', 's1', {
          type: { id: 't', name: 'Bug', level: 0 }, priority: { id: 'p', name: 'High' },
          parent: { id: '9', key: 'WEB-9', title: 'Import reliability' }, labels: [{ id: 'l', name: 'backend' }],
          // Against the clock the card reads, which ticks once a minute.
          assignee: { id: 'acc-user1', name: 'User 1' }, status_since: getMinuteNow() - 3 * DAY - 1000,
        })],
      }, { rows: [] }),
    });
    const view = mount();
    const card = await view.findByTestId('issues-card');
    expect(within(card).getByTestId('issues-card-key')).toHaveTextContent('WEB-1');
    expect(within(card).getByTestId('issues-card-type')).toHaveAttribute('title', 'Bug');
    expect(within(card).getByTestId('issues-card-priority')).toHaveAttribute('title', 'High priority');
    expect(within(card).getByTestId('issues-card-parent')).toHaveTextContent('Import reliability');
    expect(within(card).getByTestId('issues-card-labels')).toHaveTextContent('backend');
    expect(within(card).getByTestId('issues-card-estimate')).toHaveTextContent('2');
    expect(within(card).getByTestId('issues-card-assignee')).toHaveTextContent('U1');
    expect(within(card).getByTestId('issues-card-days')).toHaveTextContent('3 days in To Do');
    expect(within(view.getAllByTestId('issues-column')[1]).getByTestId('issues-column-empty')).toHaveTextContent('Nothing here');
  });

  it('says a column its first read did not carry is not read yet, loading while it is read, and that the read failed', async () => {
    const view0 = kanban({ rows: [row('1', 'a', 's1')] }, { rows: [] });
    const done = { kind: 'column', column: 2 } as Section;
    install({
      view: {
        ...view0,
        columns: [...view0.columns, { name: 'Done', statuses: [{ id: 's3', group: 'done' }], total: { rows: 0, estimate: 0 } }],
        screen: [...view0.screen, done],
        sections: view0.sections.slice(0, 1),
      } as BoardView,
    });
    let fail: (err: unknown) => void = () => {};
    setBindingMock('TrackerSectionRows', () => new Promise((_resolve, reject) => { fail = reject; }));
    const view = mount();
    const col = (i: number) => within(view.getAllByTestId('issues-column')[i]);
    await waitFor(() => expect(col(1).getByTestId('issues-column-loading').textContent?.trim()).toBe('Loading…'));
    expect(col(2).getByTestId('issues-column-unread')).toHaveTextContent('Not read yet');
    fail(new TransportError('timeout', 'the computer did not answer in time'));
    await waitFor(() => expect(col(1).getByTestId('issues-column-failed')).toHaveTextContent('Could not read this column.'));
    expect(col(1).queryByTestId('issues-column-empty')).toBeNull();
    expect(view.getByTestId('issues-read-error')).toBeInstanceOf(HTMLElement);
    // The read goes on to the next column.
    await waitFor(() => expect(col(2).getByTestId('issues-column-loading').textContent?.trim()).toBe('Loading…'));
    view.unmount();
  });

  it('stops saying a filtered column is loading once its read failed', async () => {
    const todo = Array.from({ length: 100 }, (_, i) => row(String(i + 10), `k${String(i).padStart(3, '0')}`, 's1'));
    const view0 = kanban({ rows: todo, total: 150, next: true }, { rows: [] });
    install({ view: { ...view0, sections: view0.sections.slice(0, 1) } as BoardView });
    let fail: (err: unknown) => void = () => {};
    setBindingMock('TrackerSectionRows', () => new Promise((_resolve, reject) => { fail = reject; }));
    const view = mount();
    const col = (i: number) => within(view.getAllByTestId('issues-column')[i]);
    await waitFor(() => expect(col(1).queryByTestId('issues-column-loading')).not.toBeNull());
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'export');
    await waitFor(() => expect(columns(view)[0].count).toBe('100 of 150 loaded'));
    fail(new TransportError('timeout', 'the computer did not answer in time'));
    // The To Do page the filter owes is read next; it times out too.
    await waitFor(() => expect(col(0).queryByTestId('issues-column-loading')).not.toBeNull());
    fail(new TransportError('timeout', 'the computer did not answer in time'));
    await waitFor(() => expect(col(0).getByTestId('issues-column-failed')).toHaveTextContent('Could not read more of this column.'));
    expect(col(0).queryByTestId('issues-column-loading')).toBeNull();
    expect(col(1).queryByTestId('issues-column-loading')).toBeNull();
    expect(col(1).getByTestId('issues-column-failed')).toHaveTextContent('Could not read this column.');
    view.unmount();
  });

  it('hides the fields Display turns off, for this board', async () => {
    install({ view: kanban({ rows: [row('1', 'a', 's1', { labels: [{ id: 'l', name: 'backend' }] })] }, { rows: [] }) });
    const view = mount();
    await view.findByTestId('issues-card-labels');
    await fireEvent.click(view.getByTestId('issues-display'));
    await fireEvent.click(within(menu()).getByTestId('issues-display-labels'));
    await waitFor(() => expect(view.queryByTestId('issues-card-labels')).toBeNull());
    expect([...hiddenCardFields({ backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' })]).toEqual(['labels']);
  });

  it('keeps only the cards of the verified account with Only mine, and says when the account is unknown', async () => {
    install({
      view: kanban({ rows: [row('1', 'a', 's1', { assignee: { id: 'acc-user1', name: 'User 1' } }), row('3', 'c', 's1')] }, { rows: [row('2', 'b', 's2')] }),
    });
    const view = mount();
    await waitFor(() => expect(view.getAllByTestId('issues-card')).toHaveLength(3));
    await fireEvent.click(view.getByTestId('issues-only-mine'));
    await waitFor(() => expect(view.getAllByTestId('issues-card').map((c) => c.dataset.issue)).toEqual(['1']));
    expect(columns(view).map((c) => c.count)).toEqual(['1', '0']);
    expect(within(view.getAllByTestId('issues-column')[1]).getByTestId('issues-column-empty')).toHaveTextContent('No matching cards');
    expect(view.queryByTestId('issues-only-mine-unknown')).toBeNull();
  });

  it('says Only mine cannot tell which cards are yours on an unverified connection', async () => {
    install({ choices: [choice('1', { verified_account: '' })] });
    const view = mount();
    await waitFor(() => expect(view.getAllByTestId('issues-card')).toHaveLength(2));
    await fireEvent.click(view.getByTestId('issues-only-mine'));
    await view.findByTestId('issues-only-mine-unknown');
    expect(view.getAllByTestId('issues-card')).toHaveLength(2);
  });

  it('filters by the search and reads the rest of a column it filters, saying how much has loaded', async () => {
    const todo = Array.from({ length: 100 }, (_, i) => row(String(i + 10), `k${String(i).padStart(3, '0')}`, 's1'));
    install({ view: kanban({ rows: todo, total: 150, next: true }, { rows: [row('2', 'b', 's2', { title: 'Export skips rows' })] }) });
    let answer: (page: SectionPage) => void = () => {};
    const page = setBindingMock('TrackerSectionRows', (_id: string, section: Section) => new Promise<SectionPage>((resolve) => {
      answer = resolve;
      expect(section).toMatchObject({ kind: 'column', column: 0 });
    }));
    const view = mount();
    await waitFor(() => expect(columns(view)[0].cards.length).toBeGreaterThan(0));
    expect(columns(view)[0].count).toBe('150 · 300 pts');
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'export');
    await waitFor(() => expect(columns(view)[0].count).toBe('100 of 150 loaded'));
    expect(columns(view)[1].cards).toEqual(['2']);
    expect(page).toHaveBeenCalledTimes(1);
    answer({ section: { kind: 'column', column: 0 }, rows: [row('200', 'z', 's1', { title: 'Export the rest' })] } as unknown as SectionPage);
    await waitFor(() => expect(columns(view)[0].count).toBe('1'));
    expect(columns(view)[0].cards).toEqual(['200']);
  });

  it('stops reading a column whole once its filter is cleared', async () => {
    const todo = Array.from({ length: 100 }, (_, i) => row(String(i + 10), `k${String(i).padStart(3, '0')}`, 's1'));
    install({ view: kanban({ rows: todo, total: 300, next: true }, { rows: [row('2', 'b', 's2')] }) });
    let answer: (page: SectionPage) => void = () => {};
    const page = setBindingMock('TrackerSectionRows', () => new Promise<SectionPage>((resolve) => { answer = resolve; }));
    const view = mount();
    await waitFor(() => expect(columns(view)[0].cards.length).toBeGreaterThan(0));
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'export');
    await waitFor(() => expect(page).toHaveBeenCalledTimes(1));
    setPaneFindQuery(PANE, '');
    await waitFor(() => expect(columns(view)[0].count).toBe('300 · 600 pts'));
    const more = Array.from({ length: 100 }, (_, i) => row(String(i + 200), `m${String(i).padStart(3, '0')}`, 's1'));
    answer({ section: { kind: 'column', column: 0 }, rows: more, next: { order: 'm099', issue: '299', lifetime: 1, revision: 1 } } as unknown as SectionPage);
    await waitFor(() => expect(columns(view)[0].cards).toHaveLength(200));
    await tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(page).toHaveBeenCalledTimes(1);
  });

  it("states the board's failure with when it was last read, and keeps Refresh with its answers", async () => {
    install({ failure: { kind: 'auth', message: 'The tracker refused the key.', at: 1 } });
    const refresh = setBindingMock('RefreshTrackerSubscription', async () => {
      throw new TransportError('tracker_refresh_not_run', 'The board was read 5 seconds ago, so it was not read again.');
    });
    const view = mount();
    const notice = await view.findByTestId('issues-board-failure');
    expect(notice.textContent).toMatch(/^The tracker refused the key\. Last read/);
    await fireEvent.click(view.getByTestId('issues-refresh'));
    const answered = await view.findByTestId('issues-refresh-notice');
    expect(answered).toHaveTextContent('The board was read 5 seconds ago, so it was not read again.');
    await fireEvent.click(within(answered).getByTestId('issues-refresh-again'));
    expect(refresh).toHaveBeenCalledTimes(2);
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, sync: { read_at: Date.now() } }, HOME_BACKEND);
    await waitFor(() => expect(view.queryByTestId('issues-board-failure')).toBeNull());
  });

  it('spins Refresh only while its call is out, and shows a refresh that failed', async () => {
    install();
    let fail: (err: unknown) => void = () => {};
    setBindingMock('RefreshTrackerSubscription', () => new Promise((_resolve, reject) => { fail = reject; }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    const button = view.getByTestId('issues-refresh');
    await fireEvent.click(button);
    await waitFor(() => expect(button.querySelector('.animate-spin')).not.toBeNull());
    fail(new TransportError('method_error', 'database is locked'));
    await view.findByTestId('issues-refresh-error');
    expect(button.querySelector('.animate-spin')).toBeNull();
  });

  it('says a board ended, and that a board not read yet has not been', async () => {
    install({ view: null });
    setBindingMock('TrackerBoard', async () => {
      throw new TransportError('tracker_board_not_read', 'not read');
    });
    const view = mount();
    await view.findByTestId('issues-not-read');
    expect(view.getByTestId('issues-refresh')).not.toBeDisabled();
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, ended: true }, HOME_BACKEND);
    const ended = await view.findByTestId('issues-board-ended');
    expect(view.getByTestId('issues-refresh')).toBeDisabled();
    await fireEvent.click(within(ended).getByTestId('issues-ended-settings'));
    expect(getSettingsSection()).toBe('tracker-boards');
    expect(requestedTrackerBoardsProject()).toBe('p1');
  });

  it('says no project lists a board when none does, with the way to the first project\'s boards by name', async () => {
    const mocks = install({ choices: [] });
    // The sidebar shows widgets first: it changed last and is first by hand.
    project('p1', 'widgets', 0, 5);
    project('p2', 'gadgets', 1, 1);
    const view = mount();
    await waitFor(() => expect(view.getByTestId('issues-empty')).toHaveTextContent('No project lists a board yet.'));
    expect(mocks.subscribe).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('issues-empty-settings'));
    expect(isSettingsOpen()).toBe(true);
    expect(getSettingsSection()).toBe('tracker-boards');
    expect(requestedTrackerBoardsProject()).toBe('p2');
  });

  it('opens the Boards page of the project the pane was opened from when no project lists a board', async () => {
    install({ choices: [] });
    project('p1', 'widgets', 0);
    project('p2', 'gadgets', 1);
    openedFrom.project = 'p2';
    try {
      const view = mount();
      await fireEvent.click(await view.findByTestId('issues-empty-settings'));
      expect(requestedTrackerBoardsProject()).toBe('p2');
    } finally {
      openedFrom.project = null;
    }
  });

  it('offers no way to Settings to a window that may not change a project\'s boards', async () => {
    await pairWithScopes(['threads:read', 'tracker:read']);
    install({ choices: [] });
    project('p1', 'widgets', 0);
    const view = mount();
    await waitFor(() => expect(view.getByTestId('issues-empty')).toHaveTextContent('No project lists a board yet.'));
    expect(view.queryByTestId('issues-empty-settings')).toBeNull();
    resetToLocalPage();
  });

  it('switches boards from the picker, releasing the one it showed', async () => {
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    await fireEvent.click(view.getByTestId('issues-board-picker'));
    expect(mocks.choices).toHaveBeenCalledTimes(2);
    const items = within(menu()).getAllByTestId('issues-board-choice');
    expect(items.map((i) => i.textContent?.trim())).toEqual([expect.stringContaining('Board 1'), expect.stringContaining('Board 2')]);
    await fireEvent.click(items[1]);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenLastCalledWith('p1', 'c1', '2', [], true));
    await waitFor(() => expect(mocks.release).toHaveBeenCalledWith('sub-1-1'));
    expect(picked()?.board).toBe('2');
  });

  it("lists the boards again when the computer's connections change", async () => {
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.choices).toHaveBeenCalledTimes(1);
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await waitFor(() => expect(mocks.choices).toHaveBeenCalledTimes(2));
    view.unmount();
    // Nothing is read while no pane holds the boards.
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await tick();
    expect(mocks.choices).toHaveBeenCalledTimes(2);
  });

  it('shows one column under compact, picked from a strip, and votes hidden while another screen shows', async () => {
    setCompactLayoutForTest(true);
    showCompactThread();
    noteCompactPaneShown(PANE);
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    const tabs = view.getAllByTestId('issues-column-tab');
    expect(tabs.map((t) => t.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['To Do 1 · 2 pts', 'In Progress 1 · 2 pts']);
    // The first column in progress shows first.
    expect(tabs[1]).toHaveAttribute('aria-selected', 'true');
    expect(view.getAllByTestId('issues-card').map((c) => c.dataset.issue)).toEqual(['2']);
    await fireEvent.click(tabs[0]);
    await waitFor(() => expect(view.getAllByTestId('issues-card').map((c) => c.dataset.issue)).toEqual(['1']));
    expect(view.queryByTestId('issues-only-mine')).toBeNull();

    expect(mocks.visible).not.toHaveBeenCalled();
    showCompactList();
    await waitFor(() => expect(mocks.visible).toHaveBeenCalledWith('sub-1-1', false));
    showCompactThread();
    await waitFor(() => expect(mocks.visible).toHaveBeenLastCalledWith('sub-1-1', true));
    expect(getBindingMock('SetTrackerSubscriptionVisible')).toHaveBeenCalledTimes(2);
  });

  it('subscribes with its vote down when it opens on a screen that is not shown', async () => {
    setCompactLayoutForTest(true);
    showCompactList();
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', [], false);
    expect(mocks.visible).not.toHaveBeenCalled();
    showCompactThread();
    noteCompactPaneShown(PANE);
    await waitFor(() => expect(mocks.visible).toHaveBeenCalledWith('sub-1-1', true));
  });

  it('votes hidden while another pane is promoted over the strip', async () => {
    setCompactLayoutForTest(false);
    setPaneLayoutItemsForTest([
      { id: PANE, paneId: PANE, kind: 'issues', widthPx: 600 },
      { id: 'thread-1', paneId: 'thread-1', kind: 'thread', widthPx: 600 },
    ]);
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.visible).not.toHaveBeenCalled();
    expect(promotePane('thread-1')).toBe(true);
    await waitFor(() => expect(mocks.visible).toHaveBeenCalledWith('sub-1-1', false));
    demotePane();
    await waitFor(() => expect(mocks.visible).toHaveBeenLastCalledWith('sub-1-1', true));
  });
});

describe('changing a card on the board', () => {
  it('sets the points typed at the card\'s estimate, through the ticket\'s write', async () => {
    install();
    const estimate = setBindingMock('SetTrackerEstimate', async (): Promise<WriteView> => ({ parts: [{ step: 'estimate', state: 'applied' }] }) as WriteView);
    const view = mount();
    await view.findAllByTestId('issues-card');
    const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === '1')!;
    await fireEvent.click(within(card).getByTestId('issues-card-estimate-edit'));
    const input = await within(document.body).findByTestId('ticket-field-input');
    await fireEvent.input(input, { target: { value: '5' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(estimate).toHaveBeenCalledWith('c1', { board: '1', issue: '1', value: 5 }));
    expect(within(document.body).queryByTestId('issues-card-typed')).toBeNull();
    await waitFor(() => expect(within(card).queryByTestId('ticket-write-pending')).toBeNull());
  });

  it('moves a card from its menu\'s Move to…, which reads the moves when it opens', async () => {
    install();
    const moves = setBindingMock('TrackerMoves', async () => [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }]);
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'move', state: 'applied' }] }) as WriteView);
    const view = mount();
    await view.findAllByTestId('issues-card');
    const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === '1')!;
    await fireEvent.contextMenu(card);
    expect(moves).not.toHaveBeenCalled();
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-move'));
    const target = await within(document.body).findByTestId('issues-card-move-column');
    expect(target).toHaveTextContent('In Progress');
    expect(moves).toHaveBeenCalledTimes(1);
    await fireEvent.click(target);
    await waitFor(() => expect(move).toHaveBeenCalledWith('c1', { issue: '1', move: '21', fields: undefined, accept_loss: false }));
    // It waits in In Progress while the move is out, and the headers count it there.
    await waitFor(() => expect(within(document.body).queryByTestId('action-menu')).toBeNull());
  });

  /** Moves card `issue` by its menu's Move to… into the one column its
   * move reaches. */
  async function moveByMenu(view: ReturnType<typeof mount>, issue: string): Promise<void> {
    const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === issue)!;
    await fireEvent.contextMenu(card);
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-move'));
    await fireEvent.click(await within(document.body).findByTestId('issues-card-move-column'));
    await waitFor(() => expect(within(document.body).queryByTestId('action-menu')).toBeNull());
  }

  /** A move binding whose answers the test gives one at a time. */
  function heldMoves(): { calls: unknown[][]; answer: (view: WriteView) => void } {
    const waiting: ((view: WriteView) => void)[] = [];
    const calls: unknown[][] = [];
    setBindingMock('MoveTrackerIssue', (...args: unknown[]) => {
      calls.push(args);
      return new Promise<WriteView>((resolve) => waiting.push(resolve));
    });
    return { calls, answer: (view) => waiting.shift()!(view) };
  }

  it('counts a waiting card into its column by the status it moves to, and out of its own by the status it has', async () => {
    // In Progress's status is left out of counts.
    install({ view: kanban({ rows: [row('1', 'a', 's1')] }, { rows: [row('2', 'b', 's2')] }, { groups: 'g1', left_out: ['s2'] } as Partial<BoardView>) });
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => (issue === '1'
      ? [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }]
      : [{ id: '11', name: 'Back', to: { id: 's1', name: 'To Do' } }]));
    const moves = heldMoves();
    const view = mount();
    await view.findAllByTestId('issues-card');
    const before = columns(view).map((c) => c.count);
    // 1 leaves a counted status for a left-out one, 2 the other way.
    await moveByMenu(view, '1');
    await moveByMenu(view, '2');
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([['2'], ['1']]));
    expect(columns(view).map((c) => c.count)).toEqual(before);
    moves.answer({ parts: [{ step: 'move', state: 'applied' }] } as WriteView);
    moves.answer({ parts: [{ step: 'move', state: 'applied' }] } as WriteView);
  });

  it('draws a waiting card from the board as it is now: hidden by the filter, and with a change a patch brought', async () => {
    install();
    setBindingMock('TrackerMoves', async () => [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }]);
    setBindingMock('TrackerBoardPatch', async (): Promise<BoardPatch> => ({
      lifetime: 1, revision: 2,
      sections: [{ section: { kind: 'column', column: 0 }, rows: [{ ...row('1', 'a', 's1'), estimate: 5 }] }, { section: { kind: 'column', column: 1 }, rows: [] }],
    } as unknown as BoardPatch));
    const moves = heldMoves();
    const view = mount();
    await view.findAllByTestId('issues-card');
    await moveByMenu(view, '1');
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([[], ['1', '2']]));
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'Issue 2');
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([[], ['2']]));
    setPaneFindQuery(PANE, '');
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([[], ['1', '2']]));
    // Its estimate changes on the tracker while the move is out.
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, changed: ['1'] }, HOME_BACKEND);
    const card = () => view.getAllByTestId('issues-card').find((c) => c.dataset.issue === '1')!;
    await waitFor(() => expect(within(card()).getByTestId('issues-card-estimate')).toHaveTextContent('5'));
    expect(columns(view).map((c) => c.cards)).toEqual([[], ['1', '2']]);
    moves.answer({ parts: [{ step: 'move', state: 'applied' }] } as WriteView);
  });
});

describe("where a write's outcome shows", () => {
  beforeAll(async () => {
    await import('./TimeScreen.svelte');
    await import('./TicketScreen.svelte');
  });

  const refusal = (step: string, message: string) => ({ parts: [{ step, state: 'refused', refusal: { reason: 'tracker', message } }] }) as WriteView;
  const toasts = () => getToasts().map((t) => t.message);
  const card = (view: ReturnType<typeof mount>, issue: string) => view.getAllByTestId('issues-card').find((c) => c.dataset.issue === issue)!;

  it('says in a toast a card write that settles while the board is under Time, and keeps it nowhere', async () => {
    install();
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [] }));
    let answer: (view: WriteView) => void = () => {};
    setBindingMock('SetTrackerEstimate', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-estimate-edit'));
    const input = await within(document.body).findByTestId('ticket-field-input');
    await fireEvent.input(input, { target: { value: '5' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(within(card(view, '1')).getByTestId('ticket-write-pending')).toBeInTheDocument());
    await switchTo(view, 'Time');
    await view.findByTestId('time-screen');
    const before = toasts().length;
    answer(refusal('estimate', 'Story points are not on the edit screen'));
    await waitFor(() => expect(toasts().slice(before)).toEqual(['The tracker refused the change to WEB-1: Story points are not on the edit screen']));
    await switchTo(view, 'Board');
    await view.findAllByTestId('issues-card');
    expect(within(card(view, '1')).queryByTestId('ticket-write-outcome')).toBeNull();
  });

  it('says in a toast a card write that settles while a ticket covers the board and does not draw it', async () => {
    install({ view: sprintBoard([row('1', 'a', 's1', { sprints: ['8'] })]) });
    installTicket();
    let answer: (view: WriteView) => void = () => {};
    const sprint = setBindingMock('MoveTrackerIssuesToSprint', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await fireEvent.contextMenu(card(view, '1'));
    await fireEvent.click(await within(document.body).findByTestId('action-menu-more'));
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-sprint'));
    await fireEvent.click(await within(document.body).findByTestId('issues-card-sprint'));
    await waitFor(() => expect(sprint).toHaveBeenCalled());
    await openCard(view, '1');
    const before = toasts().length;
    answer(refusal('sprint', 'The sprint is closed'));
    await waitFor(() => expect(toasts().slice(before)).toEqual(['The tracker refused the change to WEB-1: The sprint is closed']));
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    expect(within(card(view, '1')).queryByTestId('ticket-write-outcome')).toBeNull();
  });

  it('keeps a refusal on the card where it waited and went back, with no toast, until the card is no longer drawn', async () => {
    install();
    setBindingMock('TrackerMoves', async () => [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }]);
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [] }));
    let answer: (view: WriteView) => void = () => {};
    setBindingMock('MoveTrackerIssue', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    const before = toasts().length;
    await fireEvent.contextMenu(card(view, '1'));
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-move'));
    await fireEvent.click(await within(document.body).findByTestId('issues-card-move-column'));
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([[], ['1', '2']]));
    answer(refusal('move', 'A resolution is required'));
    // Back in To Do, drawn by a card mounted anew, with its reason.
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([['1'], ['2']]));
    await waitFor(() => expect(within(card(view, '1')).getByTestId('ticket-write-outcome')).toHaveTextContent('A resolution is required'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(within(card(view, '1')).getByTestId('ticket-write-outcome')).toBeInTheDocument();
    // Its last card unmounts: the outcome goes, said nowhere.
    await switchTo(view, 'Time');
    await view.findByTestId('time-screen');
    await switchTo(view, 'Board');
    await view.findAllByTestId('issues-card');
    expect(within(card(view, '1')).queryByTestId('ticket-write-outcome')).toBeNull();
    expect(toasts().length).toBe(before);
  });

  it('keeps a refusal made on the ticket once, drawn on the ticket and the card under it, with no toast', async () => {
    install();
    installTicket();
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '3', name: 'Low' }] }));
    setBindingMock('EditTrackerIssue', async () => refusal('edit', 'Priority is not on the edit screen'));
    const view = mount();
    const screen = await openCard(view, '1');
    const before = toasts().length;
    const priority = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    await fireEvent.click(within(priority).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    await waitFor(() => expect(within(priority).getByTestId('ticket-write-outcome')).toHaveTextContent('Priority is not on the edit screen'));
    expect(within(card(view, '1')).getByTestId('ticket-write-outcome')).toHaveTextContent('Priority is not on the edit screen');
    expect(toasts().length).toBe(before);
  });

  it('keeps the draft and its Save anyway for a comment edit refused into a toast while the pane was off screen', async () => {
    setCompactLayoutForTest(false);
    setPaneLayoutItemsForTest([
      { id: PANE, paneId: PANE, kind: 'issues', widthPx: 600 },
      { id: 'thread-1', paneId: 'thread-1', kind: 'thread', widthPx: 600 },
    ]);
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 1 }));
    const loss = { parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView;
    const answers: ((view: WriteView) => void)[] = [];
    const edit = setBindingMock('EditTrackerComment', () => new Promise<WriteView>((resolve) => answers.push(resolve)));
    const view = mount();
    const screen = await openCard(view, '1');
    const comment = await waitFor(() => within(screen).getByTestId('ticket-comment'));
    await fireEvent.contextMenu(comment);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(comment).getByTestId('ticket-comment-editor'), { target: { value: 'Plain now' } });
    await fireEvent.click(within(comment).getByTestId('ticket-comment-save'));
    await waitFor(() => expect(answers).toHaveLength(1));
    expect(promotePane('thread-1')).toBe(true);
    const before = toasts().length;
    answers[0](loss);
    await waitFor(() => expect(toasts().slice(before)).toEqual(['The tracker refused the change to WEB-1: This edit would lose formatting. It would lose: a table.']));
    demotePane();
    // Back on screen: the edit is still in its editor, and no outcome under it.
    expect(within(comment).getByTestId('ticket-comment-editor')).toHaveValue('Plain now');
    expect(within(comment).queryByTestId('ticket-write-outcome')).toBeNull();
    await fireEvent.click(within(comment).getByTestId('ticket-comment-save'));
    await waitFor(() => expect(answers).toHaveLength(2));
    answers[1](loss);
    const anyway = await waitFor(() => within(comment).getByTestId('ticket-write-save-anyway'));
    await fireEvent.click(anyway);
    await waitFor(() => expect(edit).toHaveBeenLastCalledWith('c1', { issue: '1', comment: 'c1', markdown: 'Plain now', from: 'Comment 1', accept_loss: true }));
    answers[2]({ parts: [{ step: 'comment', state: 'applied' }] } as WriteView);
    await waitFor(() => expect(within(comment).queryByTestId('ticket-comment-editor')).toBeNull());
    expect(toasts().length).toBe(before + 1);
  });
});

describe("the project's status groups on the Board screen", () => {
  it('shows the cards of a status left out of counts and leaves them out of the column it adds up', async () => {
    install({ view: sprintBoard(fourOfThree(), ['s2']) });
    const view = mount();
    await waitFor(() => expect(view.getAllByTestId('issues-card')).toHaveLength(4));
    expect(columns(view)).toEqual([{ name: 'To Do', count: '3 · 9 pts', cards: ['1', '2', '3', '4'] }]);
    view.unmount();
  });

  it("changes the column's total when a save changes what is left out and the board is read again", async () => {
    install({ view: sprintBoard(fourOfThree()) });
    const view = mount();
    await waitFor(() => expect(columns(view)[0]?.count).toBe('4 · 12 pts'));
    setBindingMock('TrackerBoard', async () => sprintBoard(fourOfThree(), ['s2']));
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, read_again: true, sync: { read_at: Date.now() } }, HOME_BACKEND);
    await waitFor(() => expect(columns(view)[0]?.count).toBe('3 · 9 pts'));
    expect(columns(view)[0].cards).toEqual(['1', '2', '3', '4']);
    view.unmount();
  });

  it('says how much of a filtered column has loaded in the rows its total counts', async () => {
    // Ten of the 100 rows held are in s9, which the project leaves out:
    // the computer's total of 140 does not count them.
    const todo = Array.from({ length: 100 }, (_, i) => row(String(i + 10), `k${String(i).padStart(3, '0')}`, i < 10 ? 's9' : 's1'));
    install({ view: kanban({ rows: todo, total: 140, next: true }, { rows: [] }, { groups: 'g1', left_out: ['s9'] } as Partial<BoardView>) });
    setBindingMock('TrackerSectionRows', () => new Promise<SectionPage>(() => {}));
    const view = mount();
    await waitFor(() => expect(columns(view)[0]?.count).toBe('140 · 280 pts'));
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'issue');
    await waitFor(() => expect(columns(view)[0].count).toBe('90 of 140 loaded'));
    view.unmount();
  });

  it('counts the matching cards that count once a filtered column is whole', async () => {
    // 100 rows held, 10 of them in s9, which the project leaves out; the
    // page brings the column's last 50.
    const todo = Array.from({ length: 100 }, (_, i) => row(String(i + 10), `k${String(i).padStart(3, '0')}`, i < 10 ? 's9' : 's1'));
    install({ view: kanban({ rows: todo, total: 140, next: true }, { rows: [] }, { groups: 'g1', left_out: ['s9'] } as Partial<BoardView>) });
    let answer: (page: SectionPage) => void = () => {};
    setBindingMock('TrackerSectionRows', () => new Promise<SectionPage>((resolve) => { answer = resolve; }));
    const view = mount();
    await waitFor(() => expect(columns(view)[0]?.count).toBe('140 · 280 pts'));
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'issue');
    await waitFor(() => expect(columns(view)[0].count).toBe('90 of 140 loaded'));
    const rest = Array.from({ length: 50 }, (_, i) => row(String(i + 200), `m${String(i).padStart(3, '0')}`, 's1'));
    answer({ section: { kind: 'column', column: 0 }, rows: rest } as unknown as SectionPage);
    await waitFor(() => expect(columns(view)[0].cards).toHaveLength(150));
    expect(columns(view)[0].count).toBe('140');
    view.unmount();
  });

  it('counts the matching cards that count on a filtered sprint board', async () => {
    install({ view: sprintBoard(fourOfThree(), ['s2']) });
    const view = mount();
    await waitFor(() => expect(columns(view)[0]?.count).toBe('3 · 9 pts'));
    openPaneFind(PANE);
    setPaneFindQuery(PANE, 'issue');
    await waitFor(() => expect(columns(view)[0].count).toBe('3'));
    expect(columns(view)[0].cards).toEqual(['1', '2', '3', '4']);
    view.unmount();
  });

  it('counts the loaded cards that count while a sprint has more to load', async () => {
    install({ view: sprintBoard(fourOfThree(), ['s2'], true) });
    setBindingMock('TrackerSectionRows', () => new Promise<SectionPage>(() => {}));
    const view = mount();
    await waitFor(() => expect(columns(view)[0]?.cards).toHaveLength(4));
    expect(columns(view)[0].count).toBe('3');
    view.unmount();
  });

  it("counts on the compact strip as the column's header does", async () => {
    setCompactLayoutForTest(true);
    showCompactThread();
    noteCompactPaneShown(PANE);
    try {
      install({ view: sprintBoard(fourOfThree(), ['s2']) });
      const view = mount();
      const tab = () => view.getAllByTestId('issues-column-tab').map((t) => t.textContent?.replace(/\s+/g, ' ').trim());
      await waitFor(() => expect(tab()).toEqual(['To Do 3 · 9 pts']));
      openPaneFind(PANE);
      setPaneFindQuery(PANE, 'issue');
      await waitFor(() => expect(tab()).toEqual(['To Do 3']));
      expect(view.getAllByTestId('issues-card')).toHaveLength(4);
      view.unmount();
    } finally {
      setCompactLayoutForTest(false);
    }
  });
});

describe('which board the pane holds', () => {
  it('does not open on its own a board whose connection is turned off', async () => {
    const mocks = install({ choices: [choice('1', { opens_first: true, connection_enabled: false }), choice('2')] });
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => c[2])).toEqual(['2']);
    view.unmount();
  });

  it('lets the board picked last go when its connection is turned off, with no retry', async () => {
    writeFromAnotherWindow(BOARD_KEY, boardRef('1'));
    const mocks = install({ choices: [choice('1', { connection_enabled: false }), choice('2')] });
    mocks.subscribe.mockImplementation(async (...args: unknown[]) => {
      const board = args[2] as string;
      if (board === '1') throw new TransportError('tracker_connection_off', 'this connection is turned off');
      return { id: `sub-${board}`, seq: 10, loaded: true, view: kanban({ rows: [row('1', 'a', 's1')] }, { rows: [] }), poll_every: 10_000 };
    });
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => c[2])).toEqual(['1', '2']);
    view.unmount();
  });

  it('subscribes an ended board again once its connection is turned back on, and on a pick of it', async () => {
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, ended: true }, HOME_BACKEND);
    await view.findByTestId('issues-board-ended');
    // The answer the pane holds was read before the end: it renews nothing.
    await tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    // The connection is turned back on: its computer says so.
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(view.queryByTestId('issues-board-ended')).toBeNull());
    await view.findAllByTestId('issues-card');

    // Ended again; the picker's list answers nothing new, and the person
    // picks the board shown.
    applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 12, ended: true }, HOME_BACKEND);
    await view.findByTestId('issues-board-ended');
    setBindingMock('TrackerBoardChoices', () => new Promise<BoardChoice[]>(() => {}));
    await fireEvent.click(view.getByTestId('issues-board-picker'));
    await fireEvent.click(within(menu()).getAllByTestId('issues-board-choice')[0]);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(view.queryByTestId('issues-board-ended')).toBeNull());
    view.unmount();
  });

  it('waits for the computer of the project it was opened from, and saves nothing it chose', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read']);
    const mocks = install();
    let laptop: (rows: BoardChoice[]) => void = () => {};
    setBindingMock('TrackerBoardChoices', () => {
      if (takePinnedBackend() === 'laptop') return new Promise<BoardChoice[]>((resolve) => { laptop = resolve; });
      return Promise.resolve([choice('1', { opens_first: true })]);
    });
    openedFrom.project = 'p9';
    try {
      const view = mount();
      await waitFor(() => expect(view.getByTestId('issues-empty')).toHaveTextContent('Listing boards…'));
      await tick();
      expect(mocks.subscribe).not.toHaveBeenCalled();
      laptop([choice('7', { project: 'p9', opens_first: true })]);
      await waitFor(() => expect(mocks.subscribe).toHaveBeenCalledWith('p9', 'c1', '7', [], true));
      expect(mocks.subscribe).toHaveBeenCalledTimes(1);
      expect(picked()).toBeNull();
      view.unmount();
    } finally {
      openedFrom.project = null;
      resetStagedBackends();
    }
  });

  it("opens an active project's board before an archived one's that its computer lists first", async () => {
    // Alpha is archived; its computer still lists it first, by name.
    const mocks = install({ choices: [choice('1', { project: 'alpha', project_archived: true }), choice('2', { project: 'zulu' })] });
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['zulu', '2']]);
    view.unmount();
  });

  it("opens the first project's board in the order its computer lists them, whatever the sidebar shows", async () => {
    // The sidebar shows widgets first; the computer lists by name.
    project('p1', 'widgets', 0, 5);
    project('p2', 'gadgets', 1, 1);
    const mocks = install({ choices: [choice('3', { project: 'p2' }), choice('4', { project: 'p2', opens_first: true }), choice('1')] });
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['p2', '4']]);
    view.unmount();
  });

  it('keeps its board and Only mine when another window of this client changes them', async () => {
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    writeFromAnotherWindow(BOARD_KEY, boardRef('2'));
    writeFromAnotherWindow('agent-overflow:frontend:issues-only-mine', true);
    await tick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(mocks.subscribe.mock.calls.map((c) => c[2])).toEqual(['1']);
    expect(view.getByTestId('issues-only-mine')).toHaveAttribute('aria-pressed', 'false');
    view.unmount();
  });

  it('says a computer it could not reach is not connected, and lists its boards when it connects', async () => {
    const mocks = install();
    setBindingMock('TrackerBoardChoices', async () => {
      throw new DisconnectedError('offline');
    });
    const view = mount();
    await view.findByTestId('issues-empty-offline');
    expect(view.getByTestId('issues-empty')).not.toHaveTextContent('No project lists a board');
    setBindingMock('TrackerBoardChoices', async () => [choice('1')]);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', [], true);
    view.unmount();
  });

  it('says only that the boards could not be listed when the first read fails', async () => {
    install();
    setBindingMock('TrackerBoardChoices', async () => {
      throw new TransportError('method_error', 'database is locked');
    });
    const view = mount();
    await view.findByTestId('issues-choices-error');
    expect(view.getByTestId('issues-empty')).not.toHaveTextContent('No project lists a board');
    view.unmount();
  });

  it("lists a computer's boards once its grant arrives", async () => {
    stageBackend();
    const mocks = install();
    setBindingMock('TrackerBoardChoices', () => Promise.resolve(takePinnedBackend() === 'laptop' ? [choice('7', { project: 'p9', opens_first: true })] : []));
    const view = mount();
    await waitFor(() => expect(view.getByTestId('issues-empty')).toHaveTextContent('Listing boards…'));
    setCarriedSessionScopes('laptop', ['tracker:read']);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenCalledWith('p9', 'c1', '7', [], true));
    view.unmount();
    resetStagedBackends();
  });
});

describe('the selection against its computer, its listing and the sidebar', () => {
  it('lets a saved board on a forgotten computer go, and opens a board of this computer', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read']);
    writeFromAnotherWindow(BOARD_KEY, boardRef('7', 'p9', 'laptop'));
    detachBackend('laptop');
    const mocks = install({ choices: [choice('1', { opens_first: true })] });
    openedFrom.project = 'p1';
    try {
      const view = mount();
      await view.findAllByTestId('issues-card');
      expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['p1', '1']]);
      view.unmount();
    } finally {
      openedFrom.project = null;
    }
  });

  it('lets a saved board go when its computer refuses this window the boards', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['threads:read']);
    writeFromAnotherWindow(BOARD_KEY, boardRef('7', 'p9', 'laptop'));
    const mocks = install({ choices: [choice('1', { opens_first: true })] });
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['p1', '1']]);
    view.unmount();
  });

  it('keeps a shown board while its computer is not connected, and shows it again when it connects', async () => {
    const mocks = install();
    const view = mount();
    await view.findAllByTestId('issues-card');
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    expect(await view.findByTestId('issues-board-offline')).toHaveTextContent('is not connected. The board shows when it connects.');
    expect(view.queryByTestId('issues-opening')).toBeNull();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await view.findAllByTestId('issues-card');
    expect(view.queryByTestId('issues-board-offline')).toBeNull();
    expect(mocks.subscribe.mock.calls.map((c) => c[2])).toEqual(['1', '1']);
    view.unmount();
  });

  it('moves the board picked last to the project that still lists it, for the next pane too', async () => {
    const both = [choice('1'), choice('1', { project: 'p2' })];
    let rows = both;
    const mocks = install();
    setBindingMock('TrackerBoardChoices', async () => rows);
    writeFromAnotherWindow(BOARD_KEY, boardRef('1'));
    const view = mount();
    await view.findAllByTestId('issues-card');
    rows = [both[1]];
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await waitFor(() => expect(mocks.subscribe).toHaveBeenLastCalledWith('p2', 'c1', '1', [], true));
    expect(picked()).toEqual(boardRef('1', 'p2'));
    view.unmount();
  });

  it("says the saved board's computer is not connected while it is not", async () => {
    stageBackend({ status: 'reconnecting' });
    setCarriedSessionScopes('laptop', ['tracker:read']);
    writeFromAnotherWindow(BOARD_KEY, boardRef('7', 'p9', 'laptop'));
    install({ choices: [choice('1', { opens_first: true })] });
    const view = mount();
    const notice = await view.findByTestId('issues-board-offline');
    expect(notice).toHaveTextContent('Laptop is not connected. The board shows when it connects.');
    expect(view.queryByTestId('issues-opening')).toBeNull();
    view.unmount();
  });

  it('follows a board to the project that still lists it when its own project stops listing it, and keeps Only mine', async () => {
    const both = [choice('1', { opens_first: true }), choice('1', { project: 'p2', opens_first: true })];
    let rows = both;
    const mocks = install({
      view: kanban({ rows: [row('1', 'a', 's1', { assignee: { id: 'acc-user1', name: 'User 1' } }), row('3', 'c', 's1')] }, { rows: [] }),
    });
    setBindingMock('TrackerBoardChoices', async () => rows);
    openedFrom.project = 'p1';
    try {
      const view = mount();
      await waitFor(() => expect(view.getAllByTestId('issues-card')).toHaveLength(2));
      await fireEvent.click(view.getByTestId('issues-only-mine'));
      await waitFor(() => expect(view.getAllByTestId('issues-card').map((c) => c.dataset.issue)).toEqual(['1']));
      rows = [both[1]];
      applyTrackerConnectionsChanged(HOME_BACKEND);
      await waitFor(() => expect(mocks.subscribe).toHaveBeenLastCalledWith('p2', 'c1', '1', [], true));
      await waitFor(() => expect(view.getAllByTestId('issues-card').map((c) => c.dataset.issue)).toEqual(['1']));
      expect(view.queryByTestId('issues-only-mine-unknown')).toBeNull();
      view.unmount();
    } finally {
      openedFrom.project = null;
    }
  });

  it('shows the way to Settings once the first project is added under an open empty pane', async () => {
    install({ choices: [] });
    const view = mount();
    await waitFor(() => expect(view.getByTestId('issues-empty')).toHaveTextContent('No project lists a board yet.'));
    expect(view.queryByTestId('issues-empty-settings')).toBeNull();
    project('p1', 'widgets', 0);
    await fireEvent.click(await view.findByTestId('issues-empty-settings'));
    expect(requestedTrackerBoardsProject()).toBe('p1');
    view.unmount();
  });

  it('opens a board while the sidebar cannot list the computers, this computer first', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read']);
    const mocks = install();
    // The sidebar has listed no computer's projects (a read failed or was refused).
    resetSidebarCatalogsForTest();
    const lists: Record<string, BoardChoice[]> = {
      [HOME_BACKEND]: [choice('2', { project: 'p1', connection_enabled: false })],
      laptop: [choice('5', { project: 'p9' })],
    };
    setBindingMock('TrackerBoardChoices', async () => lists[takePinnedBackend() ?? HOME_BACKEND]);
    const view = mount();
    await view.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['p9', '5']]);
    view.unmount();

    lists[HOME_BACKEND] = [choice('2', { project: 'p1' })];
    mocks.subscribe.mockClear();
    const again = mount();
    await again.findAllByTestId('issues-card');
    expect(mocks.subscribe.mock.calls.map((c) => [c[0], c[2]])).toEqual([['p1', '2']]);
    again.unmount();
  });

  it('follows the project list for the way to Settings, by name', async () => {
    install({ choices: [] });
    project('p1', 'widgets', 0, 5);
    const view = mount();
    await fireEvent.click(await view.findByTestId('issues-empty-settings'));
    expect(requestedTrackerBoardsProject()).toBe('p1');
    project('p2', 'gadgets', 1, 1);
    await waitFor(async () => {
      await fireEvent.click(view.getByTestId('issues-empty-settings'));
      expect(requestedTrackerBoardsProject()).toBe('p2');
    });
    view.unmount();
  });

  it('offers the way to the connections of a computer that lets this window turn one on', async () => {
    await pairWithScopes(['threads:read', 'tracker:read']);
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate']);
    install({ choices: [choice('1', { connection_enabled: false })] });
    const view = mount();
    await view.findByTestId('issues-empty-off');
    await fireEvent.click(await view.findByTestId('issues-empty-connections'));
    expect(getSettingsSection()).toBe('tracker-connections');
    expect(getSettingsComputer()).toBe('laptop');
    view.unmount();
    resetToLocalPage();
  });

  it('offers no way to the connections to a window that may not turn one on', async () => {
    await pairWithScopes(['threads:read', 'tracker:read']);
    install({ choices: [choice('1', { connection_enabled: false })] });
    const view = mount();
    await view.findByTestId('issues-empty-off');
    expect(view.queryByTestId('issues-empty-connections')).toBeNull();
    view.unmount();
    resetToLocalPage();
  });
});

/** A ticket's view: issue `id` of the board's connection, its parent
 * `parent`, and `comments` comments, the newest first. */
function ticketDetail(id: string, opts: { parent?: string; comments?: number; more?: string; status?: string } = {}): DetailView {
  const comments: CommentView[] = Array.from({ length: opts.comments ?? 0 }, (_, i) => ({
    id: `c${opts.comments! - i}`, author: { id: 'u1', name: 'User 1' }, body: `Comment ${opts.comments! - i}`, created: 1_000 + opts.comments! - i,
  }) as CommentView);
  return {
    card: {
      id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: opts.status ?? 's1', name: opts.status === 's2' ? 'In Progress' : 'To Do', group: 'todo' },
      created: 0, updated: 0, ...(opts.parent ? { parent: { id: opts.parent, key: `WEB-${opts.parent}`, title: `Issue ${opts.parent}` } } : {}),
    },
    url: `https://example.atlassian.net/browse/WEB-${id}`, description: 'The description', fields: [], comments, more_comments: opts.more,
    links: [], attachments: [], history: [], rows: [{ board: '1', estimate: 2, unit: 'points' }],
    moves: [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress', group: 'in_progress' } }],
  } as unknown as DetailView;
}

/** The tracker a ticket's subscription names: its connection's kind and
 * the person's account there. */
interface TicketTracker {
  kind: 'jira' | 'linear';
  account: string;
}

const JIRA_USER1: TicketTracker = { kind: 'jira', account: 'acc-user1' };

function installTicket(detailOf: (issue: string) => DetailView = (issue) => ticketDetail(issue), tracker: TicketTracker = JIRA_USER1) {
  let n = 0;
  const subscribe = setBindingMock('SubscribeTrackerTicket', async (_c: string, issue: string): Promise<TicketSubscription> => {
    n += 1;
    return { id: `ticket-${issue}-${n}`, seq: 10, poll_every: 10_000, ...tracker, loaded: true, detail: detailOf(issue), read_at: Date.now() } as TicketSubscription;
  });
  const read = setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: ticketDetail('1', { status: 's2' }), account: tracker.account }));
  return { subscribe, read };
}

/** The votes sent for the handles whose IDs start with `prefix`. */
function votes(mock: ReturnType<typeof vi.fn>, prefix: string): boolean[] {
  return mock.mock.calls.filter((c) => String(c[0]).startsWith(prefix)).map((c) => c[1] as boolean);
}

async function openCard(view: ReturnType<typeof mount>, issue: string): Promise<HTMLElement> {
  await view.findAllByTestId('issues-card');
  const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === issue)!;
  await fireEvent.click(within(card).getByTestId('issues-card-open'));
  return waitFor(() => {
    const screen = view.getByTestId('ticket-screen');
    expect(screen.dataset.issue).toBe(issue);
    return screen;
  });
}

describe('the ticket in the pane', () => {
  // The ticket's chunk loads on the first open; load it before the first
  // test waits on it.
  beforeAll(async () => {
    await import('./TicketScreen.svelte');
  });

  it('opens a card\'s ticket over the board, votes the board hidden under it, and shown again when it closes', async () => {
    const mocks = install();
    const ticket = installTicket();
    const view = mount();
    await openCard(view, '1');
    expect(ticket.subscribe).toHaveBeenCalledWith('c1', '1', true);
    expect(view.getByTestId('issues-ticket')).toHaveAttribute('data-placement', 'over');
    await waitFor(() => expect(view.getByTestId('ticket-title')).toHaveTextContent('Issue 1'));
    // The board stays mounted under the ticket, its card marked.
    expect(view.getAllByTestId('issues-card').find((c) => c.dataset.issue === '1')).toHaveAttribute('data-open');
    await waitFor(() => expect(votes(mocks.visible, 'sub-')).toEqual([false]));
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(votes(mocks.visible, 'sub-')).toEqual([false, true]));
    expect(view.queryByTestId('ticket-screen')).toBeNull();
    await waitFor(() => expect(mocks.release).toHaveBeenCalledWith('ticket-1-1'));
  });

  describe('the project a ticket\'s Ask and Start work use', () => {
    function work(view: ReturnType<typeof mount>) {
      return waitFor(() => {
        const found = within(view.getByTestId('ticket-screen')).getByTestId('ticket-work');
        expect((within(found).getByTestId('ticket-work-ask') as HTMLButtonElement).disabled).toBe(false);
        return found;
      });
    }

    it('is the project the board was opened under when two projects list the board, and the person can pick another', async () => {
      project('p1', 'alpha', 1);
      project('p2', 'beta', 2);
      setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work', enabled: true, account: 'User 1' }]);
      install({ choices: [choice('1', { project: 'p1' }), choice('1', { project: 'p2', opens_first: true })] });
      installTicket();
      const start = setBindingMock('StartTicketThread', async () => { throw new Error('stop here'); });
      openedFrom.project = 'p2';
      try {
        const view = mount();
        await openCard(view, '1');
        const section = await work(view);
        await fireEvent.click(within(section).getByTestId('ticket-work-ask'));
        await waitFor(() => expect(start).toHaveBeenCalledWith('p2', expect.objectContaining({ origin: 'ask' })));
        expect((within(section).getByTestId('ticket-work-project') as HTMLSelectElement).value).toBe('p2');

        await fireEvent.change(within(section).getByTestId('ticket-work-project'), { target: { value: 'p1' } });
        await waitFor(() => expect((within(section).getByTestId('ticket-work-project') as HTMLSelectElement).value).toBe('p1'));
        await fireEvent.click(within(section).getByTestId('ticket-work-ask'));
        await waitFor(() => expect(start).toHaveBeenLastCalledWith('p1', expect.objectContaining({ origin: 'ask' })));
        view.unmount();
      } finally {
        openedFrom.project = null;
      }
    });

    it('is the project the opener named for a ticket asked for from outside, shown before anything is made', async () => {
      project('p1', 'alpha', 1);
      project('p2', 'beta', 2);
      setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work', enabled: true, account: 'User 1' }]);
      install({ choices: [choice('1', { project: 'p1', opens_first: true })] });
      installTicket();
      const start = setBindingMock('StartTicketThread', async () => { throw new Error('stop here'); });
      const view = mount();
      await view.findAllByTestId('issues-card');
      requestTicketOpen({ ref: { backend: HOME_BACKEND, connection: 'c1', issue: '77' }, seed: { key: 'WEB-77', title: 'Linked', card: null }, project: 'p2', unplaced: vi.fn() });
      const section = await work(view);
      expect((within(section).getByTestId('ticket-work-project') as HTMLSelectElement).value).toBe('p2');
      await fireEvent.click(within(section).getByTestId('ticket-work-ask'));
      await waitFor(() => expect(start).toHaveBeenCalledWith('p2', expect.anything()));
      view.unmount();
    });

    /** Two projects list board 1; the pane shows it under p1. */
    function twoProjects() {
      project('p1', 'alpha', 1);
      project('p2', 'beta', 2);
      setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work', enabled: true, account: 'User 1' }]);
      install({ choices: [choice('1', { project: 'p1', opens_first: true }), choice('1', { project: 'p2' })] });
      const ticket = installTicket();
      const start = setBindingMock('StartTicketThread', async () => { throw new Error('stop here'); });
      return { ticket, start };
    }

    function linkedFrom(projectId: string) {
      return makeThread({
        id: `thread-${projectId}`, projectId,
        ticketLink: {
          kind: 'jira', site: 'https://example.atlassian.net', issueId: '1', key: 'WEB-1', title: 'Issue 1', statusId: 's1', statusName: 'To Do',
          statusGroup: 'todo', origin: 'manual', linkedAt: 1, state: 'current', connectionId: 'c1',
        },
      });
    }

    const chosen = (section: HTMLElement) => (within(section).getByTestId('ticket-work-project') as HTMLSelectElement).value;

    it('is the project of a pull request whose chip opens the ticket already open, which stays open', async () => {
      const { ticket, start } = twoProjects();
      const view = mount();
      await openCard(view, '1');
      const section = await work(view);
      expect(chosen(section)).toBe('p1');

      await openNamedTicket(HOME_BACKEND, { key: 'WEB-1', connectionId: 'c1', issueId: '1', title: 'Issue 1' }, 'p2');
      await waitFor(() => expect(chosen(section)).toBe('p2'));
      await fireEvent.click(within(section).getByTestId('ticket-work-ask'));
      await waitFor(() => expect(start).toHaveBeenCalledWith('p2', expect.objectContaining({ origin: 'ask' })));
      // The open is the same: not subscribed again, its Work not mounted again.
      expect(ticket.subscribe).toHaveBeenCalledTimes(1);
      expect(within(view.getByTestId('ticket-screen')).getByTestId('ticket-work')).toBe(section);

      // A chip of a pull request with no project names none: p2 stays.
      await openNamedTicket(HOME_BACKEND, { key: 'WEB-1', connectionId: 'c1', issueId: '1', title: 'Issue 1' }, null);
      await tick();
      expect(chosen(section)).toBe('p2');
      view.unmount();
    });

    it('is the project of a thread whose row opens the ticket already open, unless the person picked one', async () => {
      const { ticket, start } = twoProjects();
      const view = mount();
      await view.findAllByTestId('issues-card');
      await openLinkedTicket(linkedFrom('p1'));
      const section = await work(view);
      expect(chosen(section)).toBe('p1');

      await openLinkedTicket(linkedFrom('p2'));
      await waitFor(() => expect(chosen(section)).toBe('p2'));
      expect(ticket.subscribe).toHaveBeenCalledTimes(1);

      // The person's pick holds over a later opener.
      await fireEvent.change(within(section).getByTestId('ticket-work-project'), { target: { value: 'p1' } });
      await waitFor(() => expect(chosen(section)).toBe('p1'));
      await openLinkedTicket(linkedFrom('p2'));
      await tick();
      expect(chosen(section)).toBe('p1');
      await fireEvent.click(within(section).getByTestId('ticket-work-ask'));
      await waitFor(() => expect(start).toHaveBeenCalledWith('p1', expect.objectContaining({ origin: 'ask' })));
      expect(ticket.subscribe).toHaveBeenCalledTimes(1);
      view.unmount();
    });
  });

  describe('a ticket asked for from outside the pane', () => {
    function ask(connection: string, issue: string, project: string | null, unplaced = vi.fn()) {
      requestTicketOpen({ ref: { backend: HOME_BACKEND, connection, issue }, seed: { key: `WEB-${issue}`, title: 'Linked', card: null }, project, unplaced });
      return unplaced;
    }

    it('opens over the board shown when that board is of its connection, and is taken once', async () => {
      const mocks = install();
      const ticket = installTicket();
      const view = mount();
      await view.findAllByTestId('issues-card');
      ask('c1', '77', 'p1');
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('77'));
      expect(ticket.subscribe).toHaveBeenCalledWith('c1', '77', true);
      expect(mocks.subscribe).toHaveBeenCalledTimes(1);
      expect(takeTicketOpenRequest()).toBeNull();
    });

    it("switches to its connection's board, the asking project's first, without saving it as a pick", async () => {
      const mocks = install({ choices: [choice('1', { opens_first: true }), choice('5', { connection: 'c2', project: 'p2' }), choice('6', { connection: 'c2', project: 'p3' })] });
      const ticket = installTicket();
      const view = mount();
      await view.findAllByTestId('issues-card');
      ask('c2', '77', 'p3');
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('77'));
      expect(mocks.subscribe).toHaveBeenLastCalledWith('p3', 'c2', '6', [], false);
      expect(ticket.subscribe).toHaveBeenCalledWith('c2', '77', true);
      expect(picked()).toBeNull();
    });

    it('is taken by a pane that mounts after it was asked', async () => {
      install();
      installTicket();
      ask('c1', '77', null);
      const view = mount();
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('77'));
    });

    it('says so when no board of its connection is listed, and opens nothing', async () => {
      install();
      installTicket();
      const view = mount();
      await view.findAllByTestId('issues-card');
      const unplaced = ask('c9', '77', 'p1');
      await waitFor(() => expect(unplaced).toHaveBeenCalledWith(null));
      expect(view.queryByTestId('ticket-screen')).toBeNull();
    });
  });

  it('moves a card on the board under the ticket when a status change made in the ticket lands', async () => {
    install();
    installTicket();
    const patched = setBindingMock('TrackerBoardPatch', async (): Promise<BoardPatch> => ({
      lifetime: 1, revision: 2,
      // The changed card, now in the second column.
      sections: [{ section: { kind: 'column', column: 0 }, rows: [] }, { section: { kind: 'column', column: 1 }, rows: [row('1', 'a', 's2')] }],
    } as unknown as BoardPatch));
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => {
      // The write's refresh tells the board and the ticket before it answers.
      applyTrackerBoardEvent({ scope: { connection: 'c1', board: '1' }, seq: 11, changed: ['1'] }, HOME_BACKEND);
      trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 11, changed: true }, HOME_BACKEND);
      return { parts: [{ step: 'move', state: 'applied' }] } as WriteView;
    });
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    const item = await waitFor(() => within(menu()).getByTestId('ticket-picker-item'));
    expect(item).toHaveTextContent('In Progress');
    await fireEvent.click(item);
    await waitFor(() => expect(move).toHaveBeenCalledWith('c1', { issue: '1', move: '21', fields: undefined, accept_loss: false }));
    await waitFor(() => expect(patched).toHaveBeenCalled());
    await waitFor(() => expect(columns(view).map((c) => c.cards)).toEqual([[], ['1', '2']]));
    await waitFor(() => expect(within(status).getByTestId('ticket-field-value')).toHaveTextContent('In Progress'));
    expect(within(status).queryByTestId('ticket-write-pending')).toBeNull();
  });

  it('shows under a Start work row only the write its run sent, and a later move of the Status field under that field', async () => {
    project('p1', 'alpha', 1);
    setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work', enabled: true, account: 'User 1' }]);
    install();
    const done = { id: '22', name: 'Finish', to: { id: 's3', name: 'Done', group: 'done' } };
    const toDo = (issue: string) => {
      const detail = ticketDetail(issue);
      return { ...detail, moves: [...detail.moves, done] } as DetailView;
    };
    installTicket(toDo);
    // Every read finds the ticket still in To Do.
    setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: toDo('1'), account: JIRA_USER1.account }));
    setBindingMock('ProjectTrackerSetup', async () => ({
      revision: 'r1',
      setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: 'https://example.atlassian.net', project: 'WEB', status: 's2', assign: false, branch: 'WEB-1-retry' }] },
    }));
    setBindingMock('StartTicketThread', async () => makeThread({ id: 't-made', projectId: 'p1', title: 'WEB-1 Issue 1' }));
    setBindingMock('RefreshTrackerSubscription', async () => {
      trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 11, changed: true }, HOME_BACKEND);
    });
    // The run's move is refused; the Status field's move waits, then is
    // refused for what it would lose.
    let answerManual: (view: WriteView) => void = () => {};
    const move = setBindingMock('MoveTrackerIssue', async (_c: string, input: { move: string }): Promise<WriteView> => {
      if (input.move === '21') return { parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'A resolution is required.' } }] } as WriteView;
      return new Promise<WriteView>((resolve) => { answerManual = resolve; });
    });
    const view = mount();
    const screen = await openCard(view, '1');
    const section = within(screen).getByTestId('ticket-work');
    const start = within(section).getByTestId('ticket-work-start') as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
    await fireEvent.click(start);
    const go = await waitFor(() => {
      const found = document.querySelector<HTMLButtonElement>('[data-testid="start-work-go"]');
      expect(found?.disabled).toBe(false);
      return found!;
    });
    await fireEvent.click(go);
    const moveRow = () => within(section).getAllByTestId('start-work-step').find((s) => s.dataset.step === 'move')!;
    await waitFor(() => expect(moveRow().dataset.outcome).toBe('refused'));
    // Its own refusal shows under it while it is the status's latest write.
    expect(within(moveRow()).getByTestId('ticket-write-outcome')).toHaveTextContent('A resolution is required.');

    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    const item = await waitFor(() => within(menu()).getAllByTestId('ticket-picker-item').find((i) => i.textContent?.includes('Done'))!);
    await fireEvent.click(item);
    await waitFor(() => expect(move).toHaveBeenLastCalledWith('c1', expect.objectContaining({ move: '22' })));
    await waitFor(() => expect(within(status).getByTestId('ticket-write-pending')).toBeInTheDocument());
    expect(within(moveRow()).queryByTestId('ticket-write-pending')).toBeNull();
    expect(within(moveRow()).queryByTestId('ticket-write-outcome')).toBeNull();
    expect(moveRow()).toHaveTextContent('Move the ticket to In Progress');
    expect(moveRow()).toHaveTextContent('Refused.');

    answerManual({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'loss', message: 'The description would lose its table.' } }] } as WriteView);
    await waitFor(() => expect(within(status).getByTestId('ticket-write-save-anyway')).toBeInTheDocument());
    expect(within(status).getByTestId('ticket-write-outcome')).toHaveTextContent('The description would lose its table.');
    expect(within(moveRow()).queryByTestId('ticket-write-outcome')).toBeNull();
    expect(within(moveRow()).queryByTestId('ticket-write-save-anyway')).toBeNull();
    expect(moveRow()).not.toHaveTextContent('The description would lose its table.');
    expect(moveRow()).toHaveTextContent('Refused.');
    view.unmount();
  });

  it('shows the tracker\'s reason under a change it refused, and keeps the value as it was', async () => {
    install();
    installTicket();
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '1', name: 'Highest' }, { id: '3', name: 'Low' }] }));
    const edit = setBindingMock('EditTrackerIssue', async (): Promise<WriteView> => ({
      parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'tracker', message: 'Priority is not on the edit screen of this issue' } }],
    } as WriteView));
    const view = mount();
    const screen = await openCard(view, '1');
    const priority = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    await fireEvent.click(within(priority).getByTestId('ticket-field-value'));
    const items = await waitFor(() => {
      const found = within(menu()).getAllByTestId('ticket-picker-item');
      expect(found).toHaveLength(2);
      return found;
    });
    await fireEvent.click(items[1]);
    await waitFor(() => expect(edit).toHaveBeenCalledWith('c1', { issue: '1', fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '3' }] } }], accept_loss: false }));
    await waitFor(() => expect(within(priority).getByTestId('ticket-write-outcome')).toHaveTextContent('Priority is not on the edit screen of this issue'));
    expect(within(priority).getByTestId('ticket-write-outcome')).toHaveAttribute('data-kind', 'refused');
    expect(within(priority).getByTestId('ticket-field-value')).toHaveTextContent('None');
  });

  it('sets and clears the points typed in their field', async () => {
    install();
    installTicket();
    const estimate = setBindingMock('SetTrackerEstimate', async (): Promise<WriteView> => ({ parts: [{ step: 'estimate', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const points = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'points')!;
    const type = async (value: string) => {
      await fireEvent.click(within(points).getByTestId('ticket-field-value'));
      const input = within(points).getByTestId('ticket-field-input');
      expect(input).toHaveValue(2);
      await fireEvent.input(input, { target: { value } });
      await fireEvent.keyDown(input, { key: 'Enter' });
    };
    await type('8');
    await waitFor(() => expect(estimate).toHaveBeenCalledWith('c1', { board: '1', issue: '1', value: 8 }));
    await waitFor(() => expect(within(points).getByTestId('ticket-field-value')).toBeEnabled());
    await type('');
    await waitFor(() => expect(estimate).toHaveBeenLastCalledWith('c1', { board: '1', issue: '1', value: undefined }));
    expect(estimate).toHaveBeenCalledTimes(2);
  });

  it('shows a field of several typed values without an editor, and edits a single typed value in place', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      fields: [
        { id: 'tags', name: 'Tags', kind: 'text', list: true, editable: true, required: false, value: { kind: 'text', texts: ['A', 'B'] } },
        { id: 'due', name: 'Due date', kind: 'date', list: false, editable: true, required: false, value: { kind: 'date', dates: ['2026-01-02'] } },
      ],
    } as unknown as DetailView));
    const edit = setBindingMock('EditTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'edit', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(within(screen).getByTestId('ticket-more-fields'));
    const fieldOf = (id: string) => within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === id)!;
    const tags = within(fieldOf('tags')).getByTestId('ticket-field-value');
    expect(tags).toHaveTextContent('A, B');
    expect(tags.tagName).toBe('SPAN');
    await fireEvent.click(tags);
    expect(within(fieldOf('tags')).queryByTestId('ticket-field-input')).toBeNull();
    await fireEvent.click(within(fieldOf('due')).getByTestId('ticket-field-value'));
    const due = within(fieldOf('due')).getByTestId('ticket-field-input');
    await fireEvent.input(due, { target: { value: '2026-02-03' } });
    await fireEvent.keyDown(due, { key: 'Enter' });
    await waitFor(() => expect(edit).toHaveBeenCalledWith('c1', { issue: '1', fields: [{ field: 'due', typed: ['2026-02-03'] }], accept_loss: false }));
    expect(edit).toHaveBeenCalledTimes(1);
  });

  it('opens the ticket in the tracker in this browser when the page is not on the computer', async () => {
    install();
    installTicket();
    const host = setBindingMock('OpenExternalURL', async () => undefined);
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const page = window.location.href;
    // A connected browser: the page is served to another machine.
    (window as unknown as { happyDOM: { setURL(url: string): void } }).happyDOM.setURL('http://192.168.1.20:8123/');
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await fireEvent.click(await within(screen).findByTestId('ticket-open-tracker'));
      await fireEvent.contextMenu(within(screen).getByTestId('ticket-header'));
      await fireEvent.click(await within(document.body).findByTestId('ticket-menu-open-tracker'));
      await waitFor(() => expect(open).toHaveBeenCalledTimes(2));
      expect(open.mock.calls.map((c) => c[0])).toEqual(['https://example.atlassian.net/browse/WEB-1', 'https://example.atlassian.net/browse/WEB-1']);
      expect(host).not.toHaveBeenCalled();
    } finally {
      (window as unknown as { happyDOM: { setURL(url: string): void } }).happyDOM.setURL(page);
      open.mockRestore();
    }
  });

  it('asks the tracker for typed text when its list holds more matches, once per pause, showing only the answer for the latest text', async () => {
    install();
    installTicket();
    const answers = new Map<string, (list: { choices: { id: string; name: string }[]; more?: boolean }) => void>();
    const choices = setBindingMock('TrackerChoices', (_c: string, _s: unknown, _f: string, text: string) => new Promise((resolve) => {
      if (text === '') {
        resolve({ choices: Array.from({ length: 50 }, (_, i) => ({ id: `o${i}`, name: `Option ${i}` })), more: true });
        return;
      }
      answers.set(text, resolve);
    }));
    const view = mount();
    const screen = await openCard(view, '1');
    const priority = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    await fireEvent.click(within(priority).getByTestId('ticket-field-value'));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item')).toHaveLength(50));
    const find = within(document.body).getByTestId('ticket-picker-find');
    for (const text of ['R', 'Re']) await fireEvent.input(find, { target: { value: text } });
    await waitFor(() => expect(answers.has('Re')).toBe(true));
    expect(choices.mock.calls.map((c) => c[3])).toEqual(['', 'Re']);
    // The person types on; the answer for "Re" lands after the one for
    // "Rel" and is not shown.
    await fireEvent.input(find, { target: { value: 'Rel' } });
    await waitFor(() => expect(answers.has('Rel')).toBe(true));
    const items = () => within(menu()).queryAllByTestId('ticket-picker-item').map((i) => i.textContent?.trim());
    answers.get('Rel')!({ choices: [{ id: 'r1', name: 'Release 1' }, { id: 'r9', name: 'Release 9' }] });
    await waitFor(() => expect(items()).toEqual(['Release 1', 'Release 9']));
    answers.get('Re')!({ choices: [{ id: 'r1', name: 'Release 1' }, { id: 'r2', name: 'Reopened' }] });
    await new Promise((r) => setTimeout(r, 50));
    expect(items()).toEqual(['Release 1', 'Release 9']);
    expect(choices.mock.calls.map((c) => c[3])).toEqual(['', 'Re', 'Rel']);
  });

  it('filters a whole list of values as the person types, asking nothing more', async () => {
    install();
    installTicket();
    const choices = setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '1', name: 'Highest' }, { id: '3', name: 'Low' }, { id: '4', name: 'Lowest' }] }));
    const view = mount();
    const screen = await openCard(view, '1');
    const priority = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    await fireEvent.click(within(priority).getByTestId('ticket-field-value'));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item')).toHaveLength(3));
    await fireEvent.input(within(document.body).getByTestId('ticket-picker-find'), { target: { value: 'low' } });
    await new Promise((r) => setTimeout(r, 400));
    expect(within(menu()).getAllByTestId('ticket-picker-item').map((i) => i.textContent?.trim())).toEqual(['Low', 'Lowest']);
    expect(choices).toHaveBeenCalledTimes(1);
  });

  it('says a ticket that could not be read once, and nothing about reading it', async () => {
    install();
    setBindingMock('SubscribeTrackerTicket', async (): Promise<TicketSubscription> => ({
      id: 'ticket-1-1', seq: 10, poll_every: 10_000, loaded: false, read_at: 0,
      failure: { kind: 'auth', message: 'Jira refused the credential.', at: 1 },
    }) as unknown as TicketSubscription);
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(within(screen).getByTestId('ticket-content')).toHaveTextContent('Jira refused the credential.'));
    expect(screen.textContent?.split('Jira refused the credential.').length).toBe(2);
    expect(within(screen).queryByTestId('ticket-opening')).toBeNull();
  });

  it('says a ticket whose read of this computer\'s copy failed, and nothing about reading it', async () => {
    install();
    setBindingMock('SubscribeTrackerTicket', async (): Promise<TicketSubscription> => ({ id: 'ticket-1-1', seq: 10, poll_every: 10_000, loaded: false, read_at: 0 }) as unknown as TicketSubscription);
    setBindingMock('TrackerTicket', async () => { throw new TransportError('internal', 'the copy could not be read'); });
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(within(screen).getByTestId('ticket-opening')).toBeInTheDocument());
    trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 11, changed: true }, HOME_BACKEND);
    await waitFor(() => expect(within(screen).getByTestId('ticket-read-error')).toHaveTextContent('The copy could not be read.'));
    expect(within(screen).queryByTestId('ticket-opening')).toBeNull();
    expect(within(screen).queryByTestId('ticket-read-error-dismiss')).toBeNull();
  });

  it('says a ticket could not be opened, and nothing about opening it', async () => {
    install();
    setBindingMock('SubscribeTrackerTicket', async () => { throw new TransportError('internal', 'the computer could not open it'); });
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(within(screen).getByTestId('ticket-subscribe-error')).toHaveTextContent('Could not open this ticket'));
    expect(within(screen).queryByTestId('ticket-opening')).toBeNull();
  });

  it('says once that a ticket\'s connection was turned off', async () => {
    install();
    setBindingMock('SubscribeTrackerTicket', async () => { throw new TransportError('tracker_connection_off', 'this tracker connection is turned off'); });
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(within(screen).getByTestId('ticket-ended-empty')).toBeInTheDocument());
    expect(within(screen).queryByTestId('ticket-ended')).toBeNull();
  });

  it('says once that the computer is not connected while its ticket covers the board', async () => {
    install();
    installTicket();
    const view = mount();
    await openCard(view, '1');
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    await waitFor(() => expect(view.getByTestId('ticket-offline')).toBeInTheDocument());
    expect(view.queryByTestId('issues-board-offline')).toBeNull();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await waitFor(() => expect(view.queryByTestId('ticket-offline')).toBeNull());
    await fireEvent.click(view.getByTestId('ticket-close'));
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    expect(await view.findByTestId('issues-board-offline')).toBeInTheDocument();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
  });

  it('changes the type from the mark in the ticket\'s header', async () => {
    install();
    installTicket();
    const choices = setBindingMock('TrackerChoices', async () => ({ choices: [{ id: 't1', name: 'Task' }, { id: 't2', name: 'Bug' }] }));
    const edit = setBindingMock('EditTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'edit', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const mark = within(screen).getByTestId('ticket-type-mark');
    expect(mark.tagName).toBe('BUTTON');
    await fireEvent.click(mark);
    const items = await waitFor(() => {
      const found = within(menu()).getAllByTestId('ticket-picker-item');
      expect(found).toHaveLength(2);
      return found;
    });
    expect(choices).toHaveBeenCalledWith('c1', { issue: '1' }, 'type', '');
    await fireEvent.click(items[1]);
    await waitFor(() => expect(edit).toHaveBeenCalledWith('c1', { issue: '1', fields: [{ field: 'type', value: { kind: 'option', choices: [{ id: 't2' }] } }], accept_loss: false }));
  });

  it('steps back or closes the whole trail from the ticket\'s menu', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { parent: String(Number(issue) + 1) }));
    const view = mount();
    await openCard(view, '1');
    for (const next of ['2', '3']) {
      await fireEvent.click(await view.findByTestId('ticket-parent'));
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe(next));
    }
    const openMenu = async () => {
      await fireEvent.contextMenu(view.getByTestId('ticket-header'));
      return within(document.body);
    };
    await fireEvent.click(await (await openMenu()).findByTestId('ticket-menu-back'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('2'));
    const items = await openMenu();
    expect(await items.findByTestId('ticket-menu-back')).toBeInTheDocument();
    await fireEvent.click(items.getByTestId('ticket-menu-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    // With no trail the menu closes and has no Back.
    await openCard(view, '1');
    const first = await openMenu();
    await first.findByTestId('ticket-menu-close');
    expect(first.queryByTestId('ticket-menu-back')).toBeNull();
  });

  it('fills a move\'s own fields from that move\'s values, one read per picker, and sends the move once with them', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      fields: [{ id: 'environment', name: 'Environment', kind: 'rich_text', editable: true, value: { kind: 'rich_text', texts: ['Old notes'] } }],
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [
          { id: 'resolution', name: 'Resolution', kind: 'option', required: true, editable: true, value: {} },
          { id: 'versions', name: 'Fix versions', kind: 'option', list: true, editable: true, value: {} },
          { id: 'environment', name: 'Environment', kind: 'rich_text', editable: true, value: {} },
        ],
      }],
    }) as unknown as DetailView);
    const choices = setBindingMock('TrackerChoices', async (_c: string, _scope: unknown, field: string) => (field === 'resolution'
      ? { choices: [{ id: '1', name: 'Fixed' }, { id: '2', name: 'Won\'t do' }] }
      : { choices: [{ id: 'v1', name: '1.0' }, { id: 'v2', name: '1.1' }, { id: 'v3', name: '2.0' }] }));
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'move', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    const form = await within(screen).findByTestId('ticket-move-form');
    const formField = (id: string) => within(form).getAllByTestId('ticket-field').find((f) => f.dataset.field === id)!;
    // Resolution: the move's values, filtered here as the person types.
    await fireEvent.click(within(formField('resolution')).getByTestId('ticket-field-value'));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item')).toHaveLength(2));
    await fireEvent.input(within(document.body).getByTestId('ticket-picker-find'), { target: { value: 'fix' } });
    await new Promise((r) => setTimeout(r, 400));
    expect(within(menu()).getAllByTestId('ticket-picker-item').map((i) => i.textContent?.trim())).toEqual(['Fixed']);
    await fireEvent.click(within(menu()).getByTestId('ticket-picker-item'));
    expect(choices.mock.calls).toEqual([['c1', { issue: '1', move: '31' }, 'resolution', '']]);
    expect(within(formField('resolution')).getByTestId('ticket-field-value')).toHaveTextContent('Fixed');
    // Fix versions holds several: each pick adds one.
    await fireEvent.click(within(formField('versions')).getByTestId('ticket-field-value'));
    await waitFor(() => expect(within(menu()).getAllByTestId('ticket-picker-item')).toHaveLength(3));
    await fireEvent.click(within(menu()).getAllByTestId('ticket-picker-item')[0]);
    await fireEvent.click(within(menu()).getAllByTestId('ticket-picker-item')[2]);
    await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
    expect(within(formField('versions')).getByTestId('ticket-field-value')).toHaveTextContent('1.0, 2.0');
    expect(choices).toHaveBeenCalledTimes(2);
    // Environment is written as Markdown, from the text the ticket holds.
    const environment = within(form).getByTestId('ticket-move-text');
    expect(environment).toHaveValue('Old notes');
    await fireEvent.input(environment, { target: { value: 'New notes' } });
    await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
    await waitFor(() => expect(move).toHaveBeenCalledWith('c1', {
      issue: '1', move: '31', accept_loss: false, fields: [
        { field: 'resolution', value: { kind: 'option', choices: [{ id: '1' }] } },
        { field: 'versions', value: { kind: 'option', choices: [{ id: 'v1' }, { id: 'v3' }] } },
        { field: 'environment', markdown: { markdown: 'New notes', from: 'Old notes' } },
      ],
    }));
    await waitFor(() => expect(within(screen).queryByTestId('ticket-move-form')).toBeNull());
    expect(move).toHaveBeenCalledTimes(1);
  });

  it('keeps a move\'s form and what it holds when the tracker refuses the move', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      moves: [{ id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' }, fields: [{ id: 'resolution', name: 'Resolution', kind: 'option', required: true, editable: true, value: {} }] }],
    }) as unknown as DetailView);
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '1', name: 'Fixed' }] }));
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => ({
      parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'The workflow refused this move.' } }],
    }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    const form = await within(screen).findByTestId('ticket-move-form');
    const resolution = within(form).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'resolution')!;
    await fireEvent.click(within(resolution).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
    await waitFor(() => expect(within(status).getByTestId('ticket-write-outcome')).toHaveTextContent('The workflow refused this move.'));
    expect(within(screen).getByTestId('ticket-move-form')).toBe(form);
    expect(within(resolution).getByTestId('ticket-field-value')).toHaveTextContent('Fixed');
    expect(move).toHaveBeenCalledTimes(1);
  });

  it('says a move needs a field the app cannot set, with the way to the tracker, and offers no Move', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [{ id: 'attachment', name: 'Attachment', kind: 'read_only', required: true, editable: false, value: {} }],
      }],
    }) as unknown as DetailView);
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'move', state: 'applied' }] }) as WriteView);
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const host = setBindingMock('OpenExternalURL', async () => undefined);
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
      await fireEvent.click(within(status).getByTestId('ticket-field-value'));
      await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
      const form = await within(screen).findByTestId('ticket-move-form');
      expect(within(form).getByTestId('ticket-move-unsettable')).toHaveTextContent('Moving to Done needs Attachment, which the app cannot set. Open the ticket in the tracker to make this move.');
      expect(within(form).queryByTestId('ticket-move-submit')).toBeNull();
      await fireEvent.click(within(form).getByTestId('ticket-move-open-tracker'));
      await waitFor(() => expect([...host.mock.calls, ...open.mock.calls].map((c) => c[0])).toEqual(['https://example.atlassian.net/browse/WEB-1']));
      expect(move).not.toHaveBeenCalled();
    } finally {
      open.mockRestore();
    }
  });

  it('offers Save anyway on a move only for the form refused: a change, Cancel or another move withdraws it, and an applied retry closes the form', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [
          { id: 'resolution', name: 'Resolution', kind: 'option', required: true, editable: true, value: {} },
          { id: 'environment', name: 'Environment', kind: 'rich_text', editable: true, value: {} },
        ],
      }],
    }) as unknown as DetailView);
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '1', name: 'Fixed' }, { id: '2', name: 'Won\'t do' }] }));
    const move = setBindingMock('MoveTrackerIssue', async (_c: string, input: { accept_loss: boolean }): Promise<WriteView> => (input.accept_loss
      ? { parts: [{ step: 'move', state: 'applied' }] }
      : { parts: [{ step: 'move', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    const openForm = async () => {
      await fireEvent.click(within(status).getByTestId('ticket-field-value'));
      await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
      return within(screen).findByTestId('ticket-move-form');
    };
    const pick = async (form: HTMLElement, name: string) => {
      const resolution = within(form).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'resolution')!;
      await fireEvent.click(within(resolution).getByTestId('ticket-field-value'));
      const items = await waitFor(() => within(menu()).getAllByTestId('ticket-picker-item'));
      await fireEvent.click(items.find((i) => i.textContent?.includes(name))!);
    };
    const refused = async (form: HTMLElement) => {
      await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
      return waitFor(() => within(status).getByTestId('ticket-write-save-anyway'));
    };
    const offered = () => within(status).queryByTestId('ticket-write-save-anyway');
    let form = await openForm();
    await pick(form, 'Fixed');
    await fireEvent.input(within(form).getByTestId('ticket-move-text'), { target: { value: 'Draft A' } });
    await refused(form);
    // A changed text, and a changed pick, withdraw it.
    await fireEvent.input(within(form).getByTestId('ticket-move-text'), { target: { value: 'Draft B' } });
    expect(offered()).toBeNull();
    await refused(form);
    await pick(form, 'Won\'t do');
    expect(offered()).toBeNull();
    // Cancel withdraws it and closes the form.
    await refused(form);
    await fireEvent.click(within(form).getByTestId('ticket-move-cancel'));
    expect(within(screen).queryByTestId('ticket-move-form')).toBeNull();
    expect(offered()).toBeNull();
    expect(within(status).queryByTestId('ticket-write-outcome')).toBeNull();
    // Picking the move again replaces the form and withdraws it.
    form = await openForm();
    await pick(form, 'Fixed');
    await refused(form);
    form = await openForm();
    expect(offered()).toBeNull();
    // Save anyway sends what was refused and closes the form once made.
    await pick(form, 'Fixed');
    await fireEvent.input(within(form).getByTestId('ticket-move-text'), { target: { value: 'Draft C' } });
    const again = await refused(form);
    const sent = move.mock.calls.length;
    await fireEvent.click(again);
    await waitFor(() => expect(move).toHaveBeenCalledTimes(sent + 1));
    expect(move).toHaveBeenLastCalledWith('c1', {
      issue: '1', move: '31', accept_loss: true, fields: [
        { field: 'resolution', value: { kind: 'option', choices: [{ id: '1' }] } },
        { field: 'environment', markdown: { markdown: 'Draft C', from: '' } },
      ],
    });
    await waitFor(() => expect(within(screen).queryByTestId('ticket-move-form')).toBeNull());
    expect(offered()).toBeNull();
  });

  it('takes no change to a move\'s form while its move is out, so the move made is what the form shows', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [
          { id: 'resolution', name: 'Resolution', kind: 'option', required: true, editable: true, value: {} },
          { id: 'environment', name: 'Environment', kind: 'rich_text', editable: true, value: {} },
        ],
      }],
    }) as unknown as DetailView);
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '1', name: 'Fixed' }, { id: '2', name: 'Won\'t do' }] }));
    let answer: ((view: WriteView) => void) | undefined;
    const move = setBindingMock('MoveTrackerIssue', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    const form = await within(screen).findByTestId('ticket-move-form');
    const resolution = within(form).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'resolution')!;
    const items = () => within(document.body).queryAllByTestId('ticket-picker-item');
    await fireEvent.click(within(resolution).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => items().find((i) => i.textContent?.includes('Fixed'))!));
    await fireEvent.input(within(form).getByTestId('ticket-move-text'), { target: { value: 'Draft A' } });
    // The picker is open again when Move is pressed.
    await fireEvent.click(within(resolution).getByTestId('ticket-field-value'));
    await waitFor(() => expect(items()).toHaveLength(2));
    await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
    await waitFor(() => expect(answer).toBeDefined());
    // While the move is out, neither the open picker nor the field takes a pick.
    await waitFor(() => expect(items()).toHaveLength(0));
    expect(within(resolution).getByTestId('ticket-field-value')).toBeDisabled();
    await fireEvent.click(within(resolution).getByTestId('ticket-field-value'));
    expect(items()).toHaveLength(0);
    expect(within(form).getByTestId('ticket-move-text')).toBeDisabled();
    expect(within(resolution).getByTestId('ticket-field-value')).toHaveTextContent('Fixed');
    answer!({ parts: [{ step: 'move', state: 'applied' }] } as WriteView);
    await waitFor(() => expect(within(screen).queryByTestId('ticket-move-form')).toBeNull());
    expect(move).toHaveBeenCalledTimes(1);
    expect(move).toHaveBeenCalledWith('c1', {
      issue: '1', move: '31', accept_loss: false, fields: [
        { field: 'resolution', value: { kind: 'option', choices: [{ id: '1' }] } },
        { field: 'environment', markdown: { markdown: 'Draft A', from: '' } },
      ],
    });
  });

  it('offers no Save anyway for a move whose form went with its ticket closed while the move was out', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [{ id: 'environment', name: 'Environment', kind: 'rich_text', editable: true, value: {} }],
      }],
    }) as unknown as DetailView);
    let answer: ((view: WriteView) => void) | undefined;
    const move = setBindingMock('MoveTrackerIssue', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    let screen = await openCard(view, '1');
    const status = () => within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status()).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    const form = await within(screen).findByTestId('ticket-move-form');
    await fireEvent.input(within(form).getByTestId('ticket-move-text'), { target: { value: 'Draft A' } });
    await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
    await waitFor(() => expect(answer).toBeDefined());
    // Closed while the move is out, the form goes; reopened before the refusal lands.
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    screen = await openCard(view, '1');
    expect(within(screen).queryByTestId('ticket-move-form')).toBeNull();
    answer!({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView);
    await waitFor(() => expect(within(status()).getByTestId('ticket-write-outcome')).toHaveTextContent('This edit would lose formatting.'));
    expect(within(status()).queryByTestId('ticket-write-save-anyway')).toBeNull();
    expect(move).toHaveBeenCalledTimes(1);
  });

  it('starts a move\'s Description from the description the ticket shows, and keeps that as its edit base', async () => {
    install();
    const detail = (issue: string, description: string) => ({
      ...ticketDetail(issue),
      description,
      moves: [{
        id: '31', name: 'Resolve', to: { id: 's3', name: 'Done', group: 'done' },
        fields: [{ id: 'description', name: 'Description', kind: 'rich_text', editable: true, value: {} }],
      }],
    }) as unknown as DetailView;
    installTicket((issue) => detail(issue, 'The description'));
    setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: detail('1', 'Changed elsewhere') }));
    const move = setBindingMock('MoveTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'move', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const status = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'status')!;
    await fireEvent.click(within(status).getByTestId('ticket-field-value'));
    await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    const form = await within(screen).findByTestId('ticket-move-form');
    const text = within(form).getByTestId('ticket-move-text');
    expect(text).toHaveValue('The description');
    // The ticket is read again meanwhile: the edit is still from what the
    // form started from.
    trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 11, changed: true }, HOME_BACKEND);
    const description = within(screen).getAllByTestId('ticket-section').find((s) => s.dataset.field === 'description')!;
    await waitFor(() => expect(within(description).getByTestId('ticket-section-text')).toHaveTextContent('Changed elsewhere'));
    expect(text).toHaveValue('The description');
    await fireEvent.input(text, { target: { value: 'The description, edited' } });
    await fireEvent.click(within(form).getByTestId('ticket-move-submit'));
    await waitFor(() => expect(move).toHaveBeenCalledWith('c1', {
      issue: '1', move: '31', accept_loss: false, fields: [{ field: 'description', markdown: { markdown: 'The description, edited', from: 'The description' } }],
    }));
  });

  it('keeps every comment loaded once a posted comment moves the first page past an older page', async () => {
    install();
    const comment = (n: number) => ({ id: `c${n}`, author: { id: 'u1', name: 'User 1' }, body: `Comment ${n}`, created: 1_000 + n }) as CommentView;
    // Jira's pages: the newest 50, and the token T reads comments 1 to T.
    const newest = (last: number) => ({ ...ticketDetail('1'), comments: Array.from({ length: 50 }, (_, i) => comment(last - i)), more_comments: String(last - 50) }) as DetailView;
    installTicket(() => newest(51));
    const pages = setBindingMock('TrackerComments', async (_c: string, _i: string, token: string) => ({
      comments: Array.from({ length: Number(token) }, (_, i) => comment(Number(token) - i)), next: '',
    }));
    setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: newest(52) }));
    setBindingMock('AddTrackerComment', async (): Promise<WriteView> => {
      trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 11, changed: true }, HOME_BACKEND);
      return { parts: [{ step: 'comment', state: 'applied' }] } as WriteView;
    });
    const view = mount();
    const screen = await openCard(view, '1');
    const mounted = () => within(screen).queryAllByTestId('ticket-comment').map((c) => c.dataset.comment);
    await waitFor(() => expect(mounted()).toHaveLength(5));
    for (let i = 0; i < 4 && within(screen).queryByTestId('ticket-comments-earlier'); i++) {
      const before = mounted().length;
      await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
      await waitFor(() => expect(mounted().length).toBeGreaterThan(before));
    }
    expect(mounted()).toEqual(Array.from({ length: 51 }, (_, i) => `c${i + 1}`));
    expect(pages.mock.calls.map((c) => c[2])).toEqual(['1']);
    await fireEvent.input(within(screen).getByTestId('ticket-comment-input'), { target: { value: 'Comment 52' } });
    await fireEvent.click(within(screen).getByTestId('ticket-comment-send'));
    await waitFor(() => expect(mounted()).toEqual(Array.from({ length: 52 }, (_, i) => `c${i + 1}`)));
    await waitFor(() => expect(within(screen).getByTestId('ticket-comment-input')).toHaveValue(''));
    // One read of the page now before the first: the one page held.
    expect(pages.mock.calls.map((c) => c[2])).toEqual(['1', '2']);
    expect(within(screen).queryByTestId('ticket-comments-earlier')).toBeNull();
  });

  it('searches for text typed before the first answer said the list holds more, and drops an older search\'s answer', async () => {
    install();
    installTicket();
    let first: ((list: unknown) => void) | undefined;
    const searches = new Map<string, { resolve: (list: unknown) => void; reject: (err: unknown) => void }>();
    const choices = setBindingMock('TrackerChoices', (_c: string, _s: unknown, _f: string, text: string) => new Promise((resolve, reject) => {
      if (text === '') first = resolve;
      else searches.set(text, { resolve, reject });
    }));
    const view = mount();
    const screen = await openCard(view, '1');
    const priority = within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    await fireEvent.click(within(priority).getByTestId('ticket-field-value'));
    const find = await waitFor(() => within(document.body).getByTestId('ticket-picker-find'));
    // Pasted before the opening read answers.
    await fireEvent.input(find, { target: { value: 'Option 77' } });
    await waitFor(() => expect(first).toBeDefined());
    first!({ choices: Array.from({ length: 50 }, (_, i) => ({ id: `o${i}`, name: `Option ${i}` })), more: true });
    await waitFor(() => expect(searches.has('Option 77')).toBe(true));
    const items = () => within(menu()).queryAllByTestId('ticket-picker-item').map((i) => i.textContent?.trim());
    expect(within(document.body).queryByTestId('ticket-picker-none')).toBeNull();
    searches.get('Option 77')!.resolve({ choices: [{ id: 'o77', name: 'Option 77' }] });
    await waitFor(() => expect(items()).toEqual(['Option 77']));
    // The search for "Re" fails after the person typed on: nothing of it shows.
    await fireEvent.input(find, { target: { value: 'Re' } });
    await waitFor(() => expect(searches.has('Re')).toBe(true));
    await fireEvent.input(find, { target: { value: 'Rel' } });
    searches.get('Re')!.reject(new Error('The tracker did not answer'));
    searches.get('Re')!.resolve({ choices: [] });
    await new Promise((r) => setTimeout(r, 20));
    expect(within(document.body).queryByTestId('ticket-picker-error')).toBeNull();
    await waitFor(() => expect(searches.has('Rel')).toBe(true));
    searches.get('Rel')!.resolve({ choices: [{ id: 'r1', name: 'Release 1' }] });
    await waitFor(() => expect(items()).toEqual(['Release 1']));
    expect(choices.mock.calls.map((c) => c[3])).toEqual(['', 'Option 77', 'Re', 'Rel']);
  });

  it('opens a card\'s ticket from the card\'s menu, and copies its key from it', async () => {
    install();
    installTicket();
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);
    const view = mount();
    await view.findAllByTestId('issues-card');
    const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === '2')!;
    await fireEvent.contextMenu(card);
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-copy-key'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('WEB-2'));
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(within(document.body).queryByTestId('issues-card-menu-copy-key')).toBeNull();
    writeText.mockRestore();
    await fireEvent.contextMenu(card);
    await fireEvent.click(await within(document.body).findByTestId('issues-card-menu-open'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('2'));
  });

  it('closes a comment\'s editor once Save anyway sends the edit that would lose content', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 1 }));
    const edit = setBindingMock('EditTrackerComment', async (_c: string, input: { accept_loss: boolean }): Promise<WriteView> => (input.accept_loss
      ? { parts: [{ step: 'comment', state: 'applied' }] }
      : { parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const comment = await waitFor(() => within(screen).getByTestId('ticket-comment'));
    await fireEvent.contextMenu(comment);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(comment).getByTestId('ticket-comment-editor'), { target: { value: 'Plain now' } });
    await fireEvent.click(within(comment).getByTestId('ticket-comment-save'));
    const again = await waitFor(() => within(comment).getByTestId('ticket-write-save-anyway'));
    expect(within(comment).getByTestId('ticket-comment-editor')).toHaveValue('Plain now');
    await fireEvent.click(again);
    await waitFor(() => expect(edit).toHaveBeenLastCalledWith('c1', { issue: '1', comment: 'c1', markdown: 'Plain now', from: 'Comment 1', accept_loss: true }));
    expect(edit).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(within(comment).queryByTestId('ticket-comment-editor')).toBeNull());
    expect(within(comment).queryByTestId('ticket-write-outcome')).toBeNull();
  });

  it('withdraws Save anyway once the description is changed or the edit canceled, and checks a revised text again', async () => {
    install();
    installTicket();
    const loss = { parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView;
    const edit = setBindingMock('EditTrackerIssue', async (): Promise<WriteView> => loss);
    const view = mount();
    const screen = await openCard(view, '1');
    const description = within(screen).getAllByTestId('ticket-section').find((s) => s.dataset.field === 'description')!;
    const editor = async () => {
      await fireEvent.click(within(description).getByTestId('ticket-section-edit'));
      return within(description).getByTestId('ticket-section-editor');
    };
    await fireEvent.input(await editor(), { target: { value: 'Draft A' } });
    await fireEvent.click(within(description).getByTestId('ticket-section-save'));
    await waitFor(() => within(description).getByTestId('ticket-write-save-anyway'));
    // The person revises the text: the consent was for Draft A.
    await fireEvent.input(within(description).getByTestId('ticket-section-editor'), { target: { value: 'Draft B' } });
    expect(within(description).queryByTestId('ticket-write-save-anyway')).toBeNull();
    await fireEvent.click(within(description).getByTestId('ticket-section-save'));
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
    expect(edit).toHaveBeenLastCalledWith('c1', { issue: '1', fields: [{ field: 'description', markdown: { markdown: 'Draft B', from: 'The description' } }], accept_loss: false });
    await waitFor(() => within(description).getByTestId('ticket-write-save-anyway'));
    // Cancel withdraws it too, and the restored text offers nothing.
    await fireEvent.click(within(description).getByTestId('ticket-section-cancel'));
    expect(within(description).queryByTestId('ticket-write-save-anyway')).toBeNull();
    expect(within(description).queryByTestId('ticket-write-outcome')).toBeNull();
    expect(edit).toHaveBeenCalledTimes(2);
  });

  it('offers no Save anyway for a description edit that went with its ticket closed while it was saving', async () => {
    install();
    installTicket();
    let answer: ((view: WriteView) => void) | undefined;
    const edit = setBindingMock('EditTrackerIssue', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    let screen = await openCard(view, '1');
    const description = () => within(screen).getAllByTestId('ticket-section').find((s) => s.dataset.field === 'description')!;
    await fireEvent.click(within(description()).getByTestId('ticket-section-edit'));
    await fireEvent.input(within(description()).getByTestId('ticket-section-editor'), { target: { value: 'Draft A' } });
    await fireEvent.click(within(description()).getByTestId('ticket-section-save'));
    await waitFor(() => expect(answer).toBeDefined());
    // Closed while it saves, the edit goes; reopened before the refusal lands.
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    screen = await openCard(view, '1');
    expect(within(description()).queryByTestId('ticket-section-editor')).toBeNull();
    answer!({ parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView);
    await waitFor(() => expect(within(description()).getByTestId('ticket-write-outcome')).toHaveTextContent('This edit would lose formatting.'));
    expect(within(description()).queryByTestId('ticket-write-save-anyway')).toBeNull();
    expect(edit).toHaveBeenCalledTimes(1);
  });

  it('withdraws Save anyway once a comment\'s edit is changed or canceled', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 1 }));
    const edit = setBindingMock('EditTrackerComment', async (): Promise<WriteView> => ({
      parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }],
    }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const comment = await waitFor(() => within(screen).getByTestId('ticket-comment'));
    const startEdit = async () => {
      await fireEvent.contextMenu(comment);
      await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    };
    await startEdit();
    await fireEvent.input(within(comment).getByTestId('ticket-comment-editor'), { target: { value: 'Draft A' } });
    await fireEvent.click(within(comment).getByTestId('ticket-comment-save'));
    await waitFor(() => within(comment).getByTestId('ticket-write-save-anyway'));
    await fireEvent.input(within(comment).getByTestId('ticket-comment-editor'), { target: { value: 'Draft B' } });
    expect(within(comment).queryByTestId('ticket-write-save-anyway')).toBeNull();
    await fireEvent.click(within(comment).getByTestId('ticket-comment-save'));
    await waitFor(() => within(comment).getByTestId('ticket-write-save-anyway'));
    expect(edit).toHaveBeenLastCalledWith('c1', { issue: '1', comment: 'c1', markdown: 'Draft B', from: 'Comment 1', accept_loss: false });
    await fireEvent.click(within(comment).getByTestId('ticket-comment-cancel'));
    expect(within(comment).queryByTestId('ticket-write-save-anyway')).toBeNull();
    expect(edit).toHaveBeenCalledTimes(2);
  });

  it('steps back through the tickets opened from one another, at most 20, then closes; Close and a board pick leave at once', async () => {
    install();
    // Each ticket's parent is the next issue.
    const ticket = installTicket((issue) => ticketDetail(issue, { parent: String(Number(issue) + 1) }));
    clearCommandRegistry();
    registerIssuesCommands();
    const escape = focusIssuesPane();
    const view = mount();
    await openCard(view, '1');
    for (let i = 2; i <= 23; i += 1) {
      await fireEvent.click(await view.findByTestId('ticket-parent'));
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe(String(i)));
    }
    expect(ticket.subscribe).toHaveBeenCalledTimes(23);
    // Back returns through the 20 tickets before this one: 22 to 3.
    const back: string[] = [];
    for (let i = 0; i < 20; i += 1) {
      if (i % 2 === 0) await fireEvent.click(view.getByTestId('ticket-back'));
      else escape();
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe(String(22 - i)));
      back.push(view.getByTestId('ticket-screen').dataset.issue!);
    }
    expect(back[0]).toBe('22');
    expect(back.at(-1)).toBe('3');
    // With none left to step back to, Back closes the ticket.
    expect(view.queryByTestId('ticket-back')).toBeNull();
    escape();
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());

    // Close leaves at once, whatever the trail.
    await openCard(view, '1');
    await fireEvent.click(await view.findByTestId('ticket-parent'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('2'));
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());

    // A pick of another board drops the ticket and its trail.
    await openCard(view, '1');
    await fireEvent.click(view.getByTestId('issues-board-picker'));
    await fireEvent.click(within(menu()).getAllByTestId('issues-board-choice')[1]);
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    clearCommandRegistry();
  });

  it('shows the linked tickets grouped by relation, each opening its ticket that Back returns from', async () => {
    install();
    const linked = (id: string, relation: string) => ({
      id: `l${id}`, type: '10000', relation, issue: { id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: 's1', name: 'To Do', group: 'todo' } },
    });
    installTicket((issue) => ({
      ...ticketDetail(issue),
      links: issue === '1' ? [linked('2', 'blocks'), linked('3', 'is blocked by'), linked('4', 'blocks')] : [],
    }) as unknown as DetailView);
    const view = mount();
    const screen = await openCard(view, '1');
    const groups = await waitFor(() => {
      const found = within(screen).getAllByTestId('ticket-link-group');
      expect(found).toHaveLength(2);
      return found;
    });
    expect(groups.map((g) => [
      within(g).getByTestId('ticket-link-relation').textContent,
      within(g).getAllByTestId('ticket-link').map((l) => l.textContent?.replace(/\s+/g, ' ').trim()),
    ])).toEqual([
      ['blocks', ['WEB-2 Issue 2', 'WEB-4 Issue 4']],
      ['is blocked by', ['WEB-3 Issue 3']],
    ]);
    await fireEvent.click(within(groups[1]).getByTestId('ticket-link'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('3'));
    await fireEvent.click(view.getByTestId('ticket-back'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('1'));
  });

  it('mounts the newest 20 changes of the history and 20 more a press, ordering them once', async () => {
    install();
    // Oldest first, as a changelog lists them, with two out of order.
    const history = Array.from({ length: 50 }, (_, i) => ({ at: 1_000 + i, field: `f${i}`, to: `v${i}` }));
    [history[10], history[11]] = [history[11], history[10]];
    installTicket((issue) => ({ ...ticketDetail(issue), history }) as unknown as DetailView);
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(within(screen).getByRole('radio', { name: 'History' }));
    const fields = () => within(screen).queryAllByTestId('ticket-history-event').map((e) => e.textContent?.match(/changed (f\d+)/)?.[1]);
    await waitFor(() => expect(fields()).toHaveLength(20));
    expect(fields().slice(0, 2)).toEqual(['f49', 'f48']);
    const sort = vi.spyOn(Array.prototype, 'sort');
    try {
      await fireEvent.click(within(screen).getByTestId('ticket-history-earlier'));
      await waitFor(() => expect(fields()).toHaveLength(40));
      await fireEvent.click(within(screen).getByTestId('ticket-history-earlier'));
      await waitFor(() => expect(fields()).toHaveLength(50));
      expect(sort).not.toHaveBeenCalled();
    } finally {
      sort.mockRestore();
    }
    expect(fields().slice(37, 41)).toEqual(['f12', 'f11', 'f10', 'f9']);
  });

  it('shows an applied edit or delete of an older page\'s comment, re-reading that page for an edit only', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 50, more: '50' }));
    // Read again, the first page is the same: no write here adds a comment
    // past it or takes one from it.
    setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: ticketDetail('1', { comments: 50, more: '50' }) }));
    // The tracker's older page: what it holds changes with the writes.
    const older = Array.from({ length: 10 }, (_, i) => ({ id: `old${i}`, author: { id: 'u1', name: 'User 1' }, body: `Older ${i}`, created: 900 - i }));
    const page = setBindingMock('TrackerComments', async (_c: string, _i: string, token: string) => {
      expect(token).toBe('50');
      return { comments: older.map((c) => ({ ...c })), next: '' };
    });
    setBindingMock('EditTrackerComment', async (_c: string, input: { comment: string; markdown: string }): Promise<WriteView> => {
      older.find((c) => c.id === input.comment)!.body = input.markdown;
      return { parts: [{ step: 'comment', state: 'applied' }] } as WriteView;
    });
    setBindingMock('DeleteTrackerComment', async (_c: string, input: { comment: string }): Promise<WriteView> => {
      older.splice(older.findIndex((c) => c.id === input.comment), 1);
      return { parts: [{ step: 'comment', state: 'applied' }] } as WriteView;
    });
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findAllByTestId('ticket-comment');
    for (let i = 0; i < 3; i += 1) await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    await waitFor(() => expect(page).toHaveBeenCalledTimes(1));
    const comment = (id: string) => waitFor(() => {
      const found = within(screen).getAllByTestId('ticket-comment').find((c) => c.dataset.comment === id);
      expect(found).toBeDefined();
      return found!;
    });
    // Edit an older comment.
    const two = await comment('old2');
    await fireEvent.contextMenu(two);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(two).getByTestId('ticket-comment-editor'), { target: { value: 'Older 2, edited' } });
    await fireEvent.click(within(two).getByTestId('ticket-comment-save'));
    await waitFor(() => expect(within(two).getByTestId('ticket-comment-body')).toHaveTextContent('Older 2, edited'));
    expect(page).toHaveBeenCalledTimes(2);
    // Delete another.
    const three = await comment('old3');
    await fireEvent.contextMenu(three);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-delete'));
    await fireEvent.click(within(three).getByTestId('ticket-comment-delete'));
    await waitFor(() => expect(within(screen).getAllByTestId('ticket-comment').map((c) => c.dataset.comment)).not.toContain('old3'));
    expect(page).toHaveBeenCalledTimes(2);
    // Refresh brings neither the old body nor the deleted comment back.
    const refresh = setBindingMock('RefreshTrackerSubscription', async () => undefined);
    await fireEvent.click(within(screen).getByTestId('ticket-refresh'));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    const read = getBindingMock('TrackerTicket')!;
    const reads = read.mock.calls.length;
    trackerIssueUpdated({ connection: 'c1', issue: '1', seq: 20, changed: true }, HOME_BACKEND);
    await waitFor(() => expect(read.mock.calls.length).toBe(reads + 1));
    await tick();
    const ids = within(screen).getAllByTestId('ticket-comment').map((c) => c.dataset.comment);
    expect(ids).not.toContain('old3');
    expect((await comment('old2')).textContent).toContain('Older 2, edited');
  });

  it('drops an older page whose re-read after an edit failed, and reads it again from the control', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 50, more: '50' }));
    const older = Array.from({ length: 10 }, (_, i) => ({ id: `old${i}`, author: { id: 'u1', name: 'User 1' }, body: `Older ${i}`, created: 900 - i }));
    let fail = false;
    const page = setBindingMock('TrackerComments', async () => {
      if (fail) throw new TransportError('timeout', 'Jira did not answer in time');
      return { comments: older.map((c) => ({ ...c })), next: '' };
    });
    setBindingMock('EditTrackerComment', async (_c: string, input: { comment: string; markdown: string }): Promise<WriteView> => {
      older.find((c) => c.id === input.comment)!.body = input.markdown;
      fail = true;
      return { parts: [{ step: 'comment', state: 'applied' }] } as WriteView;
    });
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findAllByTestId('ticket-comment');
    for (let i = 0; i < 3; i += 1) await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    const two = await waitFor(() => {
      const found = within(screen).getAllByTestId('ticket-comment').find((c) => c.dataset.comment === 'old2');
      expect(found).toBeDefined();
      return found!;
    });
    await fireEvent.contextMenu(two);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(two).getByTestId('ticket-comment-editor'), { target: { value: 'Older 2, edited' } });
    await fireEvent.click(within(two).getByTestId('ticket-comment-save'));
    await waitFor(() => expect(within(screen).getByTestId('ticket-comments-earlier-error')).toHaveTextContent('Jira did not answer in time'));
    expect(within(screen).getAllByTestId('ticket-comment').map((c) => c.dataset.comment).filter((id) => id?.startsWith('old'))).toEqual([]);
    fail = false;
    await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    await waitFor(() => expect((within(screen).getAllByTestId('ticket-comment').find((c) => c.dataset.comment === 'old2'))?.textContent).toContain('Older 2, edited'));
    expect(page).toHaveBeenCalledTimes(3);
  });

  it('keeps unsent text and open editors across a switch to History and back', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 1 }));
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.input(await within(screen).findByTestId('ticket-comment-input'), { target: { value: 'A new comment' } });
    const comment = within(screen).getByTestId('ticket-comment');
    await fireEvent.contextMenu(comment);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(comment).getByTestId('ticket-comment-editor'), { target: { value: 'Comment 1, half edited' } });
    await fireEvent.click(within(screen).getByRole('radio', { name: 'History' }));
    await waitFor(() => expect(within(screen).queryByTestId('ticket-comments')).toBeNull());
    await fireEvent.click(within(screen).getByRole('radio', { name: 'Comments' }));
    expect(await within(screen).findByTestId('ticket-comment-input')).toHaveValue('A new comment');
    expect(within(screen).getByTestId('ticket-comment-editor')).toHaveValue('Comment 1, half edited');
  });

  it('keeps an open edit of a comment past the newest five in view after a switch to History and back', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 8 }));
    const view = mount();
    const screen = await openCard(view, '1');
    const mounted = () => within(screen).queryAllByTestId('ticket-comment').map((c) => c.dataset.comment);
    await waitFor(() => expect(mounted()).toEqual(['c4', 'c5', 'c6', 'c7', 'c8']));
    await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    await waitFor(() => expect(mounted()).toHaveLength(8));
    const oldest = within(screen).getAllByTestId('ticket-comment').find((c) => c.dataset.comment === 'c2')!;
    await fireEvent.contextMenu(oldest);
    await fireEvent.click(await within(document.body).findByTestId('ticket-comment-menu-edit'));
    await fireEvent.input(within(oldest).getByTestId('ticket-comment-editor'), { target: { value: 'Comment 2, half edited' } });
    await fireEvent.click(within(screen).getByRole('radio', { name: 'History' }));
    await waitFor(() => expect(within(screen).queryByTestId('ticket-comments')).toBeNull());
    await fireEvent.click(within(screen).getByRole('radio', { name: 'Comments' }));
    // The window reaches back to the edit, and stays one run.
    await waitFor(() => expect(mounted()).toEqual(['c2', 'c3', 'c4', 'c5', 'c6', 'c7', 'c8']));
    expect(within(screen).getByTestId('ticket-comment-editor')).toHaveValue('Comment 2, half edited');
    expect(within(screen).getByTestId('ticket-comments-earlier')).toHaveTextContent('Show 1 earlier comment');
  });

  it.each(['mailto:user@example.com', 'javascript:alert(1)'])('offers neither Open in the tracker nor Copy link for %s, which is not a web page', async (url) => {
    install();
    installTicket((issue) => ({ ...ticketDetail(issue), url }) as DetailView);
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(within(screen).getByTestId('ticket-title')).toHaveTextContent('Issue 1'));
    await within(screen).findByTestId('ticket-comment-input');
    expect(within(screen).queryByTestId('ticket-open-tracker')).toBeNull();
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await within(document.body).findByTestId('ticket-menu-copy-key');
    expect(within(document.body).queryByTestId('ticket-menu-open-tracker')).toBeNull();
    expect(within(document.body).queryByTestId('ticket-menu-copy-link')).toBeNull();
  });

  it('keeps unsent text and its edit base across a dropped connection to the ticket\'s computer', async () => {
    install();
    const ticket = installTicket((issue) => ticketDetail(issue, { comments: 1 }));
    const edit = setBindingMock('EditTrackerIssue', async (): Promise<WriteView> => ({ parts: [{ step: 'edit', state: 'applied' }] }) as WriteView);
    const view = mount();
    const screen = await openCard(view, '1');
    const description = () => within(view.getByTestId('ticket-screen')).getAllByTestId('ticket-section').find((s) => s.dataset.field === 'description')!;
    await fireEvent.click(within(description()).getByTestId('ticket-section-edit'));
    await fireEvent.input(within(description()).getByTestId('ticket-section-editor'), { target: { value: 'A better description' } });
    await fireEvent.input(within(screen).getByTestId('ticket-comment-input'), { target: { value: 'A new comment' } });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    await waitFor(() => expect(view.queryByTestId('ticket-comment-input')).toBeNull());
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await waitFor(() => expect(ticket.subscribe).toHaveBeenCalledTimes(2));
    expect(await view.findByTestId('ticket-comment-input')).toHaveValue('A new comment');
    expect(within(description()).getByTestId('ticket-section-editor')).toHaveValue('A better description');
    await fireEvent.click(within(description()).getByTestId('ticket-section-save'));
    await waitFor(() => expect(edit).toHaveBeenCalledWith('c1', { issue: '1', fields: [{ field: 'description', markdown: { markdown: 'A better description', from: 'The description' } }], accept_loss: false }));
    await waitFor(() => expect(within(description()).queryByTestId('ticket-section-editor')).toBeNull());
  });

  it('keeps a ticket\'s unsent comment while it is on the Back trail, and drops it once the ticket leaves the trail', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { parent: String(Number(issue) + 1) }));
    const view = mount();
    await openCard(view, '1');
    await fireEvent.input(await view.findByTestId('ticket-comment-input'), { target: { value: 'Unsent on WEB-1' } });
    await fireEvent.click(view.getByTestId('ticket-parent'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('2'));
    expect(await view.findByTestId('ticket-comment-input')).toHaveValue('');
    await fireEvent.click(view.getByTestId('ticket-back'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('1'));
    expect(await view.findByTestId('ticket-comment-input')).toHaveValue('Unsent on WEB-1');
    await fireEvent.click(view.getByTestId('ticket-close'));
    await waitFor(() => expect(view.queryByTestId('ticket-screen')).toBeNull());
    await openCard(view, '1');
    expect(await view.findByTestId('ticket-comment-input')).toHaveValue('');
    // Twenty tickets on from WEB-1, it leaves the trail, and its text goes.
    await fireEvent.input(view.getByTestId('ticket-comment-input'), { target: { value: 'Unsent on WEB-1' } });
    for (let i = 2; i <= 21; i += 1) {
      await fireEvent.click(await view.findByTestId('ticket-parent'));
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe(String(i)));
    }
    expect(ticketDraft(trackerTicketKey({ backend: HOME_BACKEND, connection: 'c1', issue: '1' }), 'new-comment')?.text).toBe('Unsent on WEB-1');
    await fireEvent.click(await view.findByTestId('ticket-parent'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('22'));
    expect(ticketDraft(trackerTicketKey({ backend: HOME_BACKEND, connection: 'c1', issue: '1' }), 'new-comment')).toBeNull();
    // A pane that closes lets its tickets' drafts go.
    await fireEvent.input(view.getByTestId('ticket-comment-input'), { target: { value: 'Unsent on WEB-22' } });
    expect(ticketDraft(trackerTicketKey({ backend: HOME_BACKEND, connection: 'c1', issue: '22' }), 'new-comment')?.text).toBe('Unsent on WEB-22');
    view.unmount();
    expect(ticketDraft(trackerTicketKey({ backend: HOME_BACKEND, connection: 'c1', issue: '22' }), 'new-comment')).toBeNull();
  });

  it('lets the earlier comments be read again after the connection dropped while a page was being read', async () => {
    install();
    const ticket = installTicket((issue) => ticketDetail(issue, { comments: 3, more: 'page-2' }));
    let answer: ((page: { comments: CommentView[]; next: string }) => void) | null = null;
    setBindingMock('TrackerComments', () => new Promise((resolve) => { answer = resolve; }));
    const view = mount();
    await openCard(view, '1');
    await fireEvent.click(await view.findByTestId('ticket-comments-earlier'));
    await waitFor(() => expect(view.getByTestId('ticket-comments-earlier')).toBeDisabled());
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    await waitFor(() => expect(view.queryByTestId('ticket-comments-earlier')).toBeNull());
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await waitFor(() => expect(ticket.subscribe).toHaveBeenCalledTimes(2));
    const control = await view.findByTestId('ticket-comments-earlier');
    expect(control).toBeEnabled();
    expect(control).toHaveTextContent('Show earlier comments');
    answer!({ comments: [], next: '' });
  });

  it('mounts the newest five comments and the earlier ones 20 a press, reading the next page once the held ones run out', async () => {
    install();
    installTicket((issue) => ticketDetail(issue, { comments: 30, more: 'page-2' }));
    const page = setBindingMock('TrackerComments', async () => ({
      comments: Array.from({ length: 10 }, (_, i) => ({ id: `old${i}`, author: { id: 'u1', name: 'User 1' }, body: `Older ${i}`, created: 900 - i })),
      next: '',
    }));
    const view = mount();
    const screen = await openCard(view, '1');
    const mounted = () => within(screen).queryAllByTestId('ticket-comment').map((c) => c.dataset.comment);
    await waitFor(() => expect(mounted()).toEqual(['c26', 'c27', 'c28', 'c29', 'c30']));
    expect(within(screen).getByTestId('ticket-comments-earlier')).toHaveTextContent('Show 20 earlier comments');
    await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    await waitFor(() => expect(mounted()).toHaveLength(25));
    expect(page).not.toHaveBeenCalled();
    // Five held are left: the press reads the next page, then mounts 20.
    await fireEvent.click(within(screen).getByTestId('ticket-comments-earlier'));
    await waitFor(() => expect(mounted()).toHaveLength(40));
    expect(page).toHaveBeenCalledWith('c1', '1', 'page-2');
    expect(mounted()[0]).toBe('old9');
    expect(within(screen).queryByTestId('ticket-comments-earlier')).toBeNull();
  });
});

describe("the ticket's attachments, links, poker, estimate and logged time", () => {
  beforeAll(async () => {
    await import('./TicketScreen.svelte');
  });

  const applied = (): WriteView => ({ parts: [{ step: 'write', state: 'applied' }] }) as WriteView;
  const file = (name: string, type = 'application/pdf') => new File(['bytes'], name, { type });

  /** Uploads that answer applied; the mint's file names, in order. */
  function installUploads(): string[] {
    const names: string[] = [];
    setBindingMock('MintTrackerAttachmentUploadTicket', async (_c: string, _i: string, name: string) => {
      names.push(name);
      return `/attachments/tracker/upload?ticket=${name}`;
    });
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify(applied()), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    return names;
  }

  it('attaches files dropped anywhere on a Jira ticket, and files pasted into its comment box, whose text stays', async () => {
    install();
    installTicket();
    const names = installUploads();
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await within(screen).findByTestId('ticket-attachments');
      // Folded, the drop opens the section.
      await fireEvent.click(within(screen).getByTestId('ticket-attachments-fold'));
      expect(within(screen).getByTestId('ticket-attachments-fold')).toHaveAttribute('aria-expanded', 'false');
      const dataTransfer = { types: ['Files'], files: [file('report.pdf')], dropEffect: 'none' };
      expect(await fireEvent.dragOver(within(screen).getByTestId('ticket-content'), { dataTransfer })).toBe(false);
      expect(within(screen).getByTestId('ticket-drop')).toHaveTextContent('Drop the files to attach them to WEB-1');
      await fireEvent.drop(within(screen).getByTestId('ticket-content'), { dataTransfer });
      expect(within(screen).queryByTestId('ticket-drop')).toBeNull();
      await waitFor(() => expect(names).toEqual(['report.pdf']));
      expect(within(screen).getByTestId('ticket-attachments-fold')).toHaveAttribute('aria-expanded', 'true');

      const box = within(screen).getByTestId('ticket-comment-input');
      const shot = file('shot.png', 'image/png');
      const pasted = await fireEvent.paste(box, {
        clipboardData: { types: ['text/plain', 'Files'], items: [{ kind: 'file', type: 'image/png', getAsFile: () => shot }, { kind: 'string', type: 'text/plain', getAsFile: () => null }] },
      });
      // Not prevented: the text part goes into the box.
      expect(pasted).toBe(true);
      await waitFor(() => expect(names).toEqual(['report.pdf', 'shot.png']));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('gives the ticket a card opens the focus, so a paste of files right after attaches them', async () => {
    install();
    installTicket();
    const names = installUploads();
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await waitFor(() => expect(document.activeElement).toBe(screen));
      await fireEvent.paste(document.activeElement!, {
        clipboardData: { types: ['Files'], items: [{ kind: 'file', type: 'image/png', getAsFile: () => file('image.png', 'image/png') }] },
      });
      await waitFor(() => expect(names).toEqual(['image.png']));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  // The ticket mounts after its code loads, a later turn than the click
  // that opened it; a person's focus or press elsewhere in between, or the
  // ticket's close, ends that open's claim on the focus.
  function clickCardNow(view: ReturnType<typeof mount>, issue: string): void {
    const card = view.getAllByTestId('issues-card').find((c) => c.dataset.issue === issue)!;
    within(card).getByTestId('issues-card-open').click();
  }

  it('leaves the focus where the person put it while the ticket a card opened was loading', async () => {
    install();
    installTicket();
    const composer = document.body.appendChild(document.createElement('textarea'));
    try {
      const view = mount();
      await view.findAllByTestId('issues-card');
      clickCardNow(view, '1');
      // Before the ticket mounts: a click into another pane's composer.
      composer.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
      composer.focus();
      await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('1'));
      await tick();
      expect(document.activeElement).toBe(composer);
    } finally {
      composer.remove();
    }
  });

  it('ends the focus a card open asked for when the ticket closes before it mounts, so a linked open of it later takes none', async () => {
    install();
    installTicket();
    clearCommandRegistry();
    registerIssuesCommands();
    const escape = focusIssuesPane();
    const view = mount();
    await view.findAllByTestId('issues-card');
    const start = document.activeElement;
    clickCardNow(view, '1');
    escape();
    await tick();
    expect(view.queryByTestId('ticket-screen')).toBeNull();
    requestTicketOpen({ ref: { backend: HOME_BACKEND, connection: 'c1', issue: '1' }, seed: { key: 'WEB-1', title: 'Linked', card: null }, project: 'p1', unplaced: vi.fn() });
    const screen = await waitFor(() => view.getByTestId('ticket-screen'));
    await tick();
    expect(document.activeElement).not.toBe(screen);
    expect(document.activeElement).toBe(start);
  });

  it('takes no drop or paste of files on a Linear ticket, and offers it no Attach', async () => {
    install();
    installTicket(undefined, { kind: 'linear', account: 'user-1' });
    const names = installUploads();
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await within(screen).findByTestId('ticket-comment-input');
      expect(within(screen).queryByTestId('ticket-attachments')).toBeNull();
      const dataTransfer = { types: ['Files'], files: [file('report.pdf')] };
      expect(await fireEvent.dragOver(within(screen).getByTestId('ticket-content'), { dataTransfer })).toBe(true);
      await fireEvent.drop(within(screen).getByTestId('ticket-content'), { dataTransfer });
      await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
      await within(document.body).findByTestId('ticket-menu-copy-key');
      expect(within(document.body).queryByTestId('ticket-menu-attach')).toBeNull();
      expect(within(document.body).queryByTestId('ticket-menu-log-time')).toBeNull();
      expect(within(screen).queryByTestId('ticket-estimate')).toBeNull();
      expect(names).toEqual([]);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('shows a file the description names with its size and Save once the ticket holds it', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      description: 'See [report.pdf](media:abc).',
      attachments: [{ id: 'a1', name: 'report.pdf', mime_type: 'application/pdf', size: 2048, created: 1, author: { id: 'u1', name: 'User 1' } }],
    }) as unknown as DetailView);
    const view = mount();
    const screen = await openCard(view, '1');
    const chip = await waitFor(() => {
      const found = screen.querySelector<HTMLElement>('[data-testid="ticket-section"] [data-tracker-media]');
      expect(found).not.toBeNull();
      return found!;
    });
    await waitFor(() => expect(chip.textContent?.replace(/\s+/g, ' ')).toContain('report.pdf'));
    expect(within(chip).getByRole('button')).toBeInTheDocument();
    expect(chip.textContent).toContain('2 KB');
  });

  it('offers planning poker only where the site has it, folded until opened, and follows the votes only while it shows', async () => {
    install();
    installTicket();
    setBindingMock('TrackerPokerSupport', async () => ({ available: true }));
    const follow = setBindingMock('FollowTrackerPoker', async () => ({ loaded: true, revealed: false, votes: [], read_at: 1 }));
    const observers: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
    vi.stubGlobal('IntersectionObserver', class {
      constructor(report: (entries: Array<{ isIntersecting: boolean }>) => void) {
        observers.push(report);
      }
      observe(): void {
        observers.at(-1)?.([{ isIntersecting: true }]);
      }
      disconnect(): void {}
    });
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      const fold = await within(screen).findByTestId('ticket-poker-fold');
      expect(fold).toHaveAttribute('aria-expanded', 'false');
      expect(follow).not.toHaveBeenCalled();
      await fireEvent.click(fold);
      await waitFor(() => expect(follow).toHaveBeenCalledWith('ticket-1-1', true));
      expect(await within(screen).findByTestId('ticket-poker-votes')).toBeInTheDocument();
      await fireEvent.click(fold);
      await waitFor(() => expect(follow).toHaveBeenLastCalledWith('ticket-1-1', false));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('sends one poker follow change at a time, ending on the state the section last asked for, and says nothing of votes before a read', async () => {
    install();
    installTicket();
    setBindingMock('TrackerPokerSupport', async () => ({ available: true }));
    const answers: Array<(view: PokerView) => void> = [];
    const follow = setBindingMock('FollowTrackerPoker', () => new Promise<PokerView>((resolve) => { answers.push(resolve); }));
    const reports: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
    vi.stubGlobal('IntersectionObserver', class {
      constructor(report: (entries: Array<{ isIntersecting: boolean }>) => void) {
        reports.push(report);
      }
      observe(): void {}
      disconnect(): void {}
    });
    const unread = { loaded: false, revealed: false, votes: [], read_at: 0 } as unknown as PokerView;
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await fireEvent.click(await within(screen).findByTestId('ticket-poker-fold'));
      await waitFor(() => expect(reports).toHaveLength(1));
      const onScreen = async (shown: boolean) => {
        reports.at(-1)!([{ isIntersecting: shown }]);
        await tick();
      };
      // Scrolled into view, out and in again, and out, while the first
      // follow is out.
      await onScreen(true);
      await waitFor(() => expect(follow).toHaveBeenCalledTimes(1));
      await onScreen(false);
      await onScreen(true);
      await onScreen(false);
      expect(follow.mock.calls).toEqual([['ticket-1-1', true]]);
      answers[0]!(unread);
      await waitFor(() => expect(follow).toHaveBeenCalledTimes(2));
      expect(follow.mock.calls[1]).toEqual(['ticket-1-1', false]);
      answers[1]!(unread);
      await tick();
      expect(follow).toHaveBeenCalledTimes(2);

      // On screen again: the round is not read yet, so the section says so.
      await onScreen(true);
      await waitFor(() => expect(follow).toHaveBeenCalledTimes(3));
      answers[2]!(unread);
      await tick();
      expect(await within(screen).findByTestId('ticket-poker-reading')).toHaveTextContent('Reading the votes…');
      expect(within(screen).getByTestId('ticket-poker').textContent).not.toMatch(/No votes yet|Not revealed/);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('sets the estimate from a revealed poker round through the ticket menu', async () => {
    install();
    installTicket();
    setBindingMock('TrackerPokerSupport', async () => ({ available: true }));
    setBindingMock('FollowTrackerPoker', async () => ({
      loaded: true, revealed: true, read_at: 1,
      votes: [{ voter: { id: 'u1', name: 'User 1' }, value: 5, mine: true }, { voter: { id: 'u2', name: 'User 2' }, value: 5 }, { voter: { id: 'u3', name: 'User 3' }, value: 3 }],
    }));
    const estimate = setBindingMock('SetTrackerEstimate', async () => applied());
    vi.stubGlobal('IntersectionObserver', class {
      constructor(private report: (entries: Array<{ isIntersecting: boolean }>) => void) {}
      observe(): void {
        this.report([{ isIntersecting: true }]);
      }
      disconnect(): void {}
    });
    try {
      const view = mount();
      const screen = await openCard(view, '1');
      await fireEvent.click(await within(screen).findByTestId('ticket-poker-fold'));
      await within(screen).findByTestId('ticket-poker-result');
      await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
      const items = await within(document.body).findAllByTestId('ticket-menu-poker-estimate');
      expect(items.map((i) => i.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Set estimate to 5 (2 votes)', 'Set estimate to 3 (1 vote)']);
      await fireEvent.click(items[0]!);
      await waitFor(() => expect(estimate).toHaveBeenCalledWith('c1', { board: '1', issue: '1', value: 5 }));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('says in the poker section that the site could not be asked, asks again on Retry and on the next opening', async () => {
    install();
    installTicket();
    const support = setBindingMock('TrackerPokerSupport', async () => ({ available: false, failure: { kind: 'transient', message: 'Jira did not answer', at: 1 } }));
    const view = mount();
    let screen = await openCard(view, '1');
    const failed = await within(screen).findByTestId('ticket-poker-support-error');
    expect(failed).toHaveTextContent('Could not find out whether this site has planning poker: Jira did not answer');
    expect(within(screen).queryByTestId('ticket-poker-fold')).toBeNull();

    await fireEvent.click(within(screen).getByTestId('ticket-poker-support-retry'));
    await waitFor(() => expect(support).toHaveBeenCalledTimes(2));
    await within(screen).findByTestId('ticket-poker-support-error');

    // The next opening asks again, and an answer offers poker.
    await fireEvent.click(view.getByTestId('ticket-close'));
    support.mockResolvedValue({ available: true });
    screen = await openCard(view, '1');
    await within(screen).findByTestId('ticket-poker-fold');
    expect(support).toHaveBeenCalledTimes(3);
    expect(within(screen).queryByTestId('ticket-poker-support-error')).toBeNull();
  });

  it('asks again whether the site has poker for an open ticket once its computer connects again', async () => {
    install();
    installTicket();
    const support = setBindingMock('TrackerPokerSupport', async () => ({ available: false }));
    const view = mount();
    const screen = await openCard(view, '1');
    await waitFor(() => expect(support).toHaveBeenCalledTimes(1));
    support.mockResolvedValue({ available: true });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await within(screen).findByTestId('ticket-poker-fold');
    expect(support).toHaveBeenCalledTimes(2);
  });

  it('offers no planning poker on a site without it', async () => {
    install();
    installTicket();
    const support = setBindingMock('TrackerPokerSupport', async () => ({ available: false }));
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findByTestId('ticket-comment-input');
    await waitFor(() => expect(support).toHaveBeenCalledWith('c1'));
    expect(within(screen).queryByTestId('ticket-poker-fold')).toBeNull();
  });

  it('links the ticket to one found by key on the side its phrase reads, taking no change while the link is out', async () => {
    install();
    installTicket();
    setBindingMock('TrackerLinkTypes', async () => [{ id: '10000', name: 'Blocks', outward: 'blocks', inward: 'is blocked by' }]);
    setBindingMock('TrackerLookup', async () => ({ found: [{ key: 'WEB-7', card: { id: '7', key: 'WEB-7', title: 'Issue 7' } }] }));
    let answer: ((view: WriteView) => void) | null = null;
    const link = setBindingMock('LinkTrackerIssues', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(await within(screen).findByTestId('ticket-link-add'));
    const form = await within(document.body).findByTestId('ticket-link-form');
    const relation = await within(form).findByTestId('ticket-link-form-relation');
    expect(Array.from((relation as HTMLSelectElement).options).map((o) => o.text)).toEqual(['blocks', 'is blocked by']);
    await fireEvent.change(relation, { target: { value: '1' } });
    expect(relation).toHaveDisplayValue('is blocked by');
    await fireEvent.click(within(form).getByTestId('ticket-link-form-find'));
    await fireEvent.input(await within(document.body).findByTestId('ticket-picker-find'), { target: { value: 'WEB-7' } });
    await fireEvent.click(await within(document.body).findByTestId('ticket-picker-item'));
    await waitFor(() => expect(within(form).getByTestId('ticket-link-form-other')).toHaveTextContent('WEB-7'));
    await fireEvent.click(within(form).getByTestId('ticket-link-form-save'));
    await waitFor(() => expect(link).toHaveBeenCalledWith('c1', { type: '10000', outward: '7', inward: '1' }));
    expect(relation).toBeDisabled();
    expect(within(form).getByTestId('ticket-link-form-find')).toBeDisabled();
    expect(within(form).getByTestId('ticket-link-form-save')).toBeDisabled();
    answer!(applied());
    await waitFor(() => expect(within(document.body).queryByTestId('ticket-link-form')).toBeNull());
  });

  it('reads the link types once per opening of the link form, keeping a refusal on screen until Retry', async () => {
    install();
    installTicket();
    // A read past the third never answers, so a loop that reads again on
    // each refusal stops there instead of running the worker out of memory.
    const types = setBindingMock('TrackerLinkTypes', () =>
      types.mock.calls.length > 3 ? new Promise<never>(() => {}) : Promise.reject(new Error('link types are hidden from you')),
    );
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(await within(screen).findByTestId('ticket-link-add'));
    const form = await within(document.body).findByTestId('ticket-link-form');
    await waitFor(() => expect(types).toHaveBeenCalled());
    for (let i = 0; i < 10; i++) await Promise.resolve();
    await tick();
    expect(types).toHaveBeenCalledTimes(1);
    expect(within(form).getByTestId('ticket-link-form-types-error')).toHaveTextContent('link types are hidden from you');

    await fireEvent.click(within(form).getByTestId('ticket-link-form-types-retry'));
    await waitFor(() => expect(types).toHaveBeenCalledTimes(2));
    expect(await within(form).findByTestId('ticket-link-form-types-error')).toBeInTheDocument();

    // Closed and opened again, the form reads them again.
    await fireEvent.click(within(form).getByTestId('ticket-link-form-cancel'));
    await waitFor(() => expect(within(document.body).queryByTestId('ticket-link-form')).toBeNull());
    types.mockImplementation(async () => [{ id: '10000', name: 'Blocks', outward: 'blocks', inward: 'is blocked by' }]);
    await fireEvent.click(within(screen).getByTestId('ticket-link-add'));
    const again = await within(document.body).findByTestId('ticket-link-form');
    await within(again).findByTestId('ticket-link-form-relation');
    expect(types).toHaveBeenCalledTimes(3);
  });

  it('removes a link at once from its row and says so until the ticket drops it', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      links: [{ id: '40001', type: '10000', relation: 'blocks', issue: { id: '6', key: 'WEB-6', title: 'Issue 6', status: { id: 's1', name: 'To Do', group: 'todo' } } }],
    }) as unknown as DetailView);
    let answer: ((view: WriteView) => void) | null = null;
    const unlink = setBindingMock('UnlinkTrackerIssues', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(await within(screen).findByTestId('ticket-link-remove'));
    expect(unlink).toHaveBeenCalledWith('c1', { issue: '1', link: '40001', other: '6' });
    expect(await within(screen).findByTestId('ticket-link-removing')).toHaveTextContent('Removing…');
    expect(within(screen).getByTestId('ticket-link-remove')).toBeDisabled();
    answer!(applied());
    await waitFor(() => expect(within(screen).queryByTestId('ticket-link-removing')).toBeNull());
  });

  it('removes the link its row menu was opened on', async () => {
    install();
    installTicket((issue) => ({
      ...ticketDetail(issue),
      links: [{ id: '40001', type: '10000', relation: 'blocks', issue: { id: '6', key: 'WEB-6', title: 'Issue 6', status: { id: 's1', name: 'To Do', group: 'todo' } } }],
    }) as unknown as DetailView);
    const unlink = setBindingMock('UnlinkTrackerIssues', async () => applied());
    const view = mount();
    const screen = await openCard(view, '1');
    await fireEvent.click(await within(screen).findByTestId('ticket-link-menu'));
    await fireEvent.click(await within(document.body).findByTestId('ticket-link-menu-remove'));
    await waitFor(() => expect(unlink).toHaveBeenCalledWith('c1', { issue: '1', link: '40001', other: '6' }));
  });

  it("opens the estimate's editor from the header, and Log time's form filled with the ticket and today", async () => {
    install();
    installTicket();
    const view = mount();
    const screen = await openCard(view, '1');
    const estimate = await within(screen).findByTestId('ticket-estimate');
    expect(estimate).toHaveAccessibleName('Estimate: 2');
    await fireEvent.click(estimate);
    const points = within(screen).getByTestId('ticket-rail').querySelector<HTMLElement>('[data-field="points"]')!;
    await waitFor(() => expect(within(points).getByTestId('ticket-field-input')).toHaveValue(2));

    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click(await within(document.body).findByTestId('ticket-menu-log-time'));
    const form = await within(document.body).findByTestId('worklog-form');
    expect(within(form).getByTestId('worklog-form-ticket')).toHaveTextContent('WEB-1');
    const now = new Date();
    const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    expect(within(form).getByTestId('worklog-form-day')).toHaveValue(today);
    expect(within(form).queryByTestId('worklog-form-find')).toBeNull();
  });

  it('keeps an open Log time with what was typed through a reconnect that answers the same account, and sends nothing until that answer', async () => {
    install();
    installTicket();
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findByTestId('ticket-comment-input');
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click(await within(document.body).findByTestId('ticket-menu-log-time'));
    const form = await within(document.body).findByTestId('worklog-form');
    await fireEvent.input(within(form).getByTestId('worklog-form-day'), { target: { value: '2026-11-20' } });
    await fireEvent.input(within(form).getByTestId('worklog-form-time'), { target: { value: '1h 30m' } });
    await fireEvent.input(within(form).getByTestId('worklog-form-description'), { target: { value: 'Wrote the report' } });
    const log = setBindingMock('AddTrackerWorklog', async (): Promise<WriteView> => ({ parts: [{ step: 'add', state: 'applied' }] }) as WriteView);
    let answer: ((sub: TicketSubscription) => void) | undefined;
    setBindingMock('SubscribeTrackerTicket', () => new Promise<TicketSubscription>((resolve) => { answer = resolve; }));

    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await waitFor(() => expect(answer).toBeDefined());
    // Not answered yet: the form, what was typed and the Jira mark stay,
    // and nothing is sent as an account not confirmed.
    const kept = within(document.body).getByTestId('worklog-form');
    expect(kept).toBe(form);
    expect(within(kept).getByTestId('worklog-form-day')).toHaveValue('2026-11-20');
    expect(within(kept).getByTestId('worklog-form-time')).toHaveValue('1h 30m');
    expect(within(kept).getByTestId('worklog-form-description')).toHaveValue('Wrote the report');
    expect(within(screen).getByTestId('ticket-type-mark')).toBeInTheDocument();
    expect(within(kept).getByTestId('worklog-form-save')).toBeDisabled();
    await fireEvent.submit(kept);
    await tick();
    expect(log).not.toHaveBeenCalled();

    answer!({ id: 'ticket-1-2', seq: 20, poll_every: 10_000, ...JIRA_USER1, loaded: true, detail: ticketDetail('1'), read_at: Date.now() } as TicketSubscription);
    await waitFor(() => expect(within(kept).getByTestId('worklog-form-save')).not.toBeDisabled());
    expect(within(kept).getByTestId('worklog-form-description')).toHaveValue('Wrote the report');
    await fireEvent.click(within(kept).getByTestId('worklog-form-save'));
    await waitFor(() => expect(log).toHaveBeenCalledWith('c1', { issue: '1', day: '2026-11-20', spent_seconds: 5400, description: 'Wrote the report' }));
  });

  it('closes an open Log time for good when an answer after a reconnect says there is no account', async () => {
    install();
    installTicket();
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findByTestId('ticket-comment-input');
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click(await within(document.body).findByTestId('ticket-menu-log-time'));
    await within(document.body).findByTestId('worklog-form');
    const reconnectAnswering = async (account: string, id: string) => {
      setBindingMock('SubscribeTrackerTicket', async (): Promise<TicketSubscription> =>
        ({ id, seq: 20, poll_every: 10_000, kind: 'jira', account, loaded: true, detail: ticketDetail('1'), read_at: Date.now() }) as TicketSubscription);
      __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
      __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
      await waitFor(() => expect(within(screen).getByTestId('ticket-comment-input')).toBeInTheDocument());
      await tick();
    };
    await reconnectAnswering('', 'ticket-1-2');
    expect(within(document.body).queryByTestId('worklog-form')).toBeNull();
    await reconnectAnswering('acc-user1', 'ticket-1-3');
    expect(within(document.body).queryByTestId('worklog-form')).toBeNull();
  });

  it('offers no Log time while the app does not know whose time it logs', async () => {
    install();
    installTicket(undefined, { kind: 'jira', account: '' });
    const view = mount();
    const screen = await openCard(view, '1');
    await within(screen).findByTestId('ticket-comment-input');
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    expect(await within(document.body).findByTestId('ticket-menu-log-time')).toHaveAttribute('aria-disabled', 'true');
  });
});

/** Switches the pane to `label` through its folded tab picker (jsdom
 * measures the pane 0 wide). */
async function switchTo(view: ReturnType<typeof mount>, label: 'Board' | 'Time'): Promise<void> {
  await fireEvent.click(await view.findByTestId('issues-tabs-picker'));
  const option = within(menu()).getAllByTestId('issues-tab-option').find((o) => o.textContent?.includes(label))!;
  await fireEvent.click(option);
}

describe('the Time screen in the pane', () => {
  beforeAll(async () => {
    await import('./TimeScreen.svelte');
    await import('./TicketScreen.svelte');
  });

  // On this week's Monday, which the screen opens on.
  const worklog = { id: '501', issue: '1', key: 'WEB-1', title: 'Issue 1', author: 'acc-user1', day: mondayOf(localDay(new Date())), spent_seconds: 3600, updated: 1 };

  it('switches to Time, reads the person\'s week on the board\'s connection, votes the board hidden, and opens on Time next', async () => {
    const mocks = install();
    const stored = setBindingMock('TrackerWorklogs', async () => ({ worklogs: [worklog] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [worklog] }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await switchTo(view, 'Time');
    await view.findByTestId('time-screen');
    await waitFor(() => expect(stored).toHaveBeenCalled());
    expect(stored.mock.calls[0].slice(0, 2)).toEqual(['c1', 'acc-user1']);
    // The board is not mounted on Time, and its subscription is voted hidden.
    expect(view.queryAllByTestId('issues-card')).toHaveLength(0);
    await waitFor(() => expect(votes(mocks.visible, 'sub-')).toEqual([false]));
    // Only mine, Display and Search are the board's.
    expect(view.queryByTestId('issues-only-mine')).toBeNull();
    expect(view.queryByTestId('issues-display')).toBeNull();
    expect(view.queryByTestId('issues-search')).toBeNull();
    expect(view.getByTestId('issues-tabs-picker')).toHaveTextContent('Time');
    view.unmount();
    const again = mount();
    expect(await again.findByTestId('time-screen')).toBeInTheDocument();
    again.unmount();
  });

  it("switches to the Board on the ticket's board for a ticket asked for from outside the pane, and opens on Time next", async () => {
    const mocks = install({ choices: [choice('1', { opens_first: true }), choice('5', { connection: 'c2', project: 'p2' })] });
    installTicket();
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [worklog] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [worklog] }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await switchTo(view, 'Time');
    await view.findByTestId('time-screen');
    const ask = (connection: string, issue: string) => requestTicketOpen({
      ref: { backend: HOME_BACKEND, connection, issue }, seed: { key: `WEB-${issue}`, title: 'Linked', card: null }, project: null, unplaced: vi.fn(),
    });
    // Of the board shown.
    ask('c1', '77');
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('77'));
    expect(view.getByTestId('issues-tabs-picker')).toHaveTextContent('Board');
    expect(view.queryByTestId('time-screen')).toBeNull();
    // Of another connection's board, from Time again.
    await switchTo(view, 'Time');
    await view.findByTestId('time-screen');
    ask('c2', '88');
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('88'));
    expect(mocks.subscribe).toHaveBeenLastCalledWith('p2', 'c2', '5', [], false);
    expect(view.getByTestId('issues-tabs-picker')).toHaveTextContent('Board');
    view.unmount();
    // The person's screen stays Time; the switch was the request's.
    const again = mount();
    expect(await again.findByTestId('time-screen')).toBeInTheDocument();
    again.unmount();
  });

  it('refreshes the week from the header', async () => {
    install();
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
    const refresh = setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [] }));
    const view = mount();
    await switchTo(view, 'Time');
    await view.findByTestId('time-toolbar');
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
    await fireEvent.click(view.getByTestId('issues-refresh'));
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  });

  it('opens an entry\'s ticket through the pane\'s one ticket host, and a switch of screen closes it', async () => {
    install();
    const ticket = installTicket();
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [worklog] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [worklog] }));
    const view = mount();
    await switchTo(view, 'Time');
    await fireEvent.click(await view.findByTestId('time-entry-key'));
    await waitFor(() => expect(view.getByTestId('ticket-screen').dataset.issue).toBe('1'));
    expect(ticket.subscribe).toHaveBeenCalledWith('c1', '1', true);
    await switchTo(view, 'Board');
    await waitFor(() => expect(view.queryByTestId('issues-ticket')).toBeNull());
    await view.findAllByTestId('issues-card');
  });

  it('says a Linear board has no time tracking, and reads nothing', async () => {
    install({ choices: [choice('1', { opens_first: true, connection_kind: 'linear' })] });
    const view = mount();
    await view.findAllByTestId('issues-card');
    await switchTo(view, 'Time');
    expect(await view.findByTestId('time-linear')).toBeInTheDocument();
    expect(getBindingMock('TrackerWorklogs')?.mock.calls ?? []).toHaveLength(0);
  });

  it('opens on Time with the saved board\'s computer not connected and says so, not that it is listing boards', async () => {
    stageBackend({ status: 'reconnecting' });
    setCarriedSessionScopes('laptop', ['tracker:read']);
    writeFromAnotherWindow(BOARD_KEY, boardRef('7', 'p9', 'laptop'));
    localStorage.setItem('agent-overflow:frontend:issues-screen', JSON.stringify('time'));
    install({ choices: [choice('1', { opens_first: true })] });
    const view = mount();
    expect(await view.findByTestId('time-offline')).toHaveTextContent('Laptop is not connected. Your time shows when it connects.');
    expect(view.queryByTestId('issues-time-listing')).toBeNull();
    view.unmount();
  });

  it('says the board is gone when its listing goes while Time shows it', async () => {
    let rows = [choice('1', { opens_first: true })];
    install();
    setBindingMock('TrackerBoardChoices', async () => rows);
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
    setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [] }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await switchTo(view, 'Time');
    await view.findByTestId('time-toolbar');
    rows = [];
    applyTrackerConnectionsChanged(HOME_BACKEND);
    expect(await view.findByTestId('time-board-gone')).toHaveTextContent('No project lists this board any more, or its connection was removed. Pick another board to see your time.');
    expect(view.queryByTestId('issues-time-listing')).toBeNull();
    view.unmount();
  });
});

describe('a ticket in a pane of its own', () => {
  beforeAll(async () => {
    await import('./TicketScreen.svelte');
    await import('./TicketPaneBody.svelte');
  });
  beforeEach(() => {
    setPaneLayoutItemsForTest([]);
  });

  function placeIssuesPane(): void {
    setCompactLayoutForTest(false);
    setPaneLayoutItemsForTest([{ id: PANE, paneId: PANE, kind: 'issues', widthPx: 600 }]);
    focusPane(PANE);
  }

  function ticketPaneIds(): string[] {
    return getPaneLayoutItems().filter((item) => item.kind === 'ticket').map((item) => item.paneId);
  }

  /** The ticket pane `paneId` as PaneHost mounts it. */
  function mountTicketPane(paneId: string) {
    return render(TicketPane, { paneId });
  }

  // Each render's queries are the whole body's: these read one pane's.
  async function paneScreen(pane: ReturnType<typeof mountTicketPane>, issue: string): Promise<HTMLElement> {
    return waitFor(() => {
      const screen = within(pane.container).getByTestId('ticket-screen');
      expect(screen.dataset.issue).toBe(issue);
      return screen;
    });
  }

  function ticketReleases(mocks: ReturnType<typeof install>): string[] {
    return mocks.release.mock.calls.map((c) => String(c[0])).filter((id) => id.startsWith('ticket-'));
  }

  function card(view: ReturnType<typeof mount>, issue: string): HTMLElement {
    return within(view.container).getAllByTestId('issues-card').find((c) => c.dataset.issue === issue)!;
  }

  /** The Issues pane's own ticket, null for none. */
  function issuesTicket(view: ReturnType<typeof mount>): HTMLElement | null {
    return within(view.container).queryByTestId('ticket-screen');
  }

  async function openHere(view: ReturnType<typeof mount>, issue: string): Promise<HTMLElement> {
    await within(view.container).findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, issue)).getByTestId('issues-card-open'));
    return waitFor(() => {
      const screen = issuesTicket(view)!;
      expect(screen.dataset.issue).toBe(issue);
      return screen;
    });
  }

  it('opens a card\'s ticket in a pane of its own on Ctrl or Cmd+click and from the card\'s menu, which takes the focus; a plain click opens it here', async () => {
    placeIssuesPane();
    install();
    const ticket = installTicket();
    const view = mount();
    await view.findAllByTestId('issues-card');

    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { ctrlKey: true });
    expect(ticketPaneIds()).toHaveLength(1);
    const first = ticketPaneIds()[0];
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual([PANE, first]);
    expect(getFocusedPaneId()).toBe(first);
    expect(issuesTicket(view)).toBeNull();
    const pane = mountTicketPane(first);
    const screen = await paneScreen(pane, '1');
    await waitFor(() => expect(document.activeElement).toBe(screen));
    // The pane's header is the ticket's: its key moves the pane, its close
    // is the pane's.
    expect(within(screen).getByTestId('ticket-key').tagName).toBe('BUTTON');
    expect(within(screen).queryByTestId('ticket-back')).toBeNull();

    // Cmd on macOS opens the same way; the ticket a pane shows is not
    // opened twice.
    focusPane(PANE);
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { metaKey: true });
    expect(ticketPaneIds()).toEqual([first]);
    expect(getFocusedPaneId()).toBe(first);
    await fireEvent.click(within(card(view, '2')).getByTestId('issues-card-open'), { metaKey: true });
    expect(ticketPaneIds()).toHaveLength(2);

    // The card's one menu has it behind Show more.
    await fireEvent.contextMenu(card(view, '2'));
    await fireEvent.click(await within(document.body).findByTestId('action-menu-more'));
    expect((await within(document.body).findByTestId('issues-card-menu-own-pane'))).toHaveTextContent('Open in its own pane');
    await fireEvent.click((await within(document.body).findByTestId('issues-card-menu-own-pane')));
    expect(ticketPaneIds()).toHaveLength(2);

    await fireEvent.click(within(card(view, '2')).getByTestId('issues-card-open'));
    await waitFor(() => expect(issuesTicket(view)?.dataset.issue).toBe('2'));
    expect(ticketPaneIds()).toHaveLength(2);
    expect(ticket.subscribe.mock.calls.filter((c) => c[1] === '1')).toHaveLength(1);
  });

  it('offers no pane of its own under the compact layout: Ctrl+click opens the ticket here and no menu names a pane', async () => {
    setCompactLayoutForTest(true);
    install();
    installTicket();
    const view = mount();
    const first = (await within(view.container).findAllByTestId('issues-card'))[0];
    await fireEvent.contextMenu(first);
    await fireEvent.click(await within(document.body).findByTestId('action-menu-more'));
    await within(document.body).findByTestId('issues-card-menu-priority');
    expect(within(document.body).queryByTestId('issues-card-menu-own-pane')).toBeNull();
    await fireEvent.click(within(document.body).getByTestId('issues-card-menu-open'));
    await waitFor(() => expect(issuesTicket(view)).not.toBeNull());
    await fireEvent.click(within(view.container).getByTestId('issues-back'));
    await waitFor(() => expect(issuesTicket(view)).toBeNull());
    await fireEvent.click(within(first).getByTestId('issues-card-open'), { ctrlKey: true });
    const screen = await waitFor(() => issuesTicket(view)!);
    expect(ticketPaneIds()).toEqual([]);
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await within(document.body).findByTestId('ticket-menu-status');
    expect(within(document.body).queryByTestId('ticket-menu-own-pane')).toBeNull();
    setCompactLayoutForTest(false);
  });

  it('opens a linked ticket and the parent in a pane of their own on Ctrl+click or the link\'s menu, and in place on a plain click', async () => {
    placeIssuesPane();
    install();
    const linked = (id: string) => ({ id: `l${id}`, type: '10000', relation: 'blocks', issue: { id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: 's1', name: 'To Do', group: 'todo' } } });
    installTicket((issue) => ({ ...ticketDetail(issue, { parent: '9' }), links: issue === '1' ? [linked('3'), linked('4')] : [] }) as unknown as DetailView);
    const view = mount();
    const screen = await openHere(view, '1');
    const links = () => within(screen).getAllByTestId('ticket-link');

    await waitFor(() => expect(links()).toHaveLength(2));
    await fireEvent.click(links()[0], { ctrlKey: true });
    expect(ticketPaneIds().map((id) => ticketPaneRecord(id)!.ticket.ref.issue)).toEqual(['3']);
    expect(issuesTicket(view)?.dataset.issue).toBe('1');

    await fireEvent.click(within(screen).getAllByTestId('ticket-link-menu')[1]);
    await fireEvent.click((await within(document.body).findByTestId('ticket-link-menu-own-pane')));
    await fireEvent.click(within(screen).getByTestId('ticket-parent'), { ctrlKey: true });
    expect(ticketPaneIds().map((id) => ticketPaneRecord(id)!.ticket.ref.issue)).toEqual(['3', '4', '9']);
    // Each keeps the board it was opened from.
    expect(ticketPaneRecord(ticketPaneIds()[2])).toMatchObject({ board: '1' });

    await fireEvent.click(links()[0]);
    await waitFor(() => expect(issuesTicket(view)?.dataset.issue).toBe('3'));
    expect(ticketPaneIds()).toHaveLength(3);
  });

  it('moves the ticket open here into a pane of its own with its draft and subscription, and the board shows here; the pane\'s close lets both go', async () => {
    placeIssuesPane();
    const mocks = install();
    const ticket = installTicket();
    const view = mount();
    const screen = await openHere(view, '1');
    const key = trackerTicketKey({ backend: HOME_BACKEND, connection: 'c1', issue: '1' });
    await fireEvent.input(await within(screen).findByTestId('ticket-comment-input'), { target: { value: 'Unsent on WEB-1' } });
    expect(ticketDraft(key, 'new-comment')?.text).toBe('Unsent on WEB-1');

    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click((await within(document.body).findByTestId('ticket-menu-own-pane')));
    await waitFor(() => expect(issuesTicket(view)).toBeNull());
    expect(within(view.container).getAllByTestId('issues-card')).toHaveLength(2);
    const [paneId] = ticketPaneIds();
    expect(getFocusedPaneId()).toBe(paneId);
    // Held through the move: nothing let go before the pane's body holds it.
    await tick();
    expect(ticketDraft(key, 'new-comment')?.text).toBe('Unsent on WEB-1');
    expect(ticketReleases(mocks)).toEqual([]);

    const pane = mountTicketPane(paneId);
    const moved = await paneScreen(pane, '1');
    expect(await within(moved).findByTestId('ticket-comment-input')).toHaveValue('Unsent on WEB-1');
    await waitFor(() => expect(document.activeElement).toBe(moved));
    expect(ticket.subscribe).toHaveBeenCalledTimes(1);
    expect(ticketReleases(mocks)).toEqual([]);

    await fireEvent.click(within(pane.container).getByTestId('pane-close'));
    expect(ticketPaneIds()).toEqual([]);
    await waitFor(() => expect(ticketReleases(mocks)).toEqual(['ticket-1-1']));
    expect(ticketDraft(key, 'new-comment')).toBeNull();
    pane.unmount();
  });

  it('gives Work the project the ticket had: a moved ticket keeps the one picked, a card takes its board\'s, a restored pane none until the person picks', async () => {
    placeIssuesPane();
    project('p1', 'alpha', 1);
    project('p2', 'beta', 2);
    setBindingMock('TrackerConnections', async () => [{ id: 'c1', kind: 'jira', site: 'https://example.atlassian.net', label: 'Work', enabled: true, account: 'User 1' }]);
    install({ choices: [choice('1', { project: 'p1', opens_first: true }), choice('1', { project: 'p2' })] });
    installTicket();
    const workOf = (root: HTMLElement) => waitFor(() => within(root).getByTestId('ticket-work'));
    const picked = (work: HTMLElement) => (within(work).getByTestId('ticket-work-project') as HTMLSelectElement).value;
    const view = mount();

    // Opened here under p1's board, the person picks p2, then moves it.
    const screen = await openHere(view, '1');
    const work = await workOf(screen);
    await waitFor(() => expect(picked(work)).toBe('p1'));
    await fireEvent.change(within(work).getByTestId('ticket-work-project'), { target: { value: 'p2' } });
    await waitFor(() => expect(picked(work)).toBe('p2'));
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click((await within(document.body).findByTestId('ticket-menu-own-pane')));
    await waitFor(() => expect(issuesTicket(view)).toBeNull());
    const [movedId] = ticketPaneIds();
    const moved = mountTicketPane(movedId);
    const movedWork = await workOf(await paneScreen(moved, '1'));
    await waitFor(() => expect(picked(movedWork)).toBe('p2'));

    // A card opened in a pane of its own: the board's project.
    focusPane(PANE);
    await fireEvent.click(within(card(view, '2')).getByTestId('issues-card-open'), { ctrlKey: true });
    const second = mountTicketPane(ticketPaneIds().find((id) => id !== movedId)!);
    const secondWork = await workOf(await paneScreen(second, '2'));
    await waitFor(() => expect(picked(secondWork)).toBe('p1'));

    // A pane restored after a reload saved no project: Ask waits for a pick.
    restoreTicketPane('ticket-restored', { backend: HOME_BACKEND, connection: 'c1', issue: '1' });
    setPaneLayoutItemsForTest([...getPaneLayoutItems(), { id: 'ticket-restored', paneId: 'ticket-restored', kind: 'ticket', widthPx: 600 }]);
    const restored = mountTicketPane('ticket-restored');
    const restoredWork = await workOf(await paneScreen(restored, '1'));
    await waitFor(() => expect(within(restoredWork).getByTestId('ticket-work-ask')).toBeDisabled());
    expect(picked(restoredWork)).toBe('');
    expect(within(restoredWork).getByTestId('ticket-work-ask')).toHaveAttribute('title', 'Pick the project its thread goes in.');
    await fireEvent.change(within(restoredWork).getByTestId('ticket-work-project'), { target: { value: 'p1' } });
    await waitFor(() => expect(within(restoredWork).getByTestId('ticket-work-ask')).not.toBeDisabled());
    for (const pane of [moved, second, restored]) pane.unmount();
  });

  it('lets go of a moved ticket\'s subscription when its pane closes before its body ever held it', async () => {
    placeIssuesPane();
    const mocks = install();
    installTicket();
    const view = mount();
    const screen = await openHere(view, '1');
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    await fireEvent.click((await within(document.body).findByTestId('ticket-menu-own-pane')));
    await waitFor(() => expect(issuesTicket(view)).toBeNull());
    expect(ticketReleases(mocks)).toEqual([]);
    closeTicketPane(ticketPaneIds()[0]);
    await waitFor(() => expect(ticketReleases(mocks)).toEqual(['ticket-1-1']));
  });

  it('shows one ticket\'s state in every pane showing it: one subscription, one draft, one write', async () => {
    placeIssuesPane();
    const mocks = install();
    const ticket = installTicket();
    let answer: ((view: WriteView) => void) | undefined;
    setBindingMock('EditTrackerIssue', () => new Promise<WriteView>((resolve) => { answer = resolve; }));
    const view = mount();
    await view.findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { ctrlKey: true });
    const pane = mountTicketPane(ticketPaneIds()[0]);
    const inPane = await paneScreen(pane, '1');
    const here = await openHere(view, '1');
    expect(ticket.subscribe).toHaveBeenCalledTimes(1);

    const description = (root: HTMLElement) => within(root).getAllByTestId('ticket-section').find((s) => s.dataset.field === 'description')!;
    await fireEvent.click(within(description(inPane)).getByTestId('ticket-section-edit'));
    await fireEvent.input(within(description(inPane)).getByTestId('ticket-section-editor'), { target: { value: 'Draft B' } });
    await waitFor(() => expect(within(description(here)).getByTestId('ticket-section-editor')).toHaveValue('Draft B'));
    await fireEvent.click(within(description(here)).getByTestId('ticket-section-save'));
    await waitFor(() => expect(answer).toBeDefined());
    await waitFor(() => expect(within(description(inPane)).getByTestId('ticket-section-save')).toBeDisabled());

    // The Issues pane lets go of it; the pane still shows it.
    await fireEvent.click(within(view.container).getByTestId('ticket-close'));
    await waitFor(() => expect(issuesTicket(view)).toBeNull());
    expect(ticketReleases(mocks)).toEqual([]);
    answer!({ parts: [{ step: 'edit', state: 'applied' }] } as WriteView);
    pane.unmount();
    closeTicketPane(ticketPaneIds()[0]);
    await waitFor(() => expect(ticketReleases(mocks)).toEqual(['ticket-1-1']));
  });

  it('keeps a refusal under its control while the ticket pane is on screen, and says it in a toast while the pane is off screen', async () => {
    setCompactLayoutForTest(false);
    install();
    installTicket();
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '3', name: 'Low' }] }));
    const answers: ((view: WriteView) => void)[] = [];
    setBindingMock('EditTrackerIssue', () => new Promise<WriteView>((resolve) => answers.push(resolve)));
    const refused = { parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'tracker', message: 'Priority is not on the edit screen' } }] } as WriteView;
    const toasts = () => getToasts().map((t) => t.message);
    // No board draws the ticket: only the ticket pane's controls do.
    setPaneLayoutItemsForTest([{ id: 'thread-1', paneId: 'thread-1', kind: 'thread', widthPx: 600 }]);
    focusPane('thread-1');
    openTicketPane({ ref: { backend: HOME_BACKEND, connection: 'c1', issue: '1' }, seed: { key: 'WEB-1', title: 'Issue 1', card: null }, project: null }, { board: '1' });
    const paneId = ticketPaneIds()[0];
    const pane = mountTicketPane(paneId);
    const screen = await paneScreen(pane, '1');
    const priority = () => within(screen).getAllByTestId('ticket-field').find((f) => f.dataset.field === 'priority')!;
    const pickLow = async () => {
      await fireEvent.click(within(priority()).getByTestId('ticket-field-value'));
      await fireEvent.click(await waitFor(() => within(menu()).getByTestId('ticket-picker-item')));
    };
    const before = toasts().length;
    await pickLow();
    await waitFor(() => expect(answers).toHaveLength(1));
    answers[0](refused);
    await waitFor(() => expect(within(priority()).getByTestId('ticket-write-outcome')).toHaveTextContent('Priority is not on the edit screen'));
    expect(toasts().length).toBe(before);

    await pickLow();
    await waitFor(() => expect(answers).toHaveLength(2));
    // The thread pane fills the window: the ticket pane is off screen.
    expect(promotePane('thread-1')).toBe(true);
    answers[1](refused);
    await waitFor(() => expect(toasts().slice(before)).toEqual(['The tracker refused the change to WEB-1: Priority is not on the edit screen']));
    demotePane();
    expect(within(priority()).queryByTestId('ticket-write-outcome')).toBeNull();
  });

  it('steps back on Escape through the tickets the pane opened, and never closes the pane', async () => {
    placeIssuesPane();
    install();
    installTicket((issue) => ticketDetail(issue, { parent: String(Number(issue) + 1) }));
    clearCommandRegistry();
    registerIssuesCommands();
    const view = mount();
    await view.findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { ctrlKey: true });
    const paneId = ticketPaneIds()[0];
    const pane = mountTicketPane(paneId);
    await fireEvent.click(within(await paneScreen(pane, '1')).getByTestId('ticket-parent'));
    const second = await paneScreen(pane, '2');
    await waitFor(() => expect(document.activeElement).toBe(second));
    expect(getFocusedPaneId()).toBe(paneId);
    const escape = () => runCommand('issues.escape', makeCommandContext(null, {}));
    const flags = () => makeCommandContext(null, {}).flags;
    expect(flags()).toMatchObject({ issuesPaneFocused: true, toolDetailOpen: true });

    escape();
    const first = await paneScreen(pane, '1');
    await waitFor(() => expect(document.activeElement).toBe(first));
    expect(flags()).toMatchObject({ issuesPaneFocused: true, toolDetailOpen: false });
    escape();
    await tick();
    expect(ticketPaneIds()).toEqual([paneId]);
    expect(within(pane.container).getByTestId('ticket-screen').dataset.issue).toBe('1');
    clearCommandRegistry();
  });

  it('comes back after a reload on its ticket without taking the focus', async () => {
    placeIssuesPane();
    install();
    installTicket();
    restoreTicketPane('ticket-r', { backend: HOME_BACKEND, connection: 'c1', issue: '5' });
    setPaneLayoutItemsForTest([
      { id: PANE, paneId: PANE, kind: 'issues', widthPx: 600 },
      { id: 'ticket-r', paneId: 'ticket-r', kind: 'ticket', widthPx: 600 },
    ]);
    const start = document.activeElement;
    const pane = mountTicketPane('ticket-r');
    await paneScreen(pane, '5');
    await tick();
    expect(document.activeElement).toBe(start);
  });

  it('says so when its saved ticket did not read back, and closes', async () => {
    setPaneLayoutItemsForTest([{ id: 'ticket-x', paneId: 'ticket-x', kind: 'ticket', widthPx: 600 }]);
    const pane = mountTicketPane('ticket-x');
    expect(within(pane.container).getByTestId('ticket-pane-unknown')).toHaveTextContent('could not be read back');
    await fireEvent.click(within(pane.container).getByRole('button', { name: /close/i }));
    expect(ticketPaneIds()).toEqual([]);
  });

  it('says so when the connection it names is gone', async () => {
    install();
    setBindingMock('SubscribeTrackerTicket', async () => { throw new TransportError('tracker_connection_missing', 'The tracker connection c9 was removed.'); });
    restoreTicketPane('ticket-c', { backend: HOME_BACKEND, connection: 'c9', issue: '5' });
    setPaneLayoutItemsForTest([{ id: 'ticket-c', paneId: 'ticket-c', kind: 'ticket', widthPx: 600 }]);
    const pane = mountTicketPane('ticket-c');
    expect(await within(pane.container).findByTestId('ticket-ended-empty')).toHaveTextContent('its connection was turned off or removed');
  });

  it('says in a sentence that a computer gone from the window, or too old to read trackers, cannot show it', async () => {
    install();
    const ticket = installTicket();
    restoreTicketPane('ticket-g', { backend: 'remote-gone', connection: 'c1', issue: '5' });
    setPaneLayoutItemsForTest([{ id: 'ticket-g', paneId: 'ticket-g', kind: 'ticket', widthPx: 600 }]);
    const gone = mountTicketPane('ticket-g');
    expect(await within(gone.container).findByTestId('ticket-computer-gone')).toHaveTextContent('cannot be shown');
    gone.unmount();

    await pairWithScopes([...SCOPES_AT_V0_0_19]);
    try {
      restoreTicketPane('ticket-o', { backend: HOME_BACKEND, connection: 'c1', issue: '5' });
      setPaneLayoutItemsForTest([{ id: 'ticket-o', paneId: 'ticket-o', kind: 'ticket', widthPx: 600 }]);
      const older = mountTicketPane('ticket-o');
      expect(await within(older.container).findByTestId('ticket-computer-gone')).toHaveTextContent('does not let it read issue trackers');
      expect(ticket.subscribe).not.toHaveBeenCalled();
    } finally {
      resetToLocalPage();
    }
  });

  it('keeps the pane\'s header and its close in every state of its body: a computer gone, too old, or not connected while its grants are pending', async () => {
    install();
    const ticket = installTicket();
    const closes = async (paneId: string, sentence: string) => {
      setPaneLayoutItemsForTest([{ id: paneId, paneId, kind: 'ticket', widthPx: 600 }]);
      const pane = mountTicketPane(paneId);
      expect(await within(pane.container).findByTestId(sentence)).toBeInTheDocument();
      expect(within(pane.container).getByTestId('tool-pane-header')).toBeInTheDocument();
      expect(within(pane.container).getByTestId('pane-promote')).toBeInTheDocument();
      await fireEvent.click(within(pane.container).getByTestId('pane-close'));
      expect(ticketPaneIds()).toEqual([]);
      pane.unmount();
    };

    restoreTicketPane('ticket-g', { backend: 'remote-gone', connection: 'c1', issue: '5' });
    await closes('ticket-g', 'ticket-computer-gone');

    stageBackend({ status: 'reconnecting' });
    try {
      restoreTicketPane('ticket-w', { backend: 'laptop', connection: 'c1', issue: '5' });
      await closes('ticket-w', 'ticket-computer-offline');
    } finally {
      resetStagedBackends();
    }

    await pairWithScopes([...SCOPES_AT_V0_0_19]);
    try {
      restoreTicketPane('ticket-o', { backend: HOME_BACKEND, connection: 'c1', issue: '5' });
      await closes('ticket-o', 'ticket-computer-gone');
    } finally {
      resetToLocalPage();
    }
    expect(ticket.subscribe).not.toHaveBeenCalled();
  });

  it('says so in the pane when its computer is not connected, and not that the ticket is opening', async () => {
    install();
    installTicket();
    stageBackend({ status: 'reconnecting' });
    try {
      restoreTicketPane('ticket-w', { backend: 'laptop', connection: 'c1', issue: '5' });
      setPaneLayoutItemsForTest([{ id: 'ticket-w', paneId: 'ticket-w', kind: 'ticket', widthPx: 600 }]);
      const pane = mountTicketPane('ticket-w');
      expect(await within(pane.container).findByTestId('ticket-computer-offline')).toHaveTextContent('Laptop is not connected');
      expect(within(pane.container).queryByText(/Opening the ticket/)).toBeNull();
    } finally {
      resetStagedBackends();
    }
  });

  it('maximizes from its header: the shared control, and a double-click on its key', async () => {
    placeIssuesPane();
    install();
    installTicket();
    const view = mount();
    await within(view.container).findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { ctrlKey: true });
    const [paneId] = ticketPaneIds();
    const pane = mountTicketPane(paneId);
    const screen = await paneScreen(pane, '1');
    await fireEvent.click(within(pane.container).getByTestId('pane-promote'));
    expect(getPromotedPaneId()).toBe(paneId);
    await fireEvent.click(within(pane.container).getByTestId('pane-promote'));
    expect(getPromotedPaneId()).toBeNull();
    await fireEvent.dblClick(within(screen).getByTestId('ticket-key'));
    expect(getPromotedPaneId()).toBe(paneId);
    await fireEvent.dblClick(within(screen).getByTestId('ticket-key'));
    expect(getPromotedPaneId()).toBeNull();
  });

  it('reads its tracker and account from its subscription: none of its own controls before it answers, all of them after, with the connections read failed and the board list unanswered', async () => {
    placeIssuesPane();
    writeFromAnotherWindow(BOARD_KEY, boardRef('1'));
    install();
    installTicket();
    setBindingMock('TrackerBoardChoices', () => new Promise<BoardChoice[]>(() => {}));
    setBindingMock('TrackerConnections', async () => { throw new TransportError('internal', 'The connections could not be read.'); });
    let answer: ((sub: TicketSubscription) => void) | undefined;
    setBindingMock('SubscribeTrackerTicket', () => new Promise<TicketSubscription>((resolve) => { answer = resolve; }));
    const view = mount();
    await within(view.container).findAllByTestId('issues-card');
    await fireEvent.click(within(card(view, '1')).getByTestId('issues-card-open'), { ctrlKey: true });
    const [paneId] = ticketPaneIds();
    const pane = mountTicketPane(paneId);
    const screen = await paneScreen(pane, '1');
    await waitFor(() => expect(answer).toBeDefined());
    // Not answered yet: neither tracker's own controls.
    expect(within(screen).queryByTestId('ticket-type-mark')).toBeNull();
    expect(within(screen).queryByTestId('ticket-estimate')).toBeNull();
    answer!({ id: 'ticket-1-1', seq: 10, poll_every: 10_000, ...JIRA_USER1, loaded: true, detail: ticketDetail('1'), read_at: Date.now() } as TicketSubscription);
    // With the ticket's answer, at once: the Jira controls and Log time.
    await waitFor(() => expect(within(screen).getByTestId('ticket-type-mark')).toBeInTheDocument());
    expect(within(screen).getByTestId('ticket-estimate')).toBeInTheDocument();
    await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
    expect(await within(document.body).findByTestId('ticket-menu-log-time')).not.toHaveAttribute('aria-disabled', 'true');
    expect(within(document.body).getByTestId('ticket-menu-attach')).toBeInTheDocument();
    // Nothing of the tracker is kept with the pane.
    expect(ticketPaneCarrier(paneId)).toEqual({ backend: HOME_BACKEND, connection: 'c1', issue: '1' });
  });

  it('shows a restored pane\'s key and title once its subscription answers, and names it Ticket before', async () => {
    install();
    let answer: ((sub: TicketSubscription) => void) | undefined;
    setBindingMock('SubscribeTrackerTicket', () => new Promise<TicketSubscription>((resolve) => { answer = resolve; }));
    restoreTicketPane('ticket-r', { backend: HOME_BACKEND, connection: 'c1', issue: '5' });
    setPaneLayoutItemsForTest([{ id: 'ticket-r', paneId: 'ticket-r', kind: 'ticket', widthPx: 600 }]);
    const pane = mountTicketPane('ticket-r');
    const screen = await paneScreen(pane, '5');
    expect(within(screen).getByTestId('ticket-key')).toHaveTextContent('Ticket');
    expect(within(screen).getByTestId('ticket-title')).toHaveTextContent('');
    await waitFor(() => expect(answer).toBeDefined());
    answer!({ id: 'ticket-5-1', seq: 10, poll_every: 10_000, ...JIRA_USER1, loaded: true, detail: ticketDetail('5'), read_at: Date.now() } as TicketSubscription);
    await waitFor(() => expect(within(screen).getByTestId('ticket-key')).toHaveTextContent('WEB-5'));
    expect(within(screen).getByTestId('ticket-title')).toHaveTextContent('Issue 5');
  });

  it('copies no key on a restored pane before its ticket is read, and copies its key once it is', async () => {
    install();
    let answer: ((sub: TicketSubscription) => void) | undefined;
    setBindingMock('SubscribeTrackerTicket', () => new Promise<TicketSubscription>((resolve) => { answer = resolve; }));
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);
    try {
      restoreTicketPane('ticket-r', { backend: HOME_BACKEND, connection: 'c1', issue: '5' });
      setPaneLayoutItemsForTest([{ id: 'ticket-r', paneId: 'ticket-r', kind: 'ticket', widthPx: 600 }]);
      const pane = mountTicketPane('ticket-r');
      const screen = await paneScreen(pane, '5');
      await waitFor(() => expect(answer).toBeDefined());
      await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
      const copyKey = await within(document.body).findByTestId('ticket-menu-copy-key');
      expect(copyKey).toHaveAttribute('aria-disabled', 'true');
      await fireEvent.click(copyKey);
      await tick();
      expect(writeText).not.toHaveBeenCalled();
      await fireEvent.keyDown(document.body, { key: 'Escape' });
      await waitFor(() => expect(within(document.body).queryByTestId('ticket-menu-copy-key')).toBeNull());

      // Answered, but not read yet: still no key.
      answer!({ id: 'ticket-5-1', seq: 10, poll_every: 10_000, ...JIRA_USER1, loaded: false, read_at: 0 } as unknown as TicketSubscription);
      await waitFor(() => expect(within(screen).getByTestId('ticket-type-mark')).toBeInTheDocument());
      await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
      expect(await within(document.body).findByTestId('ticket-menu-copy-key')).toHaveAttribute('aria-disabled', 'true');
      await fireEvent.keyDown(document.body, { key: 'Escape' });
      await waitFor(() => expect(within(document.body).queryByTestId('ticket-menu-copy-key')).toBeNull());

      // Read: its key copies.
      setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: ticketDetail('5'), account: JIRA_USER1.account }));
      trackerIssueUpdated({ connection: 'c1', issue: '5', seq: 11, changed: true }, HOME_BACKEND);
      await waitFor(() => expect(within(screen).getByTestId('ticket-key')).toHaveTextContent('WEB-5'));
      await fireEvent.click(within(screen).getByTestId('ticket-menu-button'));
      await fireEvent.click(await within(document.body).findByTestId('ticket-menu-copy-key'));
      await waitFor(() => expect(writeText).toHaveBeenCalledWith('WEB-5'));
      expect(writeText).toHaveBeenCalledTimes(1);
    } finally {
      writeText.mockRestore();
    }
  });

  it('offers the parent\'s own actions on a right-click of it, in the Issues pane and in a ticket pane, never the ticket that shows it', async () => {
    placeIssuesPane();
    install();
    installTicket((issue) => ticketDetail(issue, { parent: String(Number(issue) + 10) }));
    const view = mount();
    const here = await openHere(view, '1');
    await fireEvent.contextMenu(within(here).getByTestId('ticket-parent'));
    expect(within(document.body).queryByTestId('ticket-menu-own-pane')).toBeNull();
    expect(await within(document.body).findByTestId('ticket-parent-menu-copy-key')).toHaveTextContent('Copy key');
    await fireEvent.click(within(document.body).getByTestId('ticket-parent-menu-own-pane'));
    expect(ticketPaneIds().map((id) => ticketPaneRecord(id)!.ticket.ref.issue)).toEqual(['11']);
    expect(issuesTicket(view)?.dataset.issue).toBe('1');

    const pane = mountTicketPane(ticketPaneIds()[0]);
    const inPane = await paneScreen(pane, '11');
    await fireEvent.contextMenu(within(inPane).getByTestId('ticket-parent'));
    await fireEvent.click(await within(document.body).findByTestId('ticket-parent-menu-open'));
    await paneScreen(pane, '21');
    expect(ticketPaneIds()).toHaveLength(1);
    expect(issuesTicket(view)?.dataset.issue).toBe('1');
  });
});
