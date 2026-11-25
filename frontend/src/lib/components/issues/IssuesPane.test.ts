// The Issues pane over mocked bindings: the board it opens on, its columns
// and cards, Display, Only mine and Search over the held cards (reading a
// column whole when filtered), the notices of failure, refresh and end,
// the empty pane, the compact strip, the picker and the hidden vote.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import type { BoardChoice, BoardView, RowView, Section, SectionPage } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
import IssuesPane from './IssuesPane.svelte';
import { trackerBoardUpdated as applyTrackerBoardEvent } from '../../stores/trackerBoardEvents';
import { hiddenCardFields } from '../../stores/issuesPane.svelte';
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
import { demotePane, promotePane, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { noteProject } from '../../transport/entityIndex';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { HOME_BACKEND } from '../../transport/backendKey';
import { TransportError } from '../../transport/wsClient';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

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
          assignee: { id: 'acc-user1', name: 'User 1' }, status_since: Date.now() - 3 * DAY - 1000,
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
