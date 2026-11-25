import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import TrackerBoardsSettings from './TrackerBoardsSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { getPinnedBackend } from '../../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import { DisconnectedError, TransportError } from '../../transport/wsClient';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { SCOPES_AT_V0_0_19, answerAsV0019, methodNotFound } from '../../../test/helpers/olderComputer';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { addProjectLocal } from '../../stores/projects.svelte';
import { openTrackerBoardsSettings } from '../../stores/settingsOverlay.svelte';
import { applyTrackerConnectionsChanged } from '../../stores/trackerConnections.svelte';
import type { TrackerBoardChoice, TrackerProjectBoard } from '../../stores/bindings';

/** Each project's stored list: what ProjectTrackerBoards answers and SetProjectTrackerBoards replaces. */
let stored: Record<string, TrackerBoardChoice[]> = {};

function asStored(project: string, boards: TrackerProjectBoard[]): TrackerBoardChoice[] {
  return boards.map((b) =>
    trackerBoardChoice({
      project,
      connection: b.connection,
      board: b.board,
      name: b.name ?? '',
      tracker_project: b.tracker_project ?? '',
      opens_first: b.opens_first ?? false,
    }),
  );
}

let saves: TrackerProjectBoard[][] = [];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  for (const [id, name] of [['p1', 'web'], ['p2', 'ops']]) {
    noteProject(id, '');
    addProjectLocal({ id, name, path: `/repos/${name}`, createdAt: 1, updatedAt: 1, sortPosition: id === 'p1' ? 0 : 1, archived: false });
  }
  stored = { p1: [], p2: [] };
  saves = [];
  setBindingMock('TrackerConnections', async () => [
    trackerConnection(),
    trackerConnection({ id: 'tempo-1', kind: 'tempo', parentId: 'jira-1', account: '', site: 'https://api.tempo.io' }),
    trackerConnection({ id: 'lin-off', kind: 'linear', site: 'example', account: '', enabled: false }),
  ]);
  setBindingMock('ProjectTrackerBoards', async (project: string) => stored[project]);
  setBindingMock('SetProjectTrackerBoards', async (project: string, _held: unknown, boards: TrackerProjectBoard[]) => {
    saves.push(boards);
    stored[project] = asStored(project, boards);
    return stored[project];
  });
});

/** Mounts the page on project p1. */
async function renderOnP1(): Promise<void> {
  render(TrackerBoardsSettings);
  const select = (await screen.findByTestId('settings-tracker-boards-project')) as HTMLSelectElement;
  await fireEvent.change(select, { target: { value: 'p1' } });
}

const names = (): string[] => screen.queryAllByTestId('tracker-board-name').map((el) => el.textContent ?? '');

