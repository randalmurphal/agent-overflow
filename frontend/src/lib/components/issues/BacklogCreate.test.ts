// New ticket at a Backlog section's foot, as rendered: it reads nothing
// until it opens; on a board of several projects it asks which, from the
// connection's projects, read once; a project's types and a type's fields
// are read once each and its required fields asked; Add makes the ticket
// in the section's sprint, and the next form on the board starts from the
// project and type picked; a required field the app cannot set says to
// make the ticket in the tracker.

import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { FieldView, IssueTypeView, ProjectView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import BacklogCreate from './BacklogCreate.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import type { BacklogSection } from '../../stores/trackerBacklogModel';
import { boardWriteContext } from '../../stores/trackerTicketWrites.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setBindingMock } from '../../../test/mocks/bindings-app';

const REF = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
const B = boardWriteContext(REF);
const SPRINT: BacklogSection = {
  list: 'sprint:9', kind: 'future', section: { kind: 'sprint', sprint: '9' },
  sprint: { id: '9', name: 'Sprint 9', state: 'future', total: { rows: 0, estimate: 0 } },
  rows: [], unread: false, more: false, total: { rows: 0, estimate: 0 },
} as unknown as BacklogSection;

const PROJECTS = [{ id: '10000', key: 'WEB', name: 'Web' }, { id: '10001', key: 'APP', name: 'App' }] as ProjectView[];
const TYPES = [{ id: '5', name: 'Sub-task', level: -1 }, { id: '6', name: 'Task', level: 0 }, { id: '7', name: 'Bug', level: 0 }] as IssueTypeView[];
const PRIORITY = { id: 'priority', name: 'Priority', kind: 'option', required: true, editable: true } as FieldView;

function menu(): HTMLElement {
  return document.body.querySelector<HTMLElement>('[data-popover]')!;
}

function field(container: HTMLElement, id: string): HTMLElement {
  return container.querySelector<HTMLElement>(`[data-testid="ticket-field"][data-field="${id}"]`)!;
}

async function pick(container: HTMLElement, id: string, name: string): Promise<void> {
  await fireEvent.click(within(field(container, id)).getByTestId('ticket-field-value'));
  const item = await waitFor(() => within(menu()).getAllByTestId('ticket-picker-item').find((el) => el.textContent?.includes(name))!);
  await fireEvent.click(item);
}

function open(project: string) {
  return render(BacklogCreate, { context: ticketWriteSurfaceContext(() => true), props: { b: B, boardKey: 'board-key', boardRef: REF, sec: SPRINT, project, typed: true } });
}

describe('New ticket', () => {
  it('on a board of several projects asks which, reading the projects, types and fields once each, and makes the ticket in the sprint', async () => {
    const projects = setBindingMock('TrackerProjects', async () => PROJECTS);
    const types = setBindingMock('TrackerIssueTypes', async () => TYPES);
    const fields = setBindingMock('TrackerCreateFields', async () => [PRIORITY]);
    setBindingMock('TrackerChoices', async () => ({ choices: [{ id: '2', name: 'High' }], more: false }));
    const created = setBindingMock('CreateTrackerIssue', async () => ({ parts: [{ step: 'create', state: 'applied' }] }) as WriteView);
    const first = open('');
    expect(projects).not.toHaveBeenCalled();
    await fireEvent.click(first.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(first.getByTestId('issues-backlog-create-pick-project')).toBeTruthy());
    expect(projects).toHaveBeenCalledTimes(1);
    expect(types).not.toHaveBeenCalled();

    await pick(first.container, 'project', 'App');
    // The project's first type that is not a sub-task.
    await waitFor(() => expect(within(field(first.container, 'type')).getByTestId('ticket-field-value').textContent?.trim()).toBe('Task'));
    expect(types.mock.calls).toEqual([['c1', '10001']]);
    expect(fields.mock.calls).toEqual([['c1', '10001', '6']]);
    await pick(first.container, 'type', 'Bug');
    await waitFor(() => expect(fields.mock.calls).toEqual([['c1', '10001', '6'], ['c1', '10001', '7']]));
    await waitFor(() => expect(first.getByTestId('issues-backlog-create-missing').textContent).toBe('Needs Priority'));
    await pick(first.container, 'priority', 'High');
    const title = first.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Made on a board of two projects' } });
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(created).toHaveBeenCalledTimes(1));
    expect(created.mock.calls[0]).toEqual(['c1', {
      board: '1', project: '10001', type: '7', title: 'Made on a board of two projects', sprint: '9',
      fields: [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } }],
    }]);
    await waitFor(() => expect(title.value).toBe(''));
    first.unmount();

    // The next form on the board starts from App and Bug, reading nothing
    // it holds.
    const next = open('');
    await fireEvent.click(next.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(within(field(next.container, 'project')).getByTestId('ticket-field-value').textContent?.trim()).toBe('App'));
    expect(within(field(next.container, 'type')).getByTestId('ticket-field-value').textContent?.trim()).toBe('Bug');
    expect(projects).toHaveBeenCalledTimes(1);
    expect(types).toHaveBeenCalledTimes(1);
    expect(fields).toHaveBeenCalledTimes(2);
    next.unmount();
  });

  it('once the tracker made the ticket, is done with its title even when its move into the sprint failed, and offers that move', async () => {
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({
      parts: [{ step: 'create', state: 'applied', issue: '10012' }, { step: 'sprint_move', state: 'refused', issue: '10012', refusal: { reason: 'tracker', message: 'Sprint 9 is closed.' } }],
      created: '10012', created_key: 'WEB-12',
    }) as WriteView);
    const moved = setBindingMock('MoveTrackerIssuesToSprint', async () => ({ parts: [{ step: 'sprint_move', state: 'applied', issue: '10012' }] }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true));
    await fireEvent.input(title, { target: { value: 'Made once' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(view.getByTestId('ticket-write-outcome').textContent).toContain('Created WEB-12, but it was not moved into its sprint: Sprint 9 is closed.'));
    // The title went with the made ticket: Add sends nothing more.
    await waitFor(() => expect(title.value).toBe(''));
    expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.keyDown(title, { key: 'Enter' });
    expect(created).toHaveBeenCalledTimes(1);
    await fireEvent.click(view.getByTestId('issues-backlog-create-move'));
    await waitFor(() => expect(moved).toHaveBeenCalledTimes(1));
    expect(moved.mock.calls[0]).toEqual(['c1', { board: '1', sprint: '9', issues: ['10012'], prev: undefined, next: undefined }]);
    expect(view.queryByTestId('issues-backlog-create-move')).toBeNull();
    view.unmount();
  });

  it('says it may not be in its sprint when the move\'s answer was lost, and does not make it again', async () => {
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => []);
    const created = setBindingMock('CreateTrackerIssue', async () => ({
      parts: [{ step: 'create', state: 'applied', issue: '10012' }, { step: 'sprint_move', state: 'unknown', issue: '10012', failure: { kind: 'transient', message: 'Jira did not answer in full.', at: 1 } }],
      created: '10012',
    }) as WriteView);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    const title = view.getByTestId('issues-backlog-create-title') as HTMLInputElement;
    await fireEvent.input(title, { target: { value: 'Made once' } });
    await waitFor(() => expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(view.getByTestId('issues-backlog-create-submit'));
    await waitFor(() => expect(view.getByTestId('ticket-write-outcome').textContent).toContain('Created the ticket; it may not be in its sprint: Jira did not answer in full.'));
    await waitFor(() => expect(title.value).toBe(''));
    expect(view.getByTestId('issues-backlog-create-move').textContent).toBe('Move it into Sprint 9');
    expect(created).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it('says to make the ticket in the tracker when it requires a field the app cannot set', async () => {
    setBindingMock('TrackerIssueTypes', async () => TYPES);
    setBindingMock('TrackerCreateFields', async () => [{ id: 'customfield_1', name: 'Team', kind: 'cascading', required: true, editable: true } as FieldView]);
    const view = open('10000');
    await fireEvent.click(view.getByTestId('issues-backlog-new'));
    await waitFor(() => expect(view.getByTestId('issues-backlog-create-unsettable').textContent).toBe('A Task needs Team, which the app cannot set. Make it in the tracker.'));
    expect((view.getByTestId('issues-backlog-create-submit') as HTMLButtonElement).disabled).toBe(true);
    // A board of one project asks none.
    expect(field(view.container, 'project')).toBeNull();
    view.unmount();
  });
});