describe('<TrackerBoardsSettings>', () => {
  it('lists a board from a connection’s project, and the first one added opens first', async () => {
    const targets: (BackendKey | null)[] = [];
    const projects = setBindingMock('TrackerProjects', async () => {
      targets.push(getPinnedBackend());
      return [
        { id: '10000', key: 'WEB', name: 'Web' },
        { id: '10001', key: 'OPS', name: 'Operations' },
      ];
    });
    const boards = setBindingMock('TrackerBoards', async () => {
      targets.push(getPinnedBackend());
      return [{ id: '1', project: 'WEB', name: 'WEB board', plans_sprints: true }];
    });
    await renderOnP1();
    await screen.findByTestId('settings-tracker-boards-empty');

    await fireEvent.click(screen.getByTestId('tracker-board-add-open'));
    // Only Jira and Linear connections that are on can list boards; the one there is is chosen.
    const select = screen.getByTestId('tracker-board-add-connection') as HTMLSelectElement;
    expect([...select.options].filter((o) => !o.disabled).map((o) => o.value)).toEqual(['jira-1']);
    await waitFor(() => expect(projects).toHaveBeenCalledWith('jira-1'));

    await fireEvent.input(await screen.findByTestId('tracker-board-add-filter'), { target: { value: 'oper' } });
    expect(screen.getAllByTestId('tracker-board-add-project').map((el) => el.textContent?.trim())).toEqual(['OPS · Operations']);
    await fireEvent.input(screen.getByTestId('tracker-board-add-filter'), { target: { value: 'web' } });
    await fireEvent.click(screen.getByTestId('tracker-board-add-project'));
    await waitFor(() => expect(boards).toHaveBeenCalledWith('jira-1', '10000'));
    await fireEvent.click(await screen.findByTestId('tracker-board-add-board'));

    await waitFor(() => expect(names()).toEqual(['WEB board']));
    expect(saves).toEqual([[{ connection: 'jira-1', board: '1', name: 'WEB board', tracker_project: 'WEB', opens_first: true }]]);
    expect(screen.getByTestId('tracker-board-opens-first')).toBeTruthy();
    await waitFor(() => expect(screen.queryByTestId('tracker-board-picker')).toBeNull());
    expect(targets).toEqual([HOME_BACKEND, HOME_BACKEND]);
  });

  it('marks a board the project lists already, and does not offer it again', async () => {
    stored.p1 = [trackerBoardChoice({ opens_first: true })];
    setBindingMock('TrackerProjects', async () => [{ id: '10000', key: 'WEB', name: 'Web' }]);
    setBindingMock('TrackerBoards', async () => [
      { id: '1', name: 'WEB board', plans_sprints: true },
      { id: '2', name: 'Second board', plans_sprints: false },
    ]);
    await renderOnP1();
    await fireEvent.click(await screen.findByTestId('tracker-board-add-open'));
    await fireEvent.click(await screen.findByTestId('tracker-board-add-project'));
    const offered = await screen.findAllByTestId('tracker-board-add-board');
    expect(offered.map((b) => [b.textContent?.trim(), (b as HTMLButtonElement).disabled])).toEqual([
      ['WEB board · listed', true],
      ['Second board', false],
    ]);
    await fireEvent.click(offered[1]);
    await waitFor(() => expect(names()).toEqual(['WEB board', 'Second board']));
    expect(saves[0].map((b) => [b.board, b.opens_first])).toEqual([['1', true], ['2', false]]);
  });

  it('shows the tracker being too slow as its sentence, and Try again asks again', async () => {
    const answers: (() => Promise<unknown>)[] = [
      async () => {
        throw new TransportError(
          'method_budget_exceeded',
          "Listing the tracker's projects took longer than 1m0s, so it was stopped; the tracker was too slow, so try again.",
          { detail: { ref: 'r', method: 'TrackerProjects', at: 1, chain: ['context deadline exceeded'] } },
        );
      },
      async () => [{ id: '10000', key: 'WEB', name: 'Web' }],
    ];
    const projects = setBindingMock('TrackerProjects', () => answers.shift()!());
    await renderOnP1();
    await fireEvent.click(await screen.findByTestId('tracker-board-add-open'));
    expect((await screen.findByTestId('tracker-board-add-projects-error')).textContent).toBe(
      "Listing the tracker's projects took longer than 1m0s, so it was stopped; the tracker was too slow, so try again.",
    );
    await fireEvent.click(screen.getByTestId('tracker-board-add-projects-retry'));
    await screen.findByTestId('tracker-board-add-project');
    expect(projects).toHaveBeenCalledTimes(2);
  });

  it('reorders, chooses which board opens first and removes, saving the whole list each time', async () => {
    stored.p1 = [
      trackerBoardChoice({ board: '1', name: 'A', opens_first: true }),
      trackerBoardChoice({ board: '2', name: 'B' }),
      trackerBoardChoice({ board: '3', name: 'C', connection: 'lin-off', connection_enabled: false }),
    ];
    await renderOnP1();
    await waitFor(() => expect(names()).toEqual(['A', 'B', 'C']));
    const rows = () => screen.getAllByTestId('tracker-board-row');
    expect(within(rows()[2]).getByTestId('tracker-board-connection-off')).toBeTruthy();
    expect((within(rows()[0]).getByTestId('tracker-board-up') as HTMLButtonElement).disabled).toBe(true);
    expect((within(rows()[2]).getByTestId('tracker-board-down') as HTMLButtonElement).disabled).toBe(true);

    await fireEvent.click(within(rows()[0]).getByTestId('tracker-board-down'));
    await waitFor(() => expect(names()).toEqual(['B', 'A', 'C']));
    await waitFor(() => expect((within(rows()[2]).getByTestId('tracker-board-open-first') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(within(rows()[2]).getByTestId('tracker-board-open-first'));
    await waitFor(() => expect(within(rows()[2]).queryByTestId('tracker-board-opens-first')).toBeTruthy());
    await waitFor(() => expect((within(rows()[2]).getByTestId('tracker-board-remove') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(within(rows()[2]).getByTestId('tracker-board-remove'));
    await waitFor(() => expect(names()).toEqual(['B', 'A']));

    expect(saves.map((list) => list.map((b) => `${b.board}${b.opens_first ? '*' : ''}`))).toEqual([
      ['2', '1*', '3'],
      ['2', '1', '3*'],
      ['2*', '1'],
    ]);
  });

  it('shows a refused save under the list and keeps the stored list', async () => {
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true })];
    setBindingMock('SetProjectTrackerBoards', async () => {
      throw new TransportError('tracker_connection_missing', 'this tracker connection was removed from this computer; add it again in the Issues settings, or choose another board', {
        detail: { ref: 'r', method: 'SetProjectTrackerBoards', at: 1, chain: [] },
      });
    });
    await renderOnP1();
    await fireEvent.click(await screen.findByTestId('tracker-board-remove'));
    expect((await screen.findByTestId('settings-tracker-boards-save-error')).textContent).toBe(
      'This tracker connection was removed from this computer; add it again in the Issues settings, or choose another board.',
    );
    expect(names()).toEqual(['A']);
  });

  it('says the boards could not be read, offers Retry and no edit, and shows the list once a read answers', async () => {
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true })];
    let fail = true;
    setBindingMock('ProjectTrackerBoards', async (project: string) => {
      if (fail) throw new TransportError('method_error', 'store: disk I/O error', { detail: { ref: 'r', method: 'ProjectTrackerBoards', at: 1, chain: ['store: disk I/O error'] } });
      return stored[project];
    });
    await renderOnP1();
    expect((await screen.findByTestId('settings-tracker-boards-error')).textContent).toBe('The boards could not be read: Store: disk I/O error.');
    // A read that failed is not "no boards", and nothing can be added from it.
    expect(screen.queryByTestId('settings-tracker-boards-empty')).toBeNull();
    expect(screen.queryByTestId('tracker-board-add-open')).toBeNull();
    fail = false;
    await fireEvent.click(screen.getByTestId('settings-tracker-boards-retry'));
    await waitFor(() => expect(names()).toEqual(['A']));
    expect(screen.queryByTestId('settings-tracker-boards-error')).toBeNull();
    expect((screen.getByTestId('tracker-board-remove') as HTMLButtonElement).disabled).toBe(false);
  });

  it('shows the stored list and says the boards changed when a save was built from an older list', async () => {
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true }), trackerBoardChoice({ board: '2', name: 'B' })];
    await renderOnP1();
    await waitFor(() => expect(names()).toEqual(['A', 'B']));
    // Another window removes B; this page still shows it, and removes A.
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true })];
    setBindingMock('SetProjectTrackerBoards', async () => {
      throw new TransportError('tracker_boards_changed', "this project's boards changed after this list was read; the list shown now is the saved one, so make the change again", {
        detail: { ref: 'r', method: 'SetProjectTrackerBoards', at: 1, chain: [] },
      });
    });
    await fireEvent.click(screen.getAllByTestId('tracker-board-remove')[0]);
    expect((await screen.findByTestId('settings-tracker-boards-save-error')).textContent).toBe(
      "This project's boards changed after this list was read; the list shown now is the saved one, so make the change again.",
    );
    await waitFor(() => expect(names()).toEqual(['A']));
  });

  it('after a save that failed, reads the list again and offers no edit until it answers', async () => {
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true }), trackerBoardChoice({ board: '2', name: 'B' })];
    await renderOnP1();
    await waitFor(() => expect(names()).toEqual(['A', 'B']));
    let answer!: (list: TrackerBoardChoice[]) => void;
    setBindingMock('ProjectTrackerBoards', () => new Promise<TrackerBoardChoice[]>((resolve) => (answer = resolve)));
    // The save stored the list and failed reading it back.
    setBindingMock('SetProjectTrackerBoards', async () => {
      stored.p1 = [trackerBoardChoice({ board: '2', name: 'B', opens_first: true })];
      throw new TransportError('method_error', 'trackerapp: the boards of project p1 are saved, and reading them back failed, so read them again', {
        detail: { ref: 'r', method: 'SetProjectTrackerBoards', at: 1, chain: [] },
      });
    });
    await fireEvent.click(screen.getAllByTestId('tracker-board-remove')[0]);
    await screen.findByTestId('settings-tracker-boards-save-error');
    for (const button of screen.getAllByTestId('tracker-board-remove')) expect((button as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('tracker-board-add-open') as HTMLButtonElement).disabled).toBe(true);
    answer(stored.p1);
    await waitFor(() => expect(names()).toEqual(['B']));
    expect((screen.getByTestId('tracker-board-remove') as HTMLButtonElement).disabled).toBe(false);
  });

  it('shows the project a deep link names, then each project’s own boards', async () => {
    stored.p1 = [trackerBoardChoice({ board: '9', name: 'Web board' })];
    render(TrackerBoardsSettings);
    const select = (await screen.findByTestId('settings-tracker-boards-project')) as HTMLSelectElement;
    // Without a link the page opens on another project.
    await waitFor(() => expect(select.value).toBe('p2'));
    openTrackerBoardsSettings('p1', HOME_BACKEND);
    await waitFor(() => expect(names()).toEqual(['Web board']));
    expect(select.value).toBe('p1');

    await fireEvent.change(select, { target: { value: 'p2' } });
    await screen.findByTestId('settings-tracker-boards-empty');
  });

  it('shows a listed board’s connection as the connection list has it, which follows another window', async () => {
    stored.p1 = [trackerBoardChoice({ board: '1', name: 'A', opens_first: true, connection_label: '' })];
    await renderOnP1();
    await waitFor(() => expect(names()).toEqual(['A']));
    expect(screen.queryByTestId('tracker-board-connection-off')).toBeNull();
    // Another window turns the connection off and renames it; this page hears the nudge.
    setBindingMock('TrackerConnections', async () => [trackerConnection({ enabled: false, label: 'Work' })]);
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await screen.findByTestId('tracker-board-connection-off');
    expect(screen.getByTestId('tracker-board-row').textContent).toContain('WEB · Work');
  });

  it('says the connections could not be read, with Try again, and offers nothing until they are', async () => {
    const answers: (() => Promise<unknown>)[] = [
      async () => { throw new TransportError('method_error', 'store: disk I/O error', { detail: { ref: 'r', method: 'TrackerConnections', at: 1, chain: ['store: disk I/O error'] } }); },
      async () => [trackerConnection()],
    ];
    setBindingMock('TrackerConnections', () => answers.shift()!());
    render(TrackerBoardsSettings);
    expect((await screen.findByTestId('settings-tracker-boards-connections-error')).textContent).toBe('Store: disk I/O error.');
    // Nothing is offered from connections that were never read.
    expect(screen.queryByTestId('settings-tracker-boards-project')).toBeNull();
    expect(screen.queryByTestId('tracker-board-add-open')).toBeNull();
    await fireEvent.click(screen.getByTestId('settings-tracker-boards-connections-retry'));
    const add = (await screen.findByTestId('tracker-board-add-open')) as HTMLButtonElement;
    expect(add.disabled).toBe(false);
    expect(screen.queryByTestId('settings-tracker-boards-connections-error')).toBeNull();
  });

  it('names a board that was never read by its id', async () => {
    stored.p1 = [trackerBoardChoice({ board: '42', name: '', tracker_project: '' })];
    await renderOnP1();
    await waitFor(() => expect(names()).toEqual(['Board 42']));
  });

  describe('on an attached computer', () => {
    beforeEach(() => {
      stageBackend();
    });
    afterEach(() => {
      resetStagedBackends();
    });

    // v0.0.19 grants a full-access device every scope it has, none of
    // them a tracker scope, and answers the tracker methods as missing.
    it('says a computer on v0.0.19 has no issue trackers, not that this device may not read them', async () => {
      setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
      answerAsV0019();
      let refuse!: (err: unknown) => void;
      setBindingMock('TrackerConnections', () => new Promise<never>((_resolve, reject) => (refuse = reject)));
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerBoardsSettings, needsComputer: false } });
      // Until the computer answers, its grant alone cannot say which it is.
      await view.findByTestId('settings-tracker-boards-waiting');
      expect(view.queryByTestId('settings-tracker-boards-refused')).toBeNull();
      refuse(methodNotFound());
      expect((await view.findByTestId('settings-older-computer')).textContent).toBe('Laptop runs an older version: it has no issue trackers.');
      expect(view.queryByTestId('settings-tracker-boards-refused')).toBeNull();
    });

    it('shows an unreachable computer whose grant lacks the tracker scopes as a failure with Try again', async () => {
      setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
      setBindingMock('TrackerConnections', async () => { throw new DisconnectedError('socket closed'); });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerBoardsSettings, needsComputer: false } });
      await view.findByTestId('settings-tracker-boards-connections-error');
      expect(view.getByTestId('settings-tracker-boards-connections-retry')).toBeTruthy();
      expect(view.queryByTestId('settings-tracker-boards-refused')).toBeNull();
    });

    it('keeps a current computer’s refusal of tracker:read a refusal', async () => {
      setCarriedSessionScopes('laptop', ['threads:read']);
      setBindingMock('TrackerConnections', async () => {
        throw new TransportError('scope_required', 'tracker:read required', { scope: 'tracker:read' });
      });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerBoardsSettings, needsComputer: false } });
      await view.findByTestId('settings-tracker-boards-refused');
      expect(view.queryByTestId('settings-older-computer')).toBeNull();
    });
  });
});
